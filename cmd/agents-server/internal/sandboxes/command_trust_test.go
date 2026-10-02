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

// A run on its own grants reads those and not its session's (invariant 84): it
// asks despite the session's "all", and stops asking only for what its own
// cards granted. The same session's other runs still read the session's.
func TestCommandGateReadsTheRunsOwnTrust(t *testing.T) {
	m := NewManager()
	ls, rm := `{"cmd":"ls","workdir":""}`, `{"cmd":"rm -rf x","workdir":""}`
	rc := &agents.RunContext{Context: "sess1"}
	m.Trust().ForSession("sess1").AllowAll()

	own := m.Trust().WithholdRun("sess1", "run-a")
	ctx, cancel := context.WithCancel(WithRunTrust(context.Background(), own))
	defer cancel()
	if need, _ := m.commandGate(ctx, rc, ls, ""); !need {
		t.Fatal("a run on its own grants used the session's standing trust")
	}
	own.AllowCommand(CommandHash(ls))
	if need, _ := m.commandGate(ctx, rc, ls, ""); need {
		t.Fatal("the command this run's card trusted still asks")
	}
	if need, _ := m.commandGate(ctx, rc, rm, ""); !need {
		t.Fatal("trusting one command let a different one through")
	}
	own.AllowAll()
	if need, _ := m.commandGate(ctx, rc, rm, ""); need {
		t.Fatal("the run's own \"all\" still asks")
	}
	if need, _ := m.commandGate(context.Background(), rc, rm, ""); need {
		t.Fatal("a run of the same session without its own trust should read the session's")
	}
	// The run's grants are its own: another session's ordinary run sees none of them.
	if need, _ := m.commandGate(context.Background(), &agents.RunContext{Context: "sess2"}, rm, ""); !need {
		t.Fatal("a run's own grant leaked to another session")
	}
}

// A withheld run's trust is kept by run id — a resume finds the same grants —
// and goes with its session.
func TestTrustStoreWithheldRuns(t *testing.T) {
	s := NewTrustStore()
	a := s.WithholdRun("sess1", "run-a")
	s.WithholdRun("sess2", "run-b")
	if a == nil || s.WithholdRun("sess1", "run-a") != a || s.RunTrust("run-a") != a {
		t.Fatal("a second look at a withheld run did not return the same trust")
	}
	if s.RunTrust("run-c") != nil || s.RunTrust("") != nil {
		t.Fatal("a run never withheld has a trust of its own")
	}
	s.Forget("sess1")
	if s.RunTrust("run-a") != nil {
		t.Fatal("a forgotten session's withheld run survived")
	}
	if s.RunTrust("run-b") == nil {
		t.Fatal("forgetting one session dropped another's withheld run")
	}
}
