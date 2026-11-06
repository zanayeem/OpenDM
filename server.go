package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

//go:embed extension
var extFS embed.FS

type Server struct {
	m      *Manager
	port   int
	onQuit func()
	onShow func()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20)).Decode(v)
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"app": appName, "version": appVersion})
	})

	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		views := s.m.Views()
		var speed float64
		active := 0
		for _, v := range views {
			speed += v.Speed
			if v.Status == StDownloading {
				active++
			}
		}
		writeJSON(w, 200, map[string]any{
			"downloads": views,
			"speed":     speed,
			"active":    active,
			"version":   appVersion,
		})
	})

	mux.HandleFunc("POST /api/add", func(w http.ResponseWriter, r *http.Request) {
		var req AddRequest
		if err := readJSON(r, &req); err != nil {
			writeErr(w, 400, err)
			return
		}
		ids, err := s.m.Add(req)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ids": ids})
	})

	mux.HandleFunc("POST /api/probe", func(w http.ResponseWriter, r *http.Request) {
		var req AddRequest
		if err := readJSON(r, &req); err != nil {
			writeErr(w, 400, err)
			return
		}
		u, ok := validURL(req.URL)
		if !ok {
			writeErr(w, 400, errors.New("enter a valid http:// or https:// link"))
			return
		}
		h := map[string]string{}
		if req.Referer != "" {
			h["Referer"] = req.Referer
		}
		if req.Cookie != "" {
			h["Cookie"] = req.Cookie
		}
		info, err := s.m.probe(r.Context(), u, h)
		if err != nil {
			writeErr(w, 502, err)
			return
		}
		writeJSON(w, 200, info)
	})

	mux.HandleFunc("POST /api/action", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Action     string   `json:"action"`
			IDs        []string `json:"ids"`
			DeleteFile bool     `json:"deleteFile"`
			URL        string   `json:"url"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, 400, err)
			return
		}
		for _, id := range req.IDs {
			switch req.Action {
			case "pause":
				s.m.Pause(id)
			case "resume":
				s.m.Resume(id)
			case "restart":
				go s.m.Restart(id)
			case "remove":
				s.m.Remove(id, req.DeleteFile)
			case "seturl":
				if err := s.m.SetURL(id, req.URL); err != nil {
					writeErr(w, 400, err)
					return
				}
			case "open", "folder":
				d := s.m.get(id)
				if d == nil {
					continue
				}
				d.mu.Lock()
				p, st := d.filePath(), d.Status
				d.mu.Unlock()
				if req.Action == "open" {
					if st != StCompleted {
						writeErr(w, 400, errors.New("the file is not finished yet"))
						return
					}
					_ = openPath(p)
				} else {
					if !exists(p) {
						p = filepath.Dir(p)
						_ = os.MkdirAll(p, 0o755)
						_ = openPath(p)
					} else {
						_ = revealFile(p)
					}
				}
			default:
				writeErr(w, 400, errors.New("unknown action"))
				return
			}
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	mux.HandleFunc("POST /api/all/{action}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("action") {
		case "pause":
			s.m.PauseAll()
		case "resume":
			s.m.ResumeAll()
		case "clear-completed":
			s.m.ClearCompleted()
		default:
			writeErr(w, 404, errors.New("unknown action"))
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	mux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"settings": s.m.cfg(), "platform": platformName(), "dataDir": s.m.dataDir})
	})

	mux.HandleFunc("PUT /api/settings", func(w http.ResponseWriter, r *http.Request) {
		old := s.m.cfg()
		c := old
		if err := readJSON(r, &c); err != nil {
			writeErr(w, 400, err)
			return
		}
		c.normalize()
		if err := os.MkdirAll(c.DownloadDir, 0o755); err != nil {
			writeErr(w, 400, errors.New("download folder cannot be created: "+err.Error()))
			return
		}
		if err := saveConfig(s.m.dataDir, c); err != nil {
			writeErr(w, 500, err)
			return
		}
		s.m.applyConfig(c)
		if c.StartWithWindows != old.StartWithWindows {
			setAutostart(c.StartWithWindows)
		}
		s.m.schedule()
		writeJSON(w, 200, map[string]any{"settings": c, "restartNeeded": c.Port != old.Port})
	})

	mux.HandleFunc("POST /api/pick-folder", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Start string `json:"start"`
		}
		_ = readJSON(r, &req)
		p, err := pickFolder(req.Start)
		if err != nil {
			writeErr(w, 501, err)
			return
		}
		writeJSON(w, 200, map[string]string{"path": p})
	})

	mux.HandleFunc("POST /api/open-downloads", func(w http.ResponseWriter, r *http.Request) {
		dir := s.m.cfg().DownloadDir
		_ = os.MkdirAll(dir, 0o755)
		_ = openPath(dir)
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	mux.HandleFunc("POST /api/export-extension", func(w http.ResponseWriter, r *http.Request) {
		dst := filepath.Join(s.m.dataDir, "browser-extension")
		if err := exportExtension(dst); err != nil {
			writeErr(w, 500, err)
			return
		}
		_ = openPath(dst)
		writeJSON(w, 200, map[string]string{"path": dst})
	})

	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, r *http.Request) {
		if s.onShow != nil {
			go s.onShow()
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	mux.HandleFunc("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]bool{"ok": true})
		if s.onQuit != nil {
			go func() { time.Sleep(200 * time.Millisecond); s.onQuit() }()
		}
	})

	sub, _ := fs.Sub(webFS, "web")
	static := http.FileServerFS(sub)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	})

	return s.guard(mux)
}

// guard blocks DNS-rebinding and cross-site requests: only this machine's UI
// and browser extensions may drive the API.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host != "127.0.0.1" && host != "localhost" && host != "::1" && host != "[::1]" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !allowedOrigin(r.Header.Get("Origin")) {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				http.Error(w, "json required", http.StatusUnsupportedMediaType)
				return
			}
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func allowedOrigin(origin string) bool {
	if origin == "" || origin == "null" {
		return origin == ""
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "chrome-extension", "moz-extension", "extension":
		return true
	case "http":
		h := u.Hostname()
		return h == "127.0.0.1" || h == "localhost" || h == "::1"
	}
	return false
}

func exportExtension(dst string) error {
	_ = os.RemoveAll(dst)
	return fs.WalkDir(extFS, "extension", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, "extension"), "/")
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := extFS.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

func pingExisting(port int) bool {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + itoa(port) + "/api/ping")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var v map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return v["app"] == appName
}

func askExistingToShow(port int) {
	c := &http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://127.0.0.1:"+itoa(port)+"/api/show", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := c.Do(req); err == nil {
		resp.Body.Close()
	}
}
