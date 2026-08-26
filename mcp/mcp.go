// Package mcp provides a Model Context Protocol (MCP) client that exposes a
// server's tools to an agent. It implements agents.MCPServer over the official
// modelcontextprotocol/go-sdk, supporting stdio and streamable HTTP transports.
//
// This is a separate Go module, so that the go-sdk and its transitive closure
// (uritemplate, x/oauth2, x/time, x/tools, the segmentio pair) stay out of
// every build that does not speak MCP — see docs/explanation/decisions.md §5.7. Using it costs
// one extra require:
//
//	go get github.com/zzir/agents-go/mcp
package mcp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/tracing"
)

// Options configures a Server.
type Options struct {
	// AllowedTools, when non-empty, restricts exposed tools to these names.
	AllowedTools []string
	// BlockedTools hides these tool names.
	BlockedTools []string
	// Strict applies OpenAI strict-mode normalization to each tool's input
	// schema. Some servers emit schemas incompatible with strict mode; disable
	// it if tool calls are rejected.
	Strict bool
	// ClientName/ClientVersion identify this client to the server.
	ClientName    string
	ClientVersion string

	// CacheToolsList caches the server's tool list after the first fetch so a
	// multi-turn run does not re-issue list_tools every turn. The cache is
	// invalidated automatically when the server sends a tools/list_changed
	// notification; call InvalidateToolsCache to force a refetch for servers
	// that change tools without announcing it. Static and dynamic filters still
	// run on every ListTools, against the cached list.
	CacheToolsList bool

	// ToolFilter, when set, decides per call whether a tool is exposed, applied
	// after the static AllowedTools/BlockedTools lists. It receives the original
	// (unprefixed) tool name and may consult the run context.
	ToolFilter func(ctx context.Context, rc *agents.RunContext, agent *agents.Agent, toolName string) bool

	// ToolNamePrefix is prepended to every exposed tool name (e.g. "github_") to
	// avoid collisions when multiple servers expose same-named tools. The server
	// is still called with the original name.
	ToolNamePrefix string

	// RequireApproval, when set, decides per call whether an exposed MCP tool
	// needs human approval (HITL), receiving the run context, the current agent
	// (captured per ListTools call) and the tool's
	// original (unprefixed) name. For the common static case use ApproveTools:
	//
	//	mcp.Options{RequireApproval: mcp.ApproveTools("write_file")}
	RequireApproval func(ctx context.Context, rc *agents.RunContext, agent *agents.Agent, toolName string) bool

	// MaxRetryAttempts is the number of times to retry a failed list_tools or
	// call_tool request. 0 (default) means no retries; -1 retries indefinitely.
	MaxRetryAttempts int

	// RetryBackoffBase is the base delay for exponential backoff between retries
	// (delay = RetryBackoffBase * 2^(attempt-1)). Defaults to one second when
	// retries are enabled and this is left zero.
	RetryBackoffBase time.Duration

	// UseStructuredContent controls how a tool result's structuredContent field
	// is handled. It is false by default: most servers
	// duplicate their structured data in the content blocks, so structuredContent
	// is ignored and the content blocks are sent to the model. Set it true to use
	// structuredContent exclusively (the content blocks are then ignored) for
	// servers that only populate the structured field.
	UseStructuredContent bool

	// OAuthHandler, when set, is passed to the streamable HTTP transport to
	// handle OAuth 2.1 authorization flows (authorization code + PKCE, token
	// refresh, dynamic client registration). Ignored for stdio transports.
	OAuthHandler auth.OAuthHandler

	// Redial builds a fresh transport for a connection that has died, and is
	// what makes a Server self-healing: without it a dead connection stays
	// dead and every later call fails, which is a permanent outage for every
	// agent configured with that server.
	//
	// It must return a transport that can be connected NOW — a new
	// CommandTransport with an unstarted command, a streamable transport with
	// the same endpoint, headers and OAuth handler. The context it receives is
	// the connection's own, so anything bound to it (a subprocess) lives
	// exactly as long as the connection does.
	//
	// Nil keeps the old behavior: a dead connection is reported, not repaired.
	Redial func(ctx context.Context) (mcpsdk.Transport, error)
}

// Server is a connected MCP server whose tools are exposed to an agent. It
// implements agents.MCPServer.
type Server struct {
	name string
	// session is swapped, not fixed: a connection that dies is replaced in
	// place (see redial), so every holder of this *Server recovers rather than
	// only the runs that start afterwards.
	session atomic.Pointer[mcpsdk.ClientSession]
	dialMu  sync.Mutex
	// lastDial throttles healing; see redialCooldown.
	lastDial time.Time
	opts     Options
	allowed  map[string]bool
	blocked  map[string]bool

	// closed flips when Close runs so later ListTools/CallTool report a clear
	// error instead of failing obscurely on the dead session — a long-lived
	// run can hold a *Server pointer past a reconfiguration that closed it.
	closed atomic.Bool

	// rpcCtx is the context every request on this session rides. It belongs to
	// the CONNECTION and ends only with Close — see callSession for why it can
	// never be a caller's.
	rpcCtx  context.Context
	rpcStop context.CancelFunc

	mu       sync.Mutex
	cached   []cachedTool // populated lazily when CacheToolsList is set
	cacheGen uint64       // bumped by InvalidateToolsCache; guards a mid-fetch invalidation from being overwritten
}

// cachedTool pairs an adapted tool with its original (unprefixed) MCP name, used
// for static/dynamic filtering on each ListTools call.
type cachedTool struct {
	originalName string
	tool         *agents.Tool
}

func newServer(name string, opts Options) *Server {
	s := &Server{name: name, opts: opts, allowed: map[string]bool{}, blocked: map[string]bool{}}
	// Rooted at Background, not at whoever connects: this context outlives
	// every caller and must carry none of their deadlines.
	s.rpcCtx, s.rpcStop = context.WithCancel(context.Background())
	for _, t := range opts.AllowedTools {
		s.allowed[t] = true
	}
	for _, t := range opts.BlockedTools {
		s.blocked[t] = true
	}
	return s
}

// callSession runs one request against the shared session and returns as soon
// as EITHER the request answers or the caller's context ends.
//
// The request itself rides the connection's context, never the caller's. A
// session is shared by every agent configured with this server — several runs,
// their background tasks, other conversations — and the streamable HTTP
// transport issues each request on the context it is handed. Cancelling one
// mid-flight fails the whole CONNECTION, permanently and for everyone: the
// go-sdk reads the response body, sees context.Canceled, and calls fail(),
// which is a sync.Once closing the connection's failure gate. Every later call
// by anyone then answers "client is closing" until something reconnects. One
// person stopping one run must not take out every other run's tools.
//
// The caller still gets its cancellation honored — it returns here at once —
// while the request finishes in the background and its answer is dropped. That
// costs one in-flight request; the alternative costs the connection.
func callSession[T any](ctx context.Context, s *Server, fn func(context.Context) (T, error)) (T, error) {
	type answer struct {
		val T
		err error
	}
	// Buffered, so the request goroutine never blocks on a caller that left.
	ch := make(chan answer, 1)
	go func() {
		val, err := fn(s.rpcCtx)
		ch <- answer{val, err}
	}()
	select {
	case a := <-ch:
		return a.val, a.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// redialCooldown throttles healing. A server that accepts a connection and
// drops it again would otherwise turn the watcher below into a dial loop: one
// death heals at once, a second inside this window waits for a caller to ask
// again.
const redialCooldown = 3 * time.Second

// redialConnectTimeout bounds one healing attempt's handshake. It exists
// because redial runs under dialMu: an endpoint that black-holes instead of
// refusing would otherwise hold the lock for as long as TCP keeps trying, and
// every caller waiting to report a connection error with it.
const redialConnectTimeout = 30 * time.Second

func (s *Server) newClient() *mcpsdk.Client {
	name := s.opts.ClientName
	name = cmp.Or(name, "agents-go")
	version := s.opts.ClientVersion
	version = cmp.Or(version, "0.1.0")
	return mcpsdk.NewClient(&mcpsdk.Implementation{Name: name, Version: version}, &mcpsdk.ClientOptions{
		// Drop the cached tool list when the server announces a change
		// (notifications/tools/list_changed), so CacheToolsList can never serve
		// a permanently stale list. No-op when caching is off.
		ToolListChangedHandler: func(context.Context, *mcpsdk.ToolListChangedRequest) {
			s.InvalidateToolsCache()
		},
	})
}

func (s *Server) connect(ctx context.Context, transport mcpsdk.Transport) error {
	session, err := s.newClient().Connect(ctx, transport, nil)
	if err != nil {
		return agents.Classify(agents.CodeMCP, fmt.Errorf("mcp: connecting to %q: %w", s.name, err))
	}
	s.session.Store(session)
	s.watch(session)
	return nil
}

// watch heals the connection the moment it dies, instead of leaving the next
// caller to trip over it.
//
// Without it, every death costs somebody a failed turn — and the callers who
// pay are whoever happens to be mid-run, which in practice is every background
// task at once. The watcher is one goroutine per live connection, ending with
// the connection it watches.
func (s *Server) watch(session *mcpsdk.ClientSession) {
	if s.opts.Redial == nil {
		return
	}
	go func() {
		_ = session.Wait() // returns when the connection is done, however it ended
		if s.closed.Load() {
			return
		}
		s.redial(session)
	}()
}

// redial replaces a dead session with a fresh one, and reports whether this
// call is the one that did it.
//
// A connection dies for reasons that have nothing to do with the caller who
// finds out: the server restarted, a proxy dropped an idle socket, a request
// somebody else made failed mid-body. Nothing in the go-sdk reconnects, so
// without this the FIRST such failure is permanent — every agent configured
// with the server answers "client is closing" until a person notices and
// reconnects it by hand. That is what this repairs.
//
// failed is the session the caller was using. Serialized and compared against
// what is current, so a dozen callers discovering the same dead connection
// dial once between them and the rest simply retry.
func (s *Server) redial(failed *mcpsdk.ClientSession) bool {
	if s.opts.Redial == nil || s.closed.Load() {
		return false
	}
	s.dialMu.Lock()
	defer s.dialMu.Unlock()
	if s.session.Load() != failed {
		return true // somebody else already healed it; the caller should retry
	}
	if !s.lastDial.IsZero() && time.Since(s.lastDial) < redialCooldown {
		return false
	}
	s.lastDial = time.Now()
	// Redial gets the connection's own context, which is what its contract
	// promises: a transport that binds a subprocess to it must outlive this
	// call, or the shell reconnects and is killed in the same breath. The
	// HANDSHAKE is bounded separately because it runs under dialMu — an
	// unreachable endpoint holding TCP retries for minutes would hold the lock
	// too, and with it every failed caller waiting in healed() to report an
	// error.
	transport, err := s.opts.Redial(s.rpcCtx)
	if err != nil {
		return false
	}
	hctx, cancel := context.WithTimeout(s.rpcCtx, redialConnectTimeout)
	defer cancel()
	session, err := s.newClient().Connect(hctx, transport, nil)
	if err != nil {
		return false
	}
	if failed != nil {
		_ = failed.Close()
	}
	s.session.Store(session)
	// Re-checked AFTER the store, because Close does the mirror image — closed
	// first, then close whatever the session slot holds — and the two only
	// compose into "no session survives a Close" in this order: a Close that
	// missed the swap is caught here, and a Close that saw it closes the new
	// session itself. Without this, a Close landing between the connect above
	// and the store would close the OLD session and leave the new one live
	// forever — a disabled server still holding its connection.
	if s.closed.Load() {
		_ = session.Close()
		return false
	}
	s.watch(session)
	// A new session is a new server as far as tools go — it may have restarted
	// with a different set.
	s.InvalidateToolsCache()
	return true
}

// healed reports whether err says the connection is gone and a fresh one was
// put in its place. A server closed on purpose is not healed: it was meant to
// end.
func (s *Server) healed(err error, failed *mcpsdk.ClientSession) bool {
	if err == nil || s.closed.Load() || !errors.Is(err, mcpsdk.ErrConnectionClosed) {
		return false
	}
	return s.redial(failed)
}

// NewWithTransport connects to an MCP server over an arbitrary transport. Use
// it for custom transports or in-process testing (mcpsdk.NewInMemoryTransports).
func NewWithTransport(ctx context.Context, name string, transport mcpsdk.Transport, opts Options) (*Server, error) {
	s := newServer(name, opts)
	if err := s.connect(ctx, transport); err != nil {
		return nil, err
	}
	return s, nil
}

// NewStdioServer launches an MCP server subprocess and connects over stdio.
// cmd is the command to run (e.g. exec.Command("npx", "-y", "server")).
func NewStdioServer(ctx context.Context, name string, cmd *exec.Cmd, opts Options) (*Server, error) {
	s := newServer(name, opts)
	if err := s.connect(ctx, &mcpsdk.CommandTransport{Command: cmd}); err != nil {
		return nil, err
	}
	return s, nil
}

// NewStreamableHTTPServer connects to an MCP server over the streamable HTTP
// transport at endpoint.
func NewStreamableHTTPServer(ctx context.Context, name, endpoint string, opts Options) (*Server, error) {
	s := newServer(name, opts)
	transport := &mcpsdk.StreamableClientTransport{Endpoint: endpoint}
	if opts.OAuthHandler != nil {
		transport.OAuthHandler = opts.OAuthHandler
	}
	if err := s.connect(ctx, transport); err != nil {
		return nil, err
	}
	return s, nil
}

// Name implements agents.MCPServer.
func (s *Server) Name() string { return s.name }

// Close implements agents.MCPServer, closing the session. Subsequent
// ListTools/CallTool calls fail with a "closed" error.
func (s *Server) Close() error {
	// Nil-checked because the zero Server is reachable: this type is exported,
	// and a caller (a test, a discarded handshake) can hold one that never went
	// through a constructor.
	// Marked closed FIRST: everything below ends the connection, and the
	// watcher must read this before it wakes or it would heal a server that
	// was deliberately shut down.
	s.closed.Store(true)
	if s.rpcStop != nil {
		// Before the session close, not after: closing waits for the requests
		// still in flight, and one of those may be riding a server that stopped
		// answering — the case where a caller already gave up and left it here.
		// Ending the connection's context is what lets them unwind.
		s.rpcStop()
	}
	session := s.session.Load()
	if session == nil {
		return nil
	}
	return session.Close()
}

func (s *Server) allow(toolName string) bool {
	if s.blocked[toolName] {
		return false
	}
	if len(s.allowed) > 0 {
		return s.allowed[toolName]
	}
	return true
}

// ListTools implements agents.MCPServer, fetching (or reusing a cached) tool
// list and adapting each into an agents.Tool that proxies to CallTool.
// Static (AllowedTools/BlockedTools) and dynamic (ToolFilter) filters run here on
// every call, so caching never hides a context-dependent filter decision.
func (s *Server) ListTools(ctx context.Context, rc *agents.RunContext, agent *agents.Agent) ([]*agents.Tool, error) {
	span, ctx := tracing.StartSpanFrom(ctx, "mcp.list_tools", tracing.SpanTypeMCP,
		map[string]any{"server": s.name})
	defer span.Finish()

	all, err := s.toolList(ctx)
	if err != nil {
		span.SetError(err.Error(), nil)
		return nil, err
	}
	span.Set("tools", len(all))
	var tools []*agents.Tool
	for _, ct := range all {
		if !s.allow(ct.originalName) {
			continue
		}
		if s.opts.ToolFilter != nil && !s.opts.ToolFilter(ctx, rc, agent, ct.originalName) {
			continue
		}
		tools = append(tools, s.bindApproval(ct, agent))
	}
	return tools, nil
}

// bindApproval returns the tool to expose for this ListTools call, wiring
// RequireApproval (if set) with the current agent captured per call, so the
// closure names the agent whose turn it is rather than whichever agent first
// listed the server. The cached base tool is left untouched.
func (s *Server) bindApproval(ct cachedTool, agent *agents.Agent) *agents.Tool {
	if s.opts.RequireApproval == nil {
		return ct.tool
	}
	clone := *ct.tool
	name := ct.originalName
	fn := s.opts.RequireApproval
	clone.NeedsApprovalFunc = func(ctx context.Context, rc *agents.RunContext, _ string, _ string) (bool, error) {
		return fn(ctx, rc, agent, name), nil
	}
	return &clone
}

// ApproveTools returns a RequireApproval predicate that marks exactly the
// named tools (by their original, unprefixed names) as requiring human
// approval — the common static case.
func ApproveTools(names ...string) func(context.Context, *agents.RunContext, *agents.Agent, string) bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return func(_ context.Context, _ *agents.RunContext, _ *agents.Agent, toolName string) bool {
		return set[toolName]
	}
}

// maxToolListPages bounds a tools/list pagination walk; a server with more
// pages than this is treated as misbehaving.
const maxToolListPages = 1000

// toolList returns the adapted tools, fetching them from the server (and caching
// when CacheToolsList is set) or reusing the cache.
func (s *Server) toolList(ctx context.Context) ([]cachedTool, error) {
	session := s.session.Load()
	if session == nil {
		return nil, agents.Classify(agents.CodeMCP, fmt.Errorf("mcp: server %q is not connected", s.name))
	}
	if s.closed.Load() {
		return nil, agents.Classify(agents.CodeMCP, fmt.Errorf("mcp: server %q: %w", s.name, errServerClosed))
	}
	// Fast path: a cached list is served under a short critical section, never
	// while a network call is in flight.
	var gen uint64
	if s.opts.CacheToolsList {
		s.mu.Lock()
		cached := s.cached
		gen = s.cacheGen
		s.mu.Unlock()
		if cached != nil {
			return cached, nil
		}
	}
	// Fetch outside s.mu so a slow ListTools RPC (plus retry backoff) never
	// blocks InvalidateToolsCache (list_changed handling) or a concurrent
	// same-session caller. Two callers racing a cold cache may each issue a
	// ListTools request; that duplicate work is acceptable and preferable to
	// holding the lock across a blocking network call.
	// Every page, not just the first: tools/list paginates via nextCursor, and
	// a server past page one would otherwise have those tools silently missing
	// — no error, no log, just "no such tool" when the model calls one — and,
	// with CacheToolsList, the truncated list cached as if it were complete.
	var tools []*mcpsdk.Tool
	fetch := func() error {
		tools = tools[:0]
		var params *mcpsdk.ListToolsParams
		// A faulty (or hostile) server that repeats a cursor, or never runs
		// out of pages, must produce a protocol error — not an unbounded loop
		// appending the same tools until memory runs out.
		seen := make(map[string]bool)
		for page := 0; ; page++ {
			if page >= maxToolListPages {
				return fmt.Errorf("tools/list exceeded %d pages without finishing", maxToolListPages)
			}
			res, e := callSession(ctx, s, func(rpc context.Context) (*mcpsdk.ListToolsResult, error) {
				return s.session.Load().ListTools(rpc, params)
			})
			if e != nil {
				return e
			}
			tools = append(tools, res.Tools...)
			if res.NextCursor == "" {
				return nil
			}
			if seen[res.NextCursor] {
				return fmt.Errorf("tools/list repeated cursor %q", res.NextCursor)
			}
			seen[res.NextCursor] = true
			params = &mcpsdk.ListToolsParams{Cursor: res.NextCursor}
		}
	}
	err := s.runWithRetries(ctx, fetch)
	if err != nil && s.healed(err, session) {
		// The connection this run found was somebody else's casualty, and the
		// list is idempotent — so ask again on the fresh one rather than
		// failing a turn over a connection that has already been replaced.
		// Only listing gets this: repeating a tool CALL could repeat whatever
		// it did (see the call site).
		err = s.runWithRetries(ctx, fetch)
	}
	if err != nil {
		return nil, agents.Classify(agents.CodeMCP, fmt.Errorf("mcp: listing tools for %q: %w", s.name, err))
	}
	names := s.exposedNames(tools)
	list := make([]cachedTool, 0, len(tools))
	for i, mt := range tools {
		list = append(list, cachedTool{originalName: mt.Name, tool: s.toolFor(mt, names[i])})
	}
	if s.opts.CacheToolsList {
		s.mu.Lock()
		// If another goroutine published a list while we were fetching, prefer it
		// so every caller converges on the same slice. Otherwise cache ours only
		// if no InvalidateToolsCache ran during the fetch (the generation is
		// unchanged): a list_changed arriving mid-fetch means our result may
		// already be stale, so return it to this caller but leave the cache empty
		// for the next call to refetch.
		if s.cached != nil {
			list = s.cached
		} else if s.cacheGen == gen {
			s.cached = list
		}
		s.mu.Unlock()
	}
	return list, nil
}

// maxRetryBackoff caps the delay between retries — the same cap and the same
// equal jitter as the model-side RetryPolicy, so the two feel like one system.
const maxRetryBackoff = 30 * time.Second

// JSON-RPC codes that mean the request was understood and refused, so sending
// the same bytes again produces the same refusal. Written as numbers because
// the go-sdk exports these as error values from an internal package, not as
// constants: -32005 is its ErrRejected ("invalid in the current context", and
// explicitly not a broken connection) and -32003 its ErrClientClosing.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeClientClosing  = -32003
	codeRejected       = -32005
)

// errServerClosed marks a call made after Close.
var errServerClosed = errors.New("server is closed")

// retryable reports whether err is worth another attempt.
//
// A transport failure is: each attempt reloads the session, so one the watcher
// healed in the background carries the next try. An answer the server sent is
// not — it already understood the request — and neither is a call made after
// Close, which no amount of waiting turns into a live connection.
func retryable(err error) bool {
	if errors.Is(err, errServerClosed) {
		return false
	}
	if wire, ok := errors.AsType[*jsonrpc.Error](err); ok {
		switch wire.Code {
		case codeParseError, codeInvalidRequest, codeMethodNotFound,
			codeInvalidParams, codeClientClosing, codeRejected:
			return false
		}
	}
	return true
}

// retryBackoff is the delay before the attempt after this one: the base
// doubled per attempt, capped at maxRetryBackoff, then jittered into
// [d/2, d] so servers shared by many runs are not retried in lockstep.
func retryBackoff(base time.Duration, attempt int) time.Duration {
	d := float64(base) * math.Pow(2, float64(attempt-1))
	if d > float64(maxRetryBackoff) {
		d = float64(maxRetryBackoff)
	}
	half := d / 2
	return time.Duration(half + rand.Float64()*half)
}

// runWithRetries invokes fn, retrying RETRYABLE failures up to
// MaxRetryAttempts times with capped, jittered exponential backoff.
// MaxRetryAttempts == -1 retries indefinitely; 0 disables retries. An error
// retryable rejects is returned on the spot, however many attempts remain.
func (s *Server) runWithRetries(ctx context.Context, fn func() error) error {
	base := s.opts.RetryBackoffBase
	if base <= 0 {
		base = time.Second
	}
	attempts := 0
	for {
		err := fn()
		if err == nil {
			return nil
		}
		attempts++
		if !retryable(err) {
			return err
		}
		if s.opts.MaxRetryAttempts != -1 && attempts > s.opts.MaxRetryAttempts {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryBackoff(base, attempts)):
		}
	}
}

// InvalidateToolsCache drops any cached tool list so the next ListTools refetches
// it. No-op when CacheToolsList is not set.
func (s *Server) InvalidateToolsCache() {
	s.mu.Lock()
	s.cached = nil
	s.cacheGen++
	s.mu.Unlock()
}

func (s *Server) toolFor(mt *mcpsdk.Tool, exposedName string) *agents.Tool {
	schema := schemaToMap(mt.InputSchema)
	// Capture the required-argument list from the original (non-strict) schema,
	// used for client-side validation before every call_tool request.
	required := requiredKeys(schema)
	strict := false
	if s.opts.Strict {
		// EnsureStrictJSONSchema rewrites in place, so convert a deep copy: a
		// failure must not leave a half-rewritten schema behind, and the tool
		// must not claim strict mode with a non-strict schema.
		if strictSchema, err := agents.EnsureStrictJSONSchema(deepCopySchema(schema)); err == nil {
			schema = strictSchema
			strict = true
		}
	}
	originalName := mt.Name
	// A tool's own _meta travels with every call_tool request, overriding any
	// resolver-produced metadata on key collisions.
	staticMeta := map[string]any(mt.Meta)
	tool := &agents.Tool{
		Name:             exposedName,
		Description:      resolveToolDescription(mt),
		ParamsJSONSchema: schema,
		Strict:           strict,
		// The server's own readOnlyHint, recorded for consumers that want it.
		// It is an OUTSIDE server's claim about itself: the plan-mode gate
		// deliberately ignores it and admits MCP tools by name only (spec
		// "A FIRST-PARTY tool's ReadOnly is trusted; an MCP tool's is not").
		ReadOnly: mt.Annotations != nil && mt.Annotations.ReadOnlyHint,
		// Tool failures (including isError results) are fed back to the model
		// so it can recover, matching the SDK-wide default; without this every
		// MCP error would abort the whole run.
		FailureErrorFunction: agents.DefaultToolErrorFunction,
		OnInvoke: func(ctx context.Context, _ *agents.ToolContext, argsJSON string) (agents.ToolResult, error) {
			// Always send an "arguments" object — an empty {} rather than an
			// omitted field: some servers reject calls with no arguments key.
			args := map[string]any{}
			if strings.TrimSpace(argsJSON) != "" {
				if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
					return agents.ToolResult{}, agents.Classify(agents.CodeModelBehavior, fmt.Errorf("mcp tool %q: invalid arguments: %w", exposedName, err))
				}
				if args == nil { // argsJSON was JSON null
					args = map[string]any{}
				}
			}
			// Client-side pre-validation of required parameters, before touching
			// the server: a missing required key is a *agents.UserError.
			if err := validateRequiredArgs(s.name, originalName, required, args); err != nil {
				return agents.ToolResult{}, err
			}
			// The server is always called with the original (unprefixed) name.
			params := &mcpsdk.CallToolParams{Name: originalName, Arguments: args}
			if len(staticMeta) > 0 {
				params.Meta = mcpsdk.Meta(staticMeta)
			}
			span, ctx := tracing.StartSpanFrom(ctx, "mcp.call_tool", tracing.SpanTypeMCP, map[string]any{
				"server": s.name, "tool": originalName,
			})
			defer span.Finish()

			var result *mcpsdk.CallToolResult
			session := s.session.Load()
			if err := s.runWithRetries(ctx, func() error {
				var e error
				if s.closed.Load() {
					return agents.Classify(agents.CodeMCP, fmt.Errorf("mcp: server %q: %w", s.name, errServerClosed))
				}
				result, e = callSession(ctx, s, func(rpc context.Context) (*mcpsdk.CallToolResult, error) {
					return s.session.Load().CallTool(rpc, params)
				})
				return e
			}); err != nil {
				// Repair the connection, but do NOT repeat the call on it. A
				// dead connection cannot say whether the server ran this tool
				// before the line dropped, and repeating a write twice is worse
				// than reporting it once. The model is told, it can ask again,
				// and the connection it asks on is now a live one.
				s.healed(err, session)
				span.SetError(err.Error(), nil)
				// A transport/protocol failure is fed back to the model via the
				// FailureErrorFunction (SDK-wide default) so it can recover.
				return agents.ToolResult{}, agents.Classify(agents.CodeMCP, fmt.Errorf("mcp tool %q call failed: %w", originalName, err))
			}
			span.Set("is_error", result.IsError)
			// An isError result is NOT a Go error: its content (usually the
			// error message) passes to the model verbatim so it can recover.
			// It IS marked as an error on the result, so a UI can render it as
			// one.
			return agents.ToolResult{
				IsError: result.IsError,
				Content: resultOutput(result, s.opts.UseStructuredContent),
			}, nil
		},
	}
	return tool
}

// validateRequiredArgs reports a *agents.UserError when any schema-required
// argument is missing, before the request reaches the server.
func validateRequiredArgs(serverName, toolName string, required []string, args map[string]any) error {
	if len(required) == 0 {
		return nil
	}
	var missing []string
	for _, name := range required {
		if _, ok := args[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	slices.Sort(missing)
	return agents.NewUserError("Failed to call tool %q on MCP server %q: missing required parameters: %s",
		toolName, serverName, strings.Join(missing, ", "))
}

// requiredKeys extracts the string entries of a JSON schema's "required" array.
func requiredKeys(schema map[string]any) []string {
	raw, ok := schema["required"].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// resolveToolDescription returns the best model-facing description for an MCP
// tool: its description, falling back to the display title, then the annotations
// title. Mirrors resolve_mcp_tool_description_for_model.
func resolveToolDescription(mt *mcpsdk.Tool) string {
	if mt.Description != "" {
		return mt.Description
	}
	if mt.Title != "" {
		return mt.Title
	}
	if mt.Annotations != nil && mt.Annotations.Title != "" {
		return mt.Annotations.Title
	}
	return ""
}

// exposedNames computes the public name for each listed tool: the
// ToolNamePrefix (possibly empty) + the original name.
func (s *Server) exposedNames(tools []*mcpsdk.Tool) []string {
	names := make([]string, len(tools))
	for i, mt := range tools {
		names[i] = s.opts.ToolNamePrefix + mt.Name
	}
	return names
}

// schemaToMap normalizes the MCP input schema (an any) into a map[string]any.
// The result is always a map this package owns: the schema travels on the
// go-sdk's own tool object, and the "properties" fill-in below must not write
// back into it.
func schemaToMap(schema any) map[string]any {
	m, ok := schema.(map[string]any)
	if ok {
		m = deepCopySchema(m)
	} else {
		// Round-trip through JSON for other representations (e.g. json.RawMessage).
		raw, err := json.Marshal(schema)
		if err != nil {
			return emptyObjectSchema()
		}
		if err := json.Unmarshal(raw, &m); err != nil || m == nil {
			return emptyObjectSchema()
		}
	}
	// OpenAI requires object schemas to carry "properties"; some servers omit
	// it for no-argument tools.
	if _, ok := m["properties"]; !ok {
		m["properties"] = map[string]any{}
	}
	return m
}

func emptyObjectSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// deepCopySchema clones a schema map via a JSON round-trip.
func deepCopySchema(m map[string]any) map[string]any {
	raw, err := json.Marshal(m)
	if err != nil {
		return emptyObjectSchema()
	}
	var cp map[string]any
	if err := json.Unmarshal(raw, &cp); err != nil || cp == nil {
		return emptyObjectSchema()
	}
	return cp
}

// resultOutput renders a tool result into the content parts the model receives:
// - When useStructured is set AND the result carries non-empty
// structuredContent, that field is used exclusively (JSON-encoded into a
// single text part) and the content blocks are ignored. A nil or empty
// structuredContent falls through to the content blocks instead — most
// servers duplicate their data in the content blocks, so an empty structured
// field must never blank out the result. By default (useStructured false)
// structuredContent is ignored entirely.
// - Otherwise every content block becomes its own part, so the model receives
// each one natively: text stays text, images become images, and everything
// else is JSON-encoded into a text part. A result with no blocks at all
// yields a single empty text part.
func resultOutput(result *mcpsdk.CallToolResult, useStructured bool) []agents.ToolOutputContent {
	if useStructured && hasStructuredContent(result.StructuredContent) {
		if b, err := json.Marshal(result.StructuredContent); err == nil {
			return []agents.ToolOutputContent{agents.ToolOutputText{Text: string(b)}}
		}
		// A marshal failure is unexpected for JSON-decoded content; fall through
		// to the content blocks rather than emitting an empty result.
	}
	if len(result.Content) == 0 {
		return []agents.ToolOutputContent{agents.ToolOutputText{}}
	}
	parts := make([]agents.ToolOutputContent, 0, len(result.Content))
	for _, c := range result.Content {
		switch v := c.(type) {
		case *mcpsdk.TextContent:
			parts = append(parts, agents.ToolOutputText{Text: v.Text})
		case *mcpsdk.ImageContent:
			parts = append(parts, agents.ToolOutputImageFromBytes(v.MIMEType, v.Data))
		case *mcpsdk.EmbeddedResource:
			if r := v.Resource; r != nil && isImageMIME(r.MIMEType) && len(r.Blob) > 0 {
				parts = append(parts, agents.ToolOutputImageFromBytes(r.MIMEType, r.Blob))
				continue
			}
			parts = append(parts, jsonTextPart(c))
		default:
			parts = append(parts, jsonTextPart(c))
		}
	}
	return parts
}

// hasStructuredContent reports whether a tool result's structuredContent field
// holds a usable value. The MCP schema types the field as a
// JSON object (Go: nil or map[string]any); a nil or empty object is treated as
// absent so the caller falls back to the content blocks. Empty slices/strings
// are covered defensively for non-conforming servers, while genuine scalar
// values (numbers, booleans) count as present.
func hasStructuredContent(sc any) bool {
	switch v := sc.(type) {
	case nil:
		return false
	case map[string]any:
		return len(v) > 0
	case []any:
		return len(v) > 0
	case string:
		return v != ""
	default:
		return true
	}
}

func isImageMIME(mimeType string) bool {
	return strings.HasPrefix(mimeType, "image/")
}

// jsonTextPart JSON-encodes a content block into a text part so non-image,
// non-text blocks (audio, resource links, …) are preserved rather than dropped.
func jsonTextPart(c mcpsdk.Content) agents.ToolOutputText {
	if b, err := json.Marshal(c); err == nil {
		return agents.ToolOutputText{Text: string(b)}
	}
	return agents.ToolOutputText{Text: ""}
}

// Session exposes the underlying MCP client session, for protocol surface this
// package does not adapt — prompts, resources, and whatever the SDK grows
// next. It is nil until the server is connected.
func (s *Server) Session() *mcpsdk.ClientSession { return s.session.Load() }

var _ agents.MCPServer = (*Server)(nil)
