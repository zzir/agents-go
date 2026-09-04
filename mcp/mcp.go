// Package mcp is a Model Context Protocol client that exposes a server's tools
// to an agent: agents.MCPServer over the official modelcontextprotocol/go-sdk,
// with stdio and streamable HTTP transports. It is its own module so the
// go-sdk stays out of every build that does not speak MCP (decisions §5.7).
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

	// CacheToolsList caches the server's tool list after the first fetch. The
	// cache drops on a tools/list_changed notification; InvalidateToolsCache
	// forces a refetch. Filters still run on every ListTools, against the cache.
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

	// UseStructuredContent sends a tool result's structuredContent exclusively,
	// ignoring the content blocks. Off by default: most servers duplicate their
	// structured data in the blocks.
	UseStructuredContent bool

	// OAuthHandler, when set, is passed to the streamable HTTP transport to
	// handle OAuth 2.1 authorization flows (authorization code + PKCE, token
	// refresh, dynamic client registration). Ignored for stdio transports.
	OAuthHandler auth.OAuthHandler

	// Redial builds a fresh transport for a dead connection — one that can connect
	// NOW (an unstarted command; the same endpoint, headers and OAuth handler).
	// ctx is the connection's own, so a subprocess bound to it lives as long as
	// the connection does. Nil: a dead connection is reported, not repaired.
	Redial func(ctx context.Context) (mcpsdk.Transport, error)
}

// Server is a connected MCP server whose tools are exposed to an agent. It
// implements agents.MCPServer.
type Server struct {
	name string
	// session is swapped in place when a connection dies (see redial), so every
	// holder of this *Server recovers.
	session atomic.Pointer[mcpsdk.ClientSession]
	dialMu  sync.Mutex
	// lastDial throttles healing; see redialCooldown.
	lastDial time.Time
	opts     Options
	allowed  map[string]bool
	blocked  map[string]bool

	// closed flips in Close so later ListTools/CallTool report a clear error; a
	// long-lived run can hold a *Server past a reconfiguration that closed it.
	closed atomic.Bool

	// rpcCtx is the CONNECTION's context, ending only with Close — never a
	// caller's (spec §2.16); requestCeiling bounds one request on it.
	rpcCtx         context.Context
	rpcStop        context.CancelFunc
	requestCeiling time.Duration

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

// requestCeiling bounds a request whose caller has already left (decisions §5.20).
const requestCeiling = 30 * time.Minute

func newServer(name string, opts Options) *Server {
	s := &Server{name: name, opts: opts, allowed: map[string]bool{}, blocked: map[string]bool{}, requestCeiling: requestCeiling}
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

// callSession runs fn on the connection's context and returns when EITHER it
// answers or the caller's ctx ends; a late answer is dropped (spec §2.16).
func callSession[T any](ctx context.Context, s *Server, fn func(context.Context) (T, error)) (T, error) {
	type answer struct {
		val T
		err error
	}
	// Buffered, so the request goroutine never blocks on a caller that left.
	ch := make(chan answer, 1)
	go func() {
		rctx, cancel := context.WithTimeout(s.rpcCtx, s.requestCeiling)
		defer cancel()
		val, err := fn(rctx)
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

// redialCooldown throttles healing so a server that accepts and drops
// connections cannot become a dial loop (spec §2.16).
const redialCooldown = 3 * time.Second

// redialConnectTimeout bounds one healing handshake: redial holds dialMu, so a
// black-holing endpoint would otherwise hold every failed caller with it.
const redialConnectTimeout = 30 * time.Second

func (s *Server) newClient() *mcpsdk.Client {
	name := s.opts.ClientName
	name = cmp.Or(name, "agents-go")
	version := s.opts.ClientVersion
	version = cmp.Or(version, "0.1.0")
	return mcpsdk.NewClient(&mcpsdk.Implementation{Name: name, Version: version}, &mcpsdk.ClientOptions{
		// Drop the cached tool list on notifications/tools/list_changed, so
		// CacheToolsList never serves a permanently stale list.
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

// watch heals the connection the moment it dies — one goroutine per live
// connection, ending with it (decisions §5.21).
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

// redial replaces the dead session failed with a fresh one and reports whether
// this call did it; concurrent discoverers dial once (decisions §5.21).
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
	// The transport gets the connection's context (a subprocess must outlive this
	// call); only the HANDSHAKE is bounded, because it runs under dialMu.
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
	// Re-checked AFTER the store: Close marks closed, then closes the slot, so a
	// Close racing this swap is caught here rather than leaking the new session.
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

// healed reports whether err means the connection is gone and a fresh one
// replaced it. A server closed on purpose is never healed.
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
	// closed is set FIRST so the watcher never heals a deliberate shutdown;
	// rpcStop is nil-checked because the zero Server is reachable.
	s.closed.Store(true)
	if s.rpcStop != nil {
		// Before the session close: closing waits for in-flight requests, and ending
		// the connection's context is what lets an abandoned one unwind.
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

// bindApproval wires RequireApproval with the agent captured per ListTools
// call, so the closure names whose turn it is; the cached base tool is untouched.
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
	// Fetched outside s.mu (a slow RPC must not block InvalidateToolsCache), and
	// EVERY page: a truncated list would be cached as if it were complete.
	var tools []*mcpsdk.Tool
	fetch := func() error {
		tools = tools[:0]
		var params *mcpsdk.ListToolsParams
		// A server that repeats a cursor or never runs out of pages is a protocol
		// error, not an unbounded loop.
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
		// The list is idempotent, so ask again on the healed connection; a tool
		// CALL is never repeated (decisions §5.21).
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
		// Prefer a list another goroutine published; cache ours only if no
		// InvalidateToolsCache ran mid-fetch (gen unchanged), else leave it empty.
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

// JSON-RPC codes meaning the request was understood and refused (spec §2.16),
// as numbers: the go-sdk exports them as error values from an internal package.
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

// retryable reports whether err is worth another attempt: a transport failure
// is; an answer the server sent, or a call after Close, is not (spec §2.16).
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

// retryBackoff is the delay before the next attempt: base doubled per attempt,
// capped at maxRetryBackoff, jittered into [d/2, d] (spec §2.16).
func retryBackoff(base time.Duration, attempt int) time.Duration {
	d := float64(base) * math.Pow(2, float64(attempt-1))
	if d > float64(maxRetryBackoff) {
		d = float64(maxRetryBackoff)
	}
	half := d / 2
	return time.Duration(half + rand.Float64()*half)
}

// runWithRetries retries RETRYABLE failures up to MaxRetryAttempts times (-1
// indefinitely, 0 never) with retryBackoff between attempts.
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
		// failure must leave neither a half-rewritten schema nor a false Strict.
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
		// The server's own readOnlyHint, recorded for consumers; the plan-mode
		// gate ignores it and admits MCP tools by name only (spec §2.12).
		ReadOnly: mt.Annotations != nil && mt.Annotations.ReadOnlyHint,
		// Failures (isError results included) feed back to the model, the SDK-wide
		// default; otherwise every MCP error would abort the run.
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
				// Repair the connection but do NOT repeat the call: a dead line cannot
				// say whether the server ran it (decisions §5.21).
				s.healed(err, session)
				span.SetError(err.Error(), nil)
				// A transport/protocol failure is fed back to the model via the
				// FailureErrorFunction (SDK-wide default) so it can recover.
				return agents.ToolResult{}, agents.Classify(agents.CodeMCP, fmt.Errorf("mcp tool %q call failed: %w", originalName, err))
			}
			span.Set("is_error", result.IsError)
			// An isError result is NOT a Go error: its content reaches the model
			// verbatim, marked IsError so a UI can render it as one.
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

// resolveToolDescription returns the model-facing description: description,
// else display title, else annotations title.
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

// schemaToMap normalizes the MCP input schema into a map this package owns:
// the "properties" fill-in must not write back into the go-sdk's tool object.
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

// resultOutput renders a tool result as content parts: structuredContent alone
// (JSON text) when useStructured and it is non-empty, else one part per block.
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

// hasStructuredContent reports whether structuredContent holds a usable value:
// nil and an empty object/slice/string are absent; other scalars are present.
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
