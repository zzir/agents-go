package bridge

import (
	"errors"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// neverProvider is a ModelProvider the segment never reaches: the session
// read ahead of it refuses first.
type neverProvider struct{}

func (neverProvider) Model(string) (agents.Model, error) { return nil, errors.New("unreachable") }

// A run the hub registered still fails on its own session read when the row
// is gone — fresh through Sessions.Get, resumed through RefFor. This read is
// what makes lifting the deleting mark after a committed delete safe: a run
// that slipped past the mark writes nothing.
func TestExecStreamedRefusesASessionGoneAfterRegister(t *testing.T) {
	runner, _, _, _ := newTaskTestRunner(t)
	cases := []struct {
		name string
		spec segmentSpec
	}{
		{"fresh", segmentSpec{fresh: true, failCode: protocol.CodeStreamError}},
		{"resume", segmentSpec{failCode: protocol.CodeResumeError, built: &BuildResult{
			Agent: &agents.Agent{Name: "a", Model: "m"}, Provider: neverProvider{},
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID, sessID := store.NewID(), store.NewID() // never created: as if deleted after register
			seg, ctx, err := runner.hub.register(runID, sessID, "", "agent", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer seg.finalize()
			out := runner.execStreamed(ctx, runID, sessID, "agent", "", tc.spec)
			if out.ErrCode != protocol.CodeSessionNotFound {
				t.Fatalf("outcome = %+v, want session_not_found", out)
			}
		})
	}
}
