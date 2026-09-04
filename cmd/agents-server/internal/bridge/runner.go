package bridge

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/attachments"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// EventSink receives protocol envelopes emitted during a streamed run.
type EventSink func(env *protocol.Envelope)

// Runner executes streamed agent runs. Run lifecycle, cancellation, event
// buffering, and fan-out are delegated to the hub, so a run outlives the
// connection that started it.
type Runner struct {
	db   *bun.DB
	Deps *AgentDeps
	hub  *RunHub
	// tasks owns the background-task lifecycle; nil without a task store. It
	// keeps no wake-up state: the debt lives in wakeups (see Waker).
	tasks *tasks.Manager

	// OnRunAttach, when set, runs with the run id right after a run registers in
	// the hub (fresh start or resume), before any publish — invariant 14. Written
	// once at bootstrap, read unsynchronized: wire it before anything can start
	// a run (invariant 32).
	OnRunAttach func(runID string)
	// OnBroadcast, when set, delivers an event about sessionID to every
	// connection of its owner NOT attached to exceptRunID's stream ("" = all of
	// them) — for a fact no run stream reaches everyone with (invariant 37).
	// Same wiring rule as OnRunAttach.
	OnBroadcast func(env *protocol.Envelope, exceptRunID, sessionID string)
}

// NewRunner creates a Runner backed by the given database and agent
// dependencies. rootCtx scopes every run's lifetime (see RunHub); cancelling
// it stops all in-flight runs.
func NewRunner(rootCtx context.Context, db *bun.DB, deps *AgentDeps) *Runner {
	r := &Runner{
		db:   db,
		Deps: deps,
		hub:  NewRunHub(rootCtx),
	}
	// The per-parent task cap is a live setting, resolved at each check by both
	// gates: the hub's register and the task manager's spawn/retry.
	if deps.Settings != nil {
		r.hub.maxTasks = func() int { return deps.Settings.Int(rootCtx, settings.KeyMaxTasksPerSession) }
	}
	if deps.Tasks != nil {
		r.tasks = tasks.New(tasks.Config{
			Store: store.NewTaskAdapter(deps.Tasks),
			Sessions: store.NewSessionRepoAdapter(deps.Sessions, func(ref session.Ref) session.Storage {
				return store.NewEntryStoreFor(db, ref)
			}),
			Resolver:               taskResolver{r}.Resolve,
			Launcher:               taskLauncher{r}.Launch,
			Stopper:                taskStopper{r}.Stop,
			OnFinished:             r.taskFinished,
			OnResultDelivered:      r.taskResultDelivered,
			MaxConcurrentPerParent: r.hub.maxTasks,
			OnTaskUpdate:           r.onTaskUpdate,
			NewID:                  store.NewID,
			// A workflow execution is a task of several runs; this is what
			// moves it from step to step.
			Continue: r.continueTask,
			// …and this is how task_status says where one stands.
			DescribeState: describeTaskState,
		})
	}
	// Agent building reaches the task tools through the manager; the spawn and
	// workflow tools are the server's own, built per run.
	deps.TaskManager = r.tasks
	deps.SpawnTool = r.spawnTool
	deps.WorkflowTools = r.workflowTools
	return r
}

// Tasks exposes the task manager so handlers and the startup path can reach it.
func (r *Runner) Tasks() *tasks.Manager { return r.tasks }

// Hub exposes the run hub so handlers can subscribe to run events, query
// status, and cancel runs.
func (r *Runner) Hub() *RunHub { return r.hub }

// RunOutcome is how one run SEGMENT ended, in the terms the server's terminal
// bookkeeping needs: the final text or the failure, plus the interruption an
// approval decision resumes from. Distinct from agents.RunResult, the SDK's
// result of a finished run; finishResult turns one into the other.
type RunOutcome struct {
	FinalText     string
	RunID         string
	SessionID     string
	AgentConfigID string
	ProjectID     string
	// ErrCode/ErrMessage mirror the run.error event, so terminal bookkeeping
	// and the synchronous REST response need not watch the stream.
	ErrCode    string
	ErrMessage string
	// Cancelled mirrors the run.cancelled event: the run ended by request, so
	// it carries neither a final output nor an error.
	Cancelled     bool
	Interrupted   bool
	Interruptions []*agents.ToolApprovalItem
	SDKState      *agents.RunState
}

// StartRun registers a new run for the session and launches it in the
// background under the hub's root context (so it survives the connection that
// started it). It returns the run id; subscribe via Hub() to stream events.
// onDone, if non-nil, is invoked once when the run terminates. It fails with
// ErrSessionBusy when the session already has a live run.
func (r *Runner) StartRun(sessionID, agentConfigID, projectID string, input RunInput, plan *bool, onDone func(*RunOutcome)) (string, error) {
	return r.startRunWithID(store.NewID(), sessionID, agentConfigID, projectID, input, "", plan, onDone)
}

// StartWakeRun is StartRun for a task notification delivery: same launch, plus
// the lineage (the run whose spawn started the chain) the trace records.
func (r *Runner) StartWakeRun(sessionID, agentConfigID, projectID, input, parentRunID string, onDone func(*RunOutcome)) (string, error) {
	return r.startRunWithID(store.NewID(), sessionID, agentConfigID, projectID, TextInput(input), parentRunID, nil, onDone)
}

// startRunWithID is StartRun with a caller-chosen run id: a task's row carries
// its run id before the run launches.
func (r *Runner) startRunWithID(runID, sessionID, agentConfigID, projectID string, input RunInput, wakeParentRunID string, planIntent *bool, onDone func(*RunOutcome)) (string, error) {
	return r.startRunReserved(runID, sessionID, agentConfigID, projectID, input, wakeParentRunID, planIntent, onDone, nil)
}

// startRunReserved is startRunWithID with a hook run once the session is
// RESERVED and before the launch — a write that must precede the run's own.
func (r *Runner) startRunReserved(runID, sessionID, agentConfigID, projectID string, input RunInput, wakeParentRunID string, planIntent *bool, onDone func(*RunOutcome), reserved func()) (string, error) {
	seg, ctx, plan, boundNow, err := r.reserveRun(runID, sessionID, agentConfigID, projectID)
	if err != nil {
		return "", err
	}
	if reserved != nil {
		reserved()
	}
	// The slot is held, so the plan phase is set atomically with the run using
	// it; a request refused above left the session's phase untouched.
	if err := r.ApplyPlanIntent(r.hub.rootCtx, sessionID, planIntent); err != nil {
		r.hub.unregister(runID, seg)
		return "", err
	}
	if r.OnRunAttach != nil {
		r.OnRunAttach(runID)
	}
	// After register+OnRunAttach so every live connection is attached to this
	// run's stream and receives the announcement (replayed to late joiners).
	if boundNow {
		if env, err := protocol.NewEnvelope(protocol.EventSessionProjectBound, protocol.SessionProjectBound{
			SessionID: sessionID, ProjectID: plan.projectID,
		}); err == nil {
			r.hub.publish(runID, env)
		}
	}
	r.launchSegment(seg, runID, sessionID, onDone, func() *RunOutcome {
		return r.runStreamed(ctx, runID, sessionID, agentConfigID, plan.projectID, input, wakeParentRunID)
	})
	return runID, nil
}

// launchSegment runs one segment's exec in the background. Order matters: the
// approval row lands inside exec, before finish frees the slot; finalize runs last.
func (r *Runner) launchSegment(seg *runSegment, runID, sessionID string, onDone func(*RunOutcome), exec func() *RunOutcome) {
	go func() {
		defer seg.finalize()
		// Last-resort recover (exec recovers its own panics): free the session
		// slot — a leaked slot bricks the session — and keep the process.
		defer func() {
			if p := recover(); p != nil {
				logging.Ctx(r.hub.rootCtx).Error("run teardown panicked", "run_id", runID, "panic", p, "stack", string(debug.Stack()))
				r.hub.finish(runID, false)
			}
		}()
		result := exec()
		r.hub.finish(runID, result.Interrupted)
		r.postRun(runID, sessionID, result)
		if onDone != nil {
			onDone(result)
		}
	}()
}

// segmentSpec is what differs between a fresh segment and a resume
// continuation; execStreamed holds everything they share.
type segmentSpec struct {
	// input is the user text announced in run.started and persisted when the
	// segment fails before the SDK's own per-turn save.
	input string
	// attachmentIDs are the message's image attachments (fresh runs only;
	// resumes re-announce text alone — the entries already hold the images).
	attachmentIDs []string
	// wakeParentRunID is a wake-up run's lineage, stamped on every trace span
	// (see wsProcessor.parentRunID). Empty for ordinary runs and resumes.
	wakeParentRunID string
	// failCode is the fallback error code for a failed stream drain.
	failCode string
	// fresh gates the fresh-run extras (session pre-check, run.agent_start,
	// arming the plan unlock, title generation); a resume already did them.
	fresh bool
	// start launches the SDK run — agents.Run for a fresh segment,
	// agents.ResumeRun for a continuation.
	start func(ctx context.Context, agent *agents.Agent, opts agents.RunOptions) (agents.RunStream, agents.RunControl)
}

// execStreamed executes one run segment — fresh or resumed — to completion,
// publishing events to the hub, and returns its outcome.
func (r *Runner) execStreamed(ctx context.Context, runID, sessionID, agentConfigID, projectID string, spec segmentSpec) (out *RunOutcome) {
	log := logging.Ctx(ctx)
	// Stamp the run id so a spawn_task inside the run records which run spawned
	// it — that is what lets the trace panel nest the task's wake-up run here.
	ctx = tasks.WithParentRunID(ctx, runID)

	sendEvent := func(typ string, payload any) {
		env, err := protocol.NewEnvelope(typ, payload)
		if err != nil {
			log.Error("marshal event", "error", err, "type", typ)
			return
		}
		r.hub.publish(runID, env)
	}

	// From the hub record, not a second store lookup: register/resume already
	// resolved it, so this cannot disagree with what the run registered as.
	var task *TaskMeta
	var ownerID string
	if info, ok := r.hub.Info(runID); ok {
		task, ownerID = info.Task, info.OwnerID
	}
	// Attachments are validated before anything is announced; the metadata
	// also feeds run.started so clients render thumbnails without a request.
	attMeta, attErr := r.validateAttachments(ctx, ownerID, spec.attachmentIDs)

	// A resume re-announces the prompt so a browser attached at resume can
	// render the user bubble; earlier subscribers dedup it.
	started := protocol.RunStarted{RunID: runID, SessionID: sessionID, Input: spec.input}
	if attErr == nil && len(spec.attachmentIDs) > 0 {
		base := r.Deps.Settings.S3Config(ctx).PublicBaseURL
		for _, id := range spec.attachmentIDs {
			a := attMeta[id]
			started.Attachments = append(started.Attachments, protocol.AttachmentRef{
				ID: a.ID, URL: attachments.PublicURL(base, a.Key),
			})
		}
	}
	if task != nil {
		started.ParentSessionID = task.ParentSessionID
		started.ParentRunID = task.ParentRunID
		started.TaskID = task.TaskID
		started.ToolCallID = task.ToolCallID
		started.Label = task.Label
		started.Kind = task.Kind
		started.Attempt = task.Attempt
		started.MaxAttempts = task.MaxAttempts
	}
	sendEvent(protocol.EventRunStarted, started)

	mkResult := func() *RunOutcome {
		return &RunOutcome{RunID: runID, SessionID: sessionID, AgentConfigID: agentConfigID, ProjectID: projectID}
	}
	mkErrResult := func(code, msg string) *RunOutcome {
		res := mkResult()
		res.ErrCode, res.ErrMessage = code, msg
		return res
	}

	// failCancelled ends the segment as a cancellation: save the abandoned
	// turn (on its own context), then run.cancelled.
	failCancelled := func(turn partialTurn) *RunOutcome {
		turn.userAttachments = spec.attachmentIDs
		turn.annRole = "cancelled"
		r.savePartialTurn(turn)
		sendEvent(protocol.EventRunCancelled, protocol.RunCancelled{RunID: runID})
		res := mkResult()
		res.Cancelled = true
		return res
	}

	// failLookup ends the segment on a failed session lookup: a cancel is a
	// cancel; only ErrNotFound is session_not_found (as handler startError).
	failLookup := func(err error, absentMsg string) *RunOutcome {
		if isCancellation(ctx, err) {
			return failCancelled(partialTurn{sessionID: sessionID, runID: runID, userInput: spec.input})
		}
		code, msg := protocol.CodeConfigError, err.Error()
		if errors.Is(err, store.ErrNotFound) {
			code, msg = protocol.CodeSessionNotFound, absentMsg
		}
		sendEvent(protocol.EventRunError, protocol.RunError{RunID: runID, Code: code, Message: msg})
		return mkErrResult(code, msg)
	}

	// failTurn persists the prompt and why the segment stopped (the SDK may not
	// have saved the turn; a failed resume consumed its row), then reports it.
	failTurn := func(model, code string, err error, partialReasoning, partialText string) *RunOutcome {
		turn := partialTurn{
			sessionID:        sessionID,
			runID:            runID,
			model:            model,
			userInput:        spec.input,
			userAttachments:  spec.attachmentIDs,
			partialReasoning: partialReasoning,
			partialText:      partialText,
		}
		if isCancellation(ctx, err) {
			return failCancelled(turn)
		}
		gerr := runErrorFor(runID, err, code)
		turn.annRole, turn.annMsg = "error", err.Error()
		turn.guardrail, turn.stage = gerr.Guardrail, gerr.Stage
		r.savePartialTurn(turn)
		sendEvent(protocol.EventRunError, gerr)
		return mkErrResult(gerr.Code, err.Error())
	}

	// A panic below fails THIS segment, not the process; recovered here so
	// failTurn records it durably with whatever the stream had shown.
	var partial streamedPartial
	defer func() {
		if p := recover(); p != nil {
			log.Error("run panicked", "run_id", runID, "panic", p, "stack", string(debug.Stack()))
			out = failTurn("", protocol.CodeInternal, fmt.Errorf("internal error: %v", p), partial.Reasoning(), partial.Text())
		}
	}()

	if attErr != nil {
		return failTurn("", protocol.CodeConfigError, attErr, "", "")
	}

	// Refuse to run against a session that doesn't exist — otherwise the run
	// would write orphaned messages under an arbitrary session id.
	if spec.fresh {
		if _, err := r.Deps.Sessions.Get(ctx, sessionID); err != nil {
			return failLookup(err, "session not found: "+sessionID)
		}
	}

	// A BACKGROUND run (a task's, a workflow step's) is built without the tools
	// and modes that need a person in front of it — invariant 34.
	built, err := buildFullAgent(ctx, r.Deps, agentConfigID, projectID, task != nil, ownerID)
	if err != nil {
		return failTurn("", protocol.CodeConfigError, err, "", "")
	}
	// This segment is the build's only holder; a resume releases the approval
	// path's own rebuild (see ResolveApproval).
	defer built.Release()

	// The build's prompt profile, for the Context panel; a failure costs a
	// panel section, never the run.
	if r.Deps.ContextProfiles != nil {
		if err := r.Deps.ContextProfiles.Save(ctx, sessionID, built.Profile); err != nil {
			log.Warn("failed to record the session's context profile", "error", err)
		}
	}

	agent := built.Agent
	provider := built.Provider

	if len(spec.attachmentIDs) > 0 {
		if !built.Behavior.Vision {
			return failTurn(agent.Model, protocol.CodeConfigError,
				errors.New("this agent does not accept images — enable Vision in its Behavior settings"), "", "")
		}
		if !r.Deps.Settings.S3Config(ctx).Complete() {
			return failTurn(agent.Model, protocol.CodeConfigError,
				errors.New("image attachments are not configured — an admin must fill the Attachment storage settings"), "", "")
		}
		// Bound NOW: a run paused on an approval can outlive the orphan
		// reaper's grace window, and a bound row is what the reaper leaves alone.
		if err := r.Deps.Attachments.MarkBound(ctx, spec.attachmentIDs); err != nil {
			return failTurn(agent.Model, "persist_error", err, "", "")
		}
	}

	if provider == nil {
		return failTurn(agent.Model, protocol.CodeConfigError, errors.New("no API key configured for this agent"), "", "")
	}

	if spec.fresh {
		sendEvent(protocol.EventRunAgentStart, protocol.RunAgentStart{RunID: runID, AgentName: agent.Name, AgentConfigID: agentConfigID})
	}

	sessionRef, refErr := store.RefFor(ctx, r.db, sessionID)
	if refErr != nil {
		return failLookup(refErr, "cannot resolve this session's history")
	}
	sa := store.NewEntryStoreFor(r.db, sessionRef)
	sa.SetRunID(runID)
	sa.SetModel(agent.Model)
	if spec.fresh {
		// The SESSION's plan phase (invariant 33), and the unlock this run may
		// perform. Fresh-only: a resume's rebuild already restored it.
		if err := r.restorePlanPhase(ctx, built.PlanPhase, sa, sessionRef); err != nil {
			return failTurn("", protocol.CodeConfigError, err, "", "")
		}
	}
	tracer := newTracer(ctx, sendEvent, r.Deps.Traces, sessionID, runID, spec.wakeParentRunID, r.Deps.Settings.SpanDataCap(ctx))

	runSession := wrapCompaction(sa, built, provider, sendEvent, runID)

	opts := runOptionsFor(built, runSession, provider, tracer, trustSessionID(sessionID, task), logging.Ctx(ctx))

	// The title needs only the first message, so it runs beside the run. Task
	// sessions are pre-named; a resume's original run already fired it.
	if spec.fresh && task == nil {
		go r.maybeGenerateTitle(r.hub.rootCtx, sessionID, agent.Model, spec.input, provider, sendEvent)
	}

	stream, ctrl := spec.start(ctx, agent, opts)
	r.hub.setControl(runID, ctrl)
	// The stream carries both halves of the outcome: the run's result as its
	// terminal event, or a terminal error. There is no second place to consult.
	res, err := r.drainStream(stream, runID, sendEvent, &partial, built.AgentIDs)
	streamedText, streamedReasoning := partial.Text(), partial.Reasoning()
	if err != nil {
		return failTurn(agent.Model, spec.failCode, err, streamedReasoning, streamedText)
	}

	out, err = r.finishResult(res, runID, sessionID, agentConfigID, projectID, sendEvent)
	if err != nil {
		// The pause could not be made durable (invariant 37: an approval IS a
		// row): fail the segment instead, retryable; nothing was announced.
		return failTurn(agent.Model, "persist_error", err, streamedReasoning, streamedText)
	}
	return out
}

// runStreamed executes one fresh run segment to completion, publishing events
// to the hub, and returns its outcome.
func (r *Runner) runStreamed(ctx context.Context, runID, sessionID, agentConfigID, projectID string, input RunInput, wakeParentRunID string) *RunOutcome {
	return r.execStreamed(ctx, runID, sessionID, agentConfigID, projectID, segmentSpec{
		input:           input.Text,
		attachmentIDs:   input.AttachmentIDs,
		wakeParentRunID: wakeParentRunID,
		failCode:        "stream_error",
		fresh:           true,
		start: func(ctx context.Context, agent *agents.Agent, opts agents.RunOptions) (agents.RunStream, agents.RunControl) {
			// Empty input means "continue from the branch point" (regenerate):
			// an empty ITEM LIST, so no empty user turn is appended.
			var runInput any = input.Text
			if len(input.AttachmentIDs) > 0 {
				runInput = input.items()
			} else if input.Text == "" {
				runInput = []agents.InputItem{}
			}
			return agents.Run(ctx, agent, runInput, opts)
		},
	})
}

// ResumeRun registers a continuation of a paused run and launches it in the
// background under the hub root context, reopening the SAME hub run (one id,
// one event sequence across interrupt/resume). onDone fires once when the
// continuation terminates. Fails with ErrSessionBusy if the session has a live
// run. verify, when non-nil, runs AFTER the run is registered (a concurrent
// stop's cancel can find it) but BEFORE the goroutine launches: an error
// withdraws the run and nothing executes, so an approved tool cannot cause a
// side effect ahead of a recheck.
func (r *Runner) ResumeRun(runID string, state *agents.RunState, sessionID, agentConfigID, projectID string, verify func() error, onDone func(*RunOutcome)) (string, error) {
	meta, err := r.taskMeta(r.hub.rootCtx, sessionID)
	if err != nil {
		return "", err
	}
	sess, err := r.Deps.Sessions.Get(r.hub.rootCtx, sessionID)
	if err != nil {
		return "", err
	}
	seg, ctx, reopened, err := r.hub.resume(runID, sessionID, sess.OwnerID, agentConfigID, projectID, meta)
	if err != nil {
		return "", err
	}
	if verify != nil {
		if verr := verify(); verr != nil {
			// Withdraw, don't unregister: a reopened record still has its
			// history and attached subscribers, and goes back to interrupted.
			r.hub.abortResume(runID, seg, reopened)
			return "", verr
		}
	}
	if r.OnRunAttach != nil {
		r.OnRunAttach(runID)
	}
	r.launchSegment(seg, runID, sessionID, onDone, func() *RunOutcome {
		return r.resumeStreamed(ctx, runID, state, sessionID, agentConfigID, projectID)
	})
	return runID, nil
}

// resumeStreamed continues an interrupted run under its original run id
// through execStreamed, so the resumed segment's events go live too.
func (r *Runner) resumeStreamed(ctx context.Context, runID string, state *agents.RunState, sessionID, agentConfigID, projectID string) *RunOutcome {
	return r.execStreamed(ctx, runID, sessionID, agentConfigID, projectID, segmentSpec{
		input:    session.UserText(state.UserInput),
		failCode: "resume_error",
		start: func(ctx context.Context, _ *agents.Agent, opts agents.RunOptions) (agents.RunStream, agents.RunControl) {
			return agents.ResumeRun(ctx, state, opts)
		},
	})
}

// finishResult turns a finished SDK result into the outcome. A pause is made
// DURABLE (the approval row) before it is announced; a failed write fails it.
func (r *Runner) finishResult(res *agents.RunResult, runID, sessionID, agentConfigID, projectID string, sendEvent func(string, any)) (*RunOutcome, error) {
	if len(res.Interruptions) > 0 {
		out := &RunOutcome{
			RunID:         runID,
			SessionID:     sessionID,
			AgentConfigID: agentConfigID,
			ProjectID:     projectID,
			Interrupted:   true,
			Interruptions: res.Interruptions,
			SDKState:      res.State,
		}
		if err := r.persistInterruption(out); err != nil {
			return nil, fmt.Errorf("recording the pending approval: %w", err)
		}
		for _, item := range res.Interruptions {
			sendEvent(protocol.EventRunToolCall, protocol.RunToolCall{
				RunID:         runID,
				ToolCallID:    item.CallID,
				ToolName:      item.ToolName,
				Arguments:     item.Arguments,
				NeedsApproval: true,
			})
		}
		// Terminal for this segment: waiters and SSE streams end here; the
		// decision reopens this run id and continues its sequence.
		sendEvent(protocol.EventRunInterrupted, protocol.RunInterrupted{RunID: runID})
		return out, nil
	}

	r.bindSessionAgent(sessionID, agentConfigID)

	finalText := res.FinalOutputString()
	sendEvent(protocol.EventRunOutput, protocol.RunOutput{RunID: runID, FinalOutput: finalText})
	return &RunOutcome{FinalText: finalText, RunID: runID, SessionID: sessionID, AgentConfigID: agentConfigID, ProjectID: projectID}, nil
}

// StopRunAfterTurn asks the in-flight run to stop gracefully after its current
// turn (tools + session save) instead of aborting mid-turn. Falls back to a hard
// cancel when the run has no live stop hook (e.g. between turns).
func (r *Runner) StopRunAfterTurn(runID string) {
	if !r.hub.StopAfterTurn(runID) {
		r.hub.Cancel(runID)
	}
}

// CancelRun cancels the in-flight run with the given run id, if one is active.
func (r *Runner) CancelRun(runID string) {
	r.hub.Cancel(runID)
}
