package bridge

import (
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// Buckets are positional: each build step owns exactly what it appended, so a
// tool is never attributed to the step before it (the reason this is not a name
// list to keep in step with every tool the bridge attaches).
func TestBucketToolsSinceAttributesOnlyWhatWasAdded(t *testing.T) {
	agent := &agents.Agent{}
	prof := store.PromptProfile{}

	agent.Tools = append(agent.Tools, &agents.Tool{Name: "exec_command"}, &agents.Tool{Name: "read_file"})
	mark := bucketToolsSince(agent, 0, store.ToolSourceSandbox, &prof)
	agent.Tools = append(agent.Tools, &agents.Tool{Name: "read_skill"})
	mark = bucketToolsSince(agent, mark, store.ToolSourceSkills, &prof)

	// A step that added nothing gets no bucket at all — an empty row would read
	// as "the source is attached and costs nothing".
	mark = bucketToolsSince(agent, mark, store.ToolSourceTasks, &prof)
	if mark != len(agent.Tools) {
		t.Fatalf("mark should track the tool count, got %d of %d", mark, len(agent.Tools))
	}

	if len(prof.Tools) != 2 {
		t.Fatalf("want 2 buckets (sandbox, skills), got %+v", prof.Tools)
	}
	if prof.Tools[0].Source != store.ToolSourceSandbox || prof.Tools[0].Count != 2 {
		t.Fatalf("sandbox bucket wrong: %+v", prof.Tools[0])
	}
	if prof.Tools[1].Source != store.ToolSourceSkills || prof.Tools[1].Count != 1 {
		t.Fatalf("skills bucket wrong: %+v", prof.Tools[1])
	}
	if prof.Tools[0].Chars == 0 || prof.Tools[1].Chars == 0 {
		t.Fatalf("buckets must carry a size: %+v", prof.Tools)
	}
}
