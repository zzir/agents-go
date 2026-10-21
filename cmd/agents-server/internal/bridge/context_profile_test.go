package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/middleware"
	"github.com/zzir/agents-go/cmd/agents-server/internal/mcpservers"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

func profileDeps(t *testing.T) (*AgentDeps, *store.AgentConfig) {
	t.Helper()
	db := testdb.New(t)
	agentConfigs := store.NewAgentConfigStore(db)
	ac := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "a", Model: "gpt-test", Instructions: "Be brief."}
	if err := agentConfigs.Create(context.Background(), ac); err != nil {
		t.Fatal(err)
	}
	return &AgentDeps{
		AgentConfigs: agentConfigs, Providers: store.NewProviderStore(db),
		Settings: settings.NewReader(store.NewSettingStore(db)), Memories: store.NewMemoryStore(db),
	}, ac
}

// The preamble and submit_plan are sent only while the session is planning,
// so the profile the panel reads counts them only then. The build's own
// profile keeps both: the phase is the session's, read at run time.
func TestContextProfileCountsPlanOnlyWhilePlanning(t *testing.T) {
	deps, ac := profileDeps(t)
	built, err := buildFullAgent(context.Background(), deps, ac.ID, "", false, store.LocalUserID)
	if err != nil {
		t.Fatal(err)
	}
	hasPlanBucket := func(p store.PromptProfile) bool {
		return slices.ContainsFunc(p.Tools, func(b store.ToolBucket) bool { return b.Source == store.ToolSourcePlan })
	}
	want := len(strings.TrimSpace(middleware.DefaultPlanInstructions))
	if built.Profile.PlanPreambleChars != want || !hasPlanBucket(built.Profile) {
		t.Fatalf("build profile: preamble=%d (want %d), plan bucket=%v", built.Profile.PlanPreambleChars, want, hasPlanBucket(built.Profile))
	}
	if sent := built.SentProfile(); sent.PlanPreambleChars != want || !hasPlanBucket(sent) {
		t.Fatalf("while planning: preamble=%d, plan bucket=%v, want both counted", sent.PlanPreambleChars, hasPlanBucket(sent))
	}
	if err := built.PlanPhase.Unlock(); err != nil {
		t.Fatal(err)
	}
	if sent := built.SentProfile(); sent.PlanPreambleChars != 0 || hasPlanBucket(sent) {
		t.Fatalf("once executing: preamble=%d, plan bucket=%v, want neither", sent.PlanPreambleChars, hasPlanBucket(sent))
	}
	if built.Profile.PlanPreambleChars != want || !hasPlanBucket(built.Profile) {
		t.Fatal("SentProfile edited the build's own profile")
	}
}

// A background run is told it is one; the panel counts that suffix, and
// nothing of plan mode, which a background run never enters.
func TestContextProfileCountsTheBackgroundSuffix(t *testing.T) {
	deps, ac := profileDeps(t)
	bg, err := buildFullAgent(context.Background(), deps, ac.ID, "", true, store.LocalUserID)
	if err != nil {
		t.Fatal(err)
	}
	if bg.Profile.BackgroundChars != len(BackgroundInstructions) || bg.Profile.PlanPreambleChars != 0 {
		t.Fatalf("background profile: background=%d (want %d), preamble=%d (want 0)", bg.Profile.BackgroundChars, len(BackgroundInstructions), bg.Profile.PlanPreambleChars)
	}
	if sent := bg.SentProfile(); sent.BackgroundChars != len(BackgroundInstructions) {
		t.Fatalf("sent background profile dropped the suffix: %d", sent.BackgroundChars)
	}
	chat, err := buildFullAgent(context.Background(), deps, ac.ID, "", false, store.LocalUserID)
	if err != nil {
		t.Fatal(err)
	}
	if chat.Profile.BackgroundChars != 0 {
		t.Fatalf("a chat build counted a background suffix: %d", chat.Profile.BackgroundChars)
	}
}

// fakeMCP answers a listing without a connection.
type fakeMCP struct {
	name  string
	tools []*agents.Tool
	err   error
}

func (f fakeMCP) Name() string { return f.name }
func (f fakeMCP) Close() error { return nil }
func (f fakeMCP) ListTools(context.Context, *agents.RunContext, *agents.Agent) ([]*agents.Tool, error) {
	return f.tools, f.err
}

// Sizing is one bucket per server, after the local ones and in the agent's
// order; a server that cannot answer is unknown, never zero (invariant 93).
func TestMeasureMCPToolsBucketsEachServer(t *testing.T) {
	tool := &agents.Tool{Name: "search", Description: "find things", ParamsJSONSchema: map[string]any{"type": "object"}}
	agent := &agents.Agent{Name: "a", MCPServers: []agents.MCPServer{
		fakeMCP{name: "docs", tools: []*agents.Tool{tool, tool}},
		fakeMCP{name: "down", err: errors.New("not connected")},
	}}
	prof := store.PromptProfile{Tools: []store.ToolBucket{{Source: store.ToolSourceTasks, Count: 4}}}
	measureMCPTools(context.Background(), agent, &prof)
	want := []store.ToolBucket{
		{Source: store.ToolSourceTasks, Count: 4},
		{Source: store.ToolSourceMCP + "docs", Count: 2, Chars: 2 * store.ToolChars(tool)},
		{Source: store.ToolSourceMCP + "down", Unavailable: true},
	}
	if !slices.Equal(prof.Tools, want) {
		t.Fatalf("buckets = %+v, want %+v", prof.Tools, want)
	}
}

type noArgs struct{}

// A built agent's profile ends with its MCP servers' buckets, named by the
// server, so the Context panel reads the snapshot and asks no server
// (invariant 93).
func TestBuildFullAgentProfileCarriesMCPBuckets(t *testing.T) {
	ctx := context.Background()
	db := testdb.New(t)
	mcpSrv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "docs", Version: "1.0"}, nil)
	mcpsdk.AddTool(mcpSrv, &mcpsdk.Tool{Name: "search", Description: "find things"},
		func(context.Context, *mcpsdk.CallToolRequest, noArgs) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{}, nil, nil
		})
	endpoint := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return mcpSrv }, &mcpsdk.StreamableHTTPOptions{JSONResponse: true}))
	t.Cleanup(endpoint.Close)

	mcpStore := store.NewMcpServerStore(db)
	cfg := &store.McpServerConfig{Name: "docs", OwnerID: store.LocalUserID, Enabled: true,
		Config: json.RawMessage(`{"endpoint":"` + endpoint.URL + `"}`)}
	if err := mcpStore.Create(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	manager := mcpservers.NewManager(ctx)
	t.Cleanup(manager.CloseAll)
	if err := manager.Connect(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	agentConfigs := store.NewAgentConfigStore(db)
	ac := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "a", Model: "gpt-test", Tools: store.StringList{cfg.ID}}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	deps := &AgentDeps{
		AgentConfigs: agentConfigs, Providers: store.NewProviderStore(db), McpServers: mcpStore, McpManager: manager,
		Settings: settings.NewReader(store.NewSettingStore(db)), Memories: store.NewMemoryStore(db),
	}
	built, err := buildFullAgent(ctx, deps, ac.ID, "", false, store.LocalUserID)
	if err != nil {
		t.Fatal(err)
	}
	tools := built.Profile.Tools
	last := tools[len(tools)-1]
	if last.Source != store.ToolSourceMCP+"docs" || last.Count != 1 || last.Chars == 0 || last.Unavailable {
		t.Fatalf("last bucket = %+v, want mcp:docs with its one tool sized", last)
	}
	if first := slices.IndexFunc(tools, func(b store.ToolBucket) bool { return strings.HasPrefix(b.Source, store.ToolSourceMCP) }); first != len(tools)-1 {
		t.Fatalf("an mcp bucket before the local ones: %+v", tools)
	}
	if sent := built.SentProfile(); !slices.Contains(sent.Tools, last) {
		t.Fatalf("the sent profile dropped the mcp bucket: %+v", sent.Tools)
	}
}
