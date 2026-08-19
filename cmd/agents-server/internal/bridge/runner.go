package bridge

import (
	"context"
	"errors"
	"fmt"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
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
	// tasks owns the background-task lifecycle. Nil when the server runs without
	// a task store. It keeps no wake-up state of its own: a finished task is
	// reported through OnFinished and the debt lives in wakeups (see Waker).
	tasks *tasks.Manager

	// OnRunAttach, when set, is invoked with the run id right after a run
	// registers in the hub (fresh start and approval resume alike), before any
	// event publishes. The WS layer uses it to attach every live connection to
	// the stream — run events are a broadcast bus, not a reply channel.
	//
	// Written once during bootstrap, read from run goroutines, unsynchronized:
	// nothing that can start a run may launch before it is wired (see cmd.run).
	OnRunAttach func(runID string)
	// OnBroadcast, when set, delivers an event to every connection NOT
	// attached to exceptRunID's stream — for a fact a run stream cannot carry
	// to everyone: a task paused before its step has a run id but no run
	// (README invariant 37), and a run interrupted on an approval is not one a
	// connection joining afterwards attaches to. Empty exceptRunID means every
	// connection. Same wiring rule as OnRunAttach.
	OnBroadcast func(env *protocol.Envelope, exceptRunID string)
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
	if deps.MaxTasks > 0 {
		r.hub.maxTasks = deps.MaxTasks
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
	// Agent building reaches the task tools through the manager, not the
	// runner; the spawn tool is the server's own (a workflow is what it starts
	// when told a name), built per run for the workflows on offer.
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
	SandboxID     string
	WorkDir       string
	// ErrCode/ErrMessage describe a failed run (mirroring the run.error event)
	// so terminal bookkeeping and the synchronous REST response need not have
	// watched the event stream.
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
func (r *Runner) StartRun(sessionID, agentConfigID, sandboxID, workDir, input string, plan *bool, onDone func(*RunOutcome)) (string, error) {
	return r.startRunWithID(store.NewID(), sessionID, agentConfigID, sandboxID, workDir, input, "", plan, onDone)
}

// StartWakeRun is StartRun for a task notification delivery: same launch, plus
// the lineage (the run whose spawn started the chain) the trace records.
func (r *Runner) StartWakeRun(sessionID, agentConfigID, sandboxID, workDir, input, parentRunID string, onDone func(*RunOutcome)) (string, error) {
	return r.startRunWithID(store.NewID(), sessionID, agentConfigID, sandboxID, workDir, input, parentRunID, nil, onDone)
}

// startRunWithID is StartRun with a caller-chosen run id — SpawnTask mints the
// task's run id up front so the row can carry it before the run launches.
func (r *Runner) startRunWithID(runID, sessionID, agentConfigID, sandboxID, workDir, input, wakeParentRunID string, planIntent *bool, onDone func(*RunOutcome)) (string, error) {
	return r.startRunReserved(runID, sessionID, agentConfigID, sandboxID, workDir, input, wakeParentRunID, planIntent, onDone, nil)
}

// startRunReserved is startRunWithID with a hook that runs once the session
// is RESERVED for the run and before it launches — for a write that must
// precede the run's own (a trigger's note before the message it sends) and
// must not happen when the run is refused.
func (r *Runner) startRunReserved(runID, sessionID, agentConfigID, sandboxID, workDir, input, wakeParentRunID string, planIntent *bool, onDone func(*RunOutcome), reserved func()) (string, error) {
	seg, ctx, plan, boundNow, err := r.reserveRun(runID, sessionID, agentConfigID, sandboxID, workDir)
	if err != nil {
		return "", err
	}
	if reserved != nil {
		reserved()
	}
	// The reservation is held (register succeeded): no other run can start on
	// this session now, so setting its plan phase here is atomic with the run
	// that will use it, and a request refused above (busy/limit) never reached
	// this line — its plan intent left the session untouched.
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
		if env, err := protocol.NewEnvelope(protocol.EventSessionSandboxBound, protocol.SessionSandboxBound{
			SessionID: sessionID, SandboxID: plan.sandboxID, WorkDir: plan.workDir,
		}); err == nil {
			r.hub.publish(runID, env)
		}
	}
	r.launchSegment(seg, runID, sessionID, onDone, func() *RunOutcome {
		return r.runStreamed(ctx, runID, sessionID, agentConfigID, plan.sandboxID, plan.workDir, input, wakeParentRunID)
	})
	return runID, nil
}

// launchSegment runs one segment's exec in the background with the shared
// teardown ordering: seg.finalize runs last (via defer) so the session-delete
// wait only unblocks after every write lands. A pause's approval row is
// written inside exec (finishResult), so it exists BEFORE finish releases the
// session slot — otherwise a task completing in between would auto-wake a
// parent that is actually paused on a decision.
func (r *Runner) launchSegment(seg *runSegment, runID, sessionID string, onDone func(*RunOutcome), exec func() *RunOutcome) {
	go func() {
		defer seg.finalize()
		result := exec()
		r.hub.finish(runID, result.Interrupted)
		r.postRun(runID, sessionID, result)
		if onDone != nil {
			onDone(result)
		}
	}()
}

// segmentSpec is what differs between a fresh run segment and a resume
// continuation; execStreamed holds everything they share, so the two cannot
// drift apart in the policies they carry.
type segmentSpec struct {
	// input is the user text announced in run.started and persisted when the
	// segment fails before the SDK's own per-turn save.
	input string
	// wakeParentRunID is a wake-up run's lineage: the run whose spawn started
	// the chain, stamped on every trace span (see wsProcessor.parentRunID).
	// Empty for ordinary runs — and for resumes, which is fine: the fresh
	// segment already wrote spans carrying it, and the panel takes the first
	// one it finds.
	wakeParentRunID string
	// failCode is the fallback error code for a failed stream drain.
	failCode string
	// fresh gates the fresh-run extras: the session pre-check, the
	// run.agent_start announcement, arming the plan-phase unlock, and title
	// generation. A resume reopens a run that already did all four.
	fresh bool
	// start launches the SDK run — agents.Run for a fresh segment,
	// agents.ResumeRun for a continuation.
	start func(ctx context.Context, agent *agents.Agent, opts agents.RunOptions) (agents.RunStream, agents.RunControl)
}

// execStreamed executes one run segment — fresh or resumed — to completion,
// publishing events to the hub, and returns its outcome.
func (r *Runner) execStreamed(ctx context.Context, runID, sessionID, agentConfigID, sandboxID, workDir string, spec segmentSpec) *RunOutcome {
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
	if info, ok := r.hub.Info(runID); ok {
		task = info.Task
	}
	// A resumed segment re-announces the original prompt so a late-joining
	// browser (attached at resume) can render the user bubble; earlier
	// subscribers dedup it against the bubble they already show.
	started := protocol.RunStarted{RunID: runID, SessionID: sessionID, Input: spec.input}
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
		return &RunOutcome{RunID: runID, SessionID: sessionID, AgentConfigID: agentConfigID, SandboxID: sandboxID, WorkDir: workDir}
	}
	mkErrResult := func(code, msg string) *RunOutcome {
		res := mkResult()
		res.ErrCode, res.ErrMessage = code, msg
		return res
	}

	// failCancelled ends the segment as a cancellation: save the abandoned turn,
	// then run.cancelled. savePartialTurn writes on its own context, so the
	// cancellation does not also stop the record of it from landing.
	failCancelled := func(turn partialTurn) *RunOutcome {
		turn.annRole = "cancelled"
		r.savePartialTurn(turn)
		sendEvent(protocol.EventRunCancelled, protocol.RunCancelled{RunID: runID})
		res := mkResult()
		res.Cancelled = true
		return res
	}

	// failLookup ends the segment on a session lookup that did not answer. A
	// cancelled lookup ends like any other cancel, not a red error. Otherwise,
	// matching handler/run.go startError: only ErrNotFound is session_not_found;
	// an unreachable database is a config error, so the client does not give up
	// on a session that is still there.
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

	// failTurn persists the user's prompt and why the segment stopped, then
	// reports it. A fresh run may fail before the SDK's per-turn save, and a
	// failed resume has already consumed the pending-approval row — without this
	// the turn's fate would vanish from durable state on reload. The guardrail
	// name/stage ride along so a reload rebuilds the "Blocked by guardrail X"
	// card, not a generic error.
	failTurn := func(model, code string, err error, partialReasoning, partialText string) *RunOutcome {
		turn := partialTurn{
			sessionID:        sessionID,
			runID:            runID,
			model:            model,
			userInput:        spec.input,
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

	// Refuse to run against a session that doesn't exist — otherwise the run
	// would write orphaned messages under an arbitrary session id.
	if spec.fresh {
		if _, err := r.Deps.Sessions.Get(ctx, sessionID); err != nil {
			return failLookup(err, "session not found: "+sessionID)
		}
	}

	// Build fully configured agent from DB config. A BACKGROUND run — a task's,
	// a workflow step's; both task sessions — is built without the tools and
	// modes that only make sense with a person in front of them.
	built, err := buildFullAgent(ctx, r.Deps, agentConfigID, sandboxID, workDir, task != nil)
	if err != nil {
		return failTurn("", protocol.CodeConfigError, err, "", "")
	}
	// This segment is the build's only holder, so release it whatever the
	// outcome; a resume executes the approval path's own rebuild (see
	// ResolveApproval), released by its own hand.
	defer built.Release()

	// What this build put in front of the model, for the Context panel. Only
	// the build knows it, and only the run has a session to file it under; a
	// failure here costs a panel section, never the run.
	if r.Deps.ContextProfiles != nil {
		if err := r.Deps.ContextProfiles.Save(ctx, sessionID, built.Profile); err != nil {
			log.Warn("failed to record the session's context profile", "error", err)
		}
	}

	agent := built.Agent
	provider := built.Provider
	if provider == nil {
		return failTurn(agent.Model, protocol.CodeConfigError, errors.New("no API key configured for this agent"), "", "")
	}

	// Wrap with router provider if routes exist
	provider = BuildRouterProvider(ctx, r.Deps, provider)

	if spec.fresh {
		sendEvent(protocol.EventRunAgentStart, protocol.RunAgentStart{RunID: runID, AgentName: agent.Name})
	}

	sessionRef, refErr := store.RefFor(ctx, r.db, sessionID)
	if refErr != nil {
		return failLookup(refErr, "cannot resolve this session's history")
	}
	sa := store.NewEntryStoreFor(r.db, sessionRef)
	sa.SetRunID(runID)
	sa.SetModel(agent.Model)
	if spec.fresh {
		// The session's plan phase, not this run's: an approved plan is not
		// re-asked next turn. Also arms the marker persistence for an unlock
		// this run may perform. Fresh-only: a resume runs the ResolveApproval
		// rebuild, whose phase restorePlanPhase already handled; THIS build's
		// phase hangs off an agent the resume never runs.
		if err := r.restorePlanPhase(ctx, built.PlanPhase, sa, sessionRef); err != nil {
			return failTurn("", protocol.CodeConfigError, err, "", "")
		}
	}
	tracer := newTracer(ctx, sendEvent, r.Deps.Traces, sessionID, runID, spec.wakeParentRunID, spanDataCap(ctx, r.Deps.Settings))

	runSession := wrapCompaction(sa, built, provider, sendEvent, runID)

	opts := runOptionsFor(built, runSession, provider, tracer, trustSessionID(sessionID, task), logging.Ctx(ctx))

	// Name the session in parallel with the run — the title needs only the
	// user's first message, not the answer. Task sessions are pre-named and
	// hidden; a resume's original run already fired it at its start.
	if spec.fresh && task == nil {
		go r.maybeGenerateTitle(r.hub.rootCtx, sessionID, agent.Model, spec.input, provider, sendEvent)
	}

	stream, ctrl := spec.start(ctx, agent, opts)
	r.hub.setControl(runID, ctrl)
	// The stream carries both halves of the outcome: the run's result as its
	// terminal event, or a terminal error. There is no second place to consult.
	res, streamedText, streamedReasoning, err := r.drainStream(stream, runID, sendEvent)
	if err != nil {
		return failTurn(agent.Model, spec.failCode, err, streamedReasoning, streamedText)
	}

	out, err := r.finishResult(res, runID, sessionID, agentConfigID, sandboxID, workDir, sendEvent)
	if err != nil {
		// The pause could not be made durable: a decision would have nothing
		// to act on (README invariant 37 lists what an approval IS — a row).
		// The segment ends as a failure instead, which a person can retry;
		// nothing was announced as awaiting them.
		return failTurn(agent.Model, "persist_error", err, streamedReasoning, streamedText)
	}
	return out
}

// runStreamed executes one fresh run segment to completion, publishing events
// to the hub, and returns its outcome.
func (r *Runner) runStreamed(ctx context.Context, runID, sessionID, agentConfigID, sandboxID, workDir, input, wakeParentRunID string) *RunOutcome {
	return r.execStreamed(ctx, runID, sessionID, agentConfigID, sandboxID, workDir, segmentSpec{
		input:           input,
		wakeParentRunID: wakeParentRunID,
		failCode:        "stream_error",
		fresh:           true,
		start: func(ctx context.Context, agent *agents.Agent, opts agents.RunOptions) (agents.RunStream, agents.RunControl) {
			// An empty input means "continue from where the session's branch
			// now points" — what regenerating does after switching back to the
			// user's message. Passing "" through would append an empty user
			// turn, so the run gets an empty ITEM LIST instead: nothing to
			// add, history to answer.
			var runInput any = input
			if input == "" {
				runInput = []agents.InputItem{}
			}
			return agents.Run(ctx, agent, runInput, opts)
		},
	})
}

// ResumeRun registers a continuation of a paused run (after HITL
// approval/rejection) and launches it in the background under the hub root
// context, returning the run id. onDone, if non-nil, fires once when the
// continuation terminates. Fails with ErrSessionBusy if the session has a live
// run. The resume reopens the SAME hub run (same event stream and sequence), so
// one logical run keeps one id across interrupt/resume.
// verify, when non-nil, runs AFTER the run is registered (so it is live and a
// concurrent stop's cancel can find it) but BEFORE the goroutine launches. If it
// returns an error the run is withdrawn and nothing executes — this is what
// closes the window where an approved tool could run and cause a side effect
// before a post-launch recheck could cancel it.
func (r *Runner) ResumeRun(runID string, state *agents.RunState, sessionID, agentConfigID, sandboxID, workDir string, verify func() error, onDone func(*RunOutcome)) (string, error) {
	meta, err := r.taskMeta(r.hub.rootCtx, sessionID)
	if err != nil {
		return "", err
	}
	seg, ctx, reopened, err := r.hub.resume(runID, sessionID, agentConfigID, sandboxID, workDir, meta)
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
		return r.resumeStreamed(ctx, runID, state, sessionID, agentConfigID, sandboxID, workDir)
	})
	return runID, nil
}

// resumeStreamed continues an interrupted run to completion under its original
// run id, publishing events to the (reopened) hub run, and returns its outcome.
// It streams through the same execStreamed pipeline as a fresh run so the
// resumed segment's events (the approved tool's output, later turns) go live
// instead of surfacing only in the terminal run.output.
func (r *Runner) resumeStreamed(ctx context.Context, runID string, state *agents.RunState, sessionID, agentConfigID, sandboxID, workDir string) *RunOutcome {
	return r.execStreamed(ctx, runID, sessionID, agentConfigID, sandboxID, workDir, segmentSpec{
		input:    session.UserText(state.UserInput),
		failCode: "resume_error",
		start: func(ctx context.Context, _ *agents.Agent, opts agents.RunOptions) (agents.RunStream, agents.RunControl) {
			return agents.ResumeRun(ctx, state, opts)
		},
	})
}

// finishResult turns a finished SDK result into the segment's outcome. A pause
// on approval is made DURABLE first — the pending-approval row is what a
// decision acts on, from any connection and across a restart — and only then
// announced; a persistence failure is returned, and the segment fails instead
// of pausing on nothing.
func (r *Runner) finishResult(res *agents.RunResult, runID, sessionID, agentConfigID, sandboxID, workDir string, sendEvent func(string, any)) (*RunOutcome, error) {
	if len(res.Interruptions) > 0 {
		out := &RunOutcome{
			RunID:         runID,
			SessionID:     sessionID,
			AgentConfigID: agentConfigID,
			SandboxID:     sandboxID,
			WorkDir:       workDir,
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
		// Terminal marker for this run segment: waiters and SSE streams end
		// here; the approval decision reopens this same run id and continues
		// its event sequence.
		sendEvent(protocol.EventRunInterrupted, protocol.RunInterrupted{RunID: runID})
		return out, nil
	}

	r.bindSessionAgent(sessionID, agentConfigID)

	finalText := res.FinalOutputString()
	sendEvent(protocol.EventRunOutput, protocol.RunOutput{RunID: runID, FinalOutput: finalText})
	return &RunOutcome{FinalText: finalText, RunID: runID, SessionID: sessionID, AgentConfigID: agentConfigID, SandboxID: sandboxID, WorkDir: workDir}, nil
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
