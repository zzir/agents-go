// Package ssh implements the sandbox.Sandbox interface by running commands on a
// remote host over SSH. Request files are transferred to a fresh temporary
// directory via SFTP, the command runs in a new SSH session, and the directory
// is removed afterwards.
//
// Unlike the docker backend, the SSH backend provides NO container-level
// isolation: the command runs with the privileges of the SSH user, and
// sandbox.Limits are NOT enforced (SSH has no cgroups mechanism). Point it at a
// disposable VM or an already-sandboxed host when running untrusted code.
//
// This package pulls golang.org/x/crypto and github.com/pkg/sftp; it is a
// separate module so the core agents-go module stays dependency-light.
package ssh

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/zzir/agents-go/sandbox"
)

// defaultConnectTimeout bounds the initial TCP+handshake when Options.ConnectTimeout is zero.
const defaultConnectTimeout = 15 * time.Second

// Options configures the SSH sandbox.
type Options struct {
	// Addr is the SSH server address as "host" or "host:port". Required.
	// When no port is given, 22 is assumed.
	Addr string
	// User is the SSH username. Required.
	User string
	// Auth selects the authentication method(s). At least one must be set.
	Auth AuthConfig
	// HostKey configures remote host-key verification. The zero value verifies
	// against ~/.ssh/known_hosts.
	HostKey HostKeyConfig
	// WorkDir, when set, uses this fixed remote directory as the working
	// directory for every Exec call instead of creating a temporary one.
	// The directory must already exist on the remote host. Files from
	// ExecRequest.Files are written into it and NOT cleaned up afterwards.
	WorkDir string
	// ConnectTimeout bounds the initial connection/handshake. Zero means
	// defaultConnectTimeout.
	ConnectTimeout time.Duration
	// KeepFiles, when true, leaves the per-execution working directory on the
	// remote host instead of removing it after Exec. Useful for debugging.
	KeepFiles bool
	// MaxReadFileBytes caps how many bytes ReadFile returns; larger files fail
	// with sandbox.ErrReadLimitExceeded instead of being loaded into local
	// memory. Zero (or negative) means sandbox.DefaultMaxReadFileBytes.
	MaxReadFileBytes int64
}

// AuthConfig selects how to authenticate to the SSH server. The methods are
// tried in order: SSH agent, private key, then password. At least one must be
// configured.
type AuthConfig struct {
	// UseAgent authenticates using the local SSH agent (SSH_AUTH_SOCK).
	UseAgent bool
	// KeyFile is the path to a PEM-encoded private key. "~" is expanded to the
	// user's home directory.
	KeyFile string
	// KeyBytes is a PEM-encoded private key (alternative to KeyFile).
	KeyBytes []byte
	// Passphrase decrypts an encrypted KeyFile/KeyBytes when set.
	Passphrase string
	// Password authenticates with a password.
	Password string
}

// HostKeyConfig configures remote host-key verification.
type HostKeyConfig struct {
	// Callback, when non-nil, is used directly and the other fields are ignored.
	Callback ssh.HostKeyCallback
	// KnownHostsFile is an OpenSSH known_hosts file. Defaults to
	// ~/.ssh/known_hosts. "~" is expanded to the user's home directory.
	KnownHostsFile string
	// InsecureIgnoreHostKey disables host-key verification entirely. NEVER use
	// in production — it makes the connection vulnerable to MITM attacks.
	InsecureIgnoreHostKey bool
}

// Sandbox is an SSH-backed sandbox.Sandbox.
type Sandbox struct {
	client    *ssh.Client
	sftp      *sftp.Client
	opts      Options
	agentConn net.Conn // SSH agent socket; nil when UseAgent is false
}

// New dials the SSH server and opens an SFTP subsystem, returning a ready
// Sandbox. Connection and authentication errors surface here.
func New(opts Options) (*Sandbox, error) {
	if opts.Addr == "" {
		return nil, errors.New("ssh sandbox: Addr is required")
	}
	if opts.User == "" {
		return nil, errors.New("ssh sandbox: User is required")
	}

	auth, agentConn, err := buildAuthMethods(opts.Auth)
	if err != nil {
		return nil, err
	}
	hostKey, err := buildHostKeyCallback(opts.HostKey)
	if err != nil {
		if agentConn != nil {
			_ = agentConn.Close()
		}
		return nil, err
	}
	connectTimeout := opts.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = defaultConnectTimeout
	}

	cfg := &ssh.ClientConfig{
		User:            opts.User,
		Auth:            auth,
		HostKeyCallback: hostKey,
		Timeout:         connectTimeout,
	}

	closeAgent := func() {
		if agentConn != nil {
			_ = agentConn.Close()
		}
	}
	client, err := ssh.Dial("tcp", normalizeAddr(opts.Addr), cfg)
	if err != nil {
		closeAgent()
		return nil, fmt.Errorf("ssh sandbox: dial: %w", err)
	}
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		_ = client.Close()
		closeAgent()
		return nil, fmt.Errorf("ssh sandbox: open sftp (is the SFTP subsystem enabled on the host?): %w", err)
	}
	return &Sandbox{client: client, sftp: sftpClient, opts: opts, agentConn: agentConn}, nil
}

// Exec implements sandbox.Sandbox.
func (s *Sandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	maxOut := req.EffectiveMaxOutputBytes()
	stdoutBuf := &sandbox.CappedBuffer{Max: maxOut}
	stderrBuf := &sandbox.CappedBuffer{Max: maxOut}
	res, err := s.exec(ctx, req, stdoutBuf, stderrBuf)
	if err != nil {
		return nil, err
	}
	res.Stdout = stdoutBuf.String()
	res.Stderr = stderrBuf.String()
	return res, nil
}

// ExecStream implements sandbox.ExecStreamer.
func (s *Sandbox) ExecStream(ctx context.Context, req sandbox.ExecRequest, stdout, stderr io.Writer) (*sandbox.ExecResult, error) {
	return s.exec(ctx, req, stdout, stderr)
}

// exec is the shared core for Exec and ExecStream. Output is written to the
// provided writers; the returned ExecResult has empty Stdout/Stderr fields.
func (s *Sandbox) exec(ctx context.Context, req sandbox.ExecRequest, stdout, stderr io.Writer) (*sandbox.ExecResult, error) {
	if len(req.Cmd) == 0 {
		return nil, errors.New("ssh sandbox: ExecRequest.Cmd is empty")
	}

	var workDir string
	var cleanup bool
	if s.opts.WorkDir != "" {
		workDir = s.opts.WorkDir
	} else {
		suffix, err := randomHex(8)
		if err != nil {
			return nil, fmt.Errorf("ssh sandbox: %w", err)
		}
		workDir = path.Join("/tmp", "agents-sandbox-"+suffix)
		if err := s.sftp.MkdirAll(workDir); err != nil {
			return nil, fmt.Errorf("ssh sandbox: create work dir %s: %w", workDir, err)
		}
		cleanup = !s.opts.KeepFiles
	}
	if cleanup {
		defer func() { _ = s.sftp.RemoveAll(workDir) }()
	}
	if err := s.writeFiles(workDir, req.Files); err != nil {
		return nil, err
	}

	session, err := s.client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("ssh sandbox: new session: %w", err)
	}
	defer session.Close()

	session.Stdout = stdout
	session.Stderr = stderr
	if req.Stdin != "" {
		session.Stdin = strings.NewReader(req.Stdin)
	}

	wctx, cancel := context.WithTimeout(ctx, req.EffectiveTimeout())
	defer cancel()

	if err := session.Start(buildCommand(workDir, req.Env, req.Cmd)); err != nil {
		return nil, fmt.Errorf("ssh sandbox: start: %w", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- session.Wait() }()

	res := &sandbox.ExecResult{}
	select {
	case werr := <-waitCh:
		var exitErr *ssh.ExitError
		var missingErr *ssh.ExitMissingError
		switch {
		case werr == nil:
			res.ExitCode = 0
		case errors.As(werr, &exitErr):
			res.ExitCode = exitErr.ExitStatus()
		case errors.As(werr, &missingErr):
			// The session ended without delivering an exit status (e.g. the
			// remote process was killed by a signal). Report it as a failure
			// rather than a transport error so the model sees the output.
			res.ExitCode = -1
		default:
			return nil, fmt.Errorf("ssh sandbox: wait: %w", werr)
		}
		return res, nil
	case <-wctx.Done():
		// Best-effort termination: closing the session tears down the SSH
		// channel, but whether the remote process actually dies depends on the
		// sshd implementation and configuration — many servers do not signal
		// the process when the channel closes, so the command may keep running
		// on the remote host after a timeout. Then drain Wait so the output
		// copy has completed.
		_ = session.Close()
		<-waitCh
		if cerr := ctx.Err(); cerr != nil {
			// The caller's context was canceled or hit its own deadline; that is
			// not an execution timeout.
			return nil, cerr
		}
		// Our injected timeout fired.
		res.TimedOut = true
		res.ExitCode = -1
		return res, nil
	}
}

// ReadFile implements sandbox.Sandbox. Files larger than
// Options.MaxReadFileBytes (default sandbox.DefaultMaxReadFileBytes) fail
// with sandbox.ErrReadLimitExceeded instead of being loaded into local memory.
func (s *Sandbox) ReadFile(_ context.Context, p string) ([]byte, error) {
	if s.opts.WorkDir == "" {
		return nil, sandbox.ErrNoWorkDir
	}
	f, err := s.sftp.Open(path.Join(s.opts.WorkDir, path.Clean("/"+p)))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return sandbox.ReadAllLimited(f, s.opts.MaxReadFileBytes)
}

// WriteFile implements sandbox.Sandbox.
func (s *Sandbox) WriteFile(_ context.Context, p string, content []byte) error {
	if s.opts.WorkDir == "" {
		return sandbox.ErrNoWorkDir
	}
	clean := path.Clean("/" + p)[1:]
	if clean == "" {
		return fmt.Errorf("ssh sandbox: invalid file path %q", p)
	}
	full := path.Join(s.opts.WorkDir, clean)
	if parent := path.Dir(full); parent != "." {
		if err := s.sftp.MkdirAll(parent); err != nil {
			return fmt.Errorf("ssh sandbox: mkdir %s: %w", parent, err)
		}
	}
	f, err := s.sftp.Create(full)
	if err != nil {
		return err
	}
	_, werr := f.Write(content)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

// ListDir implements sandbox.Sandbox.
func (s *Sandbox) ListDir(_ context.Context, p string) ([]sandbox.DirEntry, error) {
	if s.opts.WorkDir == "" {
		return nil, sandbox.ErrNoWorkDir
	}
	target := s.opts.WorkDir
	if p != "" && p != "." {
		target = path.Join(target, path.Clean("/"+p))
	}
	entries, err := s.sftp.ReadDir(target)
	if err != nil {
		return nil, err
	}
	result := make([]sandbox.DirEntry, len(entries))
	for i, e := range entries {
		result[i] = sandbox.DirEntry{Name: e.Name(), IsDir: e.IsDir(), Size: e.Size()}
	}
	return result, nil
}

// writeFiles writes each request file under dir via SFTP, creating parent
// directories as needed. Paths are cleaned to prevent escaping dir.
func (s *Sandbox) writeFiles(dir string, files map[string]string) error {
	for name, content := range files {
		clean := path.Clean("/" + name)[1:] // strip leading slash, prevent traversal
		if clean == "" {
			return fmt.Errorf("ssh sandbox: invalid file path %q", name)
		}
		full := path.Join(dir, clean)
		if parent := path.Dir(full); parent != "." {
			if err := s.sftp.MkdirAll(parent); err != nil {
				return fmt.Errorf("ssh sandbox: mkdir %s: %w", parent, err)
			}
		}
		f, err := s.sftp.Create(full)
		if err != nil {
			return fmt.Errorf("ssh sandbox: create %s: %w", full, err)
		}
		_, werr := io.WriteString(f, content)
		cerr := f.Close()
		if werr != nil {
			return fmt.Errorf("ssh sandbox: write %s: %w", full, werr)
		}
		if cerr != nil {
			return fmt.Errorf("ssh sandbox: close %s: %w", full, cerr)
		}
	}
	return nil
}

// Close releases the SFTP subsystem, the underlying SSH connection, and the
// SSH agent socket (if one was opened).
func (s *Sandbox) Close() error {
	var agentErr error
	if s.agentConn != nil {
		agentErr = s.agentConn.Close()
	}
	return errors.Join(s.sftp.Close(), s.client.Close(), agentErr)
}

// buildCommand assembles a single shell command line that changes into the
// working directory and execs the command with the given environment. Every
// element is single-quoted so directory names, env values and arguments cannot
// be interpreted by the shell. `exec` replaces the shell so a session-close
// signal reaches the command itself.
func buildCommand(dir string, env map[string]string, cmd []string) string {
	var b strings.Builder
	b.WriteString("cd ")
	b.WriteString(shellQuote(dir))
	b.WriteString(" && exec ")
	if len(env) > 0 {
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("env")
		for _, k := range keys {
			b.WriteByte(' ')
			b.WriteString(shellQuote(k + "=" + env[k]))
		}
		b.WriteByte(' ')
	}
	for i, c := range cmd {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(shellQuote(c))
	}
	return b.String()
}

// shellQuote returns s quoted for a POSIX shell: wrapped in single quotes with
// any embedded single quote rendered as the '\” sequence.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// normalizeAddr appends the default SSH port when addr has none.
func normalizeAddr(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(addr, "22")
}

// randomHex returns n random bytes hex-encoded.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// buildAuthMethods turns an AuthConfig into ordered ssh.AuthMethods: agent
// first, then private key, then password. When UseAgent is true, the returned
// net.Conn is the SSH agent socket and must be closed by the caller.
func buildAuthMethods(cfg AuthConfig) ([]ssh.AuthMethod, net.Conn, error) {
	var methods []ssh.AuthMethod
	var agentConn net.Conn

	if cfg.UseAgent {
		sock := os.Getenv("SSH_AUTH_SOCK")
		if sock == "" {
			return nil, nil, errors.New("ssh sandbox: Auth.UseAgent set but SSH_AUTH_SOCK is empty")
		}
		conn, err := net.Dial("unix", sock)
		if err != nil {
			return nil, nil, fmt.Errorf("ssh sandbox: connect ssh agent: %w", err)
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
			keyBytes, err = os.ReadFile(path)
			if err != nil {
				return nil, nil, fmt.Errorf("ssh sandbox: read key file: %w", err)
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
			return nil, nil, fmt.Errorf("ssh sandbox: parse private key: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if cfg.Password != "" {
		methods = append(methods, ssh.Password(cfg.Password))
	}

	if len(methods) == 0 {
		return nil, nil, errors.New("ssh sandbox: no authentication method configured (set Auth.UseAgent, KeyFile, KeyBytes or Password)")
	}
	return methods, agentConn, nil
}

// buildHostKeyCallback resolves the host-key verification strategy.
func buildHostKeyCallback(cfg HostKeyConfig) (ssh.HostKeyCallback, error) {
	switch {
	case cfg.Callback != nil:
		return cfg.Callback, nil
	case cfg.InsecureIgnoreHostKey:
		// Documented opt-in for dev/test; vulnerable to MITM. See HostKeyConfig.
		return ssh.InsecureIgnoreHostKey(), nil
	default:
		file := cfg.KnownHostsFile
		if file == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("ssh sandbox: locate known_hosts: %w", err)
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
			return nil, fmt.Errorf("ssh sandbox: load known_hosts %s: %w", file, err)
		}
		return cb, nil
	}
}

// expandHome replaces a leading "~" with the user's home directory.
func expandHome(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("ssh sandbox: expand %q: %w", p, err)
		}
		return filepath.Join(home, strings.TrimPrefix(p[1:], "/")), nil
	}
	return p, nil
}

var _ sandbox.Sandbox = (*Sandbox)(nil)
var _ sandbox.ExecStreamer = (*Sandbox)(nil)
