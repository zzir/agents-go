package store

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/uptrace/bun"
)

// WorkflowStep is one step of a fixed sequence: an agent and the prompt that
// starts its turn — a full RUN on the execution's session, task/workflow tools withheld.
type WorkflowStep struct {
	// ID is stable across edits of the definition, so an execution in flight
	// and a "retry from here" keep naming the same step; a position would shift.
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// AgentConfigID is which agent runs this step — the point of a workflow:
	// plan, exec and verify are usually different agents on different models.
	AgentConfigID string `json:"agent_config_id"`
	// Prompt is the step's input, sent as the user turn that starts it; the
	// previous steps are already in the session.
	Prompt string `json:"prompt"`
	// CompactBefore folds the conversation into a summary before this step runs.
	CompactBefore bool `json:"compact_before,omitempty"`
	// PauseBefore holds the sequence before this step until a person approves
	// it from the conversation that asked; rejecting cancels the execution.
	PauseBefore bool `json:"pause_before,omitempty"`
	// Gate makes the step a CHECK: its final output decides which edge is
	// taken (StepGate). Nil means the run's own outcome decides.
	Gate *StepGate `json:"gate,omitempty"`
	// OnSuccess and OnFailure name the step to run next (a step id, or
	// WorkflowStepEnd). Empty OnSuccess falls through to the NEXT step; empty OnFailure fails the execution.
	OnSuccess string `json:"on_success,omitempty"`
	OnFailure string `json:"on_failure,omitempty"`
}

// StepGate makes a step a check: its final output reports one of two
// sentinels (Verdict) — Pass takes on_success, Fail on_failure, no verdict
// fails the execution. docs/reference/protocol.md "Workflows" has the rules.
type StepGate struct {
	Pass string `json:"pass,omitempty"`
	Fail string `json:"fail,omitempty"`
}

// The sentinels a gate reads when it names none.
const (
	DefaultGatePass = "PASS"
	DefaultGateFail = "FAIL"
)

// gateTrim is what Verdict strips off a candidate line before comparing
// (markdown emphasis, sentence punctuation); normalizeGateWord holds a configured word to the same shape.
const gateTrim = "*_`.!:"

// normalizeGateWord is a configured word as Verdict compares it.
func normalizeGateWord(w string) string {
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(w), gateTrim))
}

// PassWord is the gate's success sentinel.
func (g *StepGate) PassWord() string {
	if w := normalizeGateWord(g.Pass); w != "" {
		return w
	}
	return DefaultGatePass
}

// FailWord is the gate's failure sentinel.
func (g *StepGate) FailWord() string {
	if w := normalizeGateWord(g.Fail); w != "" {
		return w
	}
	return DefaultGateFail
}

// Instruction is the line appended to a gated step's prompt, asking for the
// sentinel line; structured output may carry the verdict as a field instead.
func (g *StepGate) Instruction() string {
	return fmt.Sprintf("End your reply with exactly one final line that is either %s or %s: %s when the check succeeds, %s when it does not "+
		"(if you answer as a JSON object, put the verdict in a boolean \"passed\" field instead).",
		g.PassWord(), g.FailWord(), g.PassWord(), g.FailWord())
}

// Verdict reads the step's final output: a JSON object (fenced or not) with
// a boolean `passed`/`pass` or a `verdict`/`result`/`status` string equal to
// a sentinel; otherwise the LAST non-empty line, which must be a sentinel.
// ok=false means there was none.
func (g *StepGate) Verdict(output string) (passed, ok bool) {
	if passed, ok := g.jsonVerdict(output); ok {
		return passed, true
	}
	lines := strings.Split(output, "\n")
	for _, line := range slices.Backward(lines) {
		line := normalizeGateWord(line)
		if line == "" {
			continue
		}
		return g.matchWord(line)
	}
	return false, false
}

// matchWord compares one candidate against the two sentinels.
func (g *StepGate) matchWord(word string) (passed, ok bool) {
	switch {
	case strings.EqualFold(word, g.PassWord()):
		return true, true
	case strings.EqualFold(word, g.FailWord()):
		return false, true
	}
	return false, false
}

// jsonVerdict reads a structured answer: the output is one JSON object, bare
// or in a ```json fence, carrying the verdict under a conventional key.
func (g *StepGate) jsonVerdict(output string) (passed, ok bool) {
	text := strings.TrimSpace(output)
	if strings.HasPrefix(text, "```") {
		text = strings.TrimSpace(strings.TrimPrefix(text, "```json"))
		text = strings.TrimSpace(strings.TrimPrefix(text, "```"))
		text = strings.TrimSpace(strings.TrimSuffix(text, "```"))
	}
	if !strings.HasPrefix(text, "{") {
		return false, false
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(text), &obj); err != nil {
		return false, false
	}
	for _, k := range []string{"passed", "pass"} {
		if b, isBool := obj[k].(bool); isBool {
			return b, true
		}
	}
	for _, k := range []string{"verdict", "result", "status"} {
		if s, isStr := obj[k].(string); isStr {
			if passed, ok := g.matchWord(strings.TrimSpace(s)); ok {
				return passed, true
			}
		}
	}
	return false, false
}

// WorkflowStepEnd is the reserved step id an OnSuccess or OnFailure target
// names to end the execution there instead of moving to another step.
const WorkflowStepEnd = "end"

// MaxStepRuns bounds the step runs ONE execution may launch, retries
// included; the lap bound (WorkflowBudget.MaxLaps) stops a loop long before.
const MaxStepRuns = 50

// defaultMaxLaps is how many times one execution may take the same BACKWARD
// edge when its definition sets no bound.
const defaultMaxLaps = 3

// WorkflowSteps is the ordered sequence, stored as one JSON column.
type WorkflowSteps []WorkflowStep

// Value implements driver.Valuer.
func (s WorkflowSteps) Value() (driver.Value, error) { return jsonSliceValue(s) }

// Scan implements sql.Scanner.
func (s *WorkflowSteps) Scan(src any) error { return scanJSONColumn(src, s, "workflow steps") }

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

// Workflow is a fixed, ordered sequence of steps run on ONE session; which
// step runs next is the definition's answer, not the model's.
type Workflow struct {
	bun.BaseModel `bun:"table:workflows,alias:wfl"`

	ID   string `bun:"id,pk,type:uuid" json:"id"`
	Name string `bun:"name,notnull" json:"name"`
	// Description says WHEN to run this, in one line. An agent matching a
	// request against it is the only way a workflow starts, so it is required.
	Description string        `bun:"description,notnull" json:"description"`
	Steps       WorkflowSteps `bun:"steps,type:text,nullzero" json:"steps"`
	// Budget bounds every execution of this workflow (zero fields = no bound).
	Budget WorkflowBudget `bun:"budget,type:text,nullzero" json:"budget"`

	// Scope/OwnerID: row visibility and its permanent creator.
	Scope   string `bun:"scope,notnull"               json:"scope"`
	OwnerID string `bun:"owner_id,nullzero,type:uuid" json:"owner_id,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull" json:"updated_at"`
}

// WorkflowBudget is what one execution may spend before it is failed with
// the reason: step launches (at most MaxStepRuns), tokens (input + output on
// the execution's session) and minutes of step run time. Each is checked
// before a launch; a running step is not interrupted. Zero = no bound,
// except MaxLaps, whose zero is defaultMaxLaps.
type WorkflowBudget struct {
	MaxSteps   int `json:"max_steps,omitempty"`
	MaxTokens  int `json:"max_tokens,omitempty"`
	MaxMinutes int `json:"max_minutes,omitempty"`
	// MaxLaps bounds how many times one execution may take the same backward
	// edge (verify → exec, fix → review): the loop bound.
	MaxLaps int `json:"max_laps,omitempty"`
}

// LapBound is the laps one backward edge may be taken: the definition's, or
// the default.
func (b WorkflowBudget) LapBound() int {
	if b.MaxLaps > 0 {
		return b.MaxLaps
	}
	return defaultMaxLaps
}

// IsZero reports a budget that bounds nothing.
func (b WorkflowBudget) IsZero() bool { return b == WorkflowBudget{} }

// Value implements driver.Valuer.
func (b WorkflowBudget) Value() (driver.Value, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	return string(raw), nil
}

// Scan implements sql.Scanner.
func (b *WorkflowBudget) Scan(src any) error { return scanJSONColumn(src, b, "workflow budget") }

// BudgetSpent is what an execution has used so far, measured by the driver:
// steps and minutes from the launch log, tokens from the session's entries.
type BudgetSpent struct {
	Steps   int
	Tokens  int
	Minutes float64
}

// The bounds that end an execution and refuse its retry: wrapped so a caller
// can tell a refusal from a fault.
var (
	ErrBudgetExhausted = errors.New("budget exhausted")
	ErrStepCeiling     = errors.New("the workflow's edges are looping")
	ErrLoopBound       = errors.New("loop bound reached")
)

// Exceeded is the error that stops an execution over its budget, or nil.
func (b WorkflowBudget) Exceeded(spent BudgetSpent) error {
	switch {
	case b.MaxSteps > 0 && spent.Steps >= b.MaxSteps:
		return fmt.Errorf("%w: %d of %d steps", ErrBudgetExhausted, spent.Steps, b.MaxSteps)
	case b.MaxTokens > 0 && spent.Tokens >= b.MaxTokens:
		return fmt.Errorf("%w: %d of %d tokens", ErrBudgetExhausted, spent.Tokens, b.MaxTokens)
	case b.MaxMinutes > 0 && spent.Minutes >= float64(b.MaxMinutes):
		return fmt.Errorf("%w: %.1f of %d minutes", ErrBudgetExhausted, spent.Minutes, b.MaxMinutes)
	}
	return nil
}

// StepRun records which run executed which step, so a reader can group the
// transcript's turns under the step that produced them.
type StepRun struct {
	StepID string `json:"step_id"`
	RunID  string `json:"run_id"`
	// Outcome is how the run ended, written when the sequence moves on from it
	// (StepOutcome*). Empty on the current run — the task's status says.
	Outcome string `json:"outcome,omitempty"`
	// StartedAt is stamped at launch, EndedAt with the outcome: the run's time,
	// which is what the minutes budget sums.
	StartedAt time.Time `json:"started_at,omitzero"`
	EndedAt   time.Time `json:"ended_at,omitzero"`
	// Retry marks a run a person's task_retry launched — the same step again,
	// by hand, which is not a lap of the sequence's own edges.
	Retry bool `json:"retry,omitempty"`
}

// How a step's run ended, as the launch log records it.
const (
	StepOutcomeCompleted = "completed"
	StepOutcomeFailed    = "failed"
	StepOutcomePass      = "pass"
	StepOutcomeFail      = "fail"
)

// StepRuns is the execution's launch log.
type StepRuns []StepRun

// With returns the log with one more entry, started now.
func (s StepRuns) With(stepID, runID string) StepRuns {
	return append(append(make(StepRuns, 0, len(s)+1), s...), StepRun{StepID: stepID, RunID: runID, StartedAt: time.Now().UTC()})
}

// WithRetry is With for a run a person's retry launched.
func (s StepRuns) WithRetry(stepID, runID string) StepRuns {
	out := s.With(stepID, runID)
	out[len(out)-1].Retry = true
	return out
}

// SequenceRuns is how many runs the sequence's own edges launched — the log
// minus the runs a person's retry added.
func (s StepRuns) SequenceRuns() int {
	n := 0
	for i := range s {
		if !s[i].Retry {
			n++
		}
	}
	return n
}

// Minutes is the run time the log accounts for: every ended run's duration.
func (s StepRuns) Minutes() float64 {
	var d time.Duration
	for _, sr := range s {
		if !sr.StartedAt.IsZero() && !sr.EndedAt.IsZero() && sr.EndedAt.After(sr.StartedAt) {
			d += sr.EndedAt.Sub(sr.StartedAt)
		}
	}
	return d.Minutes()
}

// WorkflowState is what a workflow execution keeps in its task's State: a
// SNAPSHOT of the definition and where the sequence stands. The driver
// writes it atomically with the run it belongs to (tasks.Store.Advance) at
// the start, every launch, every step transition, and the end.
type WorkflowState struct {
	// WorkflowID names the definition this came from; it may since have been
	// edited or deleted, which is why Steps is the snapshot that executes.
	WorkflowID string        `json:"workflow_id,omitempty"`
	Steps      WorkflowSteps `json:"steps"`
	// Budget is the definition's, snapshotted with the steps.
	Budget WorkflowBudget `json:"budget,omitzero"`
	// Input is the brief: what this execution is about, written by the AGENT
	// that read the conversation. It leads the first step's turn only.
	Input string `json:"input,omitempty"`
	// StepID is the step currently running (or the one a terminal state
	// stopped at, which is what a retry resumes from).
	StepID string `json:"step_id"`
	// StepRuns is every (step, run) this execution has LAUNCHED, in order —
	// appended by the launcher, so a run that never started is not in it.
	StepRuns StepRuns `json:"step_runs,omitempty"`
	// PendingInput is the turn a PauseBefore step will start with once a
	// person approves it. Cleared at launch.
	PendingInput string `json:"pending_input,omitempty"`
	// Stopped names the bound that ended the execution for good — StoppedBy*
	// — so a client knows a retry would be refused before it asks.
	Stopped string `json:"stopped,omitempty"`
}

// The bounds that end an execution for good.
const (
	StoppedByBudget  = "budget"
	StoppedByCeiling = "ceiling"
	StoppedByLaps    = "laps"
)

// StopIfBounded checks the execution against its budget and the step
// ceiling, recording which bound stopped it on the state; nil while it may
// launch another step. A bound already recorded stands.
func (w *WorkflowState) StopIfBounded(tokens int) error {
	if w.Stopped == StoppedByLaps {
		return fmt.Errorf("%w: the sequence keeps returning to the same step", ErrLoopBound)
	}
	if err := w.OverBudget(tokens); err != nil {
		w.Stopped = StoppedByBudget
		return err
	}
	if err := w.UnderStepCeiling(); err != nil {
		w.Stopped = StoppedByCeiling
		return err
	}
	return nil
}

// StopIfLooping checks the transition against the lap bound: a BACKWARD edge
// (to an earlier step, or itself) taken one more time than allowed ends the
// execution. Laps are read off the launch log.
func (w *WorkflowState) StopIfLooping(next *WorkflowStep) error {
	if next == nil {
		return nil
	}
	from, to := w.StepIndex(w.StepID), w.StepIndex(next.ID)
	if from < 0 || to < 0 || to > from {
		return nil
	}
	laps := 0
	for i := 0; i+1 < len(w.StepRuns); i++ {
		// A retry re-runs a step by hand: the pair it makes with the run
		// before it is not the sequence taking an edge.
		if w.StepRuns[i].StepID == w.StepID && w.StepRuns[i+1].StepID == next.ID && !w.StepRuns[i+1].Retry {
			laps++
		}
	}
	if bound := w.Budget.LapBound(); laps >= bound {
		w.Stopped = StoppedByLaps
		return fmt.Errorf("%w: %s → %s looped %d times", ErrLoopBound, stepLabel(w.Current()), stepLabel(next), laps)
	}
	return nil
}

// stepLabel is how a step is named in a message: its name, else its id.
func stepLabel(s *WorkflowStep) string {
	if s == nil {
		return ""
	}
	if s.Name != "" {
		return s.Name
	}
	return s.ID
}

// DecodeWorkflowState reads a task's State. Nil or unreadable is an error: a
// workflow task whose state cannot be read has no step to run.
func DecodeWorkflowState(raw json.RawMessage) (*WorkflowState, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("workflow state is empty")
	}
	st := new(WorkflowState)
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, fmt.Errorf("decoding workflow state: %w", err)
	}
	return st, nil
}

// Encode returns the state as a task's State payload.
func (w *WorkflowState) Encode() json.RawMessage {
	b, err := json.Marshal(w)
	if err != nil {
		return nil
	}
	return b
}

// StepPrompt is the turn that starts the given step: the step's instruction,
// led by the execution's input for the FIRST step only, trailed by the
// verdict instruction for a gate.
func (w *WorkflowState) StepPrompt(step WorkflowStep) string {
	prompt := step.Prompt
	if w.Input != "" && len(w.Steps) > 0 && w.Steps[0].ID == step.ID {
		prompt = w.Input + "\n\n" + prompt
	}
	if step.Gate != nil {
		prompt += "\n\n" + step.Gate.Instruction()
	}
	return prompt
}

// RecordOutcome stamps how the CURRENT run ended onto its launch-log entry —
// the last one, since the launcher appends at every launch.
func (w *WorkflowState) RecordOutcome(runID, outcome string) {
	if n := len(w.StepRuns); n > 0 && w.StepRuns[n-1].RunID == runID {
		w.StepRuns[n-1].Outcome = outcome
		w.StepRuns[n-1].EndedAt = time.Now().UTC()
	}
}

// OverBudget is the error that stops the execution at its budget, given the
// tokens its session has spent; steps and minutes are read off the log.
func (w *WorkflowState) OverBudget(tokens int) error {
	return w.Budget.Exceeded(BudgetSpent{Steps: len(w.StepRuns), Tokens: tokens, Minutes: w.StepRuns.Minutes()})
}

// UnderStepCeiling is the error that ends an execution at MaxStepRuns, or nil
// while it may launch another step.
func (w *WorkflowState) UnderStepCeiling() error {
	if len(w.StepRuns) >= MaxStepRuns {
		return fmt.Errorf("stopped after %d steps — %w", len(w.StepRuns), ErrStepCeiling)
	}
	return nil
}

// StepIndex reports where stepID sits in the snapshot, or -1 — a display
// concern and the retry's anchor, NOT how the sequence advances.
func (w *WorkflowState) StepIndex(stepID string) int {
	for i := range w.Steps {
		if w.Steps[i].ID == stepID {
			return i
		}
	}
	return -1
}

// Step returns the step with this id, or nil.
func (w *WorkflowState) Step(stepID string) *WorkflowStep {
	if i := w.StepIndex(stepID); i >= 0 {
		return &w.Steps[i]
	}
	return nil
}

// Current returns the step the execution is on, or nil.
func (w *WorkflowState) Current() *WorkflowStep { return w.Step(w.StepID) }

// NextStep resolves where the execution goes after the step it is on, given how
// that step ended. ok=false means the execution is over — the edge said so, the
// list ran out, or a failure had no handler.
func (w *WorkflowState) NextStep(failed bool) (*WorkflowStep, bool) {
	cur := w.Current()
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

// DisplayWorkflowStarted is the display kind of the note a workflow start
// leaves on the conversation — the exchange's question (protocol.md "Workflows").
const DisplayWorkflowStarted = "workflow_started"

// WorkflowOrigin says who started an execution when no run did.
type WorkflowOrigin struct {
	// Kind is "person" (the Run… button, POST /workflows/:id/runs) or "trigger".
	Kind        string `json:"kind"`
	TriggerID   string `json:"trigger_id,omitempty"`
	TriggerKind string `json:"trigger_kind,omitempty"`
	Schedule    string `json:"schedule,omitempty"`
}

// The two origins.
const (
	OriginPerson  = "person"
	OriginTrigger = "trigger"
)

// OriginOf is the origin a trigger's fire records.
func OriginOf(t *Trigger) WorkflowOrigin {
	return WorkflowOrigin{Kind: OriginTrigger, TriggerID: t.ID, TriggerKind: t.Kind, Schedule: t.Schedule}
}

// WorkflowStarted is the note's data: which execution, of what, with what
// brief, started by whom.
type WorkflowStarted struct {
	TaskID       string         `json:"task_id"`
	WorkflowID   string         `json:"workflow_id"`
	WorkflowName string         `json:"workflow_name"`
	Brief        string         `json:"brief,omitempty"`
	Origin       WorkflowOrigin `json:"origin"`
}

// Text is the note as a line, for a renderer that knows no better.
func (w WorkflowStarted) Text() string {
	by := "you"
	if w.Origin.Kind == OriginTrigger {
		by = "trigger " + w.Origin.TriggerKind
		if w.Origin.Schedule != "" {
			by += " " + w.Origin.Schedule
		}
	}
	return fmt.Sprintf("Workflow %q started by %s", w.WorkflowName, by)
}

// DisplayTriggerFired is the display kind of the note a trigger's AGENT turn
// leaves on the conversation, right before the message it sends.
const DisplayTriggerFired = "trigger_fired"

// TriggerFired is that note's data: which trigger, which agent it prompted,
// the run the turn is, and the brief.
type TriggerFired struct {
	RunID         string         `json:"run_id"`
	AgentConfigID string         `json:"agent_config_id"`
	AgentName     string         `json:"agent_name"`
	Brief         string         `json:"brief,omitempty"`
	Origin        WorkflowOrigin `json:"origin"`
}

// Text is the note as a line, for a renderer that knows no better.
func (t TriggerFired) Text() string {
	by := "trigger " + t.Origin.TriggerKind
	if t.Origin.Schedule != "" {
		by += " " + t.Origin.Schedule
	}
	return fmt.Sprintf("Agent %q prompted by %s", t.AgentName, by)
}

// WorkflowStore persists workflow definitions.
type WorkflowStore struct {
	*CrudStore[Workflow]
}

// NewWorkflowStore returns a WorkflowStore backed by db. Names are unique
// per scope, case-insensitively (partial indexes, decisions §5.29).
func NewWorkflowStore(db *bun.DB) *WorkflowStore {
	return &WorkflowStore{NewCrudStore[Workflow](db, "workflow", "created_at DESC")}
}

// Update overwrites the definition in one transaction that reads the stored
// row (locked) and hands it to prepare (nil to skip), the shape every scoped entity uses.
func (s *WorkflowStore) Update(ctx context.Context, id string, m *Workflow, prepare func(prev *Workflow) error) error {
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return s.updateFrom(ctx, tx, id, m, prepare)
	})
	if err != nil {
		return fmt.Errorf("updating workflow %s: %w", id, err)
	}
	return nil
}

// Delete removes a definition and the triggers that fire it — a trigger with
// no workflow could only fail. Executions keep their snapshot.
func (s *WorkflowStore) Delete(ctx context.Context, id string) error {
	return s.DeleteOwnedBy(ctx, id, "")
}

// DeleteOwnedBy removes the definition and its triggers, in one transaction,
// only while it still belongs to expectOwner (decisions §5.29); an empty
// expectOwner skips the check.
func (s *WorkflowStore) DeleteOwnedBy(ctx context.Context, id, expectOwner string) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if expectOwner != "" {
			wf := new(Workflow)
			if err := lockRow(ctx, tx, wf, "id = ?", id); err != nil {
				return fmt.Errorf("deleting workflow %s: %w", id, err)
			}
			if wf.OwnerID != expectOwner {
				return fmt.Errorf("deleting workflow %s: %w", id, ErrOwnershipChanged)
			}
		}
		if _, err := tx.NewDelete().Model((*Trigger)(nil)).Where("workflow_id = ?", id).Exec(ctx); err != nil {
			return fmt.Errorf("deleting triggers of workflow %s: %w", id, err)
		}
		res, err := tx.NewDelete().Model((*Workflow)(nil)).Where("id = ?", id).Exec(ctx)
		if err == nil {
			err = requireRows(res)
		}
		if err != nil {
			return fmt.Errorf("deleting workflow %s: %w", id, err)
		}
		return nil
	})
}

// NormalizeWorkflow trims the spelling noise and fills each step's stable
// id.
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
	if b := w.Budget; b.MaxSteps < 0 || b.MaxTokens < 0 || b.MaxMinutes < 0 || b.MaxLaps < 0 {
		return fmt.Errorf("budget: a bound cannot be negative")
	}
	if w.Budget.MaxSteps > MaxStepRuns {
		return fmt.Errorf("budget: max_steps cannot exceed %d, the ceiling every execution has", MaxStepRuns)
	}
	if w.Budget.MaxLaps > MaxStepRuns {
		return fmt.Errorf("budget: max_laps cannot exceed %d, the ceiling every execution has", MaxStepRuns)
	}
	seen := make(map[string]bool, len(w.Steps))
	// Names are how the model reads and writes a definition, so a name must
	// denote one step (case-insensitively) and cannot be the "stop" target.
	names := make(map[string]bool, len(w.Steps))
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
		if s.Name != "" {
			if strings.EqualFold(s.Name, WorkflowStepEnd) {
				return fmt.Errorf("step %d: %q is reserved — it is what an edge names to stop there", i+1, WorkflowStepEnd)
			}
			key := strings.ToLower(s.Name)
			if names[key] {
				return fmt.Errorf("step %d: duplicate step name %q", i+1, s.Name)
			}
			names[key] = true
		}
		if s.ID == "" {
			s.ID = NewID()
		}
		// A gate's two words must be one line each and tell apart
		// (case-insensitively, as Verdict compares).
		if g := s.Gate; g != nil {
			if strings.ContainsAny(g.Pass, "\r\n") || strings.ContainsAny(g.Fail, "\r\n") {
				return fmt.Errorf("step %d: a gate word must be one line", i+1)
			}
			// Stored as Verdict compares them: a word that is nothing but
			// the punctuation Verdict strips could never be reported.
			for _, w := range []*string{&g.Pass, &g.Fail} {
				if strings.TrimSpace(*w) != "" && normalizeGateWord(*w) == "" {
					return fmt.Errorf("step %d: gate word %q is only punctuation", i+1, *w)
				}
				*w = normalizeGateWord(*w)
			}
			if strings.EqualFold(g.PassWord(), g.FailWord()) {
				return fmt.Errorf("step %d: the gate's pass and fail words must differ (got %q for both)", i+1, g.PassWord())
			}
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
