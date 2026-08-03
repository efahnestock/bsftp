package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---- Styles ----

var (
	headerStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	dirStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	linkStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("87")).Bold(true)
	bmHeader     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	bmPane       = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1)
	statusStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
	errStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	promptStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("213"))
	matchHint    = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Italic(true)
	// Strong, unambiguous selection: bright background, white fg, bold.
	// Applied as the *only* style on the selected row so the dir blue doesn't bleed through.
	selectedLine   = lipgloss.NewStyle().Background(lipgloss.Color("63")).Foreground(lipgloss.Color("231")).Bold(true)
	selectedDirFg  = lipgloss.NewStyle().Background(lipgloss.Color("63")).Foreground(lipgloss.Color("231")).Bold(true)
	cursorBarStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("213")).Bold(true)
	gutterStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
)

// ---- Mode ----

type mode int

const (
	modeNormal mode = iota
	modeCmd               // ':' command bar (path navigation)
	modeAddBookmark       // 'B' — entering name
	modeDownloadTo        // 'D' — entering local destination dir
	modeUploadFrom        // 'U' — entering local source file
	modeMkdir             // 'n' — entering new directory name
	modeFilter            // '/' — incremental filter
	modeConfirmDel        // 'x' — yes/no
	modeConfirmOverwrite  // before download — yes/no
	modeRename            // 'c' — change/rename
)

// ---- Messages ----

type dirLoadedMsg struct {
	gen     uint64
	path    string
	entries []Entry
	err     error
}

type previewDoneMsg struct{ err error }

type previewEventMsg struct {
	kind  string
	index int
	err   error
}

type transferDoneMsg struct {
	label string
	dest  string
	err   error
}

type bookmarkSavedMsg struct{ err error }

type statusMsg struct {
	text string
	isErr bool
}

type connEventMsg ConnEvent

// ---- Model ----

type Model struct {
	client    *Client
	bookmarks *Bookmarks

	cwd     string
	entries []Entry
	filtered []Entry
	filter  string
	cursor  int
	scroll  int

	width  int
	height int

	mode  mode
	input textinput.Model

	showHidden bool
	showBM     bool
	showHelp   bool

	previewHelper string // absolute path to bsftp-preview, or "" to use fallback

	previewActive bool
	previewN      int // current item index reported by helper
	previewM      int // total items in the session

	status string
	isErr  bool

	// completion state for command bar
	completions []string
	compIdx     int
	compBase    string // dir portion (canonical) the completions are relative to
	compPrefix  string // the textual base prefix the user typed (for cycling)

	// pending confirm state
	pendingDelete   *Entry
	pendingDownload *pendingDl

	// After the next dir load, place the cursor on this basename if present.
	pendingSelect string

	// Generation token incremented on every navigation. Only dir-load results
	// whose gen still matches loadGen are applied; the rest are stale and
	// discarded (e.g. user navigated away while a 44k-entry ReadDir was in
	// flight).
	loadGen     uint64
	loading     bool
	loadingPath string
}

type pendingDl struct {
	e    Entry
	dest string // local destination directory
}

func NewModel(c *Client, b *Bookmarks) Model {
	ti := textinput.New()
	ti.Prompt = ""
	ti.CharLimit = 1024
	return Model{
		client:      c,
		bookmarks:   b,
		cwd:         c.conn.Home,
		input:       ti,
		loadGen:     1,
		loading:     true,
		loadingPath: c.conn.Home,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		loadDir(m.client, m.cwd, m.loadGen),
		listenConnEvents(m.client.conn),
	)
}

// navigate switches cwd optimistically to `to` and dispatches a background
// load. Any in-flight load whose gen no longer matches is dropped on arrival.
func (m *Model) navigate(to string) tea.Cmd {
	m.loadGen++
	m.cwd = m.client.CleanPath(to)
	m.entries = nil
	m.filtered = nil
	m.cursor = 0
	m.scroll = 0
	m.filter = ""
	m.loading = true
	m.loadingPath = m.cwd
	m.status = ""
	m.isErr = false
	return loadDir(m.client, m.cwd, m.loadGen)
}

// refreshCwd reloads the current directory without changing it. Bumps gen so a
// stale slow load can't clobber the new one.
func (m *Model) refreshCwd() tea.Cmd {
	m.loadGen++
	m.loading = true
	m.loadingPath = m.cwd
	m.client.Invalidate(m.cwd)
	return loadDir(m.client, m.cwd, m.loadGen)
}

// listenConnEvents waits for the next connection event and routes it into
// the Bubble Tea update loop. After Update handles the event it should call
// this command again to keep the subscription alive.
func listenConnEvents(c *Conn) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-c.Events()
		if !ok {
			return nil
		}
		return connEventMsg(ev)
	}
}

// ---- Commands ----

func loadDir(c *Client, p string, gen uint64) tea.Cmd {
	return func() tea.Msg {
		entries, err := c.Readdir(p)
		return dirLoadedMsg{gen: gen, path: c.CleanPath(p), entries: entries, err: err}
	}
}

func runPreview(c *Client, host, remote string) tea.Cmd {
	return func() tea.Msg {
		local, err := fetchForPreview(c, host, remote)
		if err != nil {
			return previewDoneMsg{err: err}
		}
		err = QuickLook(local)
		return previewDoneMsg{err: err}
	}
}

// ---- streaming preview session glue -----------------------------------------

// currentPreviewEvents holds the active session's event channel so the
// awaitNextPreviewEvent tea.Cmd can read from it. There is at most one active
// preview session at a time.
var currentPreviewEvents <-chan previewEvent

// startPreviewSession spawns the Swift helper, kicks off the session, and
// returns a tea.Cmd that delivers the first event back into Update. Subsequent
// events are pulled via awaitNextPreviewEvent.
func startPreviewSession(c *Client, host string, entries []Entry, startIdx int, helperPath string) tea.Cmd {
	return func() tea.Msg {
		s, err := newPreviewSession(c, host, entries, helperPath)
		if err != nil {
			return previewEventMsg{kind: "error", err: err}
		}
		s.Run(startIdx)
		currentPreviewEvents = s.Events()
		ev, ok := <-currentPreviewEvents
		if !ok {
			return previewEventMsg{kind: "closed"}
		}
		return previewEventMsg{kind: ev.Kind, index: ev.Index, err: ev.Err}
	}
}

func awaitNextPreviewEvent() tea.Cmd {
	return func() tea.Msg {
		ch := currentPreviewEvents
		if ch == nil {
			return previewEventMsg{kind: "closed"}
		}
		ev, ok := <-ch
		if !ok {
			currentPreviewEvents = nil
			return previewEventMsg{kind: "closed"}
		}
		return previewEventMsg{kind: ev.Kind, index: ev.Index, err: ev.Err}
	}
}

// runPreviewMulti pre-caches all previewable siblings in `entries` (ordered list
// from the listing) starting at the cursor's index. The selected file is
// launched first; if it's a video, it's opened with `open` (qlmanage can't
// render video). Otherwise QuickLook is launched with the batch of non-video
// files so arrow keys flip through them inside the QL window.
func runPreviewMulti(c *Client, host string, entries []Entry, startIdx int) tea.Cmd {
	return func() tea.Msg {
		if len(entries) == 0 {
			return previewDoneMsg{err: fmt.Errorf("no previewable files")}
		}
		const maxFiles = 60
		ordered := make([]Entry, 0, len(entries))
		ordered = append(ordered, entries[startIdx:]...)
		ordered = append(ordered, entries[:startIdx]...)
		if len(ordered) > maxFiles {
			ordered = ordered[:maxFiles]
		}

		// Selected file decides the path: video → `open`; otherwise → qlmanage.
		selectedIsVideo := isVideoFile(ordered[0].Name)

		resolve := func(e Entry) (string, error) {
			rp := e.Path
			if e.Mode&os.ModeSymlink != 0 {
				t, err := c.conn.SFTP.ReadLink(rp)
				if err != nil {
					return "", err
				}
				if !path.IsAbs(t) {
					t = path.Join(path.Dir(rp), t)
				}
				rp = path.Clean(t)
			}
			return fetchForPreview(c, host, rp)
		}

		if selectedIsVideo {
			local, err := resolve(ordered[0])
			if err != nil {
				return previewDoneMsg{err: err}
			}
			return previewDoneMsg{err: exec.Command("open", local).Start()}
		}

		// QuickLook batch: skip video siblings (qlmanage would crash on them).
		var paths []string
		for _, e := range ordered {
			if isVideoFile(e.Name) {
				continue
			}
			local, err := resolve(e)
			if err != nil {
				continue
			}
			paths = append(paths, local)
		}
		if len(paths) == 0 {
			return previewDoneMsg{err: fmt.Errorf("no previewable files")}
		}
		args := append([]string{"-p"}, paths...)
		cmd := exec.Command("qlmanage", args...)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		return previewDoneMsg{err: cmd.Run()}
	}
}

type symlinkResolvedMsg struct {
	targetDir string // non-empty if the link resolved to a directory
	infoText  string // non-empty if it resolved to a non-directory
	err       error
}

// resolveSymlinkAndNav reads a symlink and stats its target. The caller's
// Update handler converts the result into a navigate() call when targetDir is
// set, so gen bookkeeping stays in one place.
func resolveSymlinkAndNav(c *Client, linkPath string) tea.Cmd {
	return func() tea.Msg {
		target, err := c.conn.SFTP.ReadLink(linkPath)
		if err != nil {
			return symlinkResolvedMsg{err: fmt.Errorf("readlink: %w", err)}
		}
		if !path.IsAbs(target) {
			target = path.Join(path.Dir(linkPath), target)
		}
		target = path.Clean(target)
		st, err := c.conn.SFTP.Stat(target)
		if err != nil {
			return symlinkResolvedMsg{err: fmt.Errorf("symlink target %s: %w", target, err)}
		}
		if st.IsDir() {
			return symlinkResolvedMsg{targetDir: target}
		}
		return symlinkResolvedMsg{infoText: "symlink → " + target}
	}
}

func runDownload(c *Client, e Entry, dest string) tea.Cmd {
	return func() tea.Msg {
		if e.IsDir {
			out, err := DownloadTree(c, e.Path, dest)
			return transferDoneMsg{label: "downloaded " + e.Name, dest: out, err: err}
		}
		out, err := DownloadFile(c, e.Path, dest)
		return transferDoneMsg{label: "downloaded " + e.Name, dest: out, err: err}
	}
}

func runUpload(c *Client, local, remoteDir string) tea.Cmd {
	return func() tea.Msg {
		out, err := UploadFile(c, local, remoteDir)
		return transferDoneMsg{label: "uploaded " + filepath.Base(local), dest: out, err: err}
	}
}

func runRename(c *Client, oldPath, newPath, newName string) tea.Cmd {
	return func() tea.Msg {
		err := c.conn.SFTP.Rename(oldPath, newPath)
		return transferDoneMsg{label: "renamed → " + newName, dest: newPath, err: err}
	}
}

func runMkdir(c *Client, p string) tea.Cmd {
	return func() tea.Msg {
		err := c.conn.SFTP.MkdirAll(p)
		return transferDoneMsg{label: "mkdir " + p, dest: p, err: err}
	}
}

func runDelete(c *Client, p string) tea.Cmd {
	return func() tea.Msg {
		err := RemoveAll(c, p)
		return transferDoneMsg{label: "deleted " + path.Base(p), dest: p, err: err}
	}
}

// ---- Update ----

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case dirLoadedMsg:
		// Drop stale loads: user has navigated elsewhere since dispatch.
		if msg.gen != m.loadGen {
			return m, nil
		}
		m.loading = false
		m.loadingPath = ""
		if msg.err != nil {
			m.status = "error: " + msg.err.Error()
			m.isErr = true
			return m, nil
		}
		m.entries = msg.entries
		m.cursor = 0
		m.scroll = 0
		m.applyFilter()
		if m.pendingSelect != "" {
			for i, e := range m.filtered {
				if e.Name == m.pendingSelect {
					m.cursor = i
					break
				}
			}
			m.pendingSelect = ""
		}
		m.status = ""
		m.isErr = false
		return m, nil

	case previewEventMsg:
		switch msg.kind {
		case "showing":
			m.previewN = msg.index
			m.status = fmt.Sprintf("previewing %d/%d…", msg.index+1, m.previewM)
			m.isErr = false
		case "closed":
			m.previewActive = false
			m.status = ""
			m.isErr = false
			return m, nil
		case "error":
			if msg.err != nil {
				m.status = "preview: " + msg.err.Error()
				m.isErr = true
			}
		}
		// Keep listening on the session channel until it closes.
		return m, awaitNextPreviewEvent()

	case previewDoneMsg:
		if msg.err != nil {
			m.status = "preview: " + msg.err.Error()
			m.isErr = true
		} else {
			m.status = ""
			m.isErr = false
		}
		return m, nil

	case transferDoneMsg:
		if msg.err != nil {
			m.status = msg.label + ": " + msg.err.Error()
			m.isErr = true
			return m, nil
		}
		m.status = msg.label + " → " + msg.dest
		m.isErr = false
		// Preserve cursor across the post-action refresh
		// (only if not already set, e.g. by rename to its new name).
		if m.pendingSelect == "" {
			if e, ok := m.currentEntry(); ok {
				m.pendingSelect = e.Name
			}
		}
		return m, m.refreshCwd()

	case bookmarkSavedMsg:
		if msg.err != nil {
			m.status = "bookmark: " + msg.err.Error()
			m.isErr = true
		} else {
			m.status = "bookmark saved"
			m.isErr = false
		}
		return m, nil

	case statusMsg:
		m.status = msg.text
		m.isErr = msg.isErr
		return m, nil

	case symlinkResolvedMsg:
		if msg.err != nil {
			m.status = msg.err.Error()
			m.isErr = true
			return m, nil
		}
		if msg.targetDir != "" {
			return m, m.navigate(msg.targetDir)
		}
		m.status = msg.infoText
		m.isErr = false
		return m, nil

	case connEventMsg:
		var follow tea.Cmd
		switch msg.Kind {
		case "lost":
			m.status = "connection lost"
			if msg.Err != nil {
				m.status += ": " + msg.Err.Error()
			}
			m.isErr = true
		case "reconnecting":
			m.status = fmt.Sprintf("reconnecting (attempt %d)…", msg.Attempt)
			m.isErr = true
		case "failed":
			text := fmt.Sprintf("reconnect attempt %d failed", msg.Attempt)
			if msg.Err != nil {
				text += ": " + msg.Err.Error()
			}
			text += " — retrying"
			m.status = text
			m.isErr = true
		case "reconnected":
			m.status = "reconnected — refreshing"
			m.isErr = false
			m.client.InvalidateAll()
			follow = m.refreshCwd()
		}
		// Keep listening for the next event.
		listen := listenConnEvents(m.client.conn)
		if follow != nil {
			return m, tea.Batch(follow, listen)
		}
		return m, listen

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) applyFilter() {
	m.filtered = m.filtered[:0]
	lf := strings.ToLower(m.filter)
	for _, e := range m.entries {
		if !m.showHidden && strings.HasPrefix(e.Name, ".") {
			continue
		}
		if lf != "" && !strings.Contains(strings.ToLower(e.Name), lf) {
			continue
		}
		m.filtered = append(m.filtered, e)
	}
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// previewCandidates returns the subset of `all` that are previewable (non-dir
// regular files or symlinks, which the preview path resolves), plus the index
// within that subset corresponding to the original cursor row.
func previewCandidates(all []Entry, cursorIdx int) ([]Entry, int) {
	var out []Entry
	startIdx := 0
	for i, e := range all {
		if e.IsDir {
			continue
		}
		if i == cursorIdx {
			startIdx = len(out)
		}
		out = append(out, e)
	}
	return out, startIdx
}

func (m Model) currentEntry() (Entry, bool) {
	if m.cursor < 0 || m.cursor >= len(m.filtered) {
		return Entry{}, false
	}
	return m.filtered[m.cursor], true
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Help modal: any key dismisses it; q still quits.
	if m.showHelp {
		if s := msg.String(); s == "q" || s == "ctrl+c" {
			return m, tea.Quit
		}
		m.showHelp = false
		return m, nil
	}

	// Modes that own the keyboard (typing into a prompt)
	if m.mode != modeNormal {
		switch msg.Type {
		case tea.KeyEsc:
			m.mode = modeNormal
			m.input.SetValue("")
			m.input.Blur()
			m.completions = nil
			return m, nil
		case tea.KeyEnter:
			return m.submitInput()
		case tea.KeyTab:
			if m.mode == modeCmd {
				return m.tabComplete(), nil
			}
		}
		// Reset completion cycle on any non-Tab key
		if msg.Type != tea.KeyTab {
			m.completions = nil
		}
		// Confirm dialog
		if m.mode == modeConfirmDel {
			s := msg.String()
			if s == "y" || s == "Y" {
				if m.pendingDelete != nil {
					e := *m.pendingDelete
					m.pendingDelete = nil
					m.mode = modeNormal
					return m, runDelete(m.client, e.Path)
				}
			}
			m.mode = modeNormal
			m.pendingDelete = nil
			return m, nil
		}
		// Overwrite confirm
		if m.mode == modeConfirmOverwrite {
			s := msg.String()
			if (s == "y" || s == "Y") && m.pendingDownload != nil {
				pd := *m.pendingDownload
				m.pendingDownload = nil
				m.mode = modeNormal
				local := filepath.Join(pd.dest, pd.e.Name)
				m.status = "downloading " + pd.e.Path + " → " + local + "…"
				m.isErr = false
				return m, runDownload(m.client, pd.e, pd.dest)
			}
			m.mode = modeNormal
			m.pendingDownload = nil
			m.status = "download cancelled"
			m.isErr = false
			return m, nil
		}
		// Filter mode is incremental
		if m.mode == modeFilter {
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			m.filter = m.input.Value()
			m.applyFilter()
			return m, cmd
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}

	// Normal mode
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "j", "down":
		if m.cursor < len(m.filtered)-1 {
			m.cursor++
		}
	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
		}
	case "ctrl+d":
		m.cursor += 10
		if m.cursor >= len(m.filtered) {
			m.cursor = len(m.filtered) - 1
		}
	case "ctrl+u":
		m.cursor -= 10
		if m.cursor < 0 {
			m.cursor = 0
		}
	case "g":
		m.cursor = 0
	case "G":
		m.cursor = len(m.filtered) - 1
	case "h", "left", "backspace":
		parent := path.Dir(m.cwd)
		if parent != m.cwd {
			m.pendingSelect = path.Base(m.cwd)
		}
		return m, m.navigate(parent)
	case "l", "right", "enter":
		if e, ok := m.currentEntry(); ok {
			if e.IsDir {
				return m, m.navigate(e.Path)
			}
			if e.Mode&os.ModeSymlink != 0 {
				m.status = "resolving " + e.Name + "…"
				m.isErr = false
				return m, resolveSymlinkAndNav(m.client, e.Path)
			}
		}
	case " ":
		if e, ok := m.currentEntry(); ok && !e.IsDir {
			cands, startIdx := previewCandidates(m.filtered, m.cursor)
			// Videos always go through the non-streaming `open` path because
			// QuickLook's video plugin crashes outside Cocoa apps.
			if isVideoFile(e.Name) || m.previewHelper == "" {
				m.status = fmt.Sprintf("preparing preview (%d file(s))…", len(cands))
				m.isErr = false
				return m, runPreviewMulti(m.client, m.client.conn.Host, cands, startIdx)
			}
			// Streaming session
			m.previewActive = true
			m.previewN = startIdx
			m.previewM = len(cands)
			m.status = fmt.Sprintf("previewing %d/%d…", startIdx+1, len(cands))
			m.isErr = false
			return m, startPreviewSession(m.client, m.client.conn.Host, cands, startIdx, m.previewHelper)
		}
	case ":":
		m.mode = modeCmd
		m.input.Placeholder = "path (Tab to complete)"
		m.input.SetValue(m.cwd)
		if !strings.HasSuffix(m.input.Value(), "/") {
			m.input.SetValue(m.input.Value() + "/")
		}
		m.input.CursorEnd()
		m.input.Focus()
	case "/":
		m.mode = modeFilter
		m.input.Placeholder = "filter"
		m.input.SetValue("")
		m.input.Focus()
	case "b":
		m.showBM = !m.showBM
	case "B":
		m.mode = modeAddBookmark
		m.input.Placeholder = "bookmark name"
		m.input.SetValue(path.Base(m.cwd))
		m.input.CursorEnd()
		m.input.Focus()
	case "d":
		if e, ok := m.currentEntry(); ok {
			home, _ := os.UserHomeDir()
			dest := filepath.Join(home, "Downloads")
			return m.beginDownload(e, dest)
		}
	case "D":
		if _, ok := m.currentEntry(); ok {
			m.mode = modeDownloadTo
			m.input.Placeholder = "local destination directory"
			home, _ := os.UserHomeDir()
			m.input.SetValue(filepath.Join(home, "Downloads"))
			m.input.CursorEnd()
			m.input.Focus()
		}
	case "u", "U":
		m.mode = modeUploadFrom
		m.input.Placeholder = "local file (paste/drag a file here)"
		m.input.SetValue("")
		m.input.Focus()
	case "n":
		m.mode = modeMkdir
		m.input.Placeholder = "new directory name"
		m.input.SetValue("")
		m.input.Focus()
	case "x":
		if e, ok := m.currentEntry(); ok {
			ec := e
			m.pendingDelete = &ec
			m.mode = modeConfirmDel
		}
	case "r":
		return m, m.refreshCwd()
	case "R":
		m.client.InvalidateAll()
		return m, m.refreshCwd()
	case ".":
		m.showHidden = !m.showHidden
		m.applyFilter()
	case "?":
		m.showHelp = !m.showHelp
	case "esc":
		if m.showHelp {
			m.showHelp = false
		}
	case "c":
		if e, ok := m.currentEntry(); ok {
			m.mode = modeRename
			m.input.Placeholder = "new name"
			m.input.SetValue(e.Name)
			m.input.CursorEnd()
			m.input.Focus()
		}
	case "o":
		// Open enclosing folder in Finder (download then reveal) — local convenience
		if e, ok := m.currentEntry(); ok && !e.IsDir {
			m.status = "opening locally…"
			return m, openLocally(m.client, m.client.conn.Host, e.Path)
		}
	case "y":
		// Yank: copy remote path to local clipboard via pbcopy
		if e, ok := m.currentEntry(); ok {
			go func(p string) {
				cmd := exec.Command("pbcopy")
				cmd.Stdin = strings.NewReader(p)
				_ = cmd.Run()
			}(e.Path)
			m.status = "yanked path"
		}
	}
	// Numeric: jump to bookmark
	if s := msg.String(); len(s) == 1 && s[0] >= '1' && s[0] <= '9' {
		idx, _ := strconv.Atoi(s)
		bms := m.bookmarks.ForHost(m.client.conn.Host)
		if idx-1 < len(bms) {
			return m, m.navigate(bms[idx-1].Path)
		}
	}
	return m, nil
}

func (m Model) beginDownload(e Entry, destDir string) (tea.Model, tea.Cmd) {
	local := filepath.Join(destDir, e.Name)
	// Local overwrite check
	if _, err := os.Stat(local); err == nil {
		ec := e
		m.pendingDownload = &pendingDl{e: ec, dest: destDir}
		m.mode = modeConfirmOverwrite
		return m, nil
	}
	m.status = "downloading " + e.Path + " → " + local + "…"
	m.isErr = false
	return m, runDownload(m.client, e, destDir)
}

func openLocally(c *Client, host, remote string) tea.Cmd {
	return func() tea.Msg {
		local, err := fetchForPreview(c, host, remote)
		if err != nil {
			return statusMsg{text: "open: " + err.Error(), isErr: true}
		}
		if err := exec.Command("open", "-R", local).Run(); err != nil {
			return statusMsg{text: "open: " + err.Error(), isErr: true}
		}
		return statusMsg{text: "revealed " + filepath.Base(local), isErr: false}
	}
}

func (m Model) tabComplete() Model {
	val := m.input.Value()
	if len(m.completions) > 0 && strings.HasPrefix(val, m.compBase) {
		// Cycle through existing completions
		m.compIdx = (m.compIdx + 1) % len(m.completions)
		m.input.SetValue(m.compBase + m.completions[m.compIdx])
		m.input.CursorEnd()
		return m
	}
	dirPart, matches, err := m.client.Complete(val)
	if err != nil || len(matches) == 0 {
		return m
	}
	// Compute base (prefix to keep — everything up to last '/')
	base := val
	if i := strings.LastIndex(val, "/"); i >= 0 {
		base = val[:i+1]
	} else {
		base = ""
	}
	// If only one match: complete fully
	if len(matches) == 1 {
		m.input.SetValue(base + matches[0])
		m.input.CursorEnd()
		m.completions = nil
		return m
	}
	// Otherwise: pick common prefix; if same as typed, prepare cycle
	common := commonPrefix(matches)
	typedBase := ""
	if i := strings.LastIndex(val, "/"); i >= 0 {
		typedBase = val[i+1:]
	} else {
		typedBase = val
	}
	if len(common) > len(typedBase) {
		m.input.SetValue(base + common)
		m.input.CursorEnd()
		m.completions = nil
		_ = dirPart
		return m
	}
	// Cycle mode
	sort.Strings(matches)
	m.completions = matches
	m.compIdx = 0
	m.compBase = base
	m.compPrefix = typedBase
	m.input.SetValue(base + matches[0])
	m.input.CursorEnd()
	return m
}

func commonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	p := ss[0]
	for _, s := range ss[1:] {
		// case-insensitive on macOS feel; keep case from first.
		i := 0
		for i < len(p) && i < len(s) && strings.EqualFold(string(p[i]), string(s[i])) {
			i++
		}
		p = p[:i]
		if p == "" {
			break
		}
	}
	return p
}

func (m Model) submitInput() (tea.Model, tea.Cmd) {
	val := strings.TrimSpace(m.input.Value())
	mode := m.mode
	m.mode = modeNormal
	m.input.Blur()
	m.completions = nil

	switch mode {
	case modeCmd:
		if val == "" {
			return m, nil
		}
		full := m.client.CleanPath(val)
		// If it's a file, preview it; else cd.
		fi, err := m.client.Stat(full)
		if err == nil && !fi.IsDir() {
			m.status = "previewing " + path.Base(full) + "…"
			return m, runPreview(m.client, m.client.conn.Host, full)
		}
		return m, m.navigate(full)
	case modeFilter:
		// Keep filter; nothing else to do.
		return m, nil
	case modeAddBookmark:
		if val == "" {
			return m, nil
		}
		m.bookmarks.Add(val, m.client.conn.Host, m.cwd)
		err := m.bookmarks.Save()
		return m, func() tea.Msg { return bookmarkSavedMsg{err: err} }
	case modeDownloadTo:
		e, ok := m.currentEntry()
		if !ok {
			return m, nil
		}
		dest := expandHome(val)
		return m.beginDownload(e, dest)
	case modeUploadFrom:
		local := expandHome(unescapeDroppedPath(val))
		if local == "" {
			return m, nil
		}
		m.status = "uploading " + filepath.Base(local) + "…"
		return m, runUpload(m.client, local, m.cwd)
	case modeMkdir:
		if val == "" {
			return m, nil
		}
		newPath := path.Join(m.cwd, val)
		return m, runMkdir(m.client, newPath)
	case modeRename:
		e, ok := m.currentEntry()
		if !ok || val == "" || val == e.Name {
			return m, nil
		}
		newPath := path.Join(m.cwd, val)
		m.status = "renaming " + e.Name + " → " + val + "…"
		m.pendingSelect = val
		return m, runRename(m.client, e.Path, newPath, val)
	}
	return m, nil
}

// ---- View ----

func (m Model) View() string {
	if m.height == 0 {
		return ""
	}
	if m.showHelp {
		return renderHelpModal(m.width, m.height)
	}
	header := headerStyle.Render(m.client.conn.Display) + dimStyle.Render(" — ") + lipgloss.NewStyle().Bold(true).Render(m.cwd)

	// Compute viewport — reserve a line for metadata if an entry is selected.
	reserved := 3 // header + status + padding
	if _, ok := m.currentEntry(); ok {
		reserved = 4
	}
	listH := m.height - reserved
	if listH < 3 {
		listH = 3
	}

	// Bookmarks pane width
	bmW := 0
	var bmView string
	if m.showBM {
		bmW = 28
		bmView = m.renderBookmarks(listH)
	}
	mainW := m.width - bmW - 1
	if mainW < 20 {
		mainW = m.width
		bmW = 0
		bmView = ""
	}

	mainView := m.renderList(mainW, listH)

	var body string
	if bmView != "" {
		body = lipgloss.JoinHorizontal(lipgloss.Top, mainView, " ", bmView)
	} else {
		body = mainView
	}

	metaLine := m.renderMetadata(m.width)
	statusLine := m.renderStatus()

	if metaLine == "" {
		return lipgloss.JoinVertical(lipgloss.Left, header, body, statusLine)
	}
	return lipgloss.JoinVertical(lipgloss.Left, header, body, metaLine, statusLine)
}

// renderMetadata returns one line describing the currently selected entry's
// permissions, size, mtime, and atime (plus "days ago" for the two times).
// Returns "" if no entry is highlighted (loading or empty dir).
func (m Model) renderMetadata(w int) string {
	e, ok := m.currentEntry()
	if !ok {
		return ""
	}
	perms := e.Mode.String()
	size := formatSize(e.Size)
	if e.IsDir {
		size = "—"
	}
	mtimeStr := formatTimeWithAge(e.ModTime)
	atimeStr := ""
	if !e.AccTime.IsZero() {
		atimeStr = formatTimeWithAge(e.AccTime)
	}

	left := dimStyle.Render(perms) + "  " + dimStyle.Render(size)
	mid := dimStyle.Render("modified ") + mtimeStr
	right := ""
	if atimeStr != "" {
		right = dimStyle.Render("accessed ") + atimeStr
	}

	full := left + "  " + mid
	if right != "" {
		full += "  " + right
	}
	// Truncate when the terminal is narrow.
	if lipgloss.Width(full) > w {
		full = truncate(full, w)
	}
	return full
}

// formatTimeWithAge renders "2026-05-25 14:30 (1.50 days ago)".
func formatTimeWithAge(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	stamp := t.Format("2006-01-02 15:04")
	days := time.Since(t).Hours() / 24
	var age string
	switch {
	case days < 0:
		age = fmt.Sprintf("%.2f days from now", -days)
	case days < 1:
		age = fmt.Sprintf("%.2f days ago", days)
	default:
		age = fmt.Sprintf("%.2f days ago", days)
	}
	return stamp + dimStyle.Render(" ("+age+")")
}

func (m *Model) ensureCursorVisible(viewH int) {
	if m.cursor < m.scroll {
		m.scroll = m.cursor
	}
	if m.cursor >= m.scroll+viewH {
		m.scroll = m.cursor - viewH + 1
	}
	if m.scroll < 0 {
		m.scroll = 0
	}
}

func (m Model) renderList(w, h int) string {
	// Mutate scroll via copy to keep View const-ish
	mp := m
	mp.ensureCursorVisible(h)

	var b strings.Builder
	if mp.loading && len(mp.filtered) == 0 {
		msg := matchHint.Render("loading " + mp.loadingPath + "…")
		b.WriteString("  " + msg + "\n")
		for i := 1; i < h; i++ {
			b.WriteString("\n")
		}
		return b.String()
	}
	end := mp.scroll + h
	if end > len(mp.filtered) {
		end = len(mp.filtered)
	}
	// Reserve gutter columns: marker + space
	const gutter = 2
	for i := mp.scroll; i < end; i++ {
		e := mp.filtered[i]
		selected := i == mp.cursor
		line := formatEntryLine(e, w-gutter, selected)
		if selected {
			marker := cursorBarStyle.Render("▌ ")
			b.WriteString(marker)
			b.WriteString(line)
		} else {
			b.WriteString(gutterStyle.Render("  "))
			b.WriteString(line)
		}
		b.WriteString("\n")
	}
	// Pad to height for stable layout
	for i := end - mp.scroll; i < h; i++ {
		b.WriteString("\n")
	}
	return b.String()
}

func padRight(s string, w int) string {
	visible := lipgloss.Width(s)
	if visible >= w {
		return s
	}
	return s + strings.Repeat(" ", w-visible)
}

func formatEntryLine(e Entry, w int, selected bool) string {
	isLink := e.Mode&os.ModeSymlink != 0
	rawName := e.Name
	switch {
	case e.IsDir:
		rawName += "/"
	case isLink:
		rawName += "@"
	}
	size := formatSize(e.Size)
	if e.IsDir {
		size = ""
	}
	nameW := w - 12
	if nameW < 10 {
		nameW = w
	}
	nameTrunc := truncate(rawName, nameW)
	plain := fmt.Sprintf("%-*s %8s", nameW, nameTrunc, size)
	if selected {
		// One style for the whole line so dir/link colors don't bleed through.
		return selectedLine.Render(plain)
	}
	var styledName string
	switch {
	case e.IsDir:
		styledName = dirStyle.Render(nameTrunc)
	case isLink:
		styledName = linkStyle.Render(nameTrunc)
	default:
		styledName = nameTrunc
	}
	pad := nameW - lipgloss.Width(styledName)
	if pad < 0 {
		pad = 0
	}
	return styledName + strings.Repeat(" ", pad) + " " + dimStyle.Render(fmt.Sprintf("%8s", size))
}

func truncate(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	// crude — operate on raw runes; styles may be slightly off
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return string(r[:w-1]) + "…"
}

func (m Model) renderBookmarks(h int) string {
	bms := m.bookmarks.ForHost(m.client.conn.Host)
	var b strings.Builder
	b.WriteString(bmHeader.Render("Bookmarks") + "\n")
	if len(bms) == 0 {
		b.WriteString(dimStyle.Render("(none — press B)") + "\n")
	}
	for i, x := range bms {
		idx := dimStyle.Render(fmt.Sprintf("%d.", i+1))
		b.WriteString(fmt.Sprintf("%s %s\n  %s\n", idx, x.Name, dimStyle.Render(truncate(x.Path, 26))))
	}
	return bmPane.Width(26).Height(h - 2).Render(b.String())
}

func (m Model) renderStatus() string {
	if m.mode != modeNormal {
		label := ""
		switch m.mode {
		case modeCmd:
			label = "cd"
		case modeFilter:
			label = "/"
		case modeAddBookmark:
			label = "bookmark"
		case modeDownloadTo:
			label = "download to"
		case modeUploadFrom:
			label = "upload from"
		case modeMkdir:
			label = "mkdir"
		case modeRename:
			label = "rename"
		case modeConfirmDel:
			name := ""
			if m.pendingDelete != nil {
				name = m.pendingDelete.Name
			}
			return errStyle.Render("delete " + name + "?  [y/N]")
		case modeConfirmOverwrite:
			if m.pendingDownload != nil {
				local := filepath.Join(m.pendingDownload.dest, m.pendingDownload.e.Name)
				return errStyle.Render(local + " exists. Overwrite?  [y/N]")
			}
			return errStyle.Render("overwrite? [y/N]")
		}
		// hint: cycle completions
		hint := ""
		if m.mode == modeCmd && len(m.completions) > 1 {
			hint = matchHint.Render(fmt.Sprintf("  (%d/%d, Tab to cycle)", m.compIdx+1, len(m.completions)))
		}
		return promptStyle.Render(label+":") + " " + m.input.View() + hint
	}
	left := m.status
	if left == "" {
		left = m.helpHint()
	}
	st := statusStyle
	if m.isErr {
		st = errStyle
	}
	return st.Render(left)
}

func (m Model) helpHint() string {
	if m.loading {
		return "loading " + m.loadingPath + "…  (press h to cancel)"
	}
	hidden := ""
	if m.showHidden {
		hidden = " ·hidden"
	}
	filter := ""
	if m.filter != "" {
		filter = " ·/" + m.filter
	}
	return fmt.Sprintf("%d items%s%s — :cd  space:preview  b:bookmarks  B:add  d:dl→Downloads  u:upload(picker)  ?:help",
		len(m.filtered), hidden, filter)
}

var (
	helpTitleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("213"))
	helpSectionStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39")).MarginTop(1)
	helpKeyStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	helpDescStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	helpBoxStyle      = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("213")).
		Background(lipgloss.Color("235")).
		Padding(1, 3)
	helpFooterStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Italic(true).MarginTop(1)
)

type helpEntry struct{ key, desc string }

var helpSections = []struct {
	title   string
	entries []helpEntry
}{
	{"Navigation", []helpEntry{
		{"j  k  ↑  ↓", "move cursor"},
		{"g  G", "top / bottom"},
		{"^u  ^d", "page up / page down"},
		{"h  ←  ⌫", "parent directory"},
		{"l  →  ↵", "enter directory"},
		{"Space", "QuickLook preview"},
		{":", "jump to path (Tab completes)"},
		{"/", "filter"},
		{".", "toggle hidden"},
		{"r  R", "refresh dir / clear all caches"},
	}},
	{"Transfers", []helpEntry{
		{"d", "download to ~/Downloads"},
		{"D", "download to specific directory"},
		{"u/U", "upload via typed/dropped path"},
		{"o", "reveal in Finder"},
		{"y", "copy remote path to clipboard"},
	}},
	{"Manage", []helpEntry{
		{"c", "rename"},
		{"n", "make directory"},
		{"x", "delete (confirm)"},
	}},
	{"Bookmarks", []helpEntry{
		{"b", "toggle bookmarks pane"},
		{"B", "add current directory as bookmark"},
		{"1–9", "jump to bookmark"},
	}},
}

func renderHelpModal(termW, termH int) string {
	var body strings.Builder
	body.WriteString(helpTitleStyle.Render("bsftp — help"))
	body.WriteString("\n")

	// Find max key column width for nice alignment within each section.
	keyW := 0
	for _, s := range helpSections {
		for _, e := range s.entries {
			if w := lipgloss.Width(e.key); w > keyW {
				keyW = w
			}
		}
	}

	for _, s := range helpSections {
		body.WriteString(helpSectionStyle.Render(s.title))
		body.WriteString("\n")
		for _, e := range s.entries {
			pad := keyW - lipgloss.Width(e.key)
			if pad < 0 {
				pad = 0
			}
			body.WriteString("  ")
			body.WriteString(helpKeyStyle.Render(e.key))
			body.WriteString(strings.Repeat(" ", pad))
			body.WriteString("   ")
			body.WriteString(helpDescStyle.Render(e.desc))
			body.WriteString("\n")
		}
	}

	body.WriteString(helpFooterStyle.Render("? or Esc to close · q to quit"))

	box := helpBoxStyle.Render(body.String())
	return lipgloss.Place(termW, termH, lipgloss.Center, lipgloss.Center, box)
}

func helpLine() string {
	return "press ? for help"
}
