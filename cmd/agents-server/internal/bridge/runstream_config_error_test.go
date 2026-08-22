package bridge

import (
	"context"
	"testing"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// A run that fails the pre-flight config check (no API key configured for the
// agent) must still persist the user's prompt and the error. The web client
// reloads the session on run.error, so without this the optimistic message and
// the error card would be wiped to an empty session — the prompt would appear
// lost and the failure would never be shown. Mirrors the post-start error path.
func TestRunStreamed_NoAPIKeyPersistsPromptAndError(t *testing.T) {
	runner, db := newBareRunner(t)
	ctx := context.Background()

	// A valid agent config with a model but no API key: BuildFullAgent succeeds
	// yet leaves Provider nil (the Settings store carries no openai_api_key
	// fallback either), so runStreamed hits the "no API key" branch.
	cfgID := mkAgent(t, store.NewAgentConfigStore(db), "keyless")

	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "t", AgentConfigID: cfgID}
	if err := runner.Deps.Sessions.Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	res := runner.runStreamed(ctx, store.NewID(), sess.ID, cfgID, "", "", "hello", "")
	if res == nil {
		t.Fatal("runStreamed returned nil result")
	}

	// The prompt survives as a replayable user item...
	if got := countDisplays(t, db, sess.ID, session.EntryKindItem, ""); got != 1 {
		t.Errorf("user item entries = %d, want 1 (prompt must survive the config failure)", got)
	}
	// ...alongside an error annotation, so the client's reload renders the failed
	// turn instead of an empty session.
	if got := countDisplays(t, db, sess.ID, session.EntryKindAnnotation, agents.DisplayError); got != 1 {
		t.Errorf("error annotation entries = %d, want 1", got)
	}
}

// A session lookup that FAILS is not a session that is absent. Only "no such
// row" is session_not_found; a store that cannot answer is a server-side
// failure, and labelling it session_not_found tells the client to give up on a
// session that is still there. Same split the REST path applies to StartRun's
// own lookup (handler.startError).
func TestRunStreamed_ClassifiesTheSessionLookup(t *testing.T) {
	t.Run("absent session", func(t *testing.T) {
		runner, _ := newBareRunner(t)
		res := runner.runStreamed(context.Background(), store.NewID(), store.NewID(), "", "", "", "hello", "")
		if res.ErrCode != protocol.CodeSessionNotFound {
			t.Errorf("err code = %q, want %q", res.ErrCode, protocol.CodeSessionNotFound)
		}
	})

	t.Run("unreachable store", func(t *testing.T) {
		runner, db := newBareRunner(t)
		sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "t"}
		if err := runner.Deps.Sessions.Create(context.Background(), sess); err != nil {
			t.Fatalf("create session: %v", err)
		}
		// The session exists; the store just cannot be asked about it.
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}

		res := runner.runStreamed(context.Background(), store.NewID(), sess.ID, "", "", "", "hello", "")
		if res.ErrCode == protocol.CodeSessionNotFound {
			t.Fatal("a dead database reported as session_not_found")
		}
		if res.ErrCode != protocol.CodeConfigError {
			t.Errorf("err code = %q, want %q", res.ErrCode, protocol.CodeConfigError)
		}
	})

	// A shutdown, or a stop pressed the instant the run starts, cancels the run
	// context while this lookup is in flight. That is the run being cancelled,
	// not the lookup failing: it must end like every other cancel — run.cancelled
	// (so the task bookkeeping records cancelled, not failed) and the prompt kept
	// under a cancelled marker, not a red config_error the user never caused.
	t.Run("cancelled run", func(t *testing.T) {
		runner, db := newBareRunner(t)
		sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "t"}
		if err := runner.Deps.Sessions.Create(context.Background(), sess); err != nil {
			t.Fatalf("create session: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		res := runner.runStreamed(ctx, store.NewID(), sess.ID, "", "", "", "hello", "")
		if !res.Cancelled {
			t.Errorf("Cancelled = false, want true (err code %q: %s)", res.ErrCode, res.ErrMessage)
		}
		if res.ErrCode != "" {
			t.Errorf("err code = %q, want empty (a cancelled run carries no error)", res.ErrCode)
		}
		if got := countDisplays(t, db, sess.ID, session.EntryKindAnnotation, agents.DisplayCancelled); got != 1 {
			t.Errorf("cancelled annotations = %d, want 1", got)
		}
		if got := countDisplays(t, db, sess.ID, session.EntryKindItem, ""); got != 1 {
			t.Errorf("user item entries = %d, want 1 (the prompt must survive the cancel)", got)
		}
	})
}
