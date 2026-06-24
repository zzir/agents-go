// Package mcp provides a Model Context Protocol (MCP) client that exposes a
// server's tools to an agent. It implements agents.MCPServer over the official
// modelcontextprotocol/go-sdk, supporting stdio and streamable HTTP transports
// (plus the deprecated SSE transport).
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"

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
	// multi-turn run does not re-issue list_tools every turn. Call
	// InvalidateToolsCache when the server's tools may have changed. Static and
	// dynamic filters still run on every ListTools, against the cached list.
	CacheToolsList bool

	// ToolFilter, when set, decides per call whether a tool is exposed, applied
	// after the static AllowedTools/BlockedTools lists. It receives the original
	// (unprefixed) tool name and may consult the run context.
	ToolFilter func(ctx context.Context, rc *agents.RunContext, agent *agents.Agent, toolName string) bool

	// ToolNamePrefix is prepended to every exposed tool name (e.g. "github_") to
	// avoid collisions when multiple servers expose same-named tools. The server
	// is still called with the original name.
	ToolNamePrefix string

	// RequireApproval, when set, marks an exposed MCP tool as needing human
	// approval (HITL) whenever it returns true for the tool's original name.
	RequireApproval func(toolName string) bool

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
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: name, Version: version}, nil)
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
		tools = append(tools, ct.tool)
	}
	return tools, nil
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
	res, err := s.session.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: listing tools for %q: %w", s.name, err)
	}
	list := make([]cachedTool, 0, len(res.Tools))
	for _, mt := range res.Tools {
		list = append(list, cachedTool{originalName: mt.Name, tool: s.toolFor(mt)})
	}
	if s.opts.CacheToolsList {
		s.cached = list
	}
	return list, nil
}

// InvalidateToolsCache drops any cached tool list so the next ListTools refetches
// it. No-op when CacheToolsList is not set.
func (s *Server) InvalidateToolsCache() {
	s.mu.Lock()
	s.cached = nil
	s.mu.Unlock()
}

func (s *Server) toolFor(mt *mcpsdk.Tool) agents.Tool {
	schema := schemaToMap(mt.InputSchema)
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
	exposedName := s.opts.ToolNamePrefix + originalName
	tool := &agents.FunctionTool{
		Name:             exposedName,
		Description:      mt.Description,
		ParamsJSONSchema: schema,
		Strict:           strict,
		// Tool failures (including isError results) are fed back to the model
		// so it can recover, matching the SDK-wide default; without this every
		// MCP error would abort the whole run.
		FailureErrorFunction: agents.DefaultToolErrorFunction,
		OnInvoke: func(ctx context.Context, _ *agents.ToolContext, argsJSON string) (any, error) {
			var args map[string]any
			if strings.TrimSpace(argsJSON) != "" {
				if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
					return nil, fmt.Errorf("mcp tool %q: invalid arguments: %w", exposedName, err)
				}
			}
			// The server is always called with the original (unprefixed) name.
			result, err := s.session.CallTool(ctx, &mcpsdk.CallToolParams{Name: originalName, Arguments: args})
			if err != nil {
				return nil, fmt.Errorf("mcp tool %q call failed: %w", originalName, err)
			}
			if result.IsError {
				return nil, fmt.Errorf("mcp tool %q returned error: %s", originalName, resultText(result))
			}
			return resultOutput(result), nil
		},
	}
	if s.opts.RequireApproval != nil && s.opts.RequireApproval(originalName) {
		tool.NeedsApproval = true
	}
	return tool
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

// resultText renders a tool result for the model: structured content wins,
// a single text block passes through verbatim, and anything else (multiple or
// non-text blocks) is JSON-encoded so no information is silently dropped.
func resultText(result *mcpsdk.CallToolResult) string {
	if result.StructuredContent != nil {
		if b, err := json.Marshal(result.StructuredContent); err == nil {
			return string(b)
		}
	}
	if len(result.Content) == 0 {
		return ""
	}
	if len(result.Content) == 1 {
		if tc, ok := result.Content[0].(*mcpsdk.TextContent); ok {
			return tc.Text
		}
	}
	if b, err := json.Marshal(result.Content); err == nil {
		return string(b)
	}
	var b strings.Builder
	for _, c := range result.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// resultOutput renders a tool result for the model. When the result carries any
// image content (an image block, or an embedded resource with an image MIME
// type) it is returned as structured multimodal content so the model receives
// native image input; every block is mapped — text stays text, images become
// images, and anything else is JSON-encoded into a text part so nothing is
// silently dropped. Otherwise it falls back to the plain-text rendering.
func resultOutput(result *mcpsdk.CallToolResult) any {
	if !hasImageContent(result.Content) {
		return resultText(result)
	}
	var parts []agents.ToolOutputContent
	if result.StructuredContent != nil {
		if b, err := json.Marshal(result.StructuredContent); err == nil {
			parts = append(parts, agents.ToolOutputText{Text: string(b)})
		}
	}
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

// hasImageContent reports whether any block is image content the model can take
// as native input.
func hasImageContent(content []mcpsdk.Content) bool {
	for _, c := range content {
		switch v := c.(type) {
		case *mcpsdk.ImageContent:
			return true
		case *mcpsdk.EmbeddedResource:
			if v.Resource != nil && isImageMIME(v.Resource.MIMEType) && len(v.Resource.Blob) > 0 {
				return true
			}
		}
	}
	return false
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
