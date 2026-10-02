package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// heldModel answers every call with a finished message, holding the first
// until release closes — long enough for a test to queue input on the run.
func heldModel(t *testing.T, release <-chan struct{}) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		send := sseWriter(w)
		sseCreated(send)
		send("response.completed", map[string]any{"type": "response.completed", "sequence_number": 1, "response": finishedResponse()})
	}))
}

// An input the run reads from its queue is announced where it was read: after
// the answer it followed, before the turn it starts, numbered within the run —
// the split a reload makes at the stored user entry (invariant 16).
func TestInjectedInputEmitsRunInjected(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	srv := heldModel(t, release)
	defer srv.Close()
	runner, sessions, _, agentConfigs := newTaskTestRunner(t)
	ac := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "a", Model: "gpt-test", ProviderID: testProvider(t, runner.db, "endpoint", "k", srv.URL)}
	if err := agentConfigs.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "chat"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	done := make(chan *RunOutcome, 1)
	runID, err := runner.StartRun(sess.ID, ac.ID, "", TextInput("deploy"), nil, func(o *RunOutcome) { done <- o })
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var events []*protocol.Envelope
	if _, ok := runner.hub.Subscribe(runID, 0, func(env *protocol.Envelope) {
		mu.Lock()
		events = append(events, env)
		mu.Unlock()
	}); !ok {
		t.Fatal("subscribe")
	}
	// The run takes input once its control is installed.
	deadline := time.Now().Add(5 * time.Second)
	for {
		delivered, ierr := runner.hub.Inject(runID, protocol.InjectQueueSteer, "use staging")
		if ierr != nil {
			t.Fatal(ierr)
		}
		if delivered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run never accepted input")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	select {
	case out := <-done:
		if out.ErrCode != "" || out.Interrupted {
			t.Fatalf("outcome = %+v, want a clean finish", out)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the run never finished")
	}

	mu.Lock()
	defer mu.Unlock()
	var order []string
	var injected []protocol.RunInjected
	for _, env := range events {
		switch env.Type {
		case protocol.EventRunMessage, protocol.EventRunOutput:
			order = append(order, env.Type)
		case protocol.EventRunInjected:
			order = append(order, env.Type)
			var p protocol.RunInjected
			if err := json.Unmarshal(env.Payload, &p); err != nil {
				t.Fatal(err)
			}
			injected = append(injected, p)
		}
	}
	want := []string{protocol.EventRunMessage, protocol.EventRunInjected, protocol.EventRunMessage, protocol.EventRunOutput}
	if len(order) != len(want) {
		t.Fatalf("events = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("events = %v, want %v", order, want)
		}
	}
	if len(injected) != 1 || injected[0].RunID != runID || injected[0].Input != "use staging" || injected[0].Index != 1 {
		t.Fatalf("run.injected = %+v, want the steer, numbered 1", injected)
	}
}

// stallingModel answers its first call once release closes — with first as
// its output, or a finished message when first is nil — and holds its second
// call, signalled on second, until the request ends.
func stallingModel(t *testing.T, release <-chan struct{}, second chan<- struct{}, first []any) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		send := sseWriter(w)
		sseCreated(send)
		resp := finishedResponse()
		switch n {
		case 1:
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			if first != nil {
				resp["output"] = first
			}
		case 2:
			second <- struct{}{}
			<-r.Context().Done()
			return
		}
		send("response.completed", map[string]any{"type": "response.completed", "sequence_number": 1, "response": resp})
	}))
}

// An input the run announced as read stays in the transcript when the run is
// then stopped — once, whether the SDK had written it (read at the final
// output) or taken it back with the failed attempt (read at a save point).
func TestInjectedInputSurvivesACancelledRun(t *testing.T) {
	cases := []struct {
		name  string
		first []any
	}{
		{"read at a save point", callOutput("call_1", "task_status", `{"task_id":"","wait_seconds":0}`)},
		{"read at the final output", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			release := make(chan struct{})
			second := make(chan struct{}, 1)
			srv := stallingModel(t, release, second, tc.first)
			defer srv.Close()
			runner, sessions, _, agentConfigs := newTaskTestRunner(t)
			ac := &store.AgentConfig{OwnerID: store.LocalUserID, Name: "a", Model: "gpt-test", ProviderID: testProvider(t, runner.db, "endpoint", "k", srv.URL)}
			if err := agentConfigs.Create(ctx, ac); err != nil {
				t.Fatal(err)
			}
			sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "chat"}
			if err := sessions.Create(ctx, sess); err != nil {
				t.Fatal(err)
			}
			done := make(chan *RunOutcome, 1)
			runID, err := runner.StartRun(sess.ID, ac.ID, "", TextInput("deploy"), nil, func(o *RunOutcome) { done <- o })
			if err != nil {
				t.Fatal(err)
			}
			injected := make(chan struct{}, 1)
			if _, ok := runner.hub.Subscribe(runID, 0, func(env *protocol.Envelope) {
				if env.Type == protocol.EventRunInjected {
					injected <- struct{}{}
				}
			}); !ok {
				t.Fatal("subscribe")
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				delivered, ierr := runner.hub.Inject(runID, protocol.InjectQueueSteer, "use staging")
				if ierr != nil {
					t.Fatal(ierr)
				}
				if delivered {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the run never accepted input")
				}
				time.Sleep(10 * time.Millisecond)
			}
			close(release)
			for _, wait := range []<-chan struct{}{injected, second} {
				select {
				case <-wait:
				case <-time.After(10 * time.Second):
					t.Fatal("the run never read the input and called the model again")
				}
			}
			runner.CancelRun(runID)
			select {
			case out := <-done:
				if !out.Cancelled {
					t.Fatalf("outcome = %+v, want a cancellation", out)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the run never ended")
			}

			entries, err := store.NewEntryStoreFor(runner.db, mustRef(t, runner.db, sess.ID)).Entries(ctx, session.Cursor{})
			if err != nil {
				t.Fatal(err)
			}
			var kept, marker int
			at := -1
			for i, e := range entries {
				if e.Kind == session.EntryKindItem && e.Source.Type == agents.SourceUser && strings.Contains(string(e.Item), "use staging") {
					kept++
					at = i
				}
				if e.Display != nil && e.Display.Kind == agents.DisplayCancelled {
					marker = i
				}
			}
			if kept != 1 {
				t.Fatalf("the transcript holds the read input %d times, want once", kept)
			}
			if at > marker {
				t.Fatalf("the input sits at %d, after the cancelled marker at %d", at, marker)
			}
		})
	}
}
