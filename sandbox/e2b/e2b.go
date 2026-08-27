// Package e2b runs a sandbox on any host that speaks the E2B API — E2B's own
// cloud, a self-hosted E2B, or a compatible service such as Alibaba Cloud's
// Function Compute cloud sandbox. One backend, one endpoint configuration:
// the differences between those are the API URL, the sandbox domain and the
// key (decisions §5.34).
//
// The client is written here rather than taken from an SDK: the control plane
// is six REST calls and the data plane is Connect-over-JSON, so the whole
// thing needs nothing outside the standard library — which keeps it in the
// root module instead of behind a submodule.
package e2b

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zzir/agents-go/sandbox"
)

// Defaults for the fields Options leaves empty.
const (
	DefaultAPIURL = "https://api.e2b.app"
	DefaultDomain = "e2b.app"
	// EnvdPort is the port envd — the daemon inside every sandbox — listens
	// on. It is part of the host name, not a URL path.
	EnvdPort = 49983
	// DefaultUser is the account E2B sandboxes run commands as.
	DefaultUser = "user"
	// DefaultWorkDir matches the workbench's contract everywhere else.
	DefaultWorkDir = "/workspace"
	// DefaultTimeout is the sandbox TTL requested at creation, in seconds.
	// It is a LEASE, refreshed while the sandbox is in use: too short and an
	// idle chat loses its container, too long and a forgotten one bills.
	DefaultTimeout = 300
)

// exportTimeout bounds a working-tree export: an archive of a real checkout
// is not a 30-second command.
const exportTimeout = 10 * time.Minute

// DataPlaneAuth selects how envd is authenticated. It is configuration rather
// than a fixed choice because compatible services differ: E2B mints a
// per-sandbox access token, and Alibaba Cloud's compatible API does not
// support one at all — it takes the API key.
type DataPlaneAuth string

const (
	// AuthAuto sends the sandbox's access token when creation returned one,
	// and the API key otherwise. It is the default and covers both known
	// services.
	AuthAuto DataPlaneAuth = ""
	// AuthAccessToken always sends the per-sandbox token; a service that
	// never mints one then fails loudly instead of silently unauthenticated.
	AuthAccessToken DataPlaneAuth = "access_token"
	// AuthAPIKey always sends the API key.
	AuthAPIKey DataPlaneAuth = "api_key"
	// AuthNone sends no credential — a self-hosted envd behind its own gate.
	AuthNone DataPlaneAuth = "none"
)

// Options configures a Sandbox.
type Options struct {
	// APIURL is the control plane base URL; empty means DefaultAPIURL.
	APIURL string
	// Domain is the suffix a sandbox's hosts are built from:
	// "<port>-<sandbox id>.<domain>". Empty means DefaultDomain.
	Domain string
	// APIKey authenticates the control plane (X-API-Key). Required.
	APIKey string
	// TemplateID is what a created sandbox is built from. Required.
	TemplateID string
	// SandboxID is a sandbox this caller already owns — resumed rather than
	// created. Empty creates one on first use.
	SandboxID string
	// OnSandboxID, when set, is called with the id of a sandbox this client
	// CREATED, so the caller can remember it across restarts. A failure is
	// fatal to the create: a sandbox nobody recorded is one nobody will ever
	// stop, and silently leaking billed compute is worse than failing.
	OnSandboxID func(ctx context.Context, id string) error
	// TimeoutSeconds is the TTL a create or resume asks for; zero means
	// DefaultTimeout.
	TimeoutSeconds int
	// AutoPause makes the TTL PAUSE the sandbox instead of killing it, so the
	// filesystem survives an idle period.
	AutoPause bool
	// AllowInternet gives the sandbox outbound network access.
	AllowInternet bool
	// Metadata tags the sandbox on the service side; the workbench uses it to
	// say which project a sandbox belongs to.
	Metadata map[string]string
	// Env is the environment every command sees.
	Env map[string]string
	// WorkDir is the working directory commands run in; empty means
	// DefaultWorkDir.
	WorkDir string
	// User is the account commands run as; empty means DefaultUser.
	User string
	// DataPlaneAuth selects the envd credential; empty means AuthAuto.
	DataPlaneAuth DataPlaneAuth
	// MaxReadFileBytes caps ReadFile; zero means the SDK default.
	MaxReadFileBytes int64
	// HTTPClient overrides the client used for both planes.
	HTTPClient *http.Client
}

// Sandbox is one E2B-compatible sandbox. The remote instance is created (or
// resumed) LAZILY, on the first call that needs it — the same shape the docker
// backend has, and what lets the constructor stay I/O-free.
type Sandbox struct {
	opts Options

	mu sync.Mutex
	// id is the sandbox this client is bound to; empty until the first
	// ensure. accessToken is envd's credential when the service minted one.
	id          string
	accessToken string
	// domain the service told us to use for this sandbox, when it did.
	domain string
}

var (
	_ sandbox.Sandbox        = (*Sandbox)(nil)
	_ sandbox.Lifecycle      = (*Sandbox)(nil)
	_ sandbox.PortForwarder  = (*Sandbox)(nil)
	_ sandbox.Exporter       = (*Sandbox)(nil)
	_ sandbox.TerminalOpener = (*Sandbox)(nil)
)

// New returns a Sandbox for opts. It performs no I/O: the remote sandbox is
// created or resumed on first use.
func New(opts Options) (*Sandbox, error) {
	if opts.APIKey == "" {
		return nil, errors.New("e2b: an API key is required")
	}
	if opts.TemplateID == "" {
		return nil, errors.New("e2b: a template id is required")
	}
	return &Sandbox{opts: opts, id: opts.SandboxID}, nil
}

func (s *Sandbox) httpClient() *http.Client {
	if s.opts.HTTPClient != nil {
		return s.opts.HTTPClient
	}
	return http.DefaultClient
}

func (s *Sandbox) apiURL() string {
	if s.opts.APIURL != "" {
		return strings.TrimSuffix(s.opts.APIURL, "/")
	}
	return DefaultAPIURL
}

func (s *Sandbox) sandboxDomain() string {
	s.mu.Lock()
	d := s.domain
	s.mu.Unlock()
	if d != "" {
		return d
	}
	if s.opts.Domain != "" {
		return s.opts.Domain
	}
	return DefaultDomain
}

func (s *Sandbox) user() string {
	if s.opts.User != "" {
		return s.opts.User
	}
	return DefaultUser
}

func (s *Sandbox) workDir() string {
	if s.opts.WorkDir != "" {
		return s.opts.WorkDir
	}
	return DefaultWorkDir
}

func (s *Sandbox) timeout() int {
	if s.opts.TimeoutSeconds > 0 {
		return s.opts.TimeoutSeconds
	}
	return DefaultTimeout
}

// hostFor builds the host a port inside the sandbox is reachable at.
func (s *Sandbox) hostFor(id string, port int) string {
	return strconv.Itoa(port) + "-" + id + "." + s.sandboxDomain()
}

// envdBase is the base URL of the sandbox's daemon.
func (s *Sandbox) envdBase(ctx context.Context) (string, error) {
	id, err := s.ensure(ctx)
	if err != nil {
		return "", err
	}
	return "https://" + s.hostFor(id, EnvdPort), nil
}

// Close releases nothing remote: the sandbox outlives this client, and which
// of stop and destroy is wanted is the caller's call (Stop / the backend's
// Reclaim). Closing must not silently end someone's work.
func (s *Sandbox) Close() error { return nil }

// resolvePath makes a sandbox-absolute path out of a caller's, which may be
// relative to the working directory — the same shell semantics every other
// backend follows (decisions §5.14).
func (s *Sandbox) resolvePath(p string) string {
	if strings.HasPrefix(p, "/") {
		return p
	}
	if p == "" {
		return s.workDir()
	}
	return s.workDir() + "/" + p
}

// deadline turns a request timeout into a context, so a hung sandbox ends the
// call rather than the process.
func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}

// errUnsupported names a capability this service does not offer, wrapped so
// callers can branch on it.
func errUnsupported(what string) error {
	return fmt.Errorf("e2b: %s: %w", what, sandbox.ErrLifecycleUnsupported)
}
