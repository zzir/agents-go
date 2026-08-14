package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"
)

// A workflow run's lifecycle. Terminal states are reached exactly once, by the
// compare-and-set writes below.
const (
	WorkflowRunning   = "running"
	WorkflowCompleted = "completed"
	WorkflowFailed    = "failed"
	WorkflowCancelled = "cancelled"
)

// WorkflowStep is one step of a fixed sequence: an agent and the prompt that
// starts its turn. A step is a full RUN on the session — tools and handoffs
// included, task/workflow tools withheld (it is itself a background run).
type WorkflowStep struct {
	// ID is stable across edits of the definition, so a run in flight and a
	// "retry from here" keep naming the same step; a position would shift.
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// AgentConfigID is which agent runs this step — the point of a workflow:
	// plan, exec and verify are usually different agents on different models.
	AgentConfigID string `json:"agent_config_id"`
	// Prompt is the step's input, sent as the user turn that starts it. The
	// previous steps are already in the session, so this instructs rather than
	// re-passes their output.
	Prompt string `json:"prompt"`
	// CompactBefore folds the conversation into a summary before this step runs.
	CompactBefore bool `json:"compact_before,omitempty"`
	// OnSuccess and OnFailure name the step to run next — a step id, or
	// WorkflowStepEnd to stop there. Their empty defaults differ: OnSuccess
	// falls through to the NEXT step in the list (the last one ends the
	// execution), an empty OnFailure fails the execution. A back-edge is how a
	// sequence loops, bounded only by MaxStepRuns. "Failure" is structural —
	// the step's run errored; a step judging its own output is what the step's
	// agent and its handoffs are for.
	OnSuccess string `json:"on_success,omitempty"`
	OnFailure string `json:"on_failure,omitempty"`
}

// WorkflowStepEnd is the reserved step id an OnSuccess or OnFailure target
// names to end the execution there instead of moving to another step.
const WorkflowStepEnd = "end"

// MaxStepRuns bounds the step runs ONE execution may produce, retries included
// — the only thing stopping an OnFailure back-edge from looping forever.
const MaxStepRuns = 50

// StepRun records which run executed which step, so a reader can group the
// flat transcript's turns back under the step that produced them (a retry
// appends another entry for the same step).
type StepRun struct {
	StepID string `json:"step_id"`
	RunID  string `json:"run_id"`
}

// StepRuns is the execution's run log, stored as one JSON column.
type StepRuns []StepRun

// Value implements driver.Valuer.
func (s StepRuns) Value() (driver.Value, error) { return jsonSliceValue(s) }

// Scan implements sql.Scanner.
func (s *StepRuns) Scan(src any) error { return scanJSONColumn(src, s, "workflow step runs") }

// jsonSliceValue marshals a JSON-backed slice column; nil stores as [] so a
// reader never has to tell "no rows yet" from "column never written".
func jsonSliceValue[T any](s []T) (driver.Value, error) {
	if s == nil {
		s = []T{}
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// scanJSONColumn reads one back. An empty or NULL column leaves dst untouched.
func scanJSONColumn(src, dst any, what string) error {
	if src == nil {
		return nil
	}
	var b []byte
	switch v := src.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return fmt.Errorf("%s: cannot scan %T", what, src)
	}
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, dst)
}

// With returns the log with one more entry — the append the CAS writes back.
func (s StepRuns) With(stepID, runID string) StepRuns {
	return append(append(make(StepRuns, 0, len(s)+1), s...), StepRun{StepID: stepID, RunID: runID})
}

// WorkflowSteps is the ordered sequence, stored as one JSON column.
type WorkflowSteps []WorkflowStep

// Value implements driver.Valuer.
func (s WorkflowSteps) Value() (driver.Value, error) { return jsonSliceValue(s) }

// Scan implements sql.Scanner.
func (s *WorkflowSteps) Scan(src any) error { return scanJSONColumn(src, s, "workflow steps") }

// Workflow is a fixed, ordered sequence of steps run on ONE session.
// Deterministic by design — which step runs next is the definition's answer,
// not the model's (that is what handoffs are for).
type Workflow struct {
	bun.BaseModel `bun:"table:workflows,alias:wfl"`

	ID   string `bun:"id,pk"        json:"id"`
	Name string `bun:"name,notnull" json:"name"`
	// Description says WHEN to run this, in one line. An agent matching a
	// request against it is the only way a workflow starts, so it is required.
	Description string        `bun:"description,notnull" json:"description"`
	Steps       WorkflowSteps `bun:"steps,type:text,nullzero" json:"steps"`

	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`
}

// WorkflowRun is one execution of a workflow. The steps run on a hidden CHILD
// session — a sequence's turns never enter the conversation that asked, only
// its result does, through a wake-up (README invariant 30). It carries a
// SNAPSHOT of the definition: editing a workflow must not steer an execution
// in flight.
type WorkflowRun struct {
	bun.BaseModel `bun:"table:workflow_runs,alias:wfr"`

	ID string `bun:"id,pk" json:"id"`
	// WorkflowID names the definition this came from; it may since have been
	// edited or deleted, which is why Steps is the snapshot that executes.
	WorkflowID string `bun:"workflow_id" json:"workflow_id,omitempty"`
	// ParentSessionID is the conversation that asked for this and is owed the
	// result; ChildSessionID is where the steps actually run.
	ParentSessionID string        `bun:"parent_session_id,notnull" json:"parent_session_id"`
	ChildSessionID  string        `bun:"child_session_id"          json:"child_session_id,omitempty"`
	Name            string        `bun:"name"                      json:"name"`
	Steps           WorkflowSteps `bun:"steps,type:text,nullzero"  json:"steps"`
	// Input is the brief: what this execution is about, written by the AGENT
	// that read the conversation. It leads the first step's turn only.
	Input string `bun:"input,nullzero" json:"input,omitempty"`
	// Result is the last step's output, kept on the row so the card can show
	// what came of this without reading the child session.
	Result string `bun:"result,nullzero" json:"result,omitempty"`
	// OriginRunID is the parent's run whose tool call started this; Inherit is
	// the configuration the result turn runs under. Both frozen at start, from
	// the run that asked (invariant 32).
	OriginRunID string `bun:"origin_run_id" json:"origin_run_id,omitempty"`
	Inherit     string `bun:"inherit,nullzero" json:"-"`

	// StepID is the step currently running (or the one a terminal state
	// stopped at, which is what a retry resumes from).
	StepID string `bun:"step_id" json:"step_id,omitempty"`
	// RunID is the run executing StepID — also the CAS token: an advance is
	// only accepted from the run the row believes is current.
	RunID string `bun:"run_id" json:"run_id,omitempty"`
	// StepRuns is every (step, run) this execution has produced, in order —
	// StepID/RunID are only ever the current one.
	StepRuns StepRuns `bun:"step_runs,type:text,nullzero" json:"step_runs,omitempty"`
	Status   string   `bun:"status,notnull" json:"status"`
	Error    string   `bun:"error,nullzero" json:"error,omitempty"`
	// Dismissed hides a terminal execution from the conversation's live strip;
	// the panel still lists it. A retry clears it — the execution is live again.
	Dismissed bool `bun:"dismissed" json:"dismissed,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`
}

// StepPrompt is the turn that starts the given step: the step's own
// instruction, led by the execution's input when the step is the FIRST one.
// Only the first, because from step two on the input is already in the
// transcript the step reads.
func (w *WorkflowRun) StepPrompt(step WorkflowStep) string {
	if w.Input == "" || len(w.Steps) == 0 || w.Steps[0].ID != step.ID {
		return step.Prompt
	}
	return w.Input + "\n\n" + step.Prompt
}

// StepIndex reports where stepID sits in the snapshot, or -1. It is a display
// concern ("step 2 of 3") and the retry's anchor — NOT how the sequence
// advances, which follows the edges below.
func (w *WorkflowRun) StepIndex(stepID string) int {
	for i := range w.Steps {
		if w.Steps[i].ID == stepID {
			return i
		}
	}
	return -1
}

// Step returns the step with this id, or nil.
func (w *WorkflowRun) Step(stepID string) *WorkflowStep {
	if i := w.StepIndex(stepID); i >= 0 {
		return &w.Steps[i]
	}
	return nil
}

// NextStep resolves where the execution goes after the step it is on, given how
// that step ended. ok=false means the execution is over — the edge said so, the
// list ran out, or a failure had no handler.
func (w *WorkflowRun) NextStep(failed bool) (*WorkflowStep, bool) {
	cur := w.Step(w.StepID)
	if cur == nil {
		return nil, false
	}
	target := cur.OnSuccess
	if failed {
		if cur.OnFailure == "" {
			return nil, false
		}
		target = cur.OnFailure
	}
	switch target {
	case WorkflowStepEnd:
		return nil, false
	case "":
		if i := w.StepIndex(w.StepID); i+1 < len(w.Steps) {
			return &w.Steps[i+1], true
		}
		return nil, false
	}
	// A target that names nothing is refused when the workflow is saved; a
	// snapshot that somehow holds one stops rather than guesses.
	next := w.Step(target)
	return next, next != nil
}

// WorkflowStore persists workflow definitions.
type WorkflowStore struct {
	*CrudStore[Workflow]
}

// NewWorkflowStore returns a WorkflowStore backed by db. Name uniqueness is
// enforced by the DB (idx_workflows_name).
func NewWorkflowStore(db *bun.DB) *WorkflowStore {
	return &WorkflowStore{NewCrudStore[Workflow](db, "workflow", "created_at DESC")}
}

// NormalizeWorkflow trims the spelling noise and fills each step's stable id,
// so a definition saved twice means the same thing and a step added through
// the API gets an id without the client having to invent one.
func NormalizeWorkflow(w *Workflow) error {
	w.Name = strings.TrimSpace(w.Name)
	w.Description = strings.TrimSpace(w.Description)
	if w.Name == "" {
		return fmt.Errorf("name is required")
	}
	if len(w.Steps) == 0 {
		return fmt.Errorf("a workflow needs at least one step")
	}
	// An agent can only choose what it can read about, and an agent choosing is
	// the only way a workflow ever starts.
	if w.Description == "" {
		return fmt.Errorf("description is required: it is what the agent matches a request against")
	}
	seen := make(map[string]bool, len(w.Steps))
	for i := range w.Steps {
		s := &w.Steps[i]
		s.ID = strings.TrimSpace(s.ID)
		s.Name = strings.TrimSpace(s.Name)
		s.Prompt = strings.TrimSpace(s.Prompt)
		if s.AgentConfigID == "" {
			return fmt.Errorf("step %d: agent_config_id is required", i+1)
		}
		if s.Prompt == "" {
			return fmt.Errorf("step %d: prompt is required", i+1)
		}
		if s.ID == "" {
			s.ID = NewID()
		}
		if s.ID == WorkflowStepEnd {
			return fmt.Errorf("step %d: %q is reserved — it is what an edge names to stop there", i+1, WorkflowStepEnd)
		}
		// A duplicate id would make "retry from here" ambiguous.
		if seen[s.ID] {
			return fmt.Errorf("step %d: duplicate step id %q", i+1, s.ID)
		}
		seen[s.ID] = true
	}
	// After every id is known, so an edge may name a step in either direction.
	for i := range w.Steps {
		for _, e := range []struct{ field, target string }{
			{"on_success", w.Steps[i].OnSuccess},
			{"on_failure", w.Steps[i].OnFailure},
		} {
			if e.target == "" || e.target == WorkflowStepEnd || seen[e.target] {
				continue
			}
			return fmt.Errorf("step %d: %s names %q, which is not a step of this workflow", i+1, e.field, e.target)
		}
	}
	return nil
}

// WorkflowRunStore persists workflow executions.
type WorkflowRunStore struct {
	db *bun.DB
}

// NewWorkflowRunStore returns a WorkflowRunStore backed by db.
func NewWorkflowRunStore(db *bun.DB) *WorkflowRunStore {
	return &WorkflowRunStore{db: db}
}

// Create inserts a new execution.
func (s *WorkflowRunStore) Create(ctx context.Context, w *WorkflowRun) error {
	if _, err := s.db.NewInsert().Model(w).Exec(ctx); err != nil {
		return fmt.Errorf("creating workflow run: %w", err)
	}
	return nil
}

// Get returns one execution.
func (s *WorkflowRunStore) Get(ctx context.Context, id string) (*WorkflowRun, error) {
	w := new(WorkflowRun)
	if err := s.db.NewSelect().Model(w).Where("id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return nil, fmt.Errorf("getting workflow run %s: %w", id, err)
	}
	return w, nil
}

// ListBySession returns the session's executions, newest first.
func (s *WorkflowRunStore) ListBySession(ctx context.Context, parentSessionID string) ([]WorkflowRun, error) {
	var out []WorkflowRun
	if err := s.db.NewSelect().Model(&out).
		Where("parent_session_id = ?", parentSessionID).
		OrderExpr("created_at DESC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing workflow runs for session %s: %w", parentSessionID, err)
	}
	return out, nil
}

// ActiveForRun returns the execution whose CURRENT step is this run, or nil —
// the run id is the only name a finished run and the row that owns it share.
func (s *WorkflowRunStore) ActiveForRun(ctx context.Context, runID string) (*WorkflowRun, error) {
	w := new(WorkflowRun)
	err := s.db.NewSelect().Model(w).
		Where("run_id = ?", runID).Where("status = ?", WorkflowRunning).
		Limit(1).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("finding the workflow run executing %s: %w", runID, err)
	}
	return w, nil
}

// ByChildSessionAny returns the execution whose steps use this session
// REGARDLESS of status, or nil. The status-filtered ByChildSession is how a run
// learns it is a live step; this one lets a resume guard tell "a stopped
// workflow's leftover approval" from "a genuine chat run".
func (s *WorkflowRunStore) ByChildSessionAny(ctx context.Context, childSessionID string) (*WorkflowRun, error) {
	if childSessionID == "" {
		return nil, nil
	}
	w := new(WorkflowRun)
	err := s.db.NewSelect().Model(w).
		Where("child_session_id = ?", childSessionID).
		OrderExpr("created_at DESC").Limit(1).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("finding any workflow for session %s: %w", childSessionID, err)
	}
	return w, nil
}

// ByChildSession returns the running execution whose steps use this session,
// or nil. It is how a run learns it is a workflow STEP: the child session is
// all it has, and a step must not get the tools a person's conversation does.
func (s *WorkflowRunStore) ByChildSession(ctx context.Context, childSessionID string) (*WorkflowRun, error) {
	if childSessionID == "" {
		return nil, nil
	}
	w := new(WorkflowRun)
	err := s.db.NewSelect().Model(w).
		Where("child_session_id = ?", childSessionID).
		Where("status = ?", WorkflowRunning).
		Limit(1).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("finding the workflow run for session %s: %w", childSessionID, err)
	}
	return w, nil
}

// CountLive returns how many executions are still running for a parent — the
// half of the background-work budget workflows account for.
func (s *WorkflowRunStore) CountLive(ctx context.Context, parentSessionID string) (int, error) {
	n, err := s.db.NewSelect().Model((*WorkflowRun)(nil)).
		Where("parent_session_id = ?", parentSessionID).
		Where("status = ?", WorkflowRunning).Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("counting live workflow runs for session %s: %w", parentSessionID, err)
	}
	return n, nil
}

// ListRunning returns every execution still recorded as running, for the
// restart reconciliation.
func (s *WorkflowRunStore) ListRunning(ctx context.Context) ([]WorkflowRun, error) {
	var out []WorkflowRun
	if err := s.db.NewSelect().Model(&out).
		Where("status = ?", WorkflowRunning).Scan(ctx); err != nil {
		return nil, fmt.Errorf("listing running workflow runs: %w", err)
	}
	return out, nil
}

// Advance moves the execution to the next step, but only while it is still
// running and still believes fromRunID is its current run — the compare-and-set
// that keeps a late callback from a superseded run from driving the sequence
// on. stepRuns is the caller's log with the new step appended; the CAS makes
// exactly one such derivation the winner.
func (s *WorkflowRunStore) Advance(ctx context.Context, id, fromRunID, nextStepID, nextRunID string, stepRuns StepRuns) (bool, error) {
	res, err := s.db.NewUpdate().Model((*WorkflowRun)(nil)).
		Set("step_id = ?", nextStepID).
		Set("run_id = ?", nextRunID).
		Set("step_runs = ?", stepRuns).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Where("status = ?", WorkflowRunning).
		Where("run_id = ?", fromRunID).
		Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("advancing workflow run %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	return err == nil && n > 0, nil
}

// Finish writes a terminal state under the same compare-and-set as Advance, so
// the ending is claimed exactly once, AND — in the same transaction — records
// the wake-up the parent is owed (nil owes nothing, e.g. a cancellation). One
// tx so a crash can never leave a finished execution whose parent is never told,
// the same guarantee the task path has. An empty fromRunID skips the run check —
// the stop path, which ends whatever is current.
func (s *WorkflowRunStore) Finish(ctx context.Context, id, fromRunID, status, errMsg, result string, wakeup *Wakeup) (bool, error) {
	var won bool
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		q := tx.NewUpdate().Model((*WorkflowRun)(nil)).
			Set("status = ?", status).
			Set("error = ?", errMsg).
			Set("result = ?", result).
			Set("updated_at = ?", time.Now().UTC()).
			Where("id = ?", id).
			Where("status = ?", WorkflowRunning)
		if fromRunID != "" {
			q = q.Where("run_id = ?", fromRunID)
		}
		res, err := q.Exec(ctx)
		if err != nil {
			return fmt.Errorf("finishing workflow run %s: %w", id, err)
		}
		n, _ := res.RowsAffected()
		won = n > 0
		if won && wakeup != nil {
			if _, err := tx.NewInsert().Model(wakeup).Exec(ctx); err != nil {
				return fmt.Errorf("recording workflow %s wake-up: %w", id, err)
			}
		}
		return nil
	})
	return won, err
}

// Restart reopens a FAILED execution at stepID with a fresh run id — the
// "retry from here" write. The compare-and-set names both the status (only a
// failure retries: reopening a completed execution would re-run side effects
// a person already has) and fromRunID, the run the caller's read believed
// current — so two retries derive exactly one winner and the run log cannot
// lose entries to a stale read.
func (s *WorkflowRunStore) Restart(ctx context.Context, id, fromRunID, stepID, runID string, stepRuns StepRuns) (bool, error) {
	res, err := s.db.NewUpdate().Model((*WorkflowRun)(nil)).
		Set("status = ?", WorkflowRunning).
		Set("step_id = ?", stepID).
		Set("run_id = ?", runID).
		Set("step_runs = ?", stepRuns).
		Set("error = ?", "").
		Set("dismissed = ?", false).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Where("status = ?", WorkflowFailed).
		Where("run_id = ?", fromRunID).
		Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("restarting workflow run %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	return err == nil && n > 0, nil
}

// Dismiss hides a terminal execution from the live strip. Terminal-only: a
// running sequence is exactly what the strip exists to show.
func (s *WorkflowRunStore) Dismiss(ctx context.Context, id string) (bool, error) {
	res, err := s.db.NewUpdate().Model((*WorkflowRun)(nil)).
		Set("dismissed = ?", true).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Where("status != ?", WorkflowRunning).
		Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("dismissing workflow run %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	return err == nil && n > 0, nil
}
