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
// paths. One constructor so a resume carries the same policies as the run it
// continues. runContext is the Context value (the exec_command approval gate
// reads a trusted session id from it).
func runOptionsFor(built *BuildResult, sess *session.Session, provider agents.ModelProvider, tracer *tracing.Tracer, runContext any) agents.RunOptions {
	opts := agents.RunOptions{
		Context: runContext,
		Conversation: agents.ConversationOptions{
			Session: sess,
			// A non-positive HistoryLimit already means "no limit" on both
			// sides, so it needs no translation.
			Settings: session.Settings{Limit: built.Session.HistoryLimit},
		},
		Exec: agents.ExecOptions{
			MaxTurns:              built.Behavior.MaxTurns,
			MaxToolConcurrency:    built.Behavior.MaxToolConcurrency,
			ErrorHandlers:         built.ErrorHandlers,
			ReasoningItemIDPolicy: built.ReasoningItemIDPolicy,
			ToolNotFoundBehavior:  toolNotFoundBehavior(built.Behavior.ToolNotFoundBehavior),
			ShouldStopAfterTurn:   stopAtTools(built.StopAtTools),
			// Context overflow → forced compaction pass → retry the turn. Only
			// bites when the session is compaction-aware; otherwise the overflow
			// reports as before (spec §2.5g).
			Overflow: agents.OverflowPolicy{MaxRetries: 2},
		},
		Guardrails: built.RunGuardrails,
		Model:      agents.ModelOptions{Provider: provider},
		Observe:    agents.ObserveOptions{Tracer: tracer, IncludeSensitiveData: built.TraceIncludeSensitive},
	}
	if built.Behavior.HandoffInputFilter == "nest_history" {
		opts.Exec.HandoffInputFilter = agents.NestHandoffHistory(agents.NestHistoryOptions{})
	}
	return opts
}

// toolNotFoundBehavior resolves the agent's setting. Unset means RETURN TO
// MODEL here, not the SDK's stricter default: a model inventing a tool name —
// or reaching for one plan mode is hiding, or one a session without a sandbox
// never had — is a routine slip, and ending the run over it takes down the
// turn, and any workflow driving it, for something the model corrects on being
// told. Set "error" to get the abort back.
func toolNotFoundBehavior(s string) agents.ToolNotFoundBehavior {
	if s == "" {
		return agents.ToolNotFoundReturnToModel
	}
	return agents.ParseToolNotFoundBehavior(s)
}

// wrapCompaction wraps sa with the compaction adapter when the agent config
// enables it. An empty summary model falls back to the agent's own model, so
// leaving the field blank does not silently disable compaction.
func wrapCompaction(sa *store.EntryStore, built *BuildResult, provider agents.ModelProvider, send func(string, any), runID string) *session.Session {
	if !built.Compaction.Enabled || provider == nil {
		return session.NewSession(sa)
	}
	summaryModel, err := summaryModelFor(provider, built.Compaction, built.Agent.Model)
	if err != nil || summaryModel == nil {
		return session.NewSession(sa)
	}
	return session.NewSession(store.NewCompactionAdapter(sa, summaryModel,
		built.Compaction.Threshold, built.Compaction.Window, built.Compaction.Prompt,
		compactionNotifier(send, runID),
	))
}

// summaryModelFor resolves the model a compaction pass summarizes with — the
// config's compaction_model, else the agent's own. One definition, shared by
// the run path and the manual CompactSession.
func summaryModelFor(provider agents.ModelProvider, compaction store.CompactionGroup, agentModel string) (agents.Model, error) {
	return provider.Model(cmp.Or(compaction.Model, agentModel))
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
			// Workflows share the per-session background budget: a task spawn
			// counts running workflow executions too, the mirror of
			// checkBackgroundBudget counting tasks. 0 on error never wrongly
			// blocks a spawn.
			ExtraLiveCount: func(ctx context.Context, parentSessionID string) int {
				if r.Deps.WorkflowRuns == nil {
					return 0
				}
				n, err := r.Deps.WorkflowRuns.CountLive(ctx, parentSessionID)
				if err != nil {
					return 0
				}
				return n
			},
		})
	}
	// Agent building reaches the task tools through the manager, not the runner.
	deps.TaskManager = r.tasks
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

// bindingPlan is one run request's resolved sandbox context: the effective
// values the run executes under, and whether this run still owes the session
// its permanent binding (first sandbox-carrying run on an unbound session).
type bindingPlan struct {
	sandboxID string
	workDir   string
	needBind  bool
	// revision is the config revision the workdir was validated against; the
	// bind CAS matches it, so a config updated between plan and write makes the
	// bind lose and re-plan rather than land a stale workdir.
	revision int64
}

// planSandboxBinding decides a run's sandbox context WITHOUT writing anything.
// A bound session overrides the request; the client's values are ignored. An
// unbound session carrying a sandbox has the request validated (config must
// exist, workdir honored by its backend — ResolveBindingWorkDir) and a bind
// planned. Runs with no sandbox resolve to none; the session stays bindable.
// The write happens in startRunWithID only after hub registration succeeds.
func (r *Runner) planSandboxBinding(ctx context.Context, sess *store.Session, sandboxID, workDir string) (bindingPlan, error) {
	if sess.SandboxID != "" {
		return bindingPlan{sandboxID: sess.SandboxID, workDir: sess.WorkDir}, nil
	}
	if sandboxID == "" {
		return bindingPlan{}, nil
	}
	cfg, err := r.Deps.SandboxConfigs.Get(ctx, sandboxID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return bindingPlan{}, ErrInvalidBinding{Reason: "sandbox not found: " + sandboxID}
		}
		return bindingPlan{}, err
	}
	canonical, err := ResolveBindingWorkDir(cfg, workDir)
	if err != nil {
		return bindingPlan{}, err
	}
	return bindingPlan{sandboxID: sandboxID, workDir: canonical, needBind: true, revision: cfg.Revision}, nil
}

// maxBindAttempts bounds the plan→register→bind loop in startRunWithID:
// three passes distinguish an unlucky race from a config under active edit.
const maxBindAttempts = 3

// startRunWithID is StartRun with a caller-chosen run id — SpawnTask mints the
// task's run id up front so the row can carry it before the run launches.
func (r *Runner) startRunWithID(runID, sessionID, agentConfigID, sandboxID, workDir, input, wakeParentRunID string, planIntent *bool, onDone func(*RunOutcome)) (string, error) {
	var (
		seg      *runSegment
		ctx      context.Context
		plan     bindingPlan
		boundNow bool
	)
	for attempt := 1; ; attempt++ {
		// Reject unknown sessions up front so we never register a run (or
		// write orphaned messages) against a non-existent session. The same
		// lookup feeds the sandbox binding below.
		sess, err := r.Deps.Sessions.Get(r.hub.rootCtx, sessionID)
		if err != nil {
			return "", err
		}
		plan, err = r.planSandboxBinding(r.hub.rootCtx, sess, sandboxID, workDir)
		if err != nil {
			return "", err
		}
		meta, err := r.taskMeta(r.hub.rootCtx, sessionID)
		if err != nil {
			return "", err
		}
		// Register first, bind second: registration is the gate that can refuse
		// (busy, deleting, draining, task limit), and binding before it would fix
		// the session's file system context for a run that never started. Holding
		// the session slot also serializes binds.
		seg, ctx, err = r.hub.register(runID, sessionID, agentConfigID, plan.sandboxID, plan.workDir, meta)
		if err != nil {
			return "", err
		}
		if !plan.needBind {
			break
		}
		won, err := r.Deps.Sessions.BindSandboxIfEmpty(r.hub.rootCtx, sessionID, plan.sandboxID, plan.workDir, plan.revision)
		if err != nil {
			r.hub.unregister(runID, seg)
			return "", err
		}
		if won {
			boundNow = true
			break
		}
		// The CAS refused: the sandbox config was deleted or bumped to a new
		// revision, or the session row was removed. Withdraw the registration and
		// go around; the next pass re-validates, refusing a vanished config (400)
		// or session (404). Only a revision moving every pass keeps the loop
		// alive, and after maxBindAttempts the retry belongs to the client.
		r.hub.unregister(runID, seg)
		if attempt == maxBindAttempts {
			return "", ErrBindingContention
		}
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
// wait only unblocks after every write lands, and afterRun persists the pending
// approval BEFORE finish releases the session slot — otherwise a task completing
// in between would auto-wake a parent that is actually paused on a decision.
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
	log := zerolog.Ctx(ctx)
	// Stamp the run id so a spawn_task inside the run records which run spawned
	// it — that is what lets the trace panel nest the task's wake-up run here.
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
		started.Attempt = task.Attempt
		started.MaxAttempts = task.MaxAttempts
	} else if r.Deps.WorkflowRuns != nil {
		// A workflow step announces its execution and parent, so every browser
		// keeps the hidden child session off its chat path — without this only
		// the one with the detail lens open knew. Best-effort: a failed lookup
		// here fails the run for real a few lines down (isBackgroundRun).
		if wf, err := r.Deps.WorkflowRuns.ByChildSessionAny(ctx, sessionID); err == nil && wf != nil {
			started.WorkflowRunID = wf.ID
			started.ParentSessionID = wf.ParentSessionID
			started.ParentRunID = wf.OriginRunID
			started.Label = wf.Name
		}
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
	// a workflow step's — is built without the tools and modes that only make
	// sense with a person in front of them.
	background, err := r.isBackgroundRun(ctx, sessionID, task)
	if err != nil {
		return failTurn("", protocol.CodeConfigError, err, "", "")
	}
	built, err := buildFullAgent(ctx, r.Deps, agentConfigID, sandboxID, workDir, background)
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
			log.Warn().Err(err).Msg("failed to record the session's context profile")
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

	opts := runOptionsFor(built, runSession, provider, tracer, trustSessionID(sessionID, task))

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

	return r.finishResult(res, runID, sessionID, agentConfigID, sandboxID, workDir, sendEvent)
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

func (r *Runner) finishResult(res *agents.RunResult, runID, sessionID, agentConfigID, sandboxID, workDir string, sendEvent func(string, any)) *RunOutcome {
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
			WorkDir:       workDir,
			Interrupted:   true,
			Interruptions: res.Interruptions,
			SDKState:      res.State,
		}
	}

	r.bindSessionAgent(sessionID, agentConfigID)

	finalText := res.FinalOutputString()
	sendEvent(protocol.EventRunOutput, protocol.RunOutput{RunID: runID, FinalOutput: finalText})
	return &RunOutcome{FinalText: finalText, RunID: runID, SessionID: sessionID, AgentConfigID: agentConfigID, SandboxID: sandboxID, WorkDir: workDir}
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

// maybeGenerateTitle names a still-default ("New Session") session from the user's
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
	if err != nil || sess.Name != "New Session" {
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

// partialTurn is what savePartialTurn writes. Its fields are all strings; build
// it with keyed fields so a misordered pair cannot slip past the compiler.
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
// cancelled or fails mid-stream. The SDK persists user input and completed turns
// incrementally, so this adds only display-only annotations, never replayed:
// - the in-flight turn's streamed reasoning and text, so a cancel mid-thought
// still shows what the model was doing;
// - a trailing marker for why the run stopped;
// and, only when the run died before the SDK persisted anything under this run
// id, the prompt as a replayable fallback.
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
		// The only durable record of a cancelled/failed turn's prompt and
		// in-flight thinking; best-effort, but never silent.
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
// row (user input or a completed turn's items) for this run id, to avoid
// duplicating the prompt the SDK's per-turn persistence normally saves.
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
