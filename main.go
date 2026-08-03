package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
)

// locatePreviewHelper looks for the bsftp-preview Swift binary next to the Go
// executable or on $PATH. Returns "" if not found; callers fall back to the
// non-streaming preview path.
func locatePreviewHelper() string {
	const name = "bsftp-preview"
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), name)
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

func main() {
	defaultHost := os.Getenv("BSFTP_HOST")
	lsMode := flag.Bool("ls", false, "list remote path and exit (smoke test, no TUI)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [-ls] [ssh-host-alias] [remote-path]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  host defaults to $BSFTP_HOST if not given\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	args := flag.Args()
	host := defaultHost
	if len(args) >= 1 {
		host = args[0]
	}
	if host == "" {
		flag.Usage()
		os.Exit(2)
	}
	startPath := ""
	if len(args) >= 2 {
		startPath = args[1]
	}

	conn, err := Dial(host)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer conn.Close()

	client := NewClient(conn)
	bm, err := LoadBookmarks()
	if err != nil {
		fmt.Fprintln(os.Stderr, "bookmarks:", err)
		os.Exit(1)
	}

	if *lsMode {
		p := startPath
		if p == "" {
			p = conn.Home
		}
		entries, err := client.Readdir(p)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ls:", err)
			os.Exit(1)
		}
		fmt.Printf("connected as %s, home=%s, listing %s (%d entries)\n", conn.Display, conn.Home, client.CleanPath(p), len(entries))
		for i, e := range entries {
			if i >= 20 {
				fmt.Printf("... (%d more)\n", len(entries)-i)
				break
			}
			kind := "f"
			if e.IsDir {
				kind = "d"
			} else if e.Mode&os.ModeSymlink != 0 {
				kind = "l"
			}
			fmt.Printf("  %s %8s  %s\n", kind, formatSize(e.Size), e.Name)
		}
		return
	}

	helperPath := locatePreviewHelper()
	if helperPath == "" {
		fmt.Fprintln(os.Stderr, "preview helper not found — falling back to qlmanage batch mode (build it with `make helper`)")
	}

	model := NewModel(client, bm)
	model.previewHelper = helperPath
	if startPath != "" {
		model.cwd = client.CleanPath(startPath)
		model.loadingPath = model.cwd
	}

	p := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "ui:", err)
		os.Exit(1)
	}
}
