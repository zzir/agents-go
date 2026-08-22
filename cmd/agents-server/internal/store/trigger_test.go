package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
)

func TestNormalizeTrigger(t *testing.T) {
	ok := &Trigger{WorkflowID: " w ", SessionID: " s ", Kind: "cron", Schedule: " @hourly ", Brief: " go "}
	if err := NormalizeTrigger(ok); err != nil {
		t.Fatal(err)
	}
	if ok.WorkflowID != "w" || ok.SessionID != "s" || ok.Schedule != "@hourly" || ok.Brief != "go" {
		t.Fatalf("not trimmed: %+v", ok)
	}
	// A webhook carries no schedule, whatever was sent.
	hook := &Trigger{WorkflowID: "w", SessionID: "s", Kind: "webhook", Schedule: "@hourly", Brief: "go"}
	if err := NormalizeTrigger(hook); err != nil || hook.Schedule != "" {
		t.Fatalf("webhook: err=%v schedule=%q, want accepted with no schedule", err, hook.Schedule)
	}
	// The target is read off the id given when unsaid — a workflow_id alone
	// still means a workflow, as before targets — and the other id is
	// cleared, so a row never names both.
	if ok.Target != TriggerTargetWorkflow {
		t.Fatalf("target = %q, want workflow inferred from workflow_id", ok.Target)
	}
	agent := &Trigger{AgentConfigID: " a ", WorkflowID: "w", Target: "agent", SessionID: "s", Kind: "webhook", Brief: "ask"}
	if err := NormalizeTrigger(agent); err != nil || agent.AgentConfigID != "a" || agent.WorkflowID != "" {
		t.Fatalf("agent target: err=%v agent=%q workflow=%q, want the agent kept and the workflow cleared", err, agent.AgentConfigID, agent.WorkflowID)
	}
	inferred := &Trigger{AgentConfigID: "a", SessionID: "s", Kind: "webhook", Brief: "ask"}
	if err := NormalizeTrigger(inferred); err != nil || inferred.Target != TriggerTargetAgent {
		t.Fatalf("agent inferred: err=%v target=%q", err, inferred.Target)
	}
	for name, bad := range map[string]*Trigger{
		"no target at all":  {SessionID: "s", Kind: "cron", Schedule: "@hourly"},
		"workflow w/o id":   {Target: "workflow", AgentConfigID: "a", SessionID: "s", Kind: "cron", Schedule: "@hourly"},
		"agent w/o id":      {Target: "agent", WorkflowID: "w", SessionID: "s", Kind: "cron", Schedule: "@hourly"},
		"unknown target":    {Target: "email", WorkflowID: "w", SessionID: "s", Kind: "cron", Schedule: "@hourly"},
		"no session":        {WorkflowID: "w", Kind: "cron", Schedule: "@hourly"},
		"cron w/o schedule": {WorkflowID: "w", SessionID: "s", Kind: "cron"},
		"unknown kind":      {WorkflowID: "w", SessionID: "s", Kind: "push"},
	} {
		if err := NormalizeTrigger(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// An agent target is checked like a workflow one: the agent must exist when
// the row is written, in the same transaction.
func TestTriggerAgentTargetIsChecked(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	_, sess := seedRefs(t, db)
	triggers := NewTriggerStore(db)
	ghost := &Trigger{Target: TriggerTargetAgent, AgentConfigID: NewID(), SessionID: sess.ID, Kind: TriggerKindWebhook, Brief: "ask", Enabled: true}
	if err := triggers.Create(ctx, ghost); !errors.Is(err, ErrTriggerRef) {
		t.Fatalf("create with a ghost agent: %v, want ErrTriggerRef", err)
	}
	ac := &AgentConfig{Name: "reviewer", Model: "m"}
	if err := NewAgentConfigStore(db).Create(ctx, ac); err != nil {
		t.Fatal(err)
	}
	live := &Trigger{Target: TriggerTargetAgent, AgentConfigID: ac.ID, SessionID: sess.ID, Kind: TriggerKindWebhook, Brief: "ask", Enabled: true}
	if err := triggers.Create(ctx, live); err != nil {
		t.Fatalf("create with a real agent: %v", err)
	}
	got, err := triggers.Get(ctx, live.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Target != TriggerTargetAgent || got.AgentConfigID != ac.ID || got.WorkflowID != "" {
		t.Fatalf("round trip: %+v", got)
	}
}

// seedRefs creates a workflow and a session a trigger may name.
func seedRefs(t *testing.T, db *bun.DB) (*Workflow, *Session) {
	t.Helper()
	ctx := context.Background()
	wf := &Workflow{Name: NewID()[:8], Description: "d", Steps: WorkflowSteps{{ID: "a", AgentConfigID: "x", Prompt: "p"}}}
	if err := NewWorkflowStore(db).Create(ctx, wf); err != nil {
		t.Fatal(err)
	}
	sess := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "s"}
	if err := NewSessionStore(db).Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	return wf, sess
}

// The secret is a column the API never serializes, written by SetSecret alone;
// the fire record by RecordFire alone; the settings by UpdateSettings alone.
func TestTriggerStoreRoundTripAndFireRecord(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	s := NewTriggerStore(db)
	wf, sess := seedRefs(t, db)
	// A trigger names a workflow and a session that exist — checked in the
	// insert's own transaction, so a delete cannot slip between check and write.
	if err := s.Create(ctx, &Trigger{WorkflowID: NewID(), SessionID: sess.ID, Kind: TriggerKindWebhook, Brief: "b"}); !errors.Is(err, ErrTriggerRef) {
		t.Fatalf("create naming no workflow = %v, want ErrTriggerRef", err)
	}
	trg := &Trigger{WorkflowID: wf.ID, SessionID: sess.ID, Kind: TriggerKindWebhook, Brief: "b", Secret: NewTriggerSecret(), Enabled: true}
	if err := s.Create(ctx, trg); err != nil {
		t.Fatal(err)
	}
	// Settings writes touch neither the secret nor the fire record; the secret
	// has its own writer.
	if err := s.UpdateSettings(ctx, trg.ID, &Trigger{WorkflowID: wf.ID, SessionID: sess.ID, Kind: TriggerKindWebhook, Brief: "changed", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSettings(ctx, trg.ID, &Trigger{WorkflowID: NewID(), SessionID: sess.ID, Brief: "x"}); !errors.Is(err, ErrTriggerRef) {
		t.Fatalf("update naming no workflow = %v, want ErrTriggerRef", err)
	}
	got, err := s.Get(ctx, trg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Secret != trg.Secret || len(got.Secret) != 64 || got.Brief != "changed" || got.Enabled {
		t.Fatalf("after settings update: %+v — want the secret kept, the settings written", got)
	}
	if err := s.SetSecret(ctx, trg.ID, "new-secret"); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Get(ctx, trg.ID); got.Secret != "new-secret" || got.Brief != "changed" {
		t.Fatalf("after rotation: %+v — want only the secret changed", got)
	}
	// The fire record and the settings update both write uuid columns by raw
	// SET: an empty id must land as NULL, not "" (a syntax error on PostgreSQL).
	started := NewID()
	if err := s.RecordFire(ctx, trg.ID, started, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, trg.ID)
	if got.LastStartedID != started || got.LastError != "" || time.Since(got.LastFiredAt) > time.Minute {
		t.Fatalf("fire record = %+v", got)
	}
	if err := s.RecordFire(ctx, trg.ID, "", "session at its cap"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, trg.ID)
	if got.LastStartedID != "" || got.LastError != "session at its cap" {
		t.Fatalf("second fire record = %+v, want the failure and no task", got)
	}
	byWf, err := s.ListByWorkflow(ctx, wf.ID)
	if err != nil || len(byWf) != 1 {
		t.Fatalf("ListByWorkflow = %v, %v", byWf, err)
	}
}

// A trigger goes with the workflow it fires and with the session it fires
// into: neither could be anything but a failing fire without them.
func TestTriggersAreDeletedWithTheirWorkflowAndSession(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	workflows, sessions, triggers := NewWorkflowStore(db), NewSessionStore(db), NewTriggerStore(db)
	wf := &Workflow{Name: "w", Description: "d", Steps: WorkflowSteps{{ID: "a", AgentConfigID: "x", Prompt: "p"}}}
	if err := workflows.Create(ctx, wf); err != nil {
		t.Fatal(err)
	}
	sess := &Session{OwnerID: LocalUserID, ID: NewID(), Name: "s"}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	otherWf, otherSess := seedRefs(t, db)
	mk := func(wfID, sessID string) string {
		trg := &Trigger{WorkflowID: wfID, SessionID: sessID, Kind: TriggerKindCron, Schedule: "@daily", Brief: "b"}
		if err := triggers.Create(ctx, trg); err != nil {
			t.Fatal(err)
		}
		return trg.ID
	}
	ofWorkflow := mk(wf.ID, otherSess.ID)
	ofSession := mk(otherWf.ID, sess.ID)
	kept := mk(otherWf.ID, otherSess.ID)

	if err := workflows.Delete(ctx, wf.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := triggers.Get(ctx, ofWorkflow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the workflow's trigger survived its workflow: %v", err)
	}
	if err := sessions.Delete(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := triggers.Get(ctx, ofSession); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the session's trigger survived its session: %v", err)
	}
	if _, err := triggers.Get(ctx, kept); err != nil {
		t.Fatalf("an unrelated trigger was deleted: %v", err)
	}
	// Deleting a workflow that is not there is still a not-found.
	if err := workflows.Delete(ctx, NewID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete of an unknown workflow = %v, want ErrNotFound", err)
	}
}

// A budget is non-negative and cannot promise more steps than the ceiling.
func TestNormalizeWorkflowChecksTheBudget(t *testing.T) {
	base := func() *Workflow {
		return &Workflow{Name: "w", Description: "d", Steps: WorkflowSteps{{ID: "a", AgentConfigID: "x", Prompt: "p"}}}
	}
	wf := base()
	wf.Budget = WorkflowBudget{MaxSteps: 5, MaxTokens: 1000, MaxMinutes: 30}
	if err := NormalizeWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	wf = base()
	wf.Budget.MaxTokens = -1
	if err := NormalizeWorkflow(wf); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("negative budget: %v", err)
	}
	wf = base()
	wf.Budget.MaxSteps = MaxStepRuns + 1
	if err := NormalizeWorkflow(wf); err == nil || !strings.Contains(err.Error(), "max_steps") {
		t.Fatalf("steps past the ceiling: %v", err)
	}
	// The budget's own arithmetic.
	b := WorkflowBudget{MaxSteps: 2, MaxTokens: 10, MaxMinutes: 1}
	if err := b.Exceeded(BudgetSpent{Steps: 1, Tokens: 9, Minutes: 0.9}); err != nil {
		t.Fatalf("under: %v", err)
	}
	if err := b.Exceeded(BudgetSpent{Steps: 2}); err == nil || !strings.Contains(err.Error(), "2 of 2 steps") {
		t.Fatalf("steps: %v", err)
	}
	if err := (WorkflowBudget{}).Exceeded(BudgetSpent{Steps: 99, Tokens: 1 << 30, Minutes: 1e6}); err != nil {
		t.Fatalf("no budget bounds nothing: %v", err)
	}
	// Round trip through the JSON column.
	var scanned WorkflowBudget
	v, _ := b.Value()
	if err := scanned.Scan(v); err != nil || scanned != b {
		t.Fatalf("scan(value) = %+v, %v", scanned, err)
	}
}
