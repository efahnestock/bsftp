package main

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"time"
)

// DownloadFile copies remote -> localDir/<basename>.
func DownloadFile(c *Client, remote string, localDir string) (string, error) {
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return "", err
	}
	rf, err := c.conn.SFTP.Open(remote)
	if err != nil {
		return "", err
	}
	defer rf.Close()
	dest := filepath.Join(localDir, path.Base(remote))
	// Remove existing file so the new download gets a fresh birth time on macOS.
	_ = os.Remove(dest)
	lf, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer lf.Close()
	if _, err := io.Copy(lf, rf); err != nil {
		return "", err
	}
	now := time.Now()
	_ = os.Chtimes(dest, now, now)
	return dest, nil
}

// DownloadTree recursively downloads remoteDir -> localParent/<basename>.
// Follows symlinks (to files or directories), with a visited set to break cycles.
// Per-entry errors are collected and reported; the walk continues so one bad entry
// doesn't abort the whole transfer.
func DownloadTree(c *Client, remoteDir string, localParent string) (string, error) {
	base := path.Base(remoteDir)
	root := filepath.Join(localParent, base)
	visited := map[string]bool{}
	skipped := downloadInto(c, remoteDir, root, visited)
	if len(skipped) > 0 {
		first := skipped[0]
		more := ""
		if len(skipped) > 1 {
			more = fmt.Sprintf(" (+%d more)", len(skipped)-1)
		}
		return root, fmt.Errorf("%d skipped — first: %s%s", len(skipped), first, more)
	}
	return root, nil
}

// downloadInto copies the contents of remoteDir into localDir. Returns a list of
// "path (reason)" strings for entries that couldn't be downloaded.
func downloadInto(c *Client, remoteDir, localDir string, visited map[string]bool) []string {
	var skipped []string
	record := func(rp string, err error) {
		skipped = append(skipped, fmt.Sprintf("%s (%v)", rp, err))
	}

	resolved := path.Clean(remoteDir)
	if visited[resolved] {
		return nil
	}
	visited[resolved] = true

	if err := os.MkdirAll(localDir, 0o755); err != nil {
		record(remoteDir, err)
		return skipped
	}

	entries, err := c.conn.SFTP.ReadDir(remoteDir)
	if err != nil {
		record(remoteDir, err)
		return skipped
	}

	for _, fi := range entries {
		rp := path.Join(remoteDir, fi.Name())
		lp := filepath.Join(localDir, fi.Name())
		mode := fi.Mode()

		switch {
		case mode&os.ModeSymlink != 0:
			// Resolve target, then dispatch on its kind.
			target, err := c.conn.SFTP.ReadLink(rp)
			if err != nil {
				record(rp, fmt.Errorf("readlink: %w", err))
				continue
			}
			if !path.IsAbs(target) {
				target = path.Join(path.Dir(rp), target)
			}
			target = path.Clean(target)
			tinfo, err := c.conn.SFTP.Stat(target)
			if err != nil {
				record(rp, fmt.Errorf("symlink target %s: %w", target, err))
				continue
			}
			if tinfo.IsDir() {
				skipped = append(skipped, downloadInto(c, target, lp, visited)...)
				continue
			}
			if tinfo.Mode().IsRegular() {
				if err := copyRegular(c, target, lp, tinfo.Size()); err != nil {
					record(rp, err)
				}
				continue
			}
			record(rp, fmt.Errorf("symlink target is non-regular (mode %s)", tinfo.Mode()))

		case mode.IsDir():
			skipped = append(skipped, downloadInto(c, rp, lp, visited)...)

		case mode.IsRegular():
			if err := copyRegular(c, rp, lp, fi.Size()); err != nil {
				record(rp, err)
			}

		default:
			record(rp, fmt.Errorf("non-regular file (mode %s)", mode))
		}
	}
	return skipped
}

func copyRegular(c *Client, remote, local string, _ int64) error {
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return err
	}
	rf, err := c.conn.SFTP.Open(remote)
	if err != nil {
		return err
	}
	defer rf.Close()
	_ = os.Remove(local)
	lf, err := os.Create(local)
	if err != nil {
		return err
	}
	defer lf.Close()
	if _, err := io.Copy(lf, rf); err != nil {
		return err
	}
	now := time.Now()
	_ = os.Chtimes(local, now, now)
	return nil
}

// UploadFile copies localPath -> remoteDir/<basename(localPath)>.
func UploadFile(c *Client, localPath string, remoteDir string) (string, error) {
	lf, err := os.Open(localPath)
	if err != nil {
		return "", err
	}
	defer lf.Close()
	if err := c.conn.SFTP.MkdirAll(remoteDir); err != nil {
		return "", err
	}
	dest := path.Join(remoteDir, filepath.Base(localPath))
	rf, err := c.conn.SFTP.Create(dest)
	if err != nil {
		return "", err
	}
	defer rf.Close()
	if _, err := io.Copy(rf, lf); err != nil {
		return "", err
	}
	return dest, nil
}

// RemoveAll deletes a file or recursively deletes a directory on the remote.
func RemoveAll(c *Client, remote string) error {
	fi, err := c.conn.SFTP.Stat(remote)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return c.conn.SFTP.Remove(remote)
	}
	// Recursive
	walker := c.conn.SFTP.Walk(remote)
	var files, dirs []string
	for walker.Step() {
		if err := walker.Err(); err != nil {
			return err
		}
		p := walker.Path()
		if walker.Stat().IsDir() {
			dirs = append(dirs, p)
		} else {
			files = append(files, p)
		}
	}
	for _, f := range files {
		if err := c.conn.SFTP.Remove(f); err != nil {
			return err
		}
	}
	// Remove deepest first
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := c.conn.SFTP.RemoveDirectory(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

// formatSize is a small helper for the UI.
func formatSize(n int64) string {
	const k = 1024
	if n < k {
		return fmt.Sprintf("%dB", n)
	}
	units := []string{"K", "M", "G", "T"}
	f := float64(n) / k
	u := 0
	for f >= k && u < len(units)-1 {
		f /= k
		u++
	}
	return fmt.Sprintf("%.1f%s", f, units[u])
}
