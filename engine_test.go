package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var payload = func() []byte {
	b := make([]byte, 24<<20+12345) // odd size on purpose
	rand.New(rand.NewSource(42)).Read(b)
	return b
}()

type slowWriter struct {
	http.ResponseWriter
	delay time.Duration
}

func (s slowWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := min(len(p), 32*1024)
		w, err := s.ResponseWriter.Write(p[:n])
		total += w
		if err != nil {
			return total, err
		}
		p = p[n:]
		time.Sleep(s.delay)
	}
	return total, nil
}

var flakyHits atomic.Int32

func testServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/file.bin", func(w http.ResponseWriter, r *http.Request) {
		// the first segment is much slower so other connections steal from it
		delay := 2 * time.Millisecond
		if strings.HasPrefix(r.Header.Get("Range"), "bytes=0-") && r.Header.Get("Range") != "bytes=0-" {
			delay = 25 * time.Millisecond
		}
		http.ServeContent(slowWriter{w, delay}, r, "file.bin", time.Now(), bytes.NewReader(payload))
	})
	mux.HandleFunc("/noranges", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.Write(payload)
	})
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush() // chunked, unknown length
		w.Write(payload[:3<<20])
	})
	mux.HandleFunc("/cd", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="report"; filename*=UTF-8''Q3%20report%20%E2%9C%93.pdf`)
		http.ServeContent(w, r, "", time.Now(), bytes.NewReader(payload[:1<<20]))
	})
	mux.HandleFunc("/flaky.bin", func(w http.ResponseWriter, r *http.Request) {
		// every 3rd ranged request sends a correct header, a little data, then drops
		var a, b int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err == nil && flakyHits.Add(1)%3 == 0 {
			conn, buf, _ := w.(http.Hijacker).Hijack()
			fmt.Fprintf(buf, "HTTP/1.1 206 Partial Content\r\nContent-Length: %d\r\nContent-Range: bytes %d-%d/%d\r\n\r\n", b-a+1, a, b, len(payload))
			buf.Write(payload[a:min(a+70000, b+1)])
			buf.Flush()
			conn.Close()
			return
		}
		http.ServeContent(w, r, "flaky.bin", time.Now(), bytes.NewReader(payload))
	})
	mux.HandleFunc("/gone", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", 404) })
	return httptest.NewServer(mux)
}

func newTestManager(t *testing.T, data, dl string) *Manager {
	cfg := defaultConfig()
	cfg.DownloadDir = dl
	cfg.SortByCategory = false
	cfg.Connections = 8
	cfg.Retries = 8
	return NewManager(cfg, data)
}

func waitStatus(t *testing.T, m *Manager, id string, want Status, timeout time.Duration) DownloadView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, v := range m.Views() {
			if v.ID == id {
				if v.Status == want {
					return v
				}
				if v.Status == StError && want != StError {
					t.Fatalf("download failed: %s", v.Error)
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", want)
	return DownloadView{}
}

func checkFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != sha256.Sum256(want) {
		t.Fatalf("checksum mismatch: got %d bytes want %d", len(got), len(want))
	}
}

func TestMultiConnectionWithDynamicSplit(t *testing.T) {
	srv := testServer()
	defer srv.Close()
	dl := t.TempDir()
	m := newTestManager(t, t.TempDir(), dl)
	ids, err := m.Add(AddRequest{URL: srv.URL + "/file.bin"})
	if err != nil {
		t.Fatal(err)
	}
	v := waitStatus(t, m, ids[0], StCompleted, 60*time.Second)
	checkFile(t, v.Path, payload)
	if len(v.Segments) <= 8 {
		t.Fatalf("expected dynamic splitting to create >8 segments, got %d", len(v.Segments))
	}
	if exists(v.Path + partSuffix) {
		t.Fatal("part file left behind")
	}
	t.Logf("segments=%d resumable=%v", len(v.Segments), v.Resumable)
}

func TestPauseResumeAcrossRestart(t *testing.T) {
	srv := testServer()
	defer srv.Close()
	data, dl := t.TempDir(), t.TempDir()
	m := newTestManager(t, data, dl)
	m.limiter.SetRate(6 << 20) // slow enough to pause mid-way
	ids, _ := m.Add(AddRequest{URL: srv.URL + "/file.bin"})
	id := ids[0]
	for {
		v := m.Views()[0]
		if v.Size > 0 && v.Downloaded > v.Size/3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	m.Pause(id)
	paused := waitStatus(t, m, id, StPaused, 10*time.Second)
	m.Shutdown() // persists state like quitting the app

	// "restart the app"
	m2 := newTestManager(t, data, dl)
	m2.Load()
	v := m2.Views()[0]
	if v.Downloaded < paused.Downloaded || v.Status != StPaused {
		t.Fatalf("state not restored: %+v", v)
	}
	m2.Resume(id)
	done := waitStatus(t, m2, id, StCompleted, 60*time.Second)
	checkFile(t, done.Path, payload)
	t.Logf("paused at %d/%d bytes, resumed OK", paused.Downloaded, paused.Size)
}

func TestNoRangeSupport(t *testing.T) {
	srv := testServer()
	defer srv.Close()
	m := newTestManager(t, t.TempDir(), t.TempDir())
	ids, _ := m.Add(AddRequest{URL: srv.URL + "/noranges"})
	v := waitStatus(t, m, ids[0], StCompleted, 30*time.Second)
	if v.Resumable {
		t.Fatal("should not be resumable")
	}
	checkFile(t, v.Path, payload)
}

func TestUnknownLengthStream(t *testing.T) {
	srv := testServer()
	defer srv.Close()
	m := newTestManager(t, t.TempDir(), t.TempDir())
	ids, _ := m.Add(AddRequest{URL: srv.URL + "/stream"})
	v := waitStatus(t, m, ids[0], StCompleted, 30*time.Second)
	checkFile(t, v.Path, payload[:3<<20])
	if v.Size != 3<<20 {
		t.Fatalf("size = %d", v.Size)
	}
}

func TestFlakyServerRetries(t *testing.T) {
	srv := testServer()
	defer srv.Close()
	m := newTestManager(t, t.TempDir(), t.TempDir())
	ids, _ := m.Add(AddRequest{URL: srv.URL + "/flaky.bin"})
	v := waitStatus(t, m, ids[0], StCompleted, 60*time.Second)
	checkFile(t, v.Path, payload)
	t.Logf("survived %d dropped connections", flakyHits.Load()/3)
}

func TestContentDispositionName(t *testing.T) {
	srv := testServer()
	defer srv.Close()
	dl := t.TempDir()
	m := newTestManager(t, t.TempDir(), dl)
	ids, _ := m.Add(AddRequest{URL: srv.URL + "/cd"})
	v := waitStatus(t, m, ids[0], StCompleted, 30*time.Second)
	if v.FileName != "Q3 report ✓.pdf" {
		t.Fatalf("name = %q", v.FileName)
	}
	// same file again gets a unique name
	ids, _ = m.Add(AddRequest{URL: srv.URL + "/cd"})
	v = waitStatus(t, m, ids[0], StCompleted, 30*time.Second)
	if v.FileName != "Q3 report ✓ (1).pdf" {
		t.Fatalf("second name = %q", v.FileName)
	}
}

func TestHTTPErrorFails(t *testing.T) {
	srv := testServer()
	defer srv.Close()
	m := newTestManager(t, t.TempDir(), t.TempDir())
	ids, _ := m.Add(AddRequest{URL: srv.URL + "/gone"})
	v := waitStatus(t, m, ids[0], StError, 10*time.Second)
	if !strings.Contains(v.Error, "404") {
		t.Fatalf("error = %q", v.Error)
	}
}

func TestRemoveCleansPartFile(t *testing.T) {
	srv := testServer()
	defer srv.Close()
	dl := t.TempDir()
	m := newTestManager(t, t.TempDir(), dl)
	m.limiter.SetRate(2 << 20)
	ids, _ := m.Add(AddRequest{URL: srv.URL + "/file.bin"})
	time.Sleep(800 * time.Millisecond)
	m.Remove(ids[0], true)
	time.Sleep(800 * time.Millisecond)
	left, _ := filepath.Glob(filepath.Join(dl, "*"))
	if len(left) != 0 {
		t.Fatalf("leftover files: %v", left)
	}
}

func TestSpeedLimit(t *testing.T) {
	srv := testServer()
	defer srv.Close()
	m := newTestManager(t, t.TempDir(), t.TempDir())
	m.limiter.SetRate(4 << 20) // 4 MB/s → ~6s for 24 MB
	start := time.Now()
	ids, _ := m.Add(AddRequest{URL: srv.URL + "/file.bin"})
	waitStatus(t, m, ids[0], StCompleted, 60*time.Second)
	el := time.Since(start)
	if el < 4500*time.Millisecond {
		t.Fatalf("limit not applied: took %v", el)
	}
	t.Logf("24MB at 4MB/s limit took %v", el.Round(100*time.Millisecond))
}

func TestRejectsWrongRange(t *testing.T) {
	if s, ok := contentRangeStart("bytes 1048576-2097151/25178169"); !ok || s != 1048576 {
		t.Fatal(s, ok)
	}
	if _, ok := contentRangeStart("garbage"); ok {
		t.Fatal("parsed garbage")
	}
}

func TestAPIGuard(t *testing.T) {
	m := newTestManager(t, t.TempDir(), t.TempDir())
	s := &Server{m: m}
	h := s.routes()
	cases := []struct {
		host, origin, ctype string
		want                int
	}{
		{"127.0.0.1:6801", "", "application/json", 200},
		{"127.0.0.1:6801", "chrome-extension://abc", "application/json", 200},
		{"127.0.0.1:6801", "https://evil.example", "application/json", 403},
		{"evil.example:6801", "", "application/json", 403},
		{"127.0.0.1:6801", "", "text/plain", 415},
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", "/api/all/pause", strings.NewReader("{}"))
		req.Host = c.host
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		req.Header.Set("Content-Type", c.ctype)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%+v → %d", c, rec.Code)
		}
	}
	_ = net.IPv4len
}
