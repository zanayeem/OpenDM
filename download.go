package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Status string

const (
	StQueued      Status = "queued"
	StDownloading Status = "downloading"
	StPaused      Status = "paused"
	StCompleted   Status = "completed"
	StError       Status = "error"
)

const (
	minSegment = 512 * 1024       // smallest initial segment
	minSplit   = 512 * 1024       // a split must leave at least this much on each side
	bufSize    = 64 * 1024        // read buffer per connection
	stallAfter = 30 * time.Second // reconnect if a connection receives nothing for this long
)

// Segment is a byte range [Start, End] (inclusive). End == -1 means "until EOF".
type Segment struct {
	Start  int64 `json:"start"`
	End    int64 `json:"end"`
	Done   int64 `json:"done"`
	active bool
}

func (s *Segment) pos() int64 { return s.Start + s.Done }

func (s *Segment) remaining() int64 {
	if s.End < 0 {
		return 1 << 62
	}
	return s.End - s.pos() + 1
}

func (s *Segment) complete() bool { return s.End >= 0 && s.pos() > s.End }

type Download struct {
	mu sync.Mutex

	ID          string            `json:"id"`
	URL         string            `json:"url"`
	FileName    string            `json:"fileName"`
	Dir         string            `json:"dir"`
	Category    string            `json:"category"`
	Size        int64             `json:"size"`
	Resumable   bool              `json:"resumable"`
	Status      Status            `json:"status"`
	Error       string            `json:"error,omitempty"`
	Connections int               `json:"connections"`
	Segments    []*Segment        `json:"segments"`
	Headers     map[string]string `json:"headers,omitempty"`
	AddedAt     time.Time         `json:"addedAt"`
	CompletedAt *time.Time        `json:"completedAt,omitempty"`
	Probed      bool              `json:"probed"`
	NameFixed   bool              `json:"nameFixed"`
	DirFixed    bool              `json:"dirFixed"`

	// runtime only
	cancel      context.CancelFunc
	done        chan struct{}
	stopTo      Status // status to adopt once the running job stops
	removed     bool
	deleteFile  bool
	speed       float64
	lastBytes   int64
	lastTick    time.Time
	activeConns int
}

func (d *Download) downloaded() int64 {
	var n int64
	for _, s := range d.Segments {
		n += s.Done
	}
	return n
}

func (d *Download) filePath() string { return filepath.Join(d.Dir, d.FileName) }
func (d *Download) partPath() string { return d.filePath() + partSuffix }

// ---------------------------------------------------------------- probing

type probeResult struct {
	FileName    string `json:"fileName"`
	Size        int64  `json:"size"`
	Resumable   bool   `json:"resumable"`
	ContentType string `json:"contentType"`
	Category    string `json:"category"`
}

type httpError struct {
	Code int
}

func (e *httpError) Error() string {
	return fmt.Sprintf("server replied %d %s", e.Code, http.StatusText(e.Code))
}

// fatal reports whether retrying the same request is pointless.
func (e *httpError) fatal() bool {
	switch e.Code {
	case 408, 425, 429, 500, 502, 503, 504:
		return false
	}
	return e.Code >= 400
}

func applyHeaders(req *http.Request, h map[string]string, ua string) {
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "*/*")
	for k, v := range h {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
}

func (m *Manager) probe(ctx context.Context, rawURL string, headers map[string]string) (*probeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	do := func(withRange bool) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		applyHeaders(req, headers, m.cfg().UserAgent)
		if withRange {
			req.Header.Set("Range", "bytes=0-")
		}
		return m.client().Do(req)
	}
	resp, err := do(true)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		resp.Body.Close()
		if resp, err = do(false); err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, &httpError{resp.StatusCode}
	}
	r := &probeResult{Size: -1, ContentType: resp.Header.Get("Content-Type")}
	if resp.StatusCode == http.StatusPartialContent {
		// Content-Range: bytes 0-1234/1235
		cr := resp.Header.Get("Content-Range")
		if i := strings.LastIndex(cr, "/"); i >= 0 {
			if total, err := strconv.ParseInt(strings.TrimSpace(cr[i+1:]), 10, 64); err == nil {
				r.Size = total
				r.Resumable = true
			}
		}
	} else if resp.ContentLength >= 0 {
		r.Size = resp.ContentLength
	}
	r.FileName = fileNameFromResponse(resp, rawURL)
	r.Category = categorize(r.FileName)
	return r, nil
}

// ---------------------------------------------------------------- planning

func planSegments(size int64, conns int, resumable bool) []*Segment {
	if !resumable || size <= 0 {
		end := int64(-1)
		if size > 0 {
			end = size - 1
		}
		return []*Segment{{Start: 0, End: end}}
	}
	n := int64(conns)
	if maxN := size / minSegment; n > maxN {
		n = maxN
	}
	if n < 1 {
		n = 1
	}
	segs := make([]*Segment, 0, n)
	chunk := size / n
	for i := int64(0); i < n; i++ {
		start := i * chunk
		end := start + chunk - 1
		if i == n-1 {
			end = size - 1
		}
		segs = append(segs, &Segment{Start: start, End: end})
	}
	return segs
}

// nextSegment hands a worker something to do: an orphaned unfinished segment
// if there is one, otherwise it splits the largest in-flight segment in half
// (IDM-style dynamic segmentation). Must be called with d.mu held.
func (d *Download) nextSegment() *Segment {
	for _, s := range d.Segments {
		if !s.active && !s.complete() {
			s.active = true
			return s
		}
	}
	if !d.Resumable {
		return nil
	}
	var best *Segment
	bestIdx := -1
	for i, s := range d.Segments {
		if s.active && s.End >= 0 && (best == nil || s.remaining() > best.remaining()) {
			best, bestIdx = s, i
		}
	}
	if best == nil || best.remaining() < 2*minSplit {
		return nil
	}
	mid := best.pos() + best.remaining()/2
	ns := &Segment{Start: mid, End: best.End, active: true}
	best.End = mid - 1
	d.Segments = append(d.Segments, nil)
	copy(d.Segments[bestIdx+2:], d.Segments[bestIdx+1:])
	d.Segments[bestIdx+1] = ns
	return ns
}

// ---------------------------------------------------------------- running

func (m *Manager) run(ctx context.Context, d *Download) {
	err := m.execute(ctx, d)

	d.mu.Lock()
	d.cancel = nil
	d.activeConns = 0
	d.speed = 0
	for _, s := range d.Segments {
		s.active = false
	}
	removed, delFile := d.removed, d.deleteFile
	notifyName := ""
	switch {
	case removed:
		_ = os.Remove(d.partPath())
		if delFile && d.Status == StCompleted {
			_ = os.Remove(d.filePath())
		}
	case err == nil:
		d.Status = StCompleted
		d.Error = ""
		now := time.Now()
		d.CompletedAt = &now
		notifyName = d.FileName
	case ctx.Err() != nil:
		d.Status = d.stopTo
		if d.Status == "" {
			d.Status = StPaused
		}
	default:
		d.Status = StError
		d.Error = err.Error()
	}
	done := d.done
	d.mu.Unlock()
	close(done)

	if notifyName != "" && m.cfg().NotifyOnComplete && m.notify != nil {
		m.notify("Download complete", notifyName)
	}
	m.markDirty()
	m.schedule()
}

func (m *Manager) execute(ctx context.Context, d *Download) error {
	d.mu.Lock()
	needProbe := !d.Probed || len(d.Segments) == 0
	rawURL, headers := d.URL, d.Headers
	d.mu.Unlock()

	if needProbe {
		info, err := m.probe(ctx, rawURL, headers)
		if err != nil {
			return err
		}
		cfg := m.cfg()
		d.mu.Lock()
		if !d.NameFixed || d.FileName == "" {
			d.FileName = info.FileName
		}
		d.FileName = sanitizeFileName(d.FileName)
		d.Category = categorize(d.FileName)
		if !d.DirFixed {
			d.Dir = cfg.DownloadDir
			if cfg.SortByCategory {
				d.Dir = filepath.Join(cfg.DownloadDir, d.Category)
			}
		}
		id := d.ID
		d.mu.Unlock()

		p := uniquePath(d.Dir, d.FileName, func(p string) bool { return m.pathTaken(p, id) })

		d.mu.Lock()
		d.FileName = filepath.Base(p)
		d.Size = info.Size
		d.Resumable = info.Resumable
		d.Segments = planSegments(info.Size, d.Connections, info.Resumable)
		d.Probed = true
		d.mu.Unlock()
		m.markDirty()
	}

	d.mu.Lock()
	dir, part, size, resumable := d.Dir, d.partPath(), d.Size, d.Resumable
	if !resumable {
		// servers without range support must start over every time
		d.Segments = planSegments(size, 1, false)
	}
	workers := 1
	if resumable {
		workers = d.Connections
	}
	d.mu.Unlock()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if !resumable {
		_ = f.Truncate(0)
	} else if st, err := f.Stat(); err == nil && st.Size() != size {
		// pre-allocate so every connection can write at its own offset
		if err := f.Truncate(size); err != nil {
			f.Close()
			return fmt.Errorf("cannot allocate %s on disk: %w", humanBytes(size), err)
		}
	}

	var (
		wg      sync.WaitGroup
		errMu   sync.Mutex
		lastErr error
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// stagger connection start a little so servers don't see a burst
			if i > 0 {
				select {
				case <-time.After(time.Duration(i) * 80 * time.Millisecond):
				case <-ctx.Done():
					return
				}
			}
			if err := m.worker(ctx, d, f); err != nil {
				errMu.Lock()
				lastErr = err
				errMu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if err := f.Close(); err != nil && lastErr == nil {
		lastErr = err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	d.mu.Lock()
	allDone := true
	for _, s := range d.Segments {
		if !s.complete() {
			allDone = false
		}
	}
	if allDone && d.Size < 0 {
		d.Size = d.downloaded()
	}
	d.mu.Unlock()
	if !allDone {
		if lastErr == nil {
			lastErr = errors.New("download stopped before completion")
		}
		return lastErr
	}

	// finalize: move .part into place
	d.mu.Lock()
	final := d.filePath()
	if exists(final) {
		final = uniquePath(d.Dir, d.FileName, nil)
		d.FileName = filepath.Base(final)
	}
	d.mu.Unlock()
	if err := os.Rename(part, final); err != nil {
		return fmt.Errorf("could not finalize file: %w", err)
	}
	return nil
}

// worker keeps pulling segments until there is nothing left to do.
func (m *Manager) worker(ctx context.Context, d *Download, f *os.File) error {
	retries := m.cfg().Retries
	failures := 0
	for {
		d.mu.Lock()
		seg := d.nextSegment()
		if seg != nil {
			d.activeConns++
		}
		d.mu.Unlock()
		if seg == nil {
			return nil
		}

		err := m.fetchSegment(ctx, d, f, seg)

		d.mu.Lock()
		seg.active = false
		d.activeConns--
		d.mu.Unlock()

		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			failures = 0
			continue
		}
		var he *httpError
		if errors.As(err, &he) && he.fatal() {
			return err
		}
		failures++
		if failures > retries {
			return err
		}
		backoff := time.Duration(1<<min(failures-1, 4)) * time.Second
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil
		}
	}
}

func (m *Manager) fetchSegment(parent context.Context, d *Download, f *os.File, seg *Segment) error {
	d.mu.Lock()
	if !d.Resumable {
		seg.Done = 0 // no ranges: every attempt starts from byte 0
	}
	from, end := seg.pos(), seg.End
	rawURL, headers, resumable := d.URL, d.Headers, d.Resumable
	d.mu.Unlock()

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var stalledFlag atomic.Bool
	watchdog := time.AfterFunc(stallAfter, func() { stalledFlag.Store(true); cancel() })
	defer watchdog.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	applyHeaders(req, headers, m.cfg().UserAgent)
	if resumable {
		if end >= 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, end))
		} else {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", from))
		}
	}
	resp, err := m.client().Do(req)
	if err != nil {
		if stalledFlag.Load() {
			return errors.New("connection stalled")
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return &httpError{resp.StatusCode}
	}
	if resumable {
		if resp.StatusCode != http.StatusPartialContent {
			return errors.New("server stopped honouring byte ranges; use Restart")
		}
		// never trust bytes that don't start where we asked
		if start, ok := contentRangeStart(resp.Header.Get("Content-Range")); !ok || start != from {
			return fmt.Errorf("server sent the wrong byte range (%q)", resp.Header.Get("Content-Range"))
		}
	}

	buf := make([]byte, bufSize)
	for {
		n, rerr := resp.Body.Read(buf)
		watchdog.Reset(stallAfter)
		if n > 0 {
			if err := m.limiter.Wait(ctx, n); err != nil {
				return err
			}
			d.mu.Lock()
			pos := seg.pos()
			w := int64(n)
			finished := false
			if seg.End >= 0 {
				allowed := seg.End - pos + 1
				if w >= allowed {
					w, finished = max(allowed, 0), true
				}
			}
			seg.Done += w
			d.mu.Unlock()
			if w > 0 {
				if _, err := f.WriteAt(buf[:w], pos); err != nil {
					d.mu.Lock()
					seg.Done -= w
					d.mu.Unlock()
					return fmt.Errorf("disk write failed: %w", err)
				}
			}
			if finished {
				return nil
			}
		}
		if rerr == io.EOF {
			d.mu.Lock()
			defer d.mu.Unlock()
			if seg.End < 0 {
				seg.End = seg.pos() - 1 // stream of unknown length finished
				return nil
			}
			if seg.complete() {
				return nil
			}
			return io.ErrUnexpectedEOF
		}
		if rerr != nil {
			if stalledFlag.Load() {
				return errors.New("connection stalled")
			}
			return rerr
		}
	}
}

// contentRangeStart parses "bytes 100-199/1000" → 100.
func contentRangeStart(h string) (int64, bool) {
	h = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(h), "bytes"))
	dash := strings.IndexByte(h, '-')
	if dash <= 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(h[:dash]), 10, 64)
	return n, err == nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
