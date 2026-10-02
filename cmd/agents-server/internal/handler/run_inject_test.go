package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// liveRun starts a run of a slow model on a fresh session of memberUser's and
// returns its ids and a channel closed when it ends.
func liveRun(t *testing.T, r rig, delay time.Duration) (sessionID, runID string, done <-chan struct{}) {
	t.Helper()
	ctx := context.Background()
	srv := slowModel(t, delay)
	t.Cleanup(srv.Close)
	pv := &store.Provider{Name: "endpoint-" + store.NewID(), APIKey: "k", BaseURL: srv.URL, OwnerID: memberUser.ID}
	if err := store.NewProviderStore(r.db).Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	ac := &store.AgentConfig{Name: "a-" + store.NewID(), Model: "gpt-test", ProviderID: pv.ID, OwnerID: memberUser.ID}
	if err := store.NewAgentConfigStore(r.db).Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{OwnerID: memberUser.ID, ID: store.NewID(), Name: "s"}
	if err := r.sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	id, err := r.runner.StartRun(sess.ID, ac.ID, "", bridge.TextInput("hi"), nil, func(*bridge.RunOutcome) { close(ended) })
	if err != nil {
		t.Fatal(err)
	}
	return sess.ID, id, ended
}

func inject(r rig, runID, body string) (int, string) {
	rec := serve(r.engine, as(memberUser, http.MethodPost, "/api/v1/runs/"+runID+"/inject", body))
	return rec.Code, rec.Body.String()
}

// POST /runs/:id/inject is run.inject's REST twin: a steer queued on the live
// run is read by it — the run takes another turn — and lands in the session
// as the user's message.
func TestInjectDeliversToTheLiveRun(t *testing.T) {
	r := statusRig(t)
	sessionID, runID, done := liveRun(t, r, 700*time.Millisecond)

	// The run takes input once its control is installed; until then it says so.
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, body := inject(r, runID, `{"queue":"steer","input":"use staging instead"}`)
		if code == http.StatusAccepted {
			var resp injectResp
			if err := json.Unmarshal([]byte(body), &resp); err != nil || resp.RunID != runID || resp.Queue != "steer" {
				t.Fatalf("202 body = %s (%v), want the run and the queue", body, err)
			}
			break
		}
		if code != http.StatusConflict || !strings.Contains(body, "starting") {
			t.Fatalf("inject while starting = %d %s, want 409 naming the start", code, body)
		}
		if time.Now().After(deadline) {
			t.Fatal("the run never accepted input")
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the run never finished")
	}
	rec := serve(r.engine, as(memberUser, http.MethodGet, "/api/v1/sessions/"+sessionID+"/messages", ""))
	var entries []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatal(err)
	}
	var users []string
	answers := 0
	for _, e := range entries {
		switch e.Role {
		case "user":
			users = append(users, e.Content)
		case "assistant":
			answers++
		}
	}
	if len(users) != 2 || users[1] != "use staging instead" || answers != 2 {
		t.Fatalf("session = users %q, %d answers; want the steer after the prompt and an answer to each", users, answers)
	}
}

// A run that has ended takes nothing, and says it has ended.
func TestInjectRefusesAnEndedRun(t *testing.T) {
	r := statusRig(t)
	_, runID, done := liveRun(t, r, 10*time.Millisecond)
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the run never finished")
	}
	code, body := inject(r, runID, `{"queue":"follow_up","input":"and then?"}`)
	if code != http.StatusConflict || !strings.Contains(body, "the run has ended") {
		t.Fatalf("inject after the end = %d %s, want 409 saying it ended", code, body)
	}
}

// The body is checked before the run is asked: an unknown queue or an empty
// input is the caller's mistake whatever state the run is in.
func TestInjectRejectsAnUnknownQueue(t *testing.T) {
	r := statusRig(t)
	_, runID, done := liveRun(t, r, 10*time.Millisecond)
	<-done
	for _, body := range []string{
		`{"queue":"urgent","input":"x"}`,
		`{"queue":"","input":"x"}`,
		`{"queue":"steer","input":"  "}`,
		`{"queue":"steer"}`,
	} {
		if code, out := inject(r, runID, body); code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", body, code, out)
		}
	}
}

// One wording for both transports: the refusal names where the run stands.
func TestInjectRefusalNamesTheState(t *testing.T) {
	for status, want := range map[bridge.RunStatus]string{
		bridge.RunInterrupted: "paused for approval",
		bridge.RunRunning:     "starting",
		bridge.RunCompleted:   "has ended",
		bridge.RunErrored:     "has ended",
		bridge.RunCancelled:   "has ended",
		"":                    "has ended",
	} {
		if got := injectRefusal(status); !strings.Contains(got, want) {
			t.Errorf("%q: %q, want it to say %q", status, got, want)
		}
	}
}
