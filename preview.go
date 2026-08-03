package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const previewSizeLimit = 512 * 1024 * 1024  // 512MB cap; QuickLook on bigger files is dubious
const previewCacheBudget = 2 * 1024 * 1024 * 1024 // 2GB on-disk cap

func previewCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		base = filepath.Join(os.Getenv("HOME"), "Library", "Caches")
	}
	d := filepath.Join(base, "bsftp", "preview")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}

// fetchForPreview downloads remote path to a cache file keyed by (host, path, size, mtime)
// and returns the local path. Skips re-download if the cache file already matches size.
func fetchForPreview(c *Client, host, remotePath string) (string, error) {
	fi, err := c.conn.SFTP.Stat(remotePath)
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		return "", fmt.Errorf("cannot preview a directory")
	}
	if fi.Size() > previewSizeLimit {
		return "", fmt.Errorf("file too large for preview (%d bytes)", fi.Size())
	}
	cacheDir, err := previewCacheDir()
	if err != nil {
		return "", err
	}

	h := sha1.Sum([]byte(host + ":" + remotePath))
	sub := filepath.Join(cacheDir, hex.EncodeToString(h[:8]))
	if err := os.MkdirAll(sub, 0o700); err != nil {
		return "", err
	}
	local := filepath.Join(sub, filepath.Base(remotePath))

	if st, err := os.Stat(local); err == nil && st.Size() == fi.Size() && st.ModTime().After(fi.ModTime()) {
		// Bump mtime so the LRU eviction sees this entry as recently used.
		now := time.Now()
		_ = os.Chtimes(local, now, now)
		return local, nil
	}

	rf, err := c.conn.SFTP.Open(remotePath)
	if err != nil {
		return "", err
	}
	defer rf.Close()
	lf, err := os.Create(local)
	if err != nil {
		return "", err
	}
	defer lf.Close()
	if _, err := io.Copy(lf, rf); err != nil {
		return "", err
	}
	return local, nil
}

// QuickLook opens a local file in macOS Quick Look (qlmanage -p).
// Videos are routed to `open` because qlmanage's video preview generator
// crashes when invoked outside a Cocoa app context.
// Returns when the preview window closes (or immediately for `open`).
func QuickLook(localPath string) error {
	if isVideoFile(localPath) {
		return exec.Command("open", localPath).Start()
	}
	cmd := exec.Command("qlmanage", "-p", localPath)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

// evictCache trims the preview cache directory down to `budget` bytes total
// by deleting the least-recently-modified files first. Files inside
// `.placeholder*` paths are kept regardless of mtime.
func evictCache(budget int64) (freed int64, err error) {
	root, err := previewCacheDir()
	if err != nil {
		return 0, err
	}
	type ent struct {
		path  string
		size  int64
		mtime time.Time
	}
	var ents []ent
	var total int64
	err = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if strings.HasPrefix(filepath.Base(p), ".placeholder") {
			return nil
		}
		ents = append(ents, ent{path: p, size: info.Size(), mtime: info.ModTime()})
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, err
	}
	if total <= budget {
		return 0, nil
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].mtime.Before(ents[j].mtime) })
	for _, e := range ents {
		if total <= budget {
			break
		}
		if err := os.Remove(e.path); err == nil {
			total -= e.size
			freed += e.size
		}
	}
	return freed, nil
}

// isVideoFile returns true for extensions that qlmanage cannot preview.
func isVideoFile(p string) bool {
	ext := strings.ToLower(filepath.Ext(p))
	switch ext {
	case ".mp4", ".mov", ".m4v", ".avi", ".mkv", ".webm", ".wmv", ".flv", ".mpg", ".mpeg", ".3gp", ".ts":
		return true
	}
	return false
}
