package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"
)

// The two ways a trigger fires: on a schedule, or on a signed HTTP call.
const (
	TriggerKindCron    = "cron"
	TriggerKindWebhook = "webhook"
)

// The two things a trigger starts: a workflow execution — a background task
// that reports back to the session — or an agent turn: the brief run as an
// ordinary message in the session itself, its reply the conversation's next
// turn.
const (
	TriggerTargetWorkflow = "workflow"
	TriggerTargetAgent    = "agent"
)

// Trigger starts work without a conversation asking: a cron schedule or a
// webhook, each firing into the session the trigger names, with the brief its
// author wrote in advance — the trigger IS the knowing party (README
// invariant 30). What it starts is its Target: a workflow (the same start a
// person's "Run…" makes, RunWorkflow) or an agent turn (the same run a
// message makes).
type Trigger struct {
	bun.BaseModel `bun:"table:triggers,alias:trg"`

	ID string `bun:"id,pk,type:uuid" json:"id"`
	// Target says what a fire starts; WorkflowID or AgentConfigID names it,
	// the other stays empty.
	Target        string `bun:"target,notnull"           json:"target"`
	WorkflowID    string `bun:"workflow_id,nullzero,type:uuid" json:"workflow_id,omitempty"`
	AgentConfigID string `bun:"agent_config_id,nullzero,type:uuid" json:"agent_config_id,omitempty"`
	// SessionID is the conversation the work reports to — or, for an agent
	// turn, happens in.
	SessionID string `bun:"session_id,notnull,type:uuid" json:"session_id"`
	Kind      string `bun:"kind,notnull"       json:"kind"`
	// Brief leads every execution or turn this trigger starts; a webhook's
	// payload is appended to it.
	Brief string `bun:"brief,notnull" json:"brief"`
	// Schedule is the cron expression (five fields, or a descriptor such as
	// @hourly or @every 10m). Cron kind only.
	Schedule string `bun:"schedule,nullzero" json:"schedule,omitempty"`
	// Secret signs a webhook's calls (HMAC-SHA256); never serialized — the API
	// shows it once, at creation or rotation.
	Secret  string `bun:"secret,nullzero" json:"-"`
	Enabled bool   `bun:"enabled,notnull" json:"enabled"`

	// What the last fire did, for the panel and for a cron that keeps failing:
	// the id it started — a task for a workflow, a run for an agent turn — or
	// why it started nothing.
	LastFiredAt   time.Time `bun:"last_fired_at,nullzero"   json:"last_fired_at,omitzero"`
	LastStartedID string    `bun:"last_started_id,nullzero,type:uuid" json:"last_started_id,omitempty"`
	LastError     string    `bun:"last_error,nullzero"      json:"last_error,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`
}

// BeforeAppendModel stamps the id and timestamps; bun invokes it on insert and update.
func (m *Trigger) BeforeAppendModel(_ context.Context, q bun.Query) error {
	return stampOnAppend(q, &m.ID, &m.CreatedAt, &m.UpdatedAt)
}

// NewTriggerSecret returns a fresh webhook signing secret (256 bits, hex).
func NewTriggerSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NormalizeTrigger trims and checks the shape of a trigger; the schedule's
// syntax is the scheduler's to judge, and the target and session named are
// the handler's to look up. An empty target is read off the id given, so a
// client from before targets (workflow_id alone) still means what it did.
func NormalizeTrigger(t *Trigger) error {
	t.Kind = strings.TrimSpace(t.Kind)
	t.Brief = strings.TrimSpace(t.Brief)
	t.Schedule = strings.TrimSpace(t.Schedule)
	t.Target = strings.TrimSpace(t.Target)
	t.WorkflowID = strings.TrimSpace(t.WorkflowID)
	t.AgentConfigID = strings.TrimSpace(t.AgentConfigID)
	t.SessionID = strings.TrimSpace(t.SessionID)
	if t.Target == "" {
		switch {
		case t.WorkflowID != "":
			t.Target = TriggerTargetWorkflow
		case t.AgentConfigID != "":
			t.Target = TriggerTargetAgent
		}
	}
	switch t.Target {
	case TriggerTargetWorkflow:
		if t.WorkflowID == "" {
			return fmt.Errorf("workflow_id is required for a workflow target")
		}
		t.AgentConfigID = ""
	case TriggerTargetAgent:
		if t.AgentConfigID == "" {
			return fmt.Errorf("agent_config_id is required for an agent target")
		}
		t.WorkflowID = ""
	default:
		return fmt.Errorf("target must be %q or %q", TriggerTargetWorkflow, TriggerTargetAgent)
	}
	if t.SessionID == "" {
		return fmt.Errorf("session_id is required: the conversation the work reports to")
	}
	switch t.Kind {
	case TriggerKindCron:
		if t.Schedule == "" {
			return fmt.Errorf("schedule is required for a cron trigger")
		}
	case TriggerKindWebhook:
		t.Schedule = ""
	default:
		return fmt.Errorf("kind must be %q or %q", TriggerKindCron, TriggerKindWebhook)
	}
	return nil
}

// TriggerStore persists triggers.
type TriggerStore struct {
	*CrudStore[Trigger]
}

// NewTriggerStore returns a TriggerStore backed by db.
func NewTriggerStore(db *bun.DB) *TriggerStore {
	return &TriggerStore{NewCrudStore[Trigger](db, "trigger", "created_at DESC").withSecrets(sealTrigger, openTrigger)}
}

// referencesExist reports whether the target and the session a trigger names
// are there, read on the same handle as the write that follows — inside a
// write transaction, so a concurrent delete cannot land between check and
// insert (the writer holds the database until it commits).
func referencesExist(ctx context.Context, tx bun.IDB, t *Trigger) error {
	target := struct {
		model any
		id    string
		what  string
	}{(*Workflow)(nil), t.WorkflowID, "workflow_id"}
	if t.Target == TriggerTargetAgent {
		target.model, target.id, target.what = (*AgentConfig)(nil), t.AgentConfigID, "agent_config_id"
	}
	for _, ref := range []struct {
		model any
		id    string
		what  string
	}{target, {(*Session)(nil), t.SessionID, "session_id"}} {
		ok, err := tx.NewSelect().Model(ref.model).Where("id = ?", ref.id).Exists(ctx)
		if err != nil {
			return fmt.Errorf("checking the trigger's %s: %w", ref.what, err)
		}
		if !ok {
			return fmt.Errorf("%w: %s names nothing", ErrTriggerRef, ref.what)
		}
	}
	return nil
}

// ErrTriggerRef marks a trigger naming a workflow, agent or session that is
// not there.
var ErrTriggerRef = errors.New("trigger reference")

// Create inserts a trigger, checking the workflow and session it names in the
// same transaction. A missing one is ErrTriggerRef.
func (s *TriggerStore) Create(ctx context.Context, t *Trigger) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := referencesExist(ctx, tx, t); err != nil {
			return err
		}
		err := sealedWrite(t, sealTrigger, openTrigger, func() error {
			_, err := tx.NewInsert().Model(t).Exec(ctx)
			return err
		})
		if err != nil {
			return fmt.Errorf("creating trigger: %w", err)
		}
		return nil
	})
}

// UpdateSettings writes what a client may set — the target, the brief, the
// schedule, the switch — and nothing else: not the secret, not the fire
// record, which have their own writers and would be clobbered by a whole-row
// update racing them. The references are checked in the same transaction.
func (s *TriggerStore) UpdateSettings(ctx context.Context, id string, t *Trigger) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := referencesExist(ctx, tx, t); err != nil {
			return err
		}
		res, err := tx.NewUpdate().Model((*Trigger)(nil)).
			Set("target = ?", t.Target).
			Set("workflow_id = ?", uuidOrNull(t.WorkflowID)).
			Set("agent_config_id = ?", uuidOrNull(t.AgentConfigID)).
			Set("session_id = ?", t.SessionID).
			Set("brief = ?", t.Brief).
			Set("schedule = ?", t.Schedule).
			Set("enabled = ?", t.Enabled).
			Set("updated_at = ?", time.Now().UTC()).
			Where("id = ?", id).
			Exec(ctx)
		if err == nil {
			err = requireRows(res)
		}
		if err != nil {
			return fmt.Errorf("updating trigger %s: %w", id, err)
		}
		return nil
	})
}

// DeleteIfWorkflow removes the trigger only while it still names workflowID
// — the self-heal of a fire that found the workflow gone must not take a
// trigger re-pointed in the meantime. A row that no longer matches is
// ErrNotFound.
func (s *TriggerStore) DeleteIfWorkflow(ctx context.Context, id, workflowID string) error {
	res, err := s.db.NewDelete().Model((*Trigger)(nil)).
		Where("id = ?", id).Where("workflow_id = ?", workflowID).Exec(ctx)
	if err == nil {
		err = requireRows(res)
	}
	if err != nil {
		return fmt.Errorf("deleting trigger %s: %w", id, err)
	}
	return nil
}

// SetSecret writes a webhook trigger's new secret and nothing else.
func (s *TriggerStore) SetSecret(ctx context.Context, id, secret string) error {
	res, err := s.db.NewUpdate().Model((*Trigger)(nil)).
		Set("secret = ?", sealSecret(secret)).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Exec(ctx)
	if err == nil {
		err = requireRows(res)
	}
	if err != nil {
		return fmt.Errorf("rotating the secret of trigger %s: %w", id, err)
	}
	return nil
}

// ListByWorkflow returns a workflow's triggers, newest first.
func (s *TriggerStore) ListByWorkflow(ctx context.Context, workflowID string) ([]Trigger, error) {
	var out []Trigger
	if err := s.db.NewSelect().Model(&out).Where("workflow_id = ?", workflowID).OrderExpr("created_at DESC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing triggers of workflow %s: %w", workflowID, err)
	}
	return out, s.openAll(out)
}

// openAll opens every row's secret after a custom select.
func (s *TriggerStore) openAll(rows []Trigger) error {
	for i := range rows {
		if err := openTrigger(&rows[i]); err != nil {
			return fmt.Errorf("listing triggers: %w", err)
		}
	}
	return nil
}

// ListByOwner returns the triggers whose session ownerID owns, newest first;
// workflowID, when set, narrows to one workflow's. A trigger is as private as
// the conversation it fires into.
func (s *TriggerStore) ListByOwner(ctx context.Context, ownerID, workflowID string) ([]Trigger, error) {
	var out []Trigger
	q := s.db.NewSelect().Model(&out).
		Join("JOIN sessions AS s ON s.id = trg.session_id").
		Where("s.owner_id = ?", ownerID)
	if workflowID != "" {
		q = q.Where("trg.workflow_id = ?", workflowID)
	}
	if err := q.OrderExpr("trg.created_at DESC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing triggers: %w", err)
	}
	return out, s.openAll(out)
}

// RecordFire writes what a fire did — the task or run it started, or why it
// started nothing.
func (s *TriggerStore) RecordFire(ctx context.Context, id, startedID, fireErr string) error {
	_, err := s.db.NewUpdate().Model((*Trigger)(nil)).
		Set("last_fired_at = ?", time.Now().UTC()).
		Set("last_started_id = ?", uuidOrNull(startedID)).
		Set("last_error = ?", fireErr).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("recording fire of trigger %s: %w", id, err)
	}
	return nil
}
