// Package mcp provides a Model Context Protocol (MCP) client that exposes a
// server's tools to an agent. It implements agents.MCPServer over the official
// modelcontextprotocol/go-sdk, supporting stdio and streamable HTTP transports.
package mcp

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
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
}

// Server is a connected MCP server whose tools are exposed to an agent. It
// implements agents.MCPServer.
type Server struct {
	name    string
	session *mcpsdk.ClientSession
	opts    Options
	allowed map[string]bool
	blocked map[string]bool

	// closed flips when Close runs so later ListTools/CallTool report a clear
	// error instead of failing obscurely on the dead session — a long-lived
	// run can hold a *Server pointer past a reconfiguration that closed it.
	closed atomic.Bool

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
	for _, t := range opts.AllowedTools {
		s.allowed[t] = true
	}
	for _, t := range opts.BlockedTools {
		s.blocked[t] = true
	}
	return s
}

func (s *Server) connect(ctx context.Context, transport mcpsdk.Transport) error {
	name := s.opts.ClientName
	name = cmp.Or(name, "agents-go")
	version := s.opts.ClientVersion
	version = cmp.Or(version, "0.1.0")
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: name, Version: version}, &mcpsdk.ClientOptions{
		// Drop the cached tool list when the server announces a change
		// (notifications/tools/list_changed), so CacheToolsList can never serve
		// a permanently stale list. No-op when caching is off.
		ToolListChangedHandler: func(context.Context, *mcpsdk.ToolListChangedRequest) {
			s.InvalidateToolsCache()
		},
	})
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return agents.Classify(agents.CodeMCP, fmt.Errorf("mcp: connecting to %q: %w", s.name, err))
	}
	s.session = session
	return nil
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
	if s.session == nil {
		return nil
	}
	s.closed.Store(true)
	return s.session.Close()
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
	if s.session == nil {
		return nil, agents.Classify(agents.CodeMCP, fmt.Errorf("mcp: server %q is not connected", s.name))
	}
	if s.closed.Load() {
		return nil, agents.Classify(agents.CodeMCP, fmt.Errorf("mcp: server %q is closed", s.name))
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
	err := s.runWithRetries(ctx, func() error {
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
			res, e := s.session.ListTools(ctx, params)
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
	})
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

// runWithRetries invokes fn, retrying failures up to MaxRetryAttempts times
// with exponential backoff (RetryBackoffBase * 2^(attempt-1)).
// MaxRetryAttempts == -1 retries indefinitely; 0 disables retries.
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
		if s.opts.MaxRetryAttempts != -1 && attempts > s.opts.MaxRetryAttempts {
			return err
		}
		// Cap the shift so an unbounded retry loop cannot overflow the exponent.
		shift := attempts - 1
		if shift > 30 {
			shift = 30
		}
		backoff := base * time.Duration(int64(1)<<shift)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
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
			if err := s.runWithRetries(ctx, func() error {
				var e error
				if s.closed.Load() {
					return agents.Classify(agents.CodeMCP, fmt.Errorf("mcp: server %q is closed", s.name))
				}
				result, e = s.session.CallTool(ctx, params)
				return e
			}); err != nil {
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
	sort.Strings(missing)
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
func schemaToMap(schema any) map[string]any {
	m, ok := schema.(map[string]any)
	if !ok {
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
func (s *Server) Session() *mcpsdk.ClientSession { return s.session }

var _ agents.MCPServer = (*Server)(nil)
