package bridge

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents/middleware"
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
