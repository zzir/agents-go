// Package mcp provides a Model Context Protocol (MCP) client that exposes a
// server's tools to an agent. It implements agents.MCPServer over the official
// modelcontextprotocol/go-sdk, supporting stdio and streamable HTTP transports
// (plus the deprecated SSE transport).
package mcp

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zzir/agents-go/agents"
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
	// is still called with the original name. Ignored when
	// IncludeServerInToolNames is set.
	ToolNamePrefix string

	// IncludeServerInToolNames auto-prefixes every exposed tool name with the
	// server name (mcp_{server}__{tool}), truncating names longer than 64
	// characters with a sha1 suffix and disambiguating any resulting collisions.
	// The server is still called with the original name. When set it takes
	// precedence over ToolNamePrefix. A rename or truncation is logged via
	// slog.Default so an auto-renamed tool is never silently capped.
	IncludeServerInToolNames bool

	// RequireApproval, when set, marks an exposed MCP tool as needing human
	// approval (HITL) whenever it returns true for the tool's original name.
	RequireApproval func(toolName string) bool

	// RequireApprovalFunc, when set, decides per call whether an exposed MCP tool
	// needs human approval, receiving the run context, the current agent, and the
	// tool's original name. It is wired to the core per-call approval mechanism
	// and takes precedence over RequireApproval. The current agent is captured per
	// ListTools call, matching the Python SDK.
	RequireApprovalFunc func(ctx context.Context, rc *agents.RunContext, agent *agents.Agent, toolName string) bool

	// ToolMetaResolver, when set, produces MCP request metadata (_meta) attached
	// to each call_tool request, receiving the run context, the tool's original
	// name, and the decoded arguments. Values it returns are overridden, per key,
	// by a tool's own static _meta, matching the Python SDK's merge order.
	ToolMetaResolver func(ctx context.Context, rc *agents.RunContext, toolName string, args map[string]any) (map[string]any, error)

	// MaxRetryAttempts is the number of times to retry a failed list_tools or
	// call_tool request. 0 (default) means no retries; -1 retries indefinitely.
	MaxRetryAttempts int

	// RetryBackoffBase is the base delay for exponential backoff between retries
	// (delay = RetryBackoffBase * 2^(attempt-1)). Defaults to one second when
	// retries are enabled and this is left zero.
	RetryBackoffBase time.Duration

	// UseStructuredContent controls how a tool result's structuredContent field
	// is handled. It is false by default (matching the Python SDK): most servers
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

	mu     sync.Mutex
	cached []cachedTool // populated lazily when CacheToolsList is set
}

// cachedTool pairs an adapted tool with its original (unprefixed) MCP name, used
// for static/dynamic filtering on each ListTools call.
type cachedTool struct {
	originalName string
	tool         agents.Tool
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
	if name == "" {
		name = "agents-go"
	}
	version := s.opts.ClientVersion
	if version == "" {
		version = "0.1.0"
	}
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
		return fmt.Errorf("mcp: connecting to %q: %w", s.name, err)
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

// NewSSEServer connects to an MCP server over the SSE transport at endpoint.
//
// Deprecated: the MCP spec replaced the HTTP+SSE transport with streamable HTTP
// (revision 2025-03-26); use [NewStreamableHTTPServer] for new servers. This is
// kept for servers that only expose a legacy SSE endpoint.
func NewSSEServer(ctx context.Context, name, endpoint string, opts Options) (*Server, error) {
	s := newServer(name, opts)
	if err := s.connect(ctx, &mcpsdk.SSEClientTransport{Endpoint: endpoint}); err != nil {
		return nil, err
	}
	return s, nil
}

// Name implements agents.MCPServer.
func (s *Server) Name() string { return s.name }

// Close implements agents.MCPServer, closing the session.
func (s *Server) Close() error {
	if s.session == nil {
		return nil
	}
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
// list and adapting each into an agents.FunctionTool that proxies to CallTool.
// Static (AllowedTools/BlockedTools) and dynamic (ToolFilter) filters run here on
// every call, so caching never hides a context-dependent filter decision.
func (s *Server) ListTools(ctx context.Context, rc *agents.RunContext, agent *agents.Agent) ([]agents.Tool, error) {
	all, err := s.toolList(ctx)
	if err != nil {
		return nil, err
	}
	var tools []agents.Tool
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

// bindApproval returns the tool to expose for this ListTools call, wiring the
// dynamic RequireApprovalFunc (if set) with the current agent captured per call
// — matching the Python SDK, which re-binds the approval closure to the current
// agent each turn. The cached base tool is left untouched.
func (s *Server) bindApproval(ct cachedTool, agent *agents.Agent) agents.Tool {
	if s.opts.RequireApprovalFunc == nil {
		return ct.tool
	}
	ft, ok := ct.tool.(*agents.FunctionTool)
	if !ok {
		return ct.tool
	}
	clone := *ft
	name := ct.originalName
	fn := s.opts.RequireApprovalFunc
	clone.NeedsApprovalFunc = func(ctx context.Context, rc *agents.RunContext, _ string, _ string) (bool, error) {
		return fn(ctx, rc, agent, name), nil
	}
	return &clone
}

// toolList returns the adapted tools, fetching them from the server (and caching
// when CacheToolsList is set) or reusing the cache.
func (s *Server) toolList(ctx context.Context) ([]cachedTool, error) {
	if s.session == nil {
		return nil, fmt.Errorf("mcp: server %q is not connected", s.name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.opts.CacheToolsList && s.cached != nil {
		return s.cached, nil
	}
	var res *mcpsdk.ListToolsResult
	err := s.runWithRetries(ctx, func() error {
		var e error
		res, e = s.session.ListTools(ctx, nil)
		return e
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: listing tools for %q: %w", s.name, err)
	}
	names := s.exposedNames(res.Tools)
	list := make([]cachedTool, 0, len(res.Tools))
	for i, mt := range res.Tools {
		list = append(list, cachedTool{originalName: mt.Name, tool: s.toolFor(mt, names[i])})
	}
	if s.opts.CacheToolsList {
		s.cached = list
	}
	return list, nil
}

// runWithRetries invokes fn, retrying failures up to MaxRetryAttempts times with
// exponential backoff (RetryBackoffBase * 2^(attempt-1)). MaxRetryAttempts == -1
// retries indefinitely; 0 disables retries. It mirrors the Python SDK's
// _run_with_retries.
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
	s.mu.Unlock()
}

func (s *Server) toolFor(mt *mcpsdk.Tool, exposedName string) agents.Tool {
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
	// resolver-produced metadata on key collisions (Python merge order).
	staticMeta := map[string]any(mt.Meta)
	tool := &agents.FunctionTool{
		Name:             exposedName,
		Description:      resolveToolDescription(mt),
		ParamsJSONSchema: schema,
		Strict:           strict,
		// Tool failures (including isError results) are fed back to the model
		// so it can recover, matching the SDK-wide default; without this every
		// MCP error would abort the whole run.
		FailureErrorFunction: agents.DefaultToolErrorFunction,
		OnInvoke: func(ctx context.Context, tc *agents.ToolContext, argsJSON string) (any, error) {
			// Always send an "arguments" object — an empty {} rather than an
			// omitted field — matching the Python SDK, which passes an empty
			// dict; some servers reject calls with no arguments key.
			args := map[string]any{}
			if strings.TrimSpace(argsJSON) != "" {
				if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
					return nil, fmt.Errorf("mcp tool %q: invalid arguments: %w", exposedName, err)
				}
				if args == nil { // argsJSON was JSON null
					args = map[string]any{}
				}
			}
			// Client-side pre-validation of required parameters, before touching
			// the server: a missing required key is a *agents.UserError, matching
			// the Python SDK's _validate_required_parameters.
			if err := validateRequiredArgs(s.name, originalName, required, args); err != nil {
				return nil, err
			}
			meta, err := s.resolveMeta(ctx, tc, originalName, args, staticMeta)
			if err != nil {
				return nil, err
			}
			// The server is always called with the original (unprefixed) name.
			params := &mcpsdk.CallToolParams{Name: originalName, Arguments: args}
			if meta != nil {
				params.Meta = meta
			}
			var result *mcpsdk.CallToolResult
			if err := s.runWithRetries(ctx, func() error {
				var e error
				result, e = s.session.CallTool(ctx, params)
				return e
			}); err != nil {
				// A transport/protocol failure is fed back to the model via the
				// FailureErrorFunction (SDK-wide default) so it can recover.
				return nil, fmt.Errorf("mcp tool %q call failed: %w", originalName, err)
			}
			// An isError result is NOT an error: its content (usually the error
			// message) passes to the model verbatim, matching the Python SDK,
			// which never inspects result.isError in invoke_mcp_tool.
			return resultOutput(result, s.opts.UseStructuredContent), nil
		},
	}
	if s.opts.RequireApproval != nil && s.opts.RequireApproval(originalName) {
		tool.NeedsApproval = true
	}
	return tool
}

// resolveMeta computes the _meta to send with a call_tool request: the resolver's
// output (if any) merged under the tool's static _meta (static wins per key).
func (s *Server) resolveMeta(ctx context.Context, tc *agents.ToolContext, toolName string, args, staticMeta map[string]any) (mcpsdk.Meta, error) {
	var resolved map[string]any
	if s.opts.ToolMetaResolver != nil {
		var rc *agents.RunContext
		if tc != nil {
			rc = tc.RunContext
		}
		var err error
		resolved, err = s.opts.ToolMetaResolver(ctx, rc, toolName, args)
		if err != nil {
			return nil, fmt.Errorf("mcp tool %q: resolving _meta: %w", toolName, err)
		}
	}
	if resolved == nil && len(staticMeta) == 0 {
		return nil, nil
	}
	merged := make(mcpsdk.Meta, len(resolved)+len(staticMeta))
	for k, v := range resolved {
		merged[k] = v
	}
	for k, v := range staticMeta {
		merged[k] = v
	}
	if len(merged) == 0 {
		return nil, nil
	}
	return merged, nil
}

// validateRequiredArgs reports a *agents.UserError when any schema-required
// argument is missing, mirroring the Python SDK's _validate_required_parameters.
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
	return &agents.UserError{AgentsError: agents.AgentsError{
		Message: fmt.Sprintf("Failed to call tool %q on MCP server %q: missing required parameters: %s",
			toolName, serverName, strings.Join(missing, ", ")),
	}}
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

const (
	mcpToolNameMaxLength = 64
	mcpToolHashLength    = 8
)

// exposedNames computes the public name for each listed tool. Without
// IncludeServerInToolNames it is the ToolNamePrefix + original name; with it,
// names are auto-prefixed mcp_{server}__{tool}, truncated to 64 characters with a
// sha1 suffix, and disambiguated on collision — mirroring the Python SDK's
// _build_prefixed_tool_name_overrides. Truncated or disambiguated names are
// logged so an auto-rename is never silent.
func (s *Server) exposedNames(tools []*mcpsdk.Tool) []string {
	names := make([]string, len(tools))
	if !s.opts.IncludeServerInToolNames {
		for i, mt := range tools {
			names[i] = s.opts.ToolNamePrefix + mt.Name
		}
		return names
	}

	baseNames := make([]string, len(tools))
	baseCounts := map[string]int{}
	for i, mt := range tools {
		baseNames[i] = buildPrefixedBaseName(s.name, mt.Name)
		baseCounts[baseNames[i]]++
	}

	type candidate struct {
		index       int
		base, seed  string
		initialName string
	}
	cands := make([]candidate, len(tools))
	for i, mt := range tools {
		base := baseNames[i]
		seed := s.name + "\x00" + mt.Name
		forceHash := baseCounts[base] > 1
		cands[i] = candidate{index: i, base: base, seed: seed, initialName: shortenToolName(base, seed, forceHash)}
	}

	// Allocate names in a deterministic order (initial name, seed, index) so a
	// collision is resolved the same way regardless of the server's tool order.
	order := make([]int, len(cands))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ca, cb := cands[order[a]], cands[order[b]]
		if ca.initialName != cb.initialName {
			return ca.initialName < cb.initialName
		}
		if ca.seed != cb.seed {
			return ca.seed < cb.seed
		}
		return ca.index < cb.index
	})

	used := map[string]bool{}
	for _, oi := range order {
		c := cands[oi]
		public := c.initialName
		for collision := 1; used[public]; collision++ {
			public = shortenToolName(c.base, fmt.Sprintf("%s\x00%d", c.seed, collision), true)
		}
		used[public] = true
		names[c.index] = public
	}

	for i, mt := range tools {
		if names[i] != baseNames[i] {
			slog.Default().Info("mcp: tool name truncated or disambiguated",
				"server", s.name, "tool", mt.Name, "exposed_name", names[i])
		}
	}
	return names
}

func buildPrefixedBaseName(server, tool string) string {
	return "mcp_" + safeToolNamePart(server, "server") + "__" + safeToolNamePart(tool, "tool")
}

// safeToolNamePart keeps ASCII alphanumerics, '_' and '-', replacing anything
// else with '_', then trims leading/trailing '_'/'-'. An empty result falls back
// to fallback. Mirrors the Python SDK's _safe_tool_name_part.
func safeToolNamePart(value, fallback string) string {
	var b strings.Builder
	for _, r := range value {
		if isASCIIAlnum(r) || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	safe := strings.Trim(b.String(), "_-")
	if safe == "" {
		return fallback
	}
	return safe
}

func isASCIIAlnum(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// shortenToolName caps a prefixed tool name at 64 characters, appending a sha1
// suffix derived from seed when the name is too long or forceHash is set.
// Mirrors the Python SDK's _shorten_tool_name.
func shortenToolName(base, seed string, forceHash bool) string {
	if !forceHash && len(base) <= mcpToolNameMaxLength {
		return base
	}
	sum := sha1.Sum([]byte(seed))
	hashSuffix := hex.EncodeToString(sum[:])[:mcpToolHashLength]
	suffix := "_" + hashSuffix
	stemLen := mcpToolNameMaxLength - len(suffix)
	stem := base
	if len(stem) > stemLen {
		stem = stem[:stemLen]
	}
	stem = strings.TrimRight(stem, "_-")
	if stem == "" {
		stem = "mcp"
	}
	return stem + suffix
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

// resultOutput renders a tool result for the model, mirroring the Python SDK's
// content conversion:
//   - When useStructured is set, the structuredContent field is used
//     exclusively (JSON-encoded to a single text output) and the content blocks
//     are ignored. By default structuredContent is ignored instead, because most
//     servers duplicate it in the content blocks.
//   - A single text block passes through as a plain string.
//   - Multiple blocks, or any non-text block, become a []ToolOutputContent list
//     so the model receives each block natively (text stays text, images become
//     images, everything else is JSON-encoded into a text part) rather than one
//     opaque JSON string.
func resultOutput(result *mcpsdk.CallToolResult, useStructured bool) any {
	if useStructured {
		if result.StructuredContent != nil {
			if b, err := json.Marshal(result.StructuredContent); err == nil {
				return string(b)
			}
		}
		return ""
	}
	if len(result.Content) == 0 {
		return ""
	}
	if len(result.Content) == 1 {
		if tc, ok := result.Content[0].(*mcpsdk.TextContent); ok {
			return tc.Text
		}
	}
	var parts []agents.ToolOutputContent
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

// ListPrompts returns the prompt templates the server exposes. A prompt can be
// turned into agent instructions via GetPrompt. params may be nil.
func (s *Server) ListPrompts(ctx context.Context, params *mcpsdk.ListPromptsParams) (*mcpsdk.ListPromptsResult, error) {
	if s.session == nil {
		return nil, fmt.Errorf("mcp: server %q is not connected", s.name)
	}
	return s.session.ListPrompts(ctx, params)
}

// GetPrompt fetches a prompt by name with the given arguments. The returned
// messages can seed an agent's instructions or input.
func (s *Server) GetPrompt(ctx context.Context, params *mcpsdk.GetPromptParams) (*mcpsdk.GetPromptResult, error) {
	if s.session == nil {
		return nil, fmt.Errorf("mcp: server %q is not connected", s.name)
	}
	return s.session.GetPrompt(ctx, params)
}

// ListResources returns the resources the server exposes. params may be nil.
func (s *Server) ListResources(ctx context.Context, params *mcpsdk.ListResourcesParams) (*mcpsdk.ListResourcesResult, error) {
	if s.session == nil {
		return nil, fmt.Errorf("mcp: server %q is not connected", s.name)
	}
	return s.session.ListResources(ctx, params)
}

// ReadResource reads a resource by URI.
func (s *Server) ReadResource(ctx context.Context, params *mcpsdk.ReadResourceParams) (*mcpsdk.ReadResourceResult, error) {
	if s.session == nil {
		return nil, fmt.Errorf("mcp: server %q is not connected", s.name)
	}
	return s.session.ReadResource(ctx, params)
}

var _ agents.MCPServer = (*Server)(nil)
