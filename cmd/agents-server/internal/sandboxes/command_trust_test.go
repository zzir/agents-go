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
