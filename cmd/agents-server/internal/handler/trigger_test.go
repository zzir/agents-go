package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
)

// fakeFirer records fires and syncs; a name in refuse makes a fire fail.
type fakeFirer struct {
	fires  []string // trigger id + "|" + payload
	synced []string
	refuse error
}

func (f *fakeFirer) Fire(_ context.Context, id, payload, _ string) (*bridge.Fired, error) {
	f.fires = append(f.fires, id+"|"+payload)
	if f.refuse != nil {
		return nil, f.refuse
	}
	return &bridge.Fired{TaskInfo: &bridge.TaskInfo{TaskID: "task-" + id, Status: "working"}}, nil
}

func (f *fakeFirer) Sync(_ context.Context, id string) { f.synced = append(f.synced, id) }

// triggerRig is a router with the trigger endpoints (API and hook) over a real
// store, one workflow and one session to name.
func triggerRig(t *testing.T) (*gin.Engine, *fakeFirer, *store.Workflow, *store.Session) {
	t.Helper()
	engine, firer, wf, sess, _ := triggerRigWithSessions(t)
	return engine, firer, wf, sess
}

// triggerRigWithSessions is triggerRig plus the session store, for a test
// that adds sessions of its own.
func triggerRigWithSessions(t *testing.T) (*gin.Engine, *fakeFirer, *store.Workflow, *store.Session, *store.SessionStore) {
	t.Helper()
	ctx := context.Background()
	db := testdb.New(t)
	agents := store.NewAgentConfigStore(db)
	ac := &store.AgentConfig{Name: "a", Model: "m", OwnerID: store.LocalUserID}
	if err := agents.Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	workflows := store.NewWorkflowStore(db)
	wf := &store.Workflow{Name: "nightly", Description: "d", OwnerID: store.LocalUserID, Steps: store.WorkflowSteps{{AgentConfigID: ac.ID, Prompt: "p"}}}
	if err := store.NormalizeWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	if err := workflows.Create(ctx, wf); err != nil {
		t.Fatal(err)
	}
	sessions := store.NewSessionStore(db)
	sess := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "chat"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	firer := &fakeFirer{}
	triggers := store.NewTriggerStore(db)
	h := NewTriggerHandler(triggers, sessions, store.NewWorkflowStore(db), store.NewAgentConfigStore(db), firer)
	engine := newTestEngine()
	api := engine.Group(server.APIPrefix)
	Handlers{Triggers: h, Authz: AuthzDeps{Sessions: sessions, Triggers: triggers}}.registerTriggers(api)
	engine.POST(server.HooksPrefix+"/:id", h.Hook)
	return engine, firer, wf, sess, sessions
}

func decodeView(t *testing.T, w *httptest.ResponseRecorder) TriggerView {
	t.Helper()
	var v TriggerView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding %s: %v", w.Body.String(), err)
	}
	return v
}

// A trigger names a workflow and a session that must exist, a cron one a
// schedule that parses; a webhook one is minted a secret shown ONCE.
func TestTriggerCreateValidatesAndMintsTheSecretOnce(t *testing.T) {
	engine, firer, wf, sess, sessions := triggerRigWithSessions(t)
	body := func(kind, schedule, wfID, sessID string) string {
		b, _ := json.Marshal(map[string]any{"workflow_id": wfID, "session_id": sessID, "kind": kind, "schedule": schedule, "brief": "go", "enabled": true})
		return string(b)
	}
	hidden := &store.Session{OwnerID: store.LocalUserID, ID: store.NewID(), Name: "task", Hidden: true}
	if err := sessions.Create(context.Background(), hidden); err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string]string{
		"unknown workflow": body("cron", "@hourly", "nope", sess.ID),
		"unknown session":  body("cron", "@hourly", wf.ID, "nope"),
		"hidden session":   body("cron", "@hourly", wf.ID, hidden.ID),
		"bad schedule":     body("cron", "every morning", wf.ID, sess.ID),
		"bad kind":         body("push", "", wf.ID, sess.ID),
	} {
		if w := doJSON(t, engine, "POST", server.APIPrefix+"/triggers", b); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400: %s", name, w.Code, w.Body.String())
		}
	}
	w := doJSON(t, engine, "POST", server.APIPrefix+"/triggers", body("webhook", "", wf.ID, sess.ID))
	if w.Code != http.StatusCreated {
		t.Fatalf("create webhook: %d %s", w.Code, w.Body.String())
	}
	created := decodeView(t, w)
	if len(created.Secret) != 64 || created.HookPath != server.HooksPrefix+"/"+created.ID || !strings.HasSuffix(created.Secret, strings.TrimPrefix(created.SecretHint, "…")) {
		t.Fatalf("created = %+v, want the minted secret, its hint and the hook path", created)
	}
	if len(firer.synced) != 1 || firer.synced[0] != created.ID {
		t.Fatalf("synced = %v, want the created trigger put on the clock", firer.synced)
	}
	// Read back, the secret is gone; only its tail is shown.
	got := decodeView(t, doJSON(t, engine, "GET", server.APIPrefix+"/triggers/"+created.ID, ""))
	if got.Secret != "" || got.SecretHint != created.SecretHint {
		t.Fatalf("read back = %+v, want no secret and the same hint", got)
	}
	// The raw JSON must not carry it under any name.
	if raw := doJSON(t, engine, "GET", server.APIPrefix+"/triggers", "").Body.String(); strings.Contains(raw, created.Secret) {
		t.Fatal("the listing leaks the secret")
	}
	// Rotation mints another, once — and the old one is dead.
	rotated := decodeView(t, doJSON(t, engine, "POST", server.APIPrefix+"/triggers/"+created.ID+"/rotate-secret", ""))
	if rotated.Secret == "" || rotated.Secret == created.Secret {
		t.Fatalf("rotated = %+v, want a new secret", rotated)
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, server.HooksPrefix+"/"+created.ID, strings.NewReader("x"))
	req.Header.Set(HookTimestampHeader, ts)
	req.Header.Set(HookSignatureHeader, SignHook(created.Secret, ts, []byte("x")))
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("hook with the rotated-away secret: %d, want 401", rec.Code)
	}
}

// The webhook is authenticated by signature over timestamp and body: right
// secret, fresh timestamp and matching body fire the trigger with the body as
// payload; anything else is 401 and fires nothing.
func TestHookVerifiesTheSignature(t *testing.T) {
	engine, firer, wf, sess := triggerRig(t)
	b, _ := json.Marshal(map[string]any{"workflow_id": wf.ID, "session_id": sess.ID, "kind": "webhook", "brief": "review the PR", "enabled": true})
	created := decodeView(t, doJSON(t, engine, "POST", server.APIPrefix+"/triggers", string(b)))
	call := func(ts, sig, body string) int {
		req := httptest.NewRequest(http.MethodPost, server.HooksPrefix+"/"+created.ID, strings.NewReader(body))
		if ts != "" {
			req.Header.Set(HookTimestampHeader, ts)
		}
		if sig != "" {
			req.Header.Set(HookSignatureHeader, sig)
		}
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec.Code
	}
	now := strconv.FormatInt(time.Now().Unix(), 10)
	payload := `{"pr":42}`
	if code := call(now, "sha256="+SignHook(created.Secret, now, []byte(payload)), payload); code != http.StatusCreated {
		t.Fatalf("a good call: %d, want 201", code)
	}
	if len(firer.fires) != 1 || firer.fires[0] != created.ID+"|"+payload {
		t.Fatalf("fires = %v, want the trigger fired with the body as payload", firer.fires)
	}
	// The very same delivery again is a replay: refused, nothing fired.
	if code := call(now, "sha256="+SignHook(created.Secret, now, []byte(payload)), payload); code != http.StatusConflict {
		t.Fatalf("a replayed call: %d, want 409", code)
	}
	// A delivery that did not fire (the session busy, say) is not one the
	// guard holds: the sender's resend of the very same request goes through
	// once the fire can.
	busy := payload + `{"n":"busy"}`
	firer.refuse = bridge.ErrSessionBusy{RunID: "r1"}
	if code := call(now, SignHook(created.Secret, now, []byte(busy)), busy); code != http.StatusConflict {
		t.Fatalf("a fire that found the session busy: %d, want 409", code)
	}
	firer.refuse = nil
	if code := call(now, SignHook(created.Secret, now, []byte(busy)), busy); code != http.StatusCreated {
		t.Fatalf("the same delivery resent after a failed fire: %d, want 201, not a replay refusal", code)
	}
	firer.fires = firer.fires[:1]
	// A different body at the same second is a new delivery.
	other := payload + `{"n":2}`
	if code := call(now, SignHook(created.Secret, now, []byte(other)), other); code != http.StatusCreated {
		t.Fatalf("a different delivery in the same second: %d, want 201", code)
	}
	if len(firer.fires) != 2 {
		t.Fatalf("fires = %v, want two", firer.fires)
	}
	stale := strconv.FormatInt(time.Now().Add(-HookTimestampSkew-time.Minute).Unix(), 10)
	for name, code := range map[string]int{
		"no headers":      call("", "", payload),
		"wrong secret":    call(now, SignHook("other", now, []byte(payload)), payload),
		"tampered body":   call(now, SignHook(created.Secret, now, []byte(payload)), payload+" "),
		"stale timestamp": call(stale, SignHook(created.Secret, stale, []byte(payload)), payload),
		"replayed ts":     call(now, SignHook(created.Secret, stale, []byte(payload)), payload),
	} {
		if code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, code)
		}
	}
	if len(firer.fires) != 2 {
		t.Fatalf("fires = %v — a refused call must fire nothing", firer.fires)
	}
	// An unknown trigger is 404, before any signature talk.
	if code := doJSON(t, engine, "POST", server.HooksPrefix+"/nope", "").Code; code != http.StatusNotFound {
		t.Fatalf("unknown trigger: %d, want 404", code)
	}
}

// A manual fire is the same fire, token-guarded, with an optional payload; a
// disabled trigger is a 400 with the reason.
func TestTriggerFireByHand(t *testing.T) {
	engine, firer, wf, sess := triggerRig(t)
	b, _ := json.Marshal(map[string]any{"workflow_id": wf.ID, "session_id": sess.ID, "kind": "cron", "schedule": "@daily", "brief": "go", "enabled": true})
	created := decodeView(t, doJSON(t, engine, "POST", server.APIPrefix+"/triggers", string(b)))
	if w := doJSON(t, engine, "POST", server.APIPrefix+"/triggers/"+created.ID+"/fire", `{"payload":"now please"}`); w.Code != http.StatusCreated {
		t.Fatalf("fire: %d %s", w.Code, w.Body.String())
	}
	if len(firer.fires) != 1 || firer.fires[0] != created.ID+"|now please" {
		t.Fatalf("fires = %v", firer.fires)
	}
	firer.refuse = bridge.ErrTriggerDisabled
	if w := doJSON(t, engine, "POST", server.APIPrefix+"/triggers/"+created.ID+"/fire", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("fire of a disabled trigger: %d, want 400", w.Code)
	}
	// Update keeps the kind; delete takes it off the clock.
	b, _ = json.Marshal(map[string]any{"workflow_id": wf.ID, "session_id": sess.ID, "kind": "webhook", "brief": "go", "enabled": false})
	if w := doJSON(t, engine, "PUT", server.APIPrefix+"/triggers/"+created.ID, string(b)); w.Code != http.StatusBadRequest {
		t.Fatalf("kind change: %d, want 400", w.Code)
	}
	if w := doJSON(t, engine, "DELETE", server.APIPrefix+"/triggers/"+created.ID, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}
	if last := firer.synced[len(firer.synced)-1]; last != created.ID {
		t.Fatalf("delete synced %q, want the trigger re-read (and found gone)", last)
	}
}
