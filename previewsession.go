package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sync"
)

// previewSession drives the Swift QuickLook helper for one preview window.
//
// Lifecycle:
//   - newPreviewSession(...) precomputes remote paths/titles and spawns helper.
//   - Run(startIdx) sends "open" and starts the IO + prefetch loops.
//   - Events() streams "showing", "closed", "error" back to the UI.
//   - The session is one-shot: it ends when the helper closes its stdout.
type previewSession struct {
	client *Client
	host   string

	remote []string // absolute remote paths (symlinks already resolved)
	titles []string

	mu     sync.Mutex
	local  []string     // local cache path once ready
	state  []fetchState // pending → fetching → ready/failed
	cur    int          // last index the helper said was showing
	closed bool

	placeholderPath string

	helper *exec.Cmd
	stdinW *json.Encoder
	stdoutR *bufio.Scanner

	events    chan previewEvent
	done      chan struct{} // closed once on shutdown
	closeOnce sync.Once
}

type fetchState int

const (
	fsPending fetchState = iota
	fsFetching
	fsReady
	fsFailed
)

type previewEvent struct {
	Kind  string // "showing", "closed", "error"
	Index int
	Err   error
}

const (
	prefetchRadius = 5
	maxConcurrentFetches = 4
)

// newPreviewSession resolves symlinks, prepares the placeholder, and spawns
// the helper subprocess. It does not yet send the "open" command.
func newPreviewSession(c *Client, host string, entries []Entry, helperPath string) (*previewSession, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("no previewable files")
	}
	titles := make([]string, len(entries))
	remote := make([]string, len(entries))
	for i, e := range entries {
		titles[i] = e.Name
		rp := e.Path
		if e.Mode&os.ModeSymlink != 0 {
			if t, err := c.conn.SFTP.ReadLink(rp); err == nil {
				if !path.IsAbs(t) {
					t = path.Join(path.Dir(rp), t)
				}
				rp = path.Clean(t)
			}
		}
		remote[i] = rp
	}

	placeholder, err := ensurePlaceholder()
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(helperPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr // surface helper crashes in the terminal scrollback
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	s := &previewSession{
		client:          c,
		host:            host,
		remote:          remote,
		titles:          titles,
		local:           make([]string, len(entries)),
		state:           make([]fetchState, len(entries)),
		placeholderPath: placeholder,
		events:          make(chan previewEvent, 16),
		done:            make(chan struct{}),
		helper:          cmd,
		stdinW:          json.NewEncoder(stdin),
		stdoutR:         sc,
	}
	return s, nil
}

func (s *previewSession) Events() <-chan previewEvent { return s.events }

// Run sends the initial open command and starts the IO + prefetch loops.
func (s *previewSession) Run(startIdx int) {
	if startIdx < 0 || startIdx >= len(s.remote) {
		startIdx = 0
	}
	s.mu.Lock()
	s.cur = startIdx
	s.mu.Unlock()

	_ = s.stdinW.Encode(map[string]any{
		"cmd":         "open",
		"count":       len(s.remote),
		"start":       startIdx,
		"placeholder": s.placeholderPath,
		"titles":      s.titles,
	})

	// Prefetch goroutine listens to a tick channel; the reader loop and each
	// fetch goroutine signal it. `s.done` (closed once by shutdown()) is what
	// they actually race on — we never close `tick`, so concurrent signal()
	// calls during shutdown can't panic on a closed channel.
	tick := make(chan struct{}, 1)
	done := s.done
	signal := func() {
		select {
		case <-done:
			return
		default:
		}
		select {
		case tick <- struct{}{}:
		case <-done:
		default:
		}
	}
	signal()

	sem := make(chan struct{}, maxConcurrentFetches)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-tick:
			}
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			s.kickWindow(sem, signal)
		}
	}()

	go s.readLoop(signal)
}

// Close requests a clean helper shutdown.
func (s *previewSession) Close() {
	_ = s.stdinW.Encode(map[string]any{"cmd": "close"})
	_ = s.helper.Process.Signal(os.Interrupt)
	s.shutdown()
}

// shutdown is idempotent. It flips s.closed and closes s.done so prefetch
// goroutines and signal() callers can exit. Safe from any goroutine.
func (s *previewSession) shutdown() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.done)
	})
}

// ---- internals -------------------------------------------------------------

func (s *previewSession) readLoop(signal func()) {
	defer func() {
		s.shutdown()
		close(s.events)
	}()
	for s.stdoutR.Scan() {
		var ev struct {
			Event   string `json:"event"`
			Index   int    `json:"index"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(s.stdoutR.Bytes(), &ev); err != nil {
			continue
		}
		switch ev.Event {
		case "showing":
			s.mu.Lock()
			s.cur = ev.Index
			s.mu.Unlock()
			s.events <- previewEvent{Kind: "showing", Index: ev.Index}
			signal()
		case "closed":
			s.events <- previewEvent{Kind: "closed"}
			return
		case "error":
			s.events <- previewEvent{Kind: "error", Err: fmt.Errorf("%s", ev.Message)}
		}
	}
}

// kickWindow walks the ±prefetchRadius window around the current index and
// spawns fetches for items still in fsPending state.
func (s *previewSession) kickWindow(sem chan struct{}, signal func()) {
	s.mu.Lock()
	cur := s.cur
	n := len(s.remote)
	s.mu.Unlock()

	for off := 0; off <= prefetchRadius; off++ {
		for _, delta := range []int{off, -off} {
			if off == 0 && delta != 0 {
				continue
			}
			i := cur + delta
			if i < 0 || i >= n {
				continue
			}
			s.mu.Lock()
			if s.state[i] != fsPending {
				s.mu.Unlock()
				continue
			}
			s.state[i] = fsFetching
			s.mu.Unlock()
			sem <- struct{}{}
			go func(idx int) {
				defer func() {
					<-sem
					signal()
				}()
				s.fetchOne(idx)
			}(i)
		}
	}
}

func (s *previewSession) fetchOne(i int) {
	s.mu.Lock()
	closed := s.closed
	remote := s.remote[i]
	s.mu.Unlock()
	if closed {
		return
	}
	local, err := fetchForPreview(s.client, s.host, remote)
	s.mu.Lock()
	if err != nil {
		s.state[i] = fsFailed
		s.mu.Unlock()
		_ = s.stdinW.Encode(map[string]any{"cmd": "failed", "index": i, "reason": err.Error()})
		return
	}
	s.state[i] = fsReady
	s.local[i] = local
	s.mu.Unlock()
	_ = s.stdinW.Encode(map[string]any{"cmd": "ready", "index": i, "path": local})
	_, _ = evictCache(previewCacheBudget)
}

// ensurePlaceholder writes a 1x1 transparent PNG to the cache dir if not
// already present and returns its absolute path.
func ensurePlaceholder() (string, error) {
	dir, err := previewCacheDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, ".placeholder.png")
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	png := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
		0x89, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9C, 0x62, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
		0x42, 0x60, 0x82,
	}
	if err := os.WriteFile(p, png, 0o644); err != nil {
		return "", err
	}
	return p, nil
}
