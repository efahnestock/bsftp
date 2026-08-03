package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kevinburke/ssh_config"
	"github.com/pkg/sftp"
	xknownhosts "github.com/skeema/knownhosts"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/term"
)

type Conn struct {
	Host    string
	SSH     *ssh.Client
	SFTP    *sftp.Client
	Home    string
	Display string

	// Re-dial state — cached so reconnect doesn't re-prompt for passphrase.
	cfg  *ssh.ClientConfig
	addr string

	mu      sync.Mutex
	healthy bool
	closed  bool

	events chan ConnEvent
}

// ConnEvent is delivered on Events() when the connection state changes.
type ConnEvent struct {
	Kind    string // "lost", "reconnecting", "reconnected", "failed"
	Err     error
	Attempt int
}

func (c *Conn) Events() <-chan ConnEvent { return c.events }

// Healthy reports whether the connection is currently believed alive.
func (c *Conn) Healthy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.healthy
}

func (c *Conn) emit(ev ConnEvent) {
	select {
	case c.events <- ev:
	default:
		// Buffer full — drop. UI only needs the latest state, not full history.
	}
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, p[2:])
	}
	return p
}

// unescapeDroppedPath cleans a local file path that was pasted or dragged into
// the terminal. Terminals insert shell-escaped paths (a backslash before
// spaces, parentheses, etc.), and users sometimes paste quoted paths. Undo both
// so the result matches the actual path on disk.
func unescapeDroppedPath(p string) string {
	p = strings.TrimSpace(p)
	if len(p) >= 2 {
		// Single-quoted: everything is literal except the '\'' sequence.
		if p[0] == '\'' && p[len(p)-1] == '\'' {
			return strings.ReplaceAll(p[1:len(p)-1], `'\''`, `'`)
		}
		// Double-quoted: only \" \\ \$ \` are escapes.
		if p[0] == '"' && p[len(p)-1] == '"' {
			return unescapeBackslashes(p[1:len(p)-1], `"\$`+"`")
		}
	}
	// Unquoted: a backslash escapes any following character.
	return unescapeBackslashes(p, "")
}

// unescapeBackslashes drops a backslash before the next character. If escapable
// is empty every character may be escaped; otherwise only characters in
// escapable have their leading backslash removed.
func unescapeBackslashes(s, escapable string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && (escapable == "" || strings.IndexByte(escapable, s[i+1]) >= 0) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func gatherAuth(cfgIdentities []string) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	var signers []ssh.Signer
	var encryptedKeys []string

	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if c, err := net.Dial("unix", sock); err == nil {
			ag := agent.NewClient(c)
			if list, err := ag.List(); err == nil && len(list) > 0 {
				methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
			}
		}
	}

	seen := map[string]bool{}
	tryKey := func(path string) {
		path = expandHome(path)
		if seen[path] {
			return
		}
		seen[path] = true
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			var pe *ssh.PassphraseMissingError
			if errors.As(err, &pe) {
				encryptedKeys = append(encryptedKeys, path)
			}
			return
		}
		signers = append(signers, signer)
	}
	for _, p := range cfgIdentities {
		tryKey(p)
	}
	// Common default key names
	for _, name := range []string{"id_ed25519", "id_rsa", "id_ecdsa"} {
		tryKey("~/.ssh/" + name)
	}

	// Prompt for passphrase on encrypted keys if we have nothing usable yet.
	if len(signers) == 0 && len(methods) == 0 && len(encryptedKeys) > 0 {
		for _, path := range encryptedKeys {
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			fmt.Fprintf(os.Stderr, "Passphrase for %s: ", path)
			pw, err := term.ReadPassword(int(syscall.Stdin))
			fmt.Fprintln(os.Stderr)
			if err != nil || len(pw) == 0 {
				continue
			}
			signer, err := ssh.ParsePrivateKeyWithPassphrase(data, pw)
			if err != nil {
				fmt.Fprintln(os.Stderr, "  ", err)
				continue
			}
			signers = append(signers, signer)
			break // one good key is enough
		}
	}

	if len(signers) > 0 {
		methods = append(methods, ssh.PublicKeys(signers...))
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("no usable SSH auth methods (no agent, no readable/decryptable key)")
	}
	return methods, nil
}

func loadKnownHosts() (*xknownhosts.HostKeyDB, error) {
	khPath := expandHome("~/.ssh/known_hosts")
	if _, err := os.Stat(khPath); err != nil {
		return nil, nil
	}
	return xknownhosts.NewDB(khPath)
}

func Dial(hostAlias string) (*Conn, error) {
	cfgPath := expandHome("~/.ssh/config")
	var cfg *ssh_config.Config
	if f, err := os.Open(cfgPath); err == nil {
		cfg, _ = ssh_config.Decode(f)
		f.Close()
	}

	get := func(key string) string {
		if cfg != nil {
			if v, _ := cfg.Get(hostAlias, key); v != "" {
				return v
			}
		}
		return ssh_config.Default(key)
	}

	hostName := get("HostName")
	if hostName == "" {
		hostName = hostAlias
	}
	user := get("User")
	if user == "" {
		user = os.Getenv("USER")
	}
	port := get("Port")
	if port == "" {
		port = "22"
	}

	var ids []string
	if cfg != nil {
		ids, _ = cfg.GetAll(hostAlias, "IdentityFile")
	}

	auth, err := gatherAuth(ids)
	if err != nil {
		return nil, err
	}
	kh, err := loadKnownHosts()
	if err != nil {
		return nil, fmt.Errorf("known_hosts: %w", err)
	}

	addr := net.JoinHostPort(hostName, port)

	var hkcb ssh.HostKeyCallback
	var algos []string
	if kh != nil {
		hkcb = kh.HostKeyCallback()
		algos = kh.HostKeyAlgorithms(addr)
	} else {
		hkcb = ssh.InsecureIgnoreHostKey()
	}

	sshCfg := &ssh.ClientConfig{
		User:              user,
		Auth:              auth,
		HostKeyCallback:   hkcb,
		HostKeyAlgorithms: algos,
		Timeout:           15 * time.Second,
	}
	client, sc, home, err := openSession(addr, sshCfg)
	if err != nil {
		return nil, err
	}

	c := &Conn{
		Host:    hostAlias,
		SSH:     client,
		SFTP:    sc,
		Home:    home,
		Display: fmt.Sprintf("%s@%s", user, hostAlias),
		cfg:     sshCfg,
		addr:    addr,
		healthy: true,
		events:  make(chan ConnEvent, 8),
	}
	go c.keepalive()
	return c, nil
}

// openSession does one round of SSH+SFTP dial.
func openSession(addr string, cfg *ssh.ClientConfig) (*ssh.Client, *sftp.Client, string, error) {
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, nil, "", fmt.Errorf("ssh dial %s: %w", addr, err)
	}
	sc, err := sftp.NewClient(client,
		sftp.UseConcurrentReads(true),
		sftp.UseConcurrentWrites(true),
		sftp.MaxConcurrentRequestsPerFile(64),
	)
	if err != nil {
		client.Close()
		return nil, nil, "", fmt.Errorf("sftp: %w", err)
	}
	home, err := sc.Getwd()
	if err != nil {
		home = "/"
	}
	return client, sc, home, nil
}

// keepalive pings the SSH connection every 30s. On failure it marks the conn
// as unhealthy, emits a "lost" event, and spawns a reconnect goroutine.
func (c *Conn) keepalive() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return
		}
		if !c.healthy {
			c.mu.Unlock()
			continue
		}
		sshClient := c.SSH
		c.mu.Unlock()
		if sshClient == nil {
			continue
		}
		_, _, err := sshClient.SendRequest("keepalive@bsftp", true, nil)
		if err != nil {
			c.markLost(err)
		}
	}
}

// markLost transitions the connection to the disconnected state and kicks off
// a background reconnect. Safe to call from any goroutine; idempotent.
func (c *Conn) markLost(err error) {
	c.mu.Lock()
	if c.closed || !c.healthy {
		c.mu.Unlock()
		return
	}
	c.healthy = false
	c.mu.Unlock()
	c.emit(ConnEvent{Kind: "lost", Err: err})
	go c.reconnectLoop()
}

// reconnectLoop retries openSession with exponential backoff until success
// or until the connection is closed.
func (c *Conn) reconnectLoop() {
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		c.emit(ConnEvent{Kind: "reconnecting", Attempt: attempt})
		client, sc, home, err := openSession(c.addr, c.cfg)
		if err == nil {
			c.mu.Lock()
			if c.SFTP != nil {
				_ = c.SFTP.Close()
			}
			if c.SSH != nil {
				_ = c.SSH.Close()
			}
			c.SSH = client
			c.SFTP = sc
			c.Home = home
			c.healthy = true
			c.mu.Unlock()
			c.emit(ConnEvent{Kind: "reconnected", Attempt: attempt})
			return
		}
		c.emit(ConnEvent{Kind: "failed", Err: err, Attempt: attempt})
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// TriggerReconnect lets a caller force the connection into the recovery path
// without waiting for the keepalive tick.
func (c *Conn) TriggerReconnect(err error) { c.markLost(err) }

func (c *Conn) Close() {
	c.mu.Lock()
	c.closed = true
	sftpc := c.SFTP
	sshc := c.SSH
	c.mu.Unlock()
	if sftpc != nil {
		sftpc.Close()
	}
	if sshc != nil {
		sshc.Close()
	}
}
