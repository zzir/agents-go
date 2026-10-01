package sandboxes

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents"
)

// CommandHash is exact over (cmd, workdir): identical args hash equal, any
// change hashes differently (so "approve this command" can't be widened).
func TestCommandHash(t *testing.T) {
	h1 := CommandHash(`{"cmd":"go test","workdir":"a"}`)
	h2 := CommandHash(`{"cmd":"go test","workdir":"a"}`)
	h3 := CommandHash(`{"cmd":"go test","workdir":"b"}`)
	h4 := CommandHash(`{"cmd":"go test ./...","workdir":"a"}`)
	if h1 != h2 {
		t.Fatal("identical args must hash equal")
	}
	if h1 == h3 || h1 == h4 {
		t.Fatal("different cmd/workdir must hash differently")
	}
}

// commandGate requires approval unless the session has trusted this exact
// command or all commands.
func TestCommandGate(t *testing.T) {
	m := NewManager()
	args := `{"cmd":"ls","workdir":""}`
	rc := &agents.RunContext{Context: "sess1"}

	// nil rc / no session context → require approval (fail safe).
	if need, _ := m.commandGate(context.Background(), nil, args, ""); !need {
		t.Fatal("nil rc → should require approval")
	}
	if need, _ := m.commandGate(context.Background(), &agents.RunContext{}, args, ""); !need {
		t.Fatal("no session → should require approval")
	}
	// Fresh session, untrusted → require approval.
	if need, _ := m.commandGate(context.Background(), rc, args, ""); !need {
		t.Fatal("untrusted → should require approval")
	}
	// Trust this exact command → no approval; a different command still requires it.
	m.Trust().ForSession("sess1").AllowCommand(CommandHash(args))
	if need, _ := m.commandGate(context.Background(), rc, args, ""); need {
		t.Fatal("trusted command → should NOT require approval")
	}
	if need, _ := m.commandGate(context.Background(), rc, `{"cmd":"rm -rf x"}`, ""); !need {
		t.Fatal("different command in trusted session → should still require approval")
	}
	// AllowAll → nothing in that session requires approval.
	m.Trust().ForSession("sess2").AllowAll()
	rc2 := &agents.RunContext{Context: "sess2"}
	if need, _ := m.commandGate(context.Background(), rc2, `{"cmd":"anything"}`, ""); need {
		t.Fatal("approveAll → should NOT require approval")
	}
}

// Forget drops a session's grants: the next command re-asks, and the entry is
// actually gone from the map rather than emptied.
func TestTrustStoreForget(t *testing.T) {
	s := NewTrustStore()
	s.ForSession("sess1").AllowAll()
	s.Forget("sess1")
	if s.ForSession("sess1").trusted(CommandHash(`{"cmd":"ls"}`)) {
		t.Fatal("a forgotten session's trust survived")
	}
	s.Forget("sess1") // ForSession above re-created it; drop it again
	s.mu.Lock()
	n := len(s.bySession)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("bySession holds %d entries after Forget, want 0", n)
	}
}

// A run marked as withheld asks for every command, whatever its session
// granted (invariant 84); the same session's unmarked run still uses the grant.
func TestCommandGateWithheldIgnoresTrust(t *testing.T) {
	m := NewManager()
	args := `{"cmd":"ls","workdir":""}`
	rc := &agents.RunContext{Context: "sess1"}
	m.Trust().ForSession("sess1").AllowAll()

	withheld := WithoutStandingTrust(context.Background())
	if need, _ := m.commandGate(withheld, rc, args, ""); !need {
		t.Fatal("a withheld run used the session's standing trust")
	}
	// A context derived from it is still withheld: the mark rides the run.
	derived, cancel := context.WithCancel(withheld)
	defer cancel()
	if need, _ := m.commandGate(derived, rc, args, ""); !need {
		t.Fatal("a context derived from a withheld run lost the mark")
	}
	if need, _ := m.commandGate(context.Background(), rc, args, ""); need {
		t.Fatal("an unmarked run on the same session should still use the grant")
	}
}

// The withheld-run record answers by run id and goes with its session.
func TestTrustStoreWithheldRuns(t *testing.T) {
	s := NewTrustStore()
	s.WithholdRun("sess1", "run-a")
	s.WithholdRun("sess2", "run-b")
	if !s.RunWithheld("run-a") || !s.RunWithheld("run-b") {
		t.Fatal("a recorded run is not reported withheld")
	}
	if s.RunWithheld("run-c") || s.RunWithheld("") {
		t.Fatal("an unrecorded run is reported withheld")
	}
	s.Forget("sess1")
	if s.RunWithheld("run-a") {
		t.Fatal("a forgotten session's withheld run survived")
	}
	if !s.RunWithheld("run-b") {
		t.Fatal("forgetting one session dropped another's withheld run")
	}
}
