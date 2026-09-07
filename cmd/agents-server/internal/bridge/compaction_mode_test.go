package bridge

import (
	"strings"
	"testing"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// The mode is an enum refused at save, and a reset mode needs compaction on:
// without a pass there is nothing to reset with.
func TestDecodeAgentSpecCompactionMode(t *testing.T) {
	for _, mode := range []string{"", "summary", "reset", "hybrid"} {
		ac := &store.AgentConfig{Name: "a", Model: "m", Compaction: store.CompactionGroup{Enabled: true, Mode: mode}}
		if _, err := DecodeAgentSpec(ac); err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
	}
	if _, err := DecodeAgentSpec(&store.AgentConfig{Name: "a", Model: "m", Compaction: store.CompactionGroup{Enabled: true, Mode: "purge"}}); err == nil || !strings.Contains(err.Error(), "compaction_mode") {
		t.Fatalf("an unknown mode: %v", err)
	}
	if _, err := DecodeAgentSpec(&store.AgentConfig{Name: "a", Model: "m", Compaction: store.CompactionGroup{Mode: "reset"}}); err == nil || !strings.Contains(err.Error(), "compaction_enabled") {
		t.Fatalf("reset without compaction: %v", err)
	}
}

// A background run summarizes whatever its agent's mode says: it has no
// memory tools to write down what a reset would keep.
func TestBackgroundRunsNeverReset(t *testing.T) {
	built := &BuildResult{Compaction: store.CompactionGroup{Enabled: true, Mode: store.CompactionModeReset}}
	if got := compactionModeFor(built, false); got != store.CompactionModeReset {
		t.Fatalf("chat run mode = %q", got)
	}
	if got := compactionModeFor(built, true); got != store.CompactionModeSummary {
		t.Fatalf("background run mode = %q, want summary", got)
	}
}
