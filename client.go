package main

import (
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
)

type Entry struct {
	Name    string
	Path    string // absolute remote path
	IsDir   bool
	Size    int64
	ModTime time.Time
	AccTime time.Time // best-effort atime; zero if server didn't provide
	Mode    os.FileMode
}

type cacheItem struct {
	entries []Entry
	at      time.Time
}

type Client struct {
	conn *Conn

	mu    sync.RWMutex
	cache map[string]cacheItem
	ttl   time.Duration
}

func NewClient(c *Conn) *Client {
	return &Client{
		conn:  c,
		cache: make(map[string]cacheItem),
		ttl:   30 * time.Second,
	}
}

// CleanPath normalizes a remote path. Handles ~ expansion to remote home.
func (c *Client) CleanPath(p string) string {
	if p == "" {
		return c.conn.Home
	}
	if p == "~" {
		return c.conn.Home
	}
	if strings.HasPrefix(p, "~/") {
		p = path.Join(c.conn.Home, p[2:])
	}
	if !path.IsAbs(p) {
		p = path.Join(c.conn.Home, p)
	}
	return path.Clean(p)
}

func (c *Client) Invalidate(p string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cache, c.CleanPath(p))
}

func (c *Client) InvalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache = make(map[string]cacheItem)
}

func (c *Client) cachedReaddir(p string) ([]Entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.cache[p]
	if !ok {
		return nil, false
	}
	if time.Since(v.at) > c.ttl {
		return nil, false
	}
	return v.entries, true
}

func (c *Client) putCache(p string, entries []Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache[p] = cacheItem{entries: entries, at: time.Now()}
}

func (c *Client) Readdir(p string) ([]Entry, error) {
	p = c.CleanPath(p)
	if v, ok := c.cachedReaddir(p); ok {
		return v, nil
	}
	infos, err := c.conn.SFTP.ReadDir(p)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(infos))
	for _, fi := range infos {
		var atime time.Time
		if st, ok := fi.Sys().(*sftp.FileStat); ok {
			atime = time.Unix(int64(st.Atime), 0)
		}
		out = append(out, Entry{
			Name:    fi.Name(),
			Path:    path.Join(p, fi.Name()),
			IsDir:   fi.IsDir(),
			AccTime: atime,
			Size:    fi.Size(),
			ModTime: fi.ModTime(),
			Mode:    fi.Mode(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	c.putCache(p, out)
	return out, nil
}

// Stat — used by command-bar to decide if input is a dir.
func (c *Client) Stat(p string) (os.FileInfo, error) {
	return c.conn.SFTP.Stat(c.CleanPath(p))
}

// Complete returns possible completions for `input` interpreted as a path.
// Returns (dirPart, matches) — matches are basenames (with trailing / for dirs).
func (c *Client) Complete(input string) (string, []string, error) {
	clean := input
	// Expand ~ but keep textual form for replacement decision in caller.
	full := c.CleanPath(clean)

	var dirPart, base string
	if strings.HasSuffix(clean, "/") || clean == "" {
		dirPart = full
		base = ""
	} else {
		dirPart = path.Dir(full)
		base = path.Base(full)
	}
	entries, err := c.Readdir(dirPart)
	if err != nil {
		return dirPart, nil, err
	}
	var matches []string
	lb := strings.ToLower(base)
	for _, e := range entries {
		if strings.HasPrefix(strings.ToLower(e.Name), lb) {
			name := e.Name
			if e.IsDir {
				name += "/"
			}
			matches = append(matches, name)
		}
	}
	return dirPart, matches, nil
}
