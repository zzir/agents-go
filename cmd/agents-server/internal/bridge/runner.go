package bridge

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/agents/session"
	"github.com/zzir/agents-go/agents/tasks"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/tracing"
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
	// tasks owns the background-task lifecycle. Nil when the server runs
	// without a task store. Task completion wake-ups keep no in-memory state
	// here: the notification debt lives on the tasks row (notify_state),
	// written atomically with the terminal status, and is drained by
	// DrainPendingTaskNotifications.
	tasks *tasks.Manager

	// OnRunAttach, when set, is invoked with the run id right after a run
	// registers in the hub (fresh start and approval resume alike), before any
	// event publishes. The WS layer uses it to attach every live connection to
	// the stream — run events are a broadcast bus, not a reply channel to
	// whoever started the run — and runs created over REST take the same path.
	//
	// Written once during bootstrap and read from run goroutines, with no
	// synchronization: nothing that can start a run may be launched before it
	// is wired (see cmd.run's ordering).
	OnRunAttach func(runID string)
}

// compactionNotifier drives the chat UI's live indicator with transient
// run.compaction status events. Trace recording is the compaction span's job
// (opened by the SDK runner via CompactionArgs.StartSpan), not the notifier's.
func compactionNotifier(send func(string, any), runID string) store.CompactionNotifier {
	return store.CompactionNotifier{
		OnStart: func() {
			send(protocol.EventRunCompaction, protocol.RunCompaction{RunID: runID, Phase: "started"})
		},
		OnDone: func(before, after int) {
			send(protocol.EventRunCompaction, protocol.RunCompaction{
				RunID:  runID,
				Phase:  "finished",
				Detail: fmt.Sprintf("compacted %d→%d items", before, after),
			})
		},
	}
}

// runOptionsFor assembles the RunOptions shared by the fresh-run and resume
// paths. One constructor, deliberately: a resume continues the same run and
// must carry the same policies, and two hand-maintained literals are exactly
// how the resume path once dropped HandoffInputFilter and
// ToolNotFoundBehavior. runContext is the Context value (the exec_command
// approval gate reads a trusted session id from it).
func runOptionsFor(built *BuildResult, sess *session.Session, provider agents.ModelProvider, tracer *tracing.Tracer, runContext any) agents.RunOptions {
	opts := agents.RunOptions{
		Context: runContext,
		Conversation: agents.ConversationOptions{
			Session: sess,
			// A non-positive HistoryLimit already means "no limit" on both
			// sides, so it needs no translation.
			Settings: session.Settings{Limit: built.HistoryLimit},
		},
		Exec: agents.ExecOptions{
			MaxTurns:              built.MaxTurns,
			MaxToolConcurrency:    built.MaxToolConcurrency,
			ErrorHandlers:         built.ErrorHandlers,
			ReasoningItemIDPolicy: built.ReasoningItemIDPolicy,
			ToolNotFoundBehavior:  agents.ParseToolNotFoundBehavior(built.ToolNotFoundBehavior),
			ShouldStopAfterTurn:   stopAtTools(built.StopAtTools),
			// Context overflow → forced compaction pass → retry the turn. Only
			// bites when the session is compaction-aware (the agent has
			// compaction enabled); otherwise recovery finds nothing to shrink
			// and the overflow reports as before (spec §2.5g).
			Overflow: agents.OverflowPolicy{MaxRetries: 2},
		},
		Guardrails: built.RunGuardrails,
		Model:      agents.ModelOptions{Provider: provider},
		Observe:    agents.ObserveOptions{Tracer: tracer, IncludeSensitiveData: built.TraceIncludeSensitive},
	}
	if built.HandoffInputFilter == "nest_history" {
		opts.Exec.HandoffInputFilter = agents.NestHandoffHistory(agents.NestHistoryOptions{})
	}
	return opts
}

// wrapCompaction wraps sa with the compaction adapter when the agent config
// enables it. An empty summary model falls back to the agent's own model, so
// leaving the field blank does not silently disable compaction.
func wrapCompaction(sa *store.EntryStore, built *BuildResult, provider agents.ModelProvider, send func(string, any), runID string) *session.Session {
	if !built.CompactionEnabled || provider == nil {
		return session.NewSession(sa)
	}
	modelName := built.CompactionModel
	modelName = cmp.Or(modelName, built.Agent.Model)
	summaryModel, err := provider.Model(modelName)
	if err != nil || summaryModel == nil {
		return session.NewSession(sa)
	}
	return session.NewSession(store.NewCompactionAdapter(sa, summaryModel,
		built.CompactionThreshold, built.CompactionWindow, built.CompactionPrompt,
		compactionNotifier(send, runID),
	))
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
			Resolver:               taskResolver{r},
			Launcher:               taskLauncher{r},
			Stopper:                taskStopper{r},
			Guard:                  taskWakeGuard{r},
			MaxConcurrentPerParent: r.hub.maxTasks,
			OnTaskUpdate:           r.onTaskUpdate,
			NewID:                  store.NewID,
		})
	}
	// Agent building reaches the task tools through the manager, not the runner.
	deps.TaskManager = r.tasks
	return r
}

// Tasks exposes the task manager so handlers and the startup path can reach it.
func (r *Runner) Tasks() *tasks.Manager { return r.tasks }

// Hub exposes the run hub so handlers can subscribe to run events, query
// status, and cancel runs.
func (r *Runner) Hub() *RunHub { return r.hub }

// RunOutcome is how one run SEGMENT ended, in the terms the server's terminal
// bookkeeping needs: the final text or the failure, plus the interruption an
// approval decision resumes from. It is deliberately not named after
// agents.RunResult — that is the SDK's result of a finished run, and
// finishResult is what turns one into the other.
type RunOutcome struct {
	FinalText     string
	RunID         string
	SessionID     string
	AgentConfigID string
	SandboxID     string
	// ErrCode/ErrMessage describe a failed run (mirroring the run.error event)
	// so terminal bookkeeping — a task row's failure reason above all, and the
	// synchronous REST path's response — does not depend on having watched the
	// event stream.
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
func (r *Runner) StartRun(sessionID, agentConfigID, sandboxID, input string, onDone func(*RunOutcome)) (string, error) {
	return r.startRunWithID(store.NewID(), sessionID, agentConfigID, sandboxID, input, onDone)
}

// startRunWithID is StartRun with a caller-chosen run id — SpawnTask mints the
// task's run id up front so the row can carry it before the run launches.
func (r *Runner) startRunWithID(runID, sessionID, agentConfigID, sandboxID, input string, onDone func(*RunOutcome)) (string, error) {
	// Reject unknown sessions up front so we never register a run (or write
	// orphaned messages) against a non-existent session.
	if _, err := r.Deps.Sessions.Get(r.hub.rootCtx, sessionID); err != nil {
		return "", err
	}
	meta, err := r.taskMeta(r.hub.rootCtx, sessionID)
	if err != nil {
		return "", err
	}
	seg, ctx, err := r.hub.register(runID, sessionID, agentConfigID, sandboxID, meta)
	if err != nil {
		return "", err
	}
	if r.OnRunAttach != nil {
		r.OnRunAttach(runID)
	}
	r.launchSegment(seg, runID, sessionID, onDone, func() *RunOutcome {
		return r.runStreamed(ctx, runID, sessionID, agentConfigID, sandboxID, input)
	})
	return runID, nil
}

// launchSegment runs one segment's exec in the background with the shared
// teardown ordering. finalize is the segment's exclusive teardown: it cancels
// the segment's context (no leaked child of the hub root) and closes its own
// done gate (never a resume's fresh gate, so no double-close); it runs last so
// the session-delete wait only unblocks after every write lands. The pending
// approval is persisted BEFORE finish releases the session slot: a task
// completing in between would see "no live run, no approvals" and auto-wake a
// parent that is actually paused on a decision.
func (r *Runner) launchSegment(seg *runSegment, runID, sessionID string, onDone func(*RunOutcome), exec func() *RunOutcome) {
	go func() {
		defer seg.finalize()
		result := exec()
		r.afterRun(runID, result)
		r.hub.finish(runID, result.Interrupted)
		r.postRun(runID, sessionID, result)
		if onDone != nil {
			onDone(result)
		}
	}()
}

// afterRun persists an interrupted run's approval state so it survives a
// restart and is resumable over REST. Persistence failure is logged, not
// fatal — the live hub still holds the run for the current process.
func (r *Runner) afterRun(runID string, result *RunOutcome) {
	if !result.Interrupted {
		return
	}
	if err := r.persistInterruption(result); err != nil {
		zerolog.Ctx(r.hub.rootCtx).Error().Err(err).Str("run_id", runID).Msg("persist pending approval")
	}
}

// segmentSpec is what differs between a fresh run segment and a resume
// continuation; execStreamed holds everything they share. One pipeline,
// deliberately: the two used to be hand-maintained mirrors, and this file's
// history shows exactly how that drifts — the resume path once silently
// dropped HandoffInputFilter and ToolNotFoundBehavior while the fresh path had
// them, and its failure path needed an annotation "mirrors runStreamed's
// partial-turn save" to stay in sync.
type segmentSpec struct {
	// input is the user text announced in run.started and persisted when the
	// segment fails before the SDK's own per-turn save.
	input string
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
func (r *Runner) execStreamed(ctx context.Context, runID, sessionID, agentConfigID, sandboxID string, spec segmentSpec) *RunOutcome {
	log := zerolog.Ctx(ctx)
	// Fresh and resumed segments both pass here: stamp the run id so a
	// spawn_task inside the run records which run spawned it — that is what
	// lets the trace panel nest the task's wake-up run under this one.
	ctx = tasks.WithParentRunID(ctx, runID)

	sendEvent := func(typ string, payload any) {
		env, err := protocol.NewEnvelope(typ, payload)
		if err != nil {
			log.Error().Err(err).Str("type", typ).Msg("marshal event")
			return
		}
		r.hub.publish(runID, env)
	}

	// From the hub record, not a second store lookup: register/resume already
	// resolved it (and refused the run if it could not), so this cannot fail
	// and cannot disagree with what the run was registered as.
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
	}
	sendEvent(protocol.EventRunStarted, started)

	mkResult := func() *RunOutcome {
		return &RunOutcome{RunID: runID, SessionID: sessionID, AgentConfigID: agentConfigID, SandboxID: sandboxID}
	}
	mkErrResult := func(code, msg string) *RunOutcome {
		res := mkResult()
		res.ErrCode, res.ErrMessage = code, msg
		return res
	}

	// failCancelled ends the segment as a cancellation: the abandoned turn's
	// durable record, then run.cancelled. savePartialTurn writes on a context of
	// its own, so the cancellation that ended the run does not also stop the
	// record of it from landing.
	failCancelled := func(turn partialTurn) *RunOutcome {
		turn.annRole = "cancelled"
		r.savePartialTurn(turn)
		sendEvent(protocol.EventRunCancelled, protocol.RunCancelled{RunID: runID})
		res := mkResult()
		res.Cancelled = true
		return res
	}

	// failLookup ends the segment on a session lookup that did not answer. A
	// cancelled run is cancelled wherever the cancellation is noticed — the
	// lookup was abandoned, not refused — so it ends like every other cancel
	// rather than showing the user a red error and charging a task with a
	// failure. What is left is classified the way the REST path classifies
	// StartRun's own lookup (handler/run.go startError): only "no such session"
	// is absence, an unreachable database is a failure to LOOK, and reporting
	// THAT as session_not_found tells the client to give up on a session that is
	// still there.
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
	// reports it. Persisting matters on both paths: a fresh run may fail before
	// the SDK's per-turn save (the reload the client runs on run.error would
	// otherwise erase the prompt), and a failed resume has consumed the
	// pending-approval row as its claim, so without this the turn's fate would
	// vanish from durable state. The guardrail name/stage ride along so a
	// reload rebuilds the "Blocked by guardrail X" card, not a generic error.
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
	// would write orphaned messages under an arbitrary session id. (A lookup
	// that genuinely failed gets no partial-turn save: the store that just
	// failed to answer is the one the save would have to land in.)
	if spec.fresh {
		if _, err := r.Deps.Sessions.Get(ctx, sessionID); err != nil {
			return failLookup(err, "session not found: "+sessionID)
		}
	}

	// Build fully configured agent from DB config. Task runs never get the
	// task tools themselves: one level of spawning, no recursive fan-out.
	built, err := buildFullAgent(ctx, r.Deps, agentConfigID, sandboxID, task != nil)
	if err != nil {
		return failTurn("", protocol.CodeConfigError, err, "", "")
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
		// The plan phase's first unlock (the approved submit_plan executing)
		// persists its durable marker through this run's store.
		//
		// Fresh-only is NOT a gap: a resume executes the agent rehydrated from
		// the RunState registry — the ResolveApproval build — so the phase that
		// can unlock there is rebuilt.PlanPhase, and restorePlanPhase armed it
		// (and restored its durable lock state) before the resume launched.
		// THIS build's phase hangs off an agent the resume never runs; arming
		// it would arm a spectator.
		armPlanUnlock(built.PlanPhase, sa)
	}
	tracer := newTracer(ctx, sendEvent, r.Deps.Traces, sessionID, runID)

	runSession := wrapCompaction(sa, built, provider, sendEvent, runID)

	opts := runOptionsFor(built, runSession, provider, tracer, trustSessionID(sessionID, task))

	// Name the session in parallel with the run — the title needs only the user's
	// first message, not the answer, so it need not wait for the run to finish.
	// Task sessions are pre-named from the task label and hidden, so skip them;
	// a resume never needs it (the original run already fired it at its start,
	// even for an approval-gated first turn, which pauses before finishing).
	if spec.fresh && task == nil {
		go r.maybeGenerateTitle(r.hub.rootCtx, sessionID, agent.Model, spec.input, provider, sendEvent)
	}

	stream, ctrl := spec.start(ctx, agent, opts)
	r.hub.setControl(runID, ctrl)
	// The stream carries both halves of the outcome: the run's result as its
	// terminal event, or a terminal error. There is no second place to consult
	// — the old API kept the error on the side of the event channel, where a
	// cancellation race could drop it from one and leave it only in the other.
	res, streamedText, streamedReasoning, err := r.drainStream(stream, runID, built.HandoffToolNames, sendEvent)
	if err != nil {
		return failTurn(agent.Model, spec.failCode, err, streamedReasoning, streamedText)
	}

	return r.finishResult(res, runID, sessionID, agentConfigID, sandboxID, sendEvent)
}

// runStreamed executes one fresh run segment to completion, publishing events
// to the hub, and returns its outcome.
func (r *Runner) runStreamed(ctx context.Context, runID, sessionID, agentConfigID, sandboxID, input string) *RunOutcome {
	return r.execStreamed(ctx, runID, sessionID, agentConfigID, sandboxID, segmentSpec{
		input:    input,
		failCode: "stream_error",
		fresh:    true,
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
// context. It returns the new run id. onDone, if non-nil, fires once when the
// continuation terminates. Fails with ErrSessionBusy if the session has a
// live run.
// runID is the id of the interrupted run being continued: the resume reopens
// the SAME hub run (same event stream, same sequence), so one logical run
// keeps one id across interrupt/resume — events, traces, and messages all
// stay under that id and the trace panel shows one group per turn.
func (r *Runner) ResumeRun(runID string, state *agents.RunState, sessionID, agentConfigID, sandboxID string, onDone func(*RunOutcome)) (string, error) {
	meta, err := r.taskMeta(r.hub.rootCtx, sessionID)
	if err != nil {
		return "", err
	}
	seg, ctx, err := r.hub.resume(runID, sessionID, agentConfigID, sandboxID, meta)
	if err != nil {
		return "", err
	}
	if r.OnRunAttach != nil {
		r.OnRunAttach(runID)
	}
	r.launchSegment(seg, runID, sessionID, onDone, func() *RunOutcome {
		return r.resumeStreamed(ctx, runID, state, sessionID, agentConfigID, sandboxID)
	})
	return runID, nil
}

// resumeStreamed continues an interrupted run to completion under its
// original run id, publishing events to the (reopened) hub run, and returns
// its outcome.
//
// It streams through the same execStreamed pipeline as a fresh run ON
// PURPOSE: the resumed segment's events (the approved tool's output, every
// later turn's text and tool calls) go live to the client instead of
// surfacing only in the terminal run.output, and a resume continues the same
// run so it must carry the same policies.
func (r *Runner) resumeStreamed(ctx context.Context, runID string, state *agents.RunState, sessionID, agentConfigID, sandboxID string) *RunOutcome {
	return r.execStreamed(ctx, runID, sessionID, agentConfigID, sandboxID, segmentSpec{
		input:    userInputText(state.UserInput),
		failCode: "resume_error",
		start: func(ctx context.Context, _ *agents.Agent, opts agents.RunOptions) (agents.RunStream, agents.RunControl) {
			return agents.ResumeRun(ctx, state, opts)
		},
	})
}

func (r *Runner) finishResult(res *agents.RunResult, runID, sessionID, agentConfigID, sandboxID string, sendEvent func(string, any)) *RunOutcome {
	if len(res.Interruptions) > 0 {
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
		return &RunOutcome{
			RunID:         runID,
			SessionID:     sessionID,
			AgentConfigID: agentConfigID,
			SandboxID:     sandboxID,
			Interrupted:   true,
			Interruptions: res.Interruptions,
			SDKState:      res.State,
		}
	}

	r.bindSessionAgent(sessionID, agentConfigID)

	finalText := res.FinalOutputString()
	sendEvent(protocol.EventRunOutput, protocol.RunOutput{RunID: runID, FinalOutput: finalText})
	return &RunOutcome{FinalText: finalText, RunID: runID, SessionID: sessionID, AgentConfigID: agentConfigID, SandboxID: sandboxID}
}

// bindSessionAgent back-fills the session's bound agent config once the run has
// produced an answer. Detached from the run's context: the run is over, and a
// client that hung up must not decide whether the binding lands.
func (r *Runner) bindSessionAgent(sessionID, agentConfigID string) {
	if err := r.Deps.Sessions.BindAgentIfEmpty(context.Background(), sessionID, agentConfigID); err != nil {
		// Best-effort back-fill of the session's bound agent; log rather than
		// swallow so a persistent failure is diagnosable.
		zerolog.Ctx(r.hub.rootCtx).Warn().Err(err).Str("session_id", sessionID).
			Msg("updating session agent config")
	}
}

// maybeGenerateTitle names a still-default ("New Chat") session from the user's
// first message. It runs IN PARALLEL with the run — the title depends only on
// the user's message, not the answer — so it is fired at run start and takes the
// input, model and provider directly rather than reading them back after the run
// (at run start the SDK has not persisted anything yet). It runs on the hub root
// context so it survives the client disconnecting.
func (r *Runner) maybeGenerateTitle(parentCtx context.Context, sessionID, model, userInput string, provider agents.ModelProvider, sendEvent func(string, any)) {
	ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel()
	log := zerolog.Ctx(ctx)

	// Only name an unnamed session. Checked first so a re-run on an already-named
	// session (every message after the first) is a cheap Get + return.
	sess, err := r.Deps.Sessions.Get(ctx, sessionID)
	if err != nil || sess.Name != "New Chat" {
		return
	}
	if userInput == "" || provider == nil {
		return
	}

	titleAgent := &agents.Agent{
		Name:         "title_gen",
		Model:        model,
		Instructions: agents.StaticInstructions("You generate concise chat titles. Reply with ONLY the title text, nothing else. No quotes. Under 30 characters."),
	}
	prompt := "Generate a short title for this chat:\n\n" + userInput
	res, err := agents.RunSync(ctx, titleAgent, prompt, agents.RunOptions{Exec: agents.ExecOptions{MaxTurns: 1}, Model: agents.ModelOptions{Provider: provider}})
	if err != nil {
		log.Warn().Err(err).Msg("title gen: run failed")
		return
	}
	title := strings.TrimSpace(res.FinalOutputString())
	title = strings.Trim(title, "\"'")
	if title == "" || len([]rune(title)) > 50 {
		log.Warn().Str("raw", title).Msg("title gen: empty or too long")
		return
	}

	// The run's own bindSessionAgent only ever sets agent_config_id, never the
	// name, so this parallel name Update cannot conflict with it.
	if err := r.Deps.Sessions.Update(ctx, sessionID, title); err != nil {
		log.Warn().Err(err).Msg("title gen: save failed")
		return
	}
	sendEvent(protocol.EventSessionTitleUpdated, protocol.SessionTitleUpdated{
		SessionID: sessionID,
		Title:     title,
	})
}

// partialTurn is what savePartialTurn writes. A struct because these are ten
// strings in a row: passed positionally, a caller that slips one out of place
// gets no complaint from the compiler, only a mislabelled turn in the
// transcript.
type partialTurn struct {
	sessionID string
	runID     string
	model     string
	// userInput is the run's prompt, saved only as a fallback — see
	// savePartialTurn.
	userInput string
	// annRole is the trailing marker's kind, "cancelled" or "error", and annMsg
	// its optional detail. Empty annRole writes no marker.
	annRole string
	annMsg  string
	// partialReasoning and partialText are the in-flight turn's streamed
	// thinking and narration.
	partialReasoning string
	partialText      string
	// guardrail and stage, when set, tag an "error" marker as a guardrail block.
	guardrail string
	stage     string
}

// savePartialTurn records what the SDK cannot save itself when a run is
// cancelled or fails mid-stream. The SDK persists the user input and every
// completed turn incrementally (see agents.runner.persistSessionItems), so
// completed segments and tool calls survive on their own. This adds, all as
// display-only annotations that are never replayed:
// - the in-flight turn's streamed reasoning and text, so a cancel during the
// thinking phase (before that turn completed) still shows what the model was
// doing instead of vanishing;
// - a trailing marker for why the run stopped;
//
// and, only when the run died before the SDK persisted anything under this run
// id (e.g. cancelled before the first turn completed), the prompt as a
// replayable fallback so it is not lost.
func (r *Runner) savePartialTurn(t partialTurn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ref, refErr := store.RefFor(ctx, r.db, t.sessionID)
	if refErr != nil {
		zerolog.Ctx(r.hub.rootCtx).Warn().Err(refErr).Str("run_id", t.runID).Str("session_id", t.sessionID).
			Msg("persisting partial turn")
		return
	}
	es := store.NewEntryStoreFor(r.db, ref)
	es.SetRunID(t.runID)
	es.SetModel(t.model)

	entries := make([]session.Entry, 0, 4)

	if t.userInput != "" && !runHasPersistedItems(ctx, es, t.runID) {
		for _, item := range agents.InputItemsFromText(t.userInput) {
			e, err := session.NewItemEntry(item, agents.Source{Type: agents.SourceUser})
			if err != nil {
				continue
			}
			entries = append(entries, e)
		}
	}

	// The in-flight turn's thinking and narration — annotations, because a
	// fabricated reasoning item would be rejected on replay and an abandoned
	// turn should not enter the model's history.
	if t.partialReasoning != "" {
		entries = append(entries, session.NewAnnotationEntry(
			agents.ItemDisplay{Kind: agents.DisplayReasoning, Text: t.partialReasoning},
			agents.Source{Type: agents.SourceModel}))
	}
	if t.partialText != "" {
		entries = append(entries, session.NewAnnotationEntry(
			agents.ItemDisplay{Kind: agents.DisplayMessage, Text: t.partialText},
			agents.Source{Type: agents.SourceModel}))
	}

	if t.annRole != "" {
		d := agents.ItemDisplay{Kind: agents.DisplayError, Text: t.annMsg}
		if t.annRole == "cancelled" {
			d.Kind = agents.DisplayCancelled
		}
		// A guardrail block carries its name and stage so a reload rebuilds the
		// typed "Blocked by guardrail X" card instead of a generic error.
		if t.guardrail != "" {
			d.Extra = map[string]any{"guardrail": t.guardrail, "stage": t.stage}
		}
		src := agents.Source{Type: agents.SourceErrorHandler}
		if t.guardrail != "" {
			src = agents.Source{Type: agents.SourceGuardrail}
		}
		entries = append(entries, session.NewAnnotationEntry(d, src))
	}

	if len(entries) == 0 {
		return
	}
	if err := es.Append(ctx, entries...); err != nil {
		// The partial-turn save is the only durable record of a cancelled or
		// failed turn's prompt and in-flight thinking; a lost write means a
		// reload shows nothing. Best-effort, but never silent.
		zerolog.Ctx(r.hub.rootCtx).Warn().Err(err).Str("run_id", t.runID).Str("session_id", t.sessionID).
			Msg("persisting partial turn")
	}
}

// isCancellation reports whether a run stopped because it was cancelled (or its
// deadline elapsed) rather than failing — whether the signal is the run's own
// context or a context error bubbled up (and wrapped) by the model provider.
func isCancellation(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// runHasPersistedItems reports whether the SDK already wrote any replayable item
// row (user input or a completed turn's items) for this run id. Used to avoid
// duplicating the prompt the SDK's per-turn persistence normally saves. It asks
// the same store handle the save will write through, so the question is asked
// of the generation that will answer it.
func runHasPersistedItems(ctx context.Context, es *store.EntryStore, runID string) bool {
	exists, err := es.RunHasItems(ctx, runID)
	if err != nil {
		// On a query error, assume something was saved: skipping a possibly
		// duplicate prompt is safer than writing a guaranteed duplicate.
		return true
	}
	return exists
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

// drainStream forwards a streamed run's events to the hub and accumulates only
// the CURRENT turn's reasoning/text so an abort can persist them as
// display-only annotations (a cancel during the thinking phase still shows what
// the model was doing). A terminal error on the event channel stops
// consumption; the caller reads the run's outcome from FinalResult.
//
// The reset is aligned with the SDK's real per-turn persist boundary, NOT with
// response.completed. The SDK saves a turn's items only AFTER its tool calls
// have run (agents/run.go stepRunAgain / stepHandoff persist post-tool-exec).
// response.completed fires when the model finishes generating — before the
// tools run — so resetting there loses this turn's streamed text/reasoning if
// the run is cancelled DURING tool execution (the SDK has not persisted it yet
// either, so a reload would then show nothing). Instead, mark the turn
// committed at response.completed and defer the reset until the NEXT turn's
// first delta arrives: by then the SDK has persisted the previous turn (its
// stepRunAgain ran before the next model call), so the buffer correctly holds
// only the still-unpersisted in-flight turn.
func (r *Runner) drainStream(stream agents.RunStream, runID string, handoffNames map[string]bool, send func(string, any)) (res *agents.RunResult, streamedText, streamedReasoning string, runErr error) {
	var text, reasoning strings.Builder
	turnCommitted := false // response.completed seen; the SDK will persist this turn after its tools run
	startNextTurn := func() {
		if turnCommitted {
			text.Reset()
			reasoning.Reset()
			turnCommitted = false
		}
	}
	for event, err := range stream {
		if err != nil {
			runErr = err
			break
		}
		if done, ok := event.(*agents.RunCompletedEvent); ok {
			// The stream's terminal event carries the finished run; it is the
			// loop's own bookkeeping and not something the client renders.
			res = done.Result
			// Except the diagnostics: a run that answered after three retries
			// or on a fallback model looks identical to one that answered
			// first time, and the difference is what explains the latency.
			for _, d := range res.Diagnostics {
				send(protocol.EventRunDiagnostic, protocol.RunDiagnostic{
					RunID:   runID,
					Type:    string(d.Type),
					Code:    string(d.Code),
					Message: d.Message,
					Details: d.Details,
				})
			}
			continue
		}
		if raw, ok := event.(*agents.RawResponsesStreamEvent); ok && raw.Data != nil {
			switch raw.Data.Type {
			case "response.created":
				// A new turn is starting: drop the previous (committed) turn's
				// buffer here, not on the first text/reasoning delta. A turn that
				// emits only a function call produces no delta, so waiting for one
				// would keep the prior turn's text and re-annotate it on cancel.
				startNextTurn()
			case "response.completed":
				turnCommitted = true
			case "response.output_text.delta":
				startNextTurn()
				text.WriteString(raw.Data.Delta)
			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				startNextTurn()
				reasoning.WriteString(raw.Data.Delta)
			}
		}
		r.handleStreamEvent(event, runID, handoffNames, send)
	}
	return res, text.String(), reasoning.String(), runErr
}

// runErrorFor builds the run.error for a terminal run failure. The code comes
// from the SDK (agents.CodeOf) so this stays correct as the SDK's vocabulary
// grows — there is deliberately no mapping table here. An error the SDK did not
// classify keeps the caller's transport-level fallback code.
//
// A guardrail tripwire additionally carries the guardrail name and the stage it
// fired at, which no code can express: the UI renders "blocked by guardrail X"
// instead of a generic red error and, on an output trip, marks the answer that
// already streamed as retracted.
func runErrorFor(runID string, err error, fallback string) protocol.RunError {
	e := protocol.RunError{RunID: runID, Code: fallback, Message: err.Error()}
	if code := agents.CodeOf(err); code != agents.CodeUnknown {
		e.Code = string(code)
	}
	var tw *agents.GuardrailTripwireError
	if errors.As(err, &tw) {
		e.Guardrail = tw.Result.Guardrail.Name
		e.Stage = string(tw.Stage())
	}
	return e
}

func (r *Runner) handleStreamEvent(event agents.StreamEvent, runID string, handoffNames map[string]bool, send func(string, any)) {
	switch e := event.(type) {
	case *agents.RawResponsesStreamEvent:
		if e.Data == nil {
			return
		}
		switch e.Data.Type {
		case "response.output_text.delta":
			if e.Data.Delta != "" {
				send(protocol.EventRunStep, protocol.RunStep{RunID: runID, Delta: e.Data.Delta})
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if e.Data.Delta != "" {
				send(protocol.EventRunReasoning, protocol.RunReasoning{RunID: runID, Delta: e.Data.Delta})
			}
		}

	case *agents.RunItemStreamEvent:
		switch e.Name {
		case "message_output_created":
			// The completed turn text, authoritative over the run.step deltas
			// that previewed it. Interim messages between tool calls only exist
			// as deltas plus this event — resumed segments and backends that
			// stream no deltas rely on it entirely.
			if e.Item.Kind == agents.ItemMessage {
				if text := e.Item.Text(); text != "" {
					send(protocol.EventRunMessage, protocol.RunMessage{RunID: runID, Text: text, ItemID: rawItemID(e.Item)})
				}
			}
		case "reasoning_item_created":
			// The completed thinking block, authoritative over the run.reasoning
			// deltas that previewed it — and the only thinking signal when the
			// backend streams no reasoning deltas or the segment was resumed.
			if e.Item.Kind == agents.ItemReasoning {
				if text := e.Item.Text(); text != "" {
					send(protocol.EventRunReasoningItem, protocol.RunReasoningItem{RunID: runID, Text: text, ItemID: rawItemID(e.Item)})
				}
			}
		case "tool_called":
			if e.Item.Kind == agents.ItemToolCall {
				fc := e.Item.FunctionCall()
				// The SDK emits tool_called for a handoff too (wrapping the
				// transfer_to_X call); it has no tool_output, so a run.tool_call
				// here would leave a tool card spinning forever. run.handoff already
				// conveys the transfer, so drop it.
				if handoffNames[fc.Name] {
					return
				}
				send(protocol.EventRunToolCall, protocol.RunToolCall{
					RunID:      runID,
					ToolCallID: fc.CallID,
					ToolName:   fc.Name,
					Arguments:  fc.Arguments,
				})
			}
		case "tool_output":
			if e.Item.Kind == agents.ItemToolCallOutput {
				send(protocol.EventRunToolResult, protocol.RunToolResult{
					RunID:      runID,
					ToolCallID: e.Item.CallID(),
					// The display rendering, not %v: a multimodal output is a
					// content list, and Go syntax for it would not match what
					// the same item reads back as from the stored session.
					Output: e.Item.Display().Output,
				})
			}
		case "handoff_requested":
			if e.Item.Kind == agents.ItemHandoffCall && e.Item.Agent != nil {
				send(protocol.EventRunHandoff, protocol.RunHandoff{
					RunID: runID,
					From:  e.Item.Agent.Name,
				})
			}
		case "injected_input_created":
			// Input injected into a live run (run.inject) is a USER entry, and
			// no live event carries one: run.started.Input covers only the
			// prompt a run begins with, and PROTOCOL.md F2's run.entry — the
			// event that will — has not shipped. The client that injected it
			// already has the text, and the SDK persists the item, so every
			// other connection picks it up on its next history load. Named
			// here rather than left out of the switch: the drop is a decision,
			// and this is where run.entry lands once it ships.
		case "handoff_occured":
			if e.Item.Kind == agents.ItemHandoffOutput && e.Item.HandoffFrom != nil && e.Item.HandoffTo != nil {
				send(protocol.EventRunHandoff, protocol.RunHandoff{
					RunID: runID,
					From:  e.Item.HandoffFrom.Name,
					To:    e.Item.HandoffTo.Name,
				})
			}
		}

	case *agents.ToolProgressEvent:
		// A tool that runs for two minutes leaves the UI with nothing but a
		// spinner otherwise. This is not the tool's answer — that arrives as
		// run.tool_result — so the client renders it as live output.
		send(protocol.EventRunToolProgress, protocol.RunToolProgress{
			RunID:    runID,
			CallID:   e.CallID,
			ToolName: e.ToolName,
			Delta:    e.Result.Text(),
			Renderer: e.Result.Display,
		})

	case *agents.AgentUpdatedStreamEvent:
		send(protocol.EventRunAgentStart, protocol.RunAgentStart{
			RunID:     runID,
			AgentName: e.NewAgent.Name,
		})
	}
}

// stopAtTools builds the turn hook for the agent config's stop_at_tools list:
// the run ends after a turn that called any of the named tools. It returns nil
// for an empty list so an unconfigured agent pays nothing.
func stopAtTools(names []string) func(context.Context, *agents.TurnResult) (bool, error) {
	if len(names) == 0 {
		return nil
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	return func(_ context.Context, tr *agents.TurnResult) (bool, error) {
		for _, called := range tr.ToolCallNames() {
			if want[called] {
				return true, nil
			}
		}
		return false, nil
	}
}

// rawItemID returns the model-assigned id of the item's raw form, or "" when
// the item carries none (a rebuilt or synthesized item).
func rawItemID(it *agents.RunItem) string {
	if it.Raw == nil {
		return ""
	}
	return it.Raw.ID
}
