package docker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// defaultSSHSocket is the remote daemon's socket when the ssh:// URL names
// none — the Docker default on every mainstream distro.
const defaultSSHSocket = "/var/run/docker.sock"

// defaultSSHConnectTimeout bounds the SSH handshake to a remote daemon.
const defaultSSHConnectTimeout = 15 * time.Second

// SSHAuth configures the SSH connection behind an ssh:// Host: how to
// authenticate (methods are tried in order: agent, key, password; at least
// one must be set) and how to verify the remote host key (the zero value
// verifies against ~/.ssh/known_hosts).
type SSHAuth struct {
	// UseAgent authenticates using the local SSH agent (SSH_AUTH_SOCK).
	UseAgent bool
	// KeyFile is the path to a PEM-encoded private key. "~" is expanded to
	// the user's home directory.
	KeyFile string
	// KeyBytes is a PEM-encoded private key (alternative to KeyFile).
	KeyBytes []byte
	// Passphrase decrypts an encrypted KeyFile/KeyBytes when set.
	Passphrase string
	// Password authenticates with a password.
	Password string
	// KnownHostsFile is an OpenSSH known_hosts file. Defaults to
	// ~/.ssh/known_hosts. "~" is expanded to the user's home directory.
	KnownHostsFile string
	// InsecureIgnoreHostKey disables host-key verification entirely. NEVER
	// use in production — it makes the connection vulnerable to MITM attacks.
	InsecureIgnoreHostKey bool
	// ConnectTimeout bounds the SSH dial+handshake. Zero means 15s.
	ConnectTimeout time.Duration
}

// sshDialer reaches a remote Docker daemon's unix socket through SSH: every
// docker API request opens a direct-streamlocal channel on one shared SSH
// connection (pure Go — no ssh binary, no docker CLI on the remote; the
// remote sshd must allow streamlocal forwarding and the SSH user must reach
// the socket). A dead connection is re-established on the next dial.
type sshDialer struct {
	addr   string // host:port
	socket string // remote unix socket path
	cfg    *ssh.ClientConfig

	mu        sync.Mutex
	client    *ssh.Client
	agentConn net.Conn
}

// newSSHDialer parses an ssh://user@host[:port][/socket] URL and prepares the
// dialer; the SSH connection itself is established lazily on first use.
func newSSHDialer(hostURL string, auth SSHAuth) (*sshDialer, error) {
	u, err := url.Parse(hostURL)
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: invalid ssh host %q: %w", hostURL, err)
	}
	if u.User == nil || u.User.Username() == "" {
		return nil, fmt.Errorf("docker sandbox: ssh host %q must carry a user (ssh://user@host)", hostURL)
	}
	addr := u.Host
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "22")
	}
	socket := defaultSSHSocket
	if u.Path != "" && u.Path != "/" {
		socket = u.Path
	}
	methods, agentConn, err := buildSSHAuthMethods(auth)
	if err != nil {
		return nil, err
	}
	hostKey, err := buildSSHHostKeyCallback(auth)
	if err != nil {
		if agentConn != nil {
			_ = agentConn.Close()
		}
		return nil, err
	}
	timeout := auth.ConnectTimeout
	if timeout <= 0 {
		timeout = defaultSSHConnectTimeout
	}
	return &sshDialer{
		addr:   addr,
		socket: socket,
		cfg: &ssh.ClientConfig{
			User:            u.User.Username(),
			Auth:            methods,
			HostKeyCallback: hostKey,
			Timeout:         timeout,
		},
		agentConn: agentConn,
	}, nil
}

// DialContext opens one channel to the remote daemon socket, (re)connecting
// the SSH transport as needed: a channel-open failure drops the cached client
// and retries once on a fresh connection, so a severed transport heals
// without surfacing every queued request's error.
func (d *sshDialer) DialContext(_ context.Context, _, _ string) (net.Conn, error) {
	client, err := d.connect(false)
	if err != nil {
		return nil, err
	}
	conn, err := client.Dial("unix", d.socket)
	if err == nil {
		return conn, nil
	}
	client, rerr := d.connect(true)
	if rerr != nil {
		return nil, fmt.Errorf("docker sandbox: ssh reconnect after %v: %w", err, rerr)
	}
	conn, err = client.Dial("unix", d.socket)
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: dialing remote socket %s: %w", d.socket, err)
	}
	return conn, nil
}

// connect returns the shared SSH client, dialing when none is cached or when
// the caller declares the cached one dead (fresh).
func (d *sshDialer) connect(fresh bool) (*ssh.Client, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.client != nil {
		if !fresh {
			return d.client, nil
		}
		_ = d.client.Close()
		d.client = nil
	}
	client, err := ssh.Dial("tcp", d.addr, d.cfg)
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: ssh dial %s: %w", d.addr, err)
	}
	d.client = client
	return client, nil
}

// Close severs the SSH transport and the agent socket, if any.
func (d *sshDialer) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.client != nil {
		_ = d.client.Close()
		d.client = nil
	}
	if d.agentConn != nil {
		_ = d.agentConn.Close()
		d.agentConn = nil
	}
	return nil
}

// buildSSHAuthMethods turns an SSHAuth into ordered ssh.AuthMethods: agent
// first, then private key, then password. When UseAgent is set, the returned
// net.Conn is the agent socket and must be closed with the dialer.
func buildSSHAuthMethods(cfg SSHAuth) ([]ssh.AuthMethod, net.Conn, error) {
	var methods []ssh.AuthMethod
	var agentConn net.Conn

	if cfg.UseAgent {
		sock := os.Getenv("SSH_AUTH_SOCK")
		if sock == "" {
			return nil, nil, errors.New("docker sandbox: SSHAuth.UseAgent set but SSH_AUTH_SOCK is empty")
		}
		conn, err := net.Dial("unix", sock)
		if err != nil {
			return nil, nil, fmt.Errorf("docker sandbox: connect ssh agent: %w", err)
		}
		agentConn = conn
		methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
	}

	if cfg.KeyFile != "" || len(cfg.KeyBytes) > 0 {
		keyBytes := cfg.KeyBytes
		if cfg.KeyFile != "" {
			path, err := expandHome(cfg.KeyFile)
			if err != nil {
				return nil, nil, err
			}
			var err2 error
			keyBytes, err2 = os.ReadFile(path)
			if err2 != nil {
				return nil, nil, fmt.Errorf("docker sandbox: read key file: %w", err2)
			}
		}
		var signer ssh.Signer
		var err error
		if cfg.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(keyBytes, []byte(cfg.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(keyBytes)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("docker sandbox: parse private key: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if cfg.Password != "" {
		methods = append(methods, ssh.Password(cfg.Password))
	}

	if len(methods) == 0 {
		return nil, nil, errors.New("docker sandbox: no SSH authentication method configured (set UseAgent, KeyFile, KeyBytes or Password)")
	}
	return methods, agentConn, nil
}

// buildSSHHostKeyCallback resolves the host-key verification strategy.
func buildSSHHostKeyCallback(cfg SSHAuth) (ssh.HostKeyCallback, error) {
	if cfg.InsecureIgnoreHostKey {
		// Documented opt-in for dev/test; vulnerable to MITM. See SSHAuth.
		return ssh.InsecureIgnoreHostKey(), nil //nolint:gosec // explicit opt-in
	}
	file := cfg.KnownHostsFile
	if file == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("docker sandbox: locate known_hosts: %w", err)
		}
		file = filepath.Join(home, ".ssh", "known_hosts")
	} else {
		expanded, err := expandHome(file)
		if err != nil {
			return nil, err
		}
		file = expanded
	}
	cb, err := knownhosts.New(file)
	if err != nil {
		return nil, fmt.Errorf("docker sandbox: load known_hosts %s: %w", file, err)
	}
	return cb, nil
}

// expandHome replaces a leading "~" with the user's home directory.
func expandHome(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("docker sandbox: expand %q: %w", p, err)
		}
		return filepath.Join(home, strings.TrimPrefix(p[1:], "/")), nil
	}
	return p, nil
}
