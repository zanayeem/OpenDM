package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Manager struct {
	mu        sync.Mutex
	downloads map[string]*Download
	order     []string

	cfgMu    sync.RWMutex
	config   Config
	httpC    *http.Client
	dataDir  string
	limiter  *RateLimiter
	dirty    chan struct{}
	notify   func(title, msg string)
	shutting bool
}

func NewManager(cfg Config, dataDir string) *Manager {
	m := &Manager{
		downloads: map[string]*Download{},
		dataDir:   dataDir,
		limiter:   &RateLimiter{},
		dirty:     make(chan struct{}, 1),
	}
	m.applyConfig(cfg)
	go m.saver()
	go m.ticker()
	return m
}

func (m *Manager) cfg() Config {
	m.cfgMu.RLock()
	defer m.cfgMu.RUnlock()
	return m.config
}

func (m *Manager) client() *http.Client {
	m.cfgMu.RLock()
	defer m.cfgMu.RUnlock()
	return m.httpC
}

func (m *Manager) applyConfig(c Config) {
	c.normalize()
	jar, _ := cookiejar.New(nil)
	proxy := http.ProxyFromEnvironment
	if c.Proxy != "" {
		if u, err := url.Parse(c.Proxy); err == nil && u.Host != "" {
			proxy = http.ProxyURL(u)
		}
	}
	tr := &http.Transport{
		Proxy: proxy,
		DialContext: (&net.Dialer{
			Timeout:   20 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// Multiple TCP connections are the whole point; HTTP/2 would multiplex
		// every segment over one connection and erase the speed-up.
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   20 * time.Second,
		ResponseHeaderTimeout: 40 * time.Second,
		DisableCompression:    true, // byte ranges must refer to the raw file
	}
	m.cfgMu.Lock()
	m.config = c
	m.httpC = &http.Client{Transport: tr, Jar: jar}
	m.cfgMu.Unlock()
	m.limiter.SetRate(int64(c.SpeedLimitKB) * 1024)
}

// ---------------------------------------------------------------- persistence

func (m *Manager) stateFile() string { return filepath.Join(m.dataDir, "downloads.json") }

func (m *Manager) Load() {
	b, err := os.ReadFile(m.stateFile())
	if err != nil {
		return
	}
	var list []*Download
	if err := json.Unmarshal(b, &list); err != nil {
		log.Printf("state file unreadable: %v", err)
		return
	}
	m.mu.Lock()
	for _, d := range list {
		if d.ID == "" || d.URL == "" {
			continue
		}
		if d.Status == StDownloading {
			d.Status = StQueued
		}
		if d.Connections <= 0 {
			d.Connections = m.cfg().Connections
		}
		m.downloads[d.ID] = d
		m.order = append(m.order, d.ID)
	}
	m.mu.Unlock()
	m.schedule()
}

func (m *Manager) save() {
	m.mu.Lock()
	list := make([]json.RawMessage, 0, len(m.order))
	for _, id := range m.order {
		d := m.downloads[id]
		d.mu.Lock()
		b, err := json.Marshal(d)
		d.mu.Unlock()
		if err == nil {
			list = append(list, b)
		}
	}
	m.mu.Unlock()
	b, _ := json.MarshalIndent(list, "", " ")
	if err := writeFileAtomic(m.stateFile(), b); err != nil {
		log.Printf("save state: %v", err)
	}
}

func (m *Manager) markDirty() {
	select {
	case m.dirty <- struct{}{}:
	default:
	}
}

func (m *Manager) saver() {
	for range m.dirty {
		m.save()
		time.Sleep(1500 * time.Millisecond) // coalesce bursts of progress updates
	}
}

// ticker computes speeds and keeps progress persisted while downloading.
func (m *Manager) ticker() {
	t := time.NewTicker(500 * time.Millisecond)
	for now := range t.C {
		active := false
		m.mu.Lock()
		for _, d := range m.downloads {
			d.mu.Lock()
			if d.Status == StDownloading {
				active = true
				cur := d.downloaded()
				if !d.lastTick.IsZero() {
					dt := now.Sub(d.lastTick).Seconds()
					inst := float64(cur-d.lastBytes) / dt
					if inst < 0 {
						inst = 0
					}
					if d.speed == 0 {
						d.speed = inst
					} else {
						d.speed = d.speed*0.6 + inst*0.4
					}
				}
				d.lastBytes, d.lastTick = cur, now
			} else {
				d.speed, d.lastTick = 0, time.Time{}
			}
			d.mu.Unlock()
		}
		m.mu.Unlock()
		if active {
			m.markDirty()
		}
	}
}

// ---------------------------------------------------------------- scheduling

func (m *Manager) schedule() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shutting {
		return
	}
	limit := m.cfg().MaxConcurrent
	active := 0
	for _, id := range m.order {
		d := m.downloads[id]
		d.mu.Lock()
		if d.Status == StDownloading {
			active++
		}
		d.mu.Unlock()
	}
	for _, id := range m.order {
		if active >= limit {
			return
		}
		d := m.downloads[id]
		d.mu.Lock()
		if d.Status == StQueued && d.cancel == nil {
			m.startLocked(d)
			active++
		}
		d.mu.Unlock()
	}
}

// startLocked must be called with d.mu held.
func (m *Manager) startLocked(d *Download) {
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.done = make(chan struct{})
	d.stopTo = StPaused
	d.Status = StDownloading
	d.Error = ""
	go m.run(ctx, d)
}

func (m *Manager) pathTaken(p, exceptID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, d := range m.downloads {
		if id == exceptID {
			continue
		}
		d.mu.Lock()
		same := d.FileName != "" && strings.EqualFold(d.filePath(), p) && d.Status != StCompleted
		d.mu.Unlock()
		if same {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- actions

type AddRequest struct {
	URL         string            `json:"url"`
	URLs        []string          `json:"urls"`
	FileName    string            `json:"fileName"`
	Dir         string            `json:"dir"`
	Connections int               `json:"connections"`
	Paused      bool              `json:"paused"`
	Referer     string            `json:"referer"`
	Cookie      string            `json:"cookie"`
	UserAgent   string            `json:"userAgent"`
	Headers     map[string]string `json:"headers"`
}

func validURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	return u.String(), true
}

func (m *Manager) Add(req AddRequest) ([]string, error) {
	urls := req.URLs
	if req.URL != "" {
		urls = append([]string{req.URL}, urls...)
	}
	headers := map[string]string{}
	for k, v := range req.Headers {
		headers[k] = v
	}
	if req.Referer != "" {
		headers["Referer"] = req.Referer
	}
	if req.Cookie != "" {
		headers["Cookie"] = req.Cookie
	}
	if req.UserAgent != "" {
		headers["User-Agent"] = req.UserAgent
	}
	cfg := m.cfg()
	conns := req.Connections
	if conns <= 0 {
		conns = cfg.Connections
	}
	conns = clamp(conns, 1, 32)

	var ids []string
	m.mu.Lock()
	for _, raw := range urls {
		u, ok := validURL(raw)
		if !ok {
			continue
		}
		d := &Download{
			ID:          newID(),
			URL:         u,
			FileName:    guessNameFromURL(u),
			Size:        -1,
			Status:      StQueued,
			Connections: conns,
			Headers:     headers,
			AddedAt:     time.Now(),
		}
		if req.Paused {
			d.Status = StPaused
		}
		if req.FileName != "" && len(urls) == 1 {
			d.FileName = sanitizeFileName(req.FileName)
			d.NameFixed = true
		}
		d.Category = categorize(d.FileName)
		d.Dir = cfg.DownloadDir
		if req.Dir != "" {
			d.Dir = filepath.Clean(req.Dir)
			d.DirFixed = true
		} else if cfg.SortByCategory {
			d.Dir = filepath.Join(cfg.DownloadDir, d.Category)
		}
		m.downloads[d.ID] = d
		m.order = append(m.order, d.ID)
		ids = append(ids, d.ID)
	}
	m.mu.Unlock()
	if len(ids) == 0 {
		return nil, errors.New("no valid http(s) links found")
	}
	m.markDirty()
	m.schedule()
	return ids, nil
}

func (m *Manager) get(id string) *Download {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.downloads[id]
}

func (m *Manager) Pause(id string) {
	d := m.get(id)
	if d == nil {
		return
	}
	d.mu.Lock()
	switch d.Status {
	case StDownloading:
		d.stopTo = StPaused
		if d.cancel != nil {
			d.cancel()
		}
	case StQueued:
		d.Status = StPaused
	}
	d.mu.Unlock()
	m.markDirty()
}

func (m *Manager) Resume(id string) {
	d := m.get(id)
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.Status == StPaused || d.Status == StError {
		d.Status = StQueued
		d.Error = ""
	}
	d.mu.Unlock()
	m.markDirty()
	m.schedule()
}

// stopAndWait cancels a running job and waits for its goroutine to exit.
func (m *Manager) stopAndWait(d *Download, to Status) {
	d.mu.Lock()
	var done chan struct{}
	if d.cancel != nil {
		d.stopTo = to
		d.cancel()
		done = d.done
	}
	d.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(15 * time.Second):
		}
	}
}

func (m *Manager) Restart(id string) {
	d := m.get(id)
	if d == nil {
		return
	}
	m.stopAndWait(d, StPaused)
	d.mu.Lock()
	if d.Probed && d.Status != StCompleted {
		_ = os.Remove(d.partPath())
	}
	d.Probed = false
	d.Segments = nil
	d.Size = -1
	d.Status = StQueued
	d.Error = ""
	d.CompletedAt = nil
	d.mu.Unlock()
	m.markDirty()
	m.schedule()
}

func (m *Manager) SetURL(id, raw string) error {
	u, ok := validURL(raw)
	if !ok {
		return errors.New("invalid URL")
	}
	d := m.get(id)
	if d == nil {
		return errors.New("not found")
	}
	m.stopAndWait(d, StPaused)
	d.mu.Lock()
	d.URL = u
	d.mu.Unlock()
	m.markDirty()
	return nil
}

func (m *Manager) Remove(id string, deleteFile bool) {
	m.mu.Lock()
	d := m.downloads[id]
	if d == nil {
		m.mu.Unlock()
		return
	}
	delete(m.downloads, id)
	for i, x := range m.order {
		if x == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	m.mu.Unlock()

	d.mu.Lock()
	if d.cancel != nil {
		d.removed, d.deleteFile = true, deleteFile
		d.cancel() // run() cleans up the .part file once its handle is closed
	} else {
		if d.Probed && d.Status != StCompleted {
			_ = os.Remove(d.partPath())
		}
		if deleteFile && d.Status == StCompleted {
			_ = os.Remove(d.filePath())
		}
	}
	d.mu.Unlock()
	m.markDirty()
	m.schedule()
}

func (m *Manager) ids() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.order...)
}

func (m *Manager) PauseAll() {
	for _, id := range m.ids() {
		m.Pause(id)
	}
}

func (m *Manager) ResumeAll() {
	for _, id := range m.ids() {
		d := m.get(id)
		if d == nil {
			continue
		}
		d.mu.Lock()
		if d.Status == StPaused || d.Status == StError {
			d.Status, d.Error = StQueued, ""
		}
		d.mu.Unlock()
	}
	m.markDirty()
	m.schedule()
}

func (m *Manager) ClearCompleted() {
	for _, id := range m.ids() {
		d := m.get(id)
		if d == nil {
			continue
		}
		d.mu.Lock()
		completed := d.Status == StCompleted
		d.mu.Unlock()
		if completed {
			m.Remove(id, false)
		}
	}
}

// Shutdown stops everything, remembering which jobs were active so they
// resume automatically on next launch.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	m.shutting = true
	list := make([]*Download, 0, len(m.downloads))
	for _, d := range m.downloads {
		list = append(list, d)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, d := range list {
		wg.Add(1)
		go func(d *Download) { defer wg.Done(); m.stopAndWait(d, StQueued) }(d)
	}
	wg.Wait()
	m.save()
}

// ---------------------------------------------------------------- views

type DownloadView struct {
	ID          string     `json:"id"`
	URL         string     `json:"url"`
	FileName    string     `json:"fileName"`
	Dir         string     `json:"dir"`
	Path        string     `json:"path"`
	Category    string     `json:"category"`
	Size        int64      `json:"size"`
	Downloaded  int64      `json:"downloaded"`
	Status      Status     `json:"status"`
	Error       string     `json:"error,omitempty"`
	Resumable   bool       `json:"resumable"`
	Probed      bool       `json:"probed"`
	Connections int        `json:"connections"`
	ActiveConns int        `json:"activeConns"`
	Speed       float64    `json:"speed"`
	ETA         float64    `json:"eta"`
	AddedAt     time.Time  `json:"addedAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	Segments    [][3]int64 `json:"segments"`
}

func (m *Manager) Views() []DownloadView {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]DownloadView, 0, len(m.order))
	for _, id := range m.order {
		d := m.downloads[id]
		d.mu.Lock()
		v := DownloadView{
			ID: d.ID, URL: d.URL, FileName: d.FileName, Dir: d.Dir, Path: d.filePath(),
			Category: d.Category, Size: d.Size, Downloaded: d.downloaded(), Status: d.Status,
			Error: d.Error, Resumable: d.Resumable, Probed: d.Probed, Connections: d.Connections,
			ActiveConns: d.activeConns, Speed: d.speed, AddedAt: d.AddedAt, CompletedAt: d.CompletedAt,
		}
		if d.Status == StCompleted && d.Size > 0 {
			v.Downloaded = d.Size
		}
		if v.Speed > 1 && v.Size > 0 {
			v.ETA = float64(v.Size-v.Downloaded) / v.Speed
		}
		for _, s := range d.Segments {
			v.Segments = append(v.Segments, [3]int64{s.Start, s.End, s.Done})
		}
		d.mu.Unlock()
		out = append(out, v)
	}
	return out
}
