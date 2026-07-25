package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// EventSink receives protocol envelopes emitted during a streamed run.
type EventSink func(env *protocol.Envelope)

// Runner executes streamed agent runs. Run lifecycle, cancellation, event
// buffering, and fan-out are delegated to the hub, so a run outlives the
// connection that started it.
type Runner struct {
	db       *bun.DB
	Deps     *AgentDeps
	hub      *RunHub
	messages *store.MessageStore

	// Task completion wake-ups have no in-memory state: the notification debt
	// lives on the tasks row (notify_state), written atomically with the
	// terminal status — see drainTaskNotifications.
	// OnRunAttach, when set, is invoked with the run id right after a run
	// registers in the hub (fresh start and approval resume alike), before any
	// event publishes. The WS layer uses it to attach every live connection to
	// the stream — run events are a broadcast bus, not a reply channel to
	// whoever started the run — and runs created over REST take the same path.
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

// sessionSettingsFor returns the run-level SessionSettings for a history-item
// cap, or nil to leave the full history loaded (limit <= 0).
func sessionSettingsFor(limit int) *agents.SessionSettings {
	if limit > 0 {
		return &agents.SessionSettings{Limit: limit}
	}
	return nil
}

// wrapCompaction wraps sa with the compaction adapter when the agent config
// enables it. An empty summary model falls back to the agent's own model, so
// leaving the field blank does not silently disable compaction.
func wrapCompaction(sa *store.SessionAdapter, built *BuildResult, provider agents.ModelProvider, send func(string, any), runID string) *agents.Session {
	if !built.CompactionEnabled || provider == nil {
		return agents.NewSession(sa)
	}
	modelName := built.CompactionModel
	if modelName == "" {
		modelName = built.Agent.Model
	}
	summaryModel, err := provider.GetModel(modelName)
	if err != nil || summaryModel == nil {
		return agents.NewSession(sa)
	}
	return agents.NewSession(store.NewCompactionAdapter(sa, summaryModel,
		built.CompactionThreshold, built.CompactionWindow, built.CompactionPrompt,
		compactionNotifier(send, runID),
	))
}

// NewRunner creates a Runner backed by the given database and agent
// dependencies. rootCtx scopes every run's lifetime (see RunHub); cancelling
// it stops all in-flight runs.
func NewRunner(rootCtx context.Context, db *bun.DB, deps *AgentDeps) *Runner {
	r := &Runner{
		db:       db,
		Deps:     deps,
		hub:      NewRunHub(rootCtx),
		messages: store.NewMessageStore(db),
	}
	if deps.MaxTasks > 0 {
		r.hub.maxTasks = deps.MaxTasks
	}
	// The runner is the task spawner; agent building only sees the interface.
	deps.TaskSpawner = r
	return r
}

// Hub exposes the run hub so handlers can subscribe to run events, query
// status, and cancel runs.
func (r *Runner) Hub() *RunHub { return r.hub }

// RunResult carries the outcome of a streamed run for the caller to persist.
type RunResult struct {
	FinalText     string
	RunID         string
	SessionID     string
	AgentConfigID string
	SandboxID     string
	// ErrCode/ErrMessage describe a failed run (mirroring the run.error event)
	// so terminal bookkeeping — a task row's failure reason above all — does
	// not depend on having watched the event stream.
	ErrCode       string
	ErrMessage    string
	Interrupted   bool
	Interruptions []*agents.ToolApprovalItem
	SDKState      *agents.RunState
}

// StartRun registers a new run for the session and launches it in the
// background under the hub's root context (so it survives the connection that
// started it). It returns the run id; subscribe via Hub() to stream events.
// onDone, if non-nil, is invoked once when the run terminates. It fails with
// ErrSessionBusy when the session already has a live run.
func (r *Runner) StartRun(sessionID, agentConfigID, sandboxID, input string, onDone func(*RunResult)) (string, error) {
	return r.startRunWithID(store.NewID(), sessionID, agentConfigID, sandboxID, input, onDone)
}

// startRunWithID is StartRun with a caller-chosen run id — SpawnTask mints the
// task's run id up front so the row can carry it before the run launches.
func (r *Runner) startRunWithID(runID, sessionID, agentConfigID, sandboxID, input string, onDone func(*RunResult)) (string, error) {
	// Reject unknown sessions up front so we never register a run (or write
	// orphaned messages) against a non-existent session.
	if _, err := r.Deps.Sessions.Get(r.hub.rootCtx, sessionID); err != nil {
		return "", err
	}
	seg, ctx, err := r.hub.register(runID, sessionID, agentConfigID, sandboxID, r.taskMeta(r.hub.rootCtx, sessionID))
	if err != nil {
		return "", err
	}
	if r.OnRunAttach != nil {
		r.OnRunAttach(runID)
	}
	go func() {
		// finalize is this segment's exclusive teardown: it cancels the segment's
		// context (no leaked child of the hub root,) and closes its own done
		// gate (never a resume's fresh gate, so no double-close,). It runs last
		// so the session-delete wait only unblocks after every write below lands.
		defer seg.finalize()
		result := r.runStreamed(ctx, runID, sessionID, agentConfigID, sandboxID, input)
		// Persist a pending approval BEFORE finish releases the session slot:
		// a task completing in between would see "no live run, no approvals"
		// and auto-wake a parent that is actually paused on a decision.
		r.afterRun(runID, result)
		r.hub.finish(runID, result.Interrupted)
		r.postRun(runID, sessionID, result)
		if onDone != nil {
			onDone(result)
		}
	}()
	return runID, nil
}

// afterRun persists an interrupted run's approval state so it survives a
// restart and is resumable over REST. Persistence failure is logged, not
// fatal — the live hub still holds the run for the current process.
func (r *Runner) afterRun(runID string, result *RunResult) {
	if !result.Interrupted {
		return
	}
	if err := r.persistInterruption(result); err != nil {
		zerolog.Ctx(r.hub.rootCtx).Error().Err(err).Str("run_id", runID).Msg("persist pending approval")
	}
}

// runStreamed executes one run segment to completion, publishing events to
// the hub, and returns its outcome.
func (r *Runner) runStreamed(ctx context.Context, runID, sessionID, agentConfigID, sandboxID, input string) *RunResult {
	log := zerolog.Ctx(ctx)

	sendEvent := func(typ string, payload any) {
		env, err := protocol.NewEnvelope(typ, payload)
		if err != nil {
			log.Error().Err(err).Str("type", typ).Msg("marshal event")
			return
		}
		r.hub.publish(runID, env)
	}

	task := r.taskMeta(ctx, sessionID)
	started := protocol.RunStarted{RunID: runID, SessionID: sessionID, Input: input}
	if task != nil {
		started.ParentSessionID = task.ParentSessionID
		started.ParentRunID = task.ParentRunID
		started.TaskID = task.TaskID
		started.ToolCallID = task.ToolCallID
		started.Label = task.Label
	}
	sendEvent(protocol.EventRunStarted, started)

	mkResult := func() *RunResult {
		return &RunResult{RunID: runID, SessionID: sessionID, AgentConfigID: agentConfigID, SandboxID: sandboxID}
	}
	mkErrResult := func(code, msg string) *RunResult {
		res := mkResult()
		res.ErrCode, res.ErrMessage = code, msg
		return res
	}

	// Refuse to run against a session that doesn't exist — otherwise the run
	// would write orphaned messages under an arbitrary session id.
	if _, err := r.Deps.Sessions.Get(ctx, sessionID); err != nil {
		sendEvent(protocol.EventRunError, protocol.RunError{
			RunID:   runID,
			Code:    protocol.CodeSessionNotFound,
			Message: "session not found: " + sessionID,
		})
		return mkErrResult(protocol.CodeSessionNotFound, "session not found: "+sessionID)
	}

	// Build fully configured agent from DB config. Task runs never get the
	// task tools themselves: one level of spawning, no recursive fan-out.
	built, err := buildFullAgent(ctx, r.Deps, agentConfigID, sandboxID, task != nil)
	if err != nil {
		// Persist the prompt + error so the user's message and the failure survive
		// the reload the client runs on run.error (the run never reached the SDK's
		// per-turn save). Mirrors the post-start error path below.
		r.savePartialTurn(sessionID, runID, "", input, "error", err.Error(), "", "", "", "")
		sendEvent(protocol.EventRunError, protocol.RunError{
			RunID:   runID,
			Code:    protocol.CodeConfigError,
			Message: err.Error(),
		})
		return mkErrResult(protocol.CodeConfigError, err.Error())
	}

	agent := built.Agent
	provider := built.Provider
	if provider == nil {
		const msg = "no API key configured for this agent"
		r.savePartialTurn(sessionID, runID, agent.Model, input, "error", msg, "", "", "", "")
		sendEvent(protocol.EventRunError, protocol.RunError{
			RunID:   runID,
			Code:    protocol.CodeConfigError,
			Message: msg,
		})
		return mkErrResult(protocol.CodeConfigError, msg)
	}

	// Wrap with router provider if routes exist
	provider = BuildRouterProvider(ctx, r.Deps, provider)

	sendEvent(protocol.EventRunAgentStart, protocol.RunAgentStart{RunID: runID, AgentName: agent.Name})

	sa := store.NewSessionAdapter(r.db, sessionID)
	sa.SetRunID(runID)
	sa.SetModel(agent.Model)
	tracer := newTracer(sendEvent, r.Deps.Traces, sessionID, runID)

	runSession := wrapCompaction(sa, built, provider, sendEvent, runID)

	opts := agents.RunOptions{
		// exec_command's approval gate reads a session id from here.
		Context: trustSessionID(sessionID, task),
		Conversation: agents.ConversationOptions{
			Session:               runSession,
			UsePreviousResponseID: built.UsePreviousResponseID,
		},
		Exec: agents.ExecOptions{
			MaxTurns:           built.MaxTurns,
			MaxToolConcurrency: built.MaxToolConcurrency,
			ErrorHandlers:      built.ErrorHandlers,
		},
		Model:   agents.ModelOptions{Provider: provider},
		Observe: agents.ObserveOptions{Tracer: tracer},
	}
	if built.HandoffInputFilter == "nest_history" {
		opts.Exec.HandoffInputFilter = agents.NestHandoffHistory(agents.NestHistoryOptions{})
	}
	opts.Conversation.Settings = sessionSettingsFor(built.HistoryLimit)
	opts.Exec.ReasoningItemIDPolicy = built.ReasoningItemIDPolicy
	opts.Guardrails = built.RunGuardrails
	opts.Exec.ToolNotFoundBehavior = agents.ParseToolNotFoundBehavior(built.ToolNotFoundBehavior)
	opts.Exec.ShouldStopAfterTurn = stopAtTools(built.StopAtTools)

	// Name the session in parallel with the run — the title needs only the user's
	// first message, not the answer, so it need not wait for the run to finish.
	// Task sessions are pre-named from the task label and hidden, so skip them.
	if task == nil {
		go r.maybeGenerateTitle(r.hub.rootCtx, sessionID, agent.Model, input, provider, sendEvent)
	}

	stream, ctrl := agents.Run(ctx, agent, input, opts)
	r.hub.setControl(runID, ctrl)
	// The stream carries both halves of the outcome: the run's result as its
	// terminal event, or a terminal error. There is no second place to consult
	// — the old API kept the error on the side of the event channel, where a
	// cancellation race could drop it from one and leave it only in the other.
	res, streamedText, streamedReasoning, err := r.drainStream(stream, runID, built.HandoffToolNames, sendEvent)
	if err != nil {
		if isCancellation(ctx, err) {
			r.savePartialTurn(sessionID, runID, agent.Model, input, "cancelled", "", streamedReasoning, streamedText, "", "")
			sendEvent(protocol.EventRunCancelled, protocol.RunCancelled{RunID: runID})
		} else {
			// Persist the guardrail name/stage alongside the error so a reload
			// rebuilds the "Blocked by guardrail X" card, not a generic error.
			gerr := runErrorFor(runID, err, "stream_error")
			r.savePartialTurn(sessionID, runID, agent.Model, input, "error", err.Error(), streamedReasoning, streamedText, gerr.Guardrail, gerr.Stage)
			sendEvent(protocol.EventRunError, gerr)
			return mkErrResult(gerr.Code, err.Error())
		}
		return mkResult()
	}

	return r.finishResult(res, runID, sessionID, agentConfigID, sandboxID, sendEvent)
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
func (r *Runner) ResumeRun(runID string, state *agents.RunState, sessionID, agentConfigID, sandboxID string, onDone func(*RunResult)) (string, error) {
	seg, ctx, err := r.hub.resume(runID, sessionID, agentConfigID, sandboxID, r.taskMeta(r.hub.rootCtx, sessionID))
	if err != nil {
		return "", err
	}
	if r.OnRunAttach != nil {
		r.OnRunAttach(runID)
	}
	go func() {
		// See startRunWithID: this segment owns its teardown, so a later resume
		// swapping in a fresh gate can never collide with this goroutine's close.
		defer seg.finalize()
		result := r.resumeStreamed(ctx, runID, state, sessionID, agentConfigID, sandboxID)
		// Same ordering rationale as StartRun: approval row before slot release.
		r.afterRun(runID, result)
		r.hub.finish(runID, result.Interrupted)
		r.postRun(runID, sessionID, result)
		if onDone != nil {
			onDone(result)
		}
	}()
	return runID, nil
}

// resumeStreamed continues an interrupted run to completion under its
// original run id, publishing events to the (reopened) hub run, and returns
// its outcome.
func (r *Runner) resumeStreamed(ctx context.Context, runID string, state *agents.RunState, sessionID, agentConfigID, sandboxID string) *RunResult {
	log := zerolog.Ctx(ctx)

	sendEvent := func(typ string, payload any) {
		env, err := protocol.NewEnvelope(typ, payload)
		if err != nil {
			log.Error().Err(err).Str("type", typ).Msg("marshal event")
			return
		}
		r.hub.publish(runID, env)
	}

	mkResult := func() *RunResult {
		return &RunResult{RunID: runID, SessionID: sessionID, AgentConfigID: agentConfigID, SandboxID: sandboxID}
	}
	mkErrResult := func(code, msg string) *RunResult {
		res := mkResult()
		res.ErrCode, res.ErrMessage = code, msg
		return res
	}

	task := r.taskMeta(ctx, sessionID)
	// The resumed segment re-announces the original prompt so a late-joining
	// browser (attached at resume) can render the user bubble; earlier
	// subscribers dedup it against the bubble they already show.
	started := protocol.RunStarted{RunID: runID, SessionID: sessionID, Input: userInputText(state.UserInput)}
	if task != nil {
		started.ParentSessionID = task.ParentSessionID
		started.ParentRunID = task.ParentRunID
		started.TaskID = task.TaskID
		started.ToolCallID = task.ToolCallID
		started.Label = task.Label
	}
	sendEvent(protocol.EventRunStarted, started)

	// Any failed continuation must persist the user's prompt (and the error):
	// the pending-approval row was consumed as the resume's claim and the SDK
	// saves the turn only on success — without this, a failed resume would
	// lose the whole turn from durable state. Mirrors runStreamed's
	// partial-turn save, including the in-flight turn's streamed text/reasoning.
	failTurn := func(model, code string, err error, partialReasoning, partialText string) *RunResult {
		// The original run persisted the prompt and its completed turns under this
		// same run id before pausing, so a failed resume only annotates why it
		// stopped — cancelled or errored, mirroring runStreamed.
		if isCancellation(ctx, err) {
			r.savePartialTurn(sessionID, runID, model, userInputText(state.UserInput), "cancelled", "", partialReasoning, partialText, "", "")
			sendEvent(protocol.EventRunCancelled, protocol.RunCancelled{RunID: runID})
		} else {
			gerr := runErrorFor(runID, err, code)
			r.savePartialTurn(sessionID, runID, model, userInputText(state.UserInput), "error", err.Error(), partialReasoning, partialText, gerr.Guardrail, gerr.Stage)
			sendEvent(protocol.EventRunError, gerr)
			return mkErrResult(gerr.Code, err.Error())
		}
		return mkResult()
	}

	built, err := buildFullAgent(ctx, r.Deps, agentConfigID, sandboxID, task != nil)
	if err != nil {
		return failTurn("", "config_error", err, "", "")
	}
	provider := built.Provider
	if provider == nil {
		return failTurn(built.Agent.Model, "config_error", errors.New("no API key configured for this agent"), "", "")
	}

	provider = BuildRouterProvider(ctx, r.Deps, provider)

	resumeSA := store.NewSessionAdapter(r.db, sessionID)
	resumeSA.SetRunID(runID)
	resumeSA.SetModel(built.Agent.Model)
	tracer := newTracer(sendEvent, r.Deps.Traces, sessionID, runID)

	resumeSession := wrapCompaction(resumeSA, built, provider, sendEvent, runID)

	// Stream the continuation like a fresh run: the resumed segment's events
	// (the approved tool's output, every later turn's text and tool calls) go
	// live to the client instead of surfacing only in the terminal
	// run.output — a resume that silently swallowed its middle turns is what
	// made approved runs "jump" to their final answer.
	stream, ctrl := agents.ResumeRun(ctx, state, agents.RunOptions{
		Guardrails: built.RunGuardrails,
		// exec_command's approval gate reads a session id from here.
		Context: trustSessionID(sessionID, task),
		Conversation: agents.ConversationOptions{
			Session:               resumeSession,
			UsePreviousResponseID: built.UsePreviousResponseID,
			Settings:              sessionSettingsFor(built.HistoryLimit),
		},
		Exec: agents.ExecOptions{
			MaxTurns:           built.MaxTurns,
			MaxToolConcurrency: built.MaxToolConcurrency,
			ErrorHandlers:      built.ErrorHandlers,
			// A resume continues the same run, so it carries the same stop
			// policy: without it an approved run would sail past the tool it
			// was configured to stop at.
			ShouldStopAfterTurn:   stopAtTools(built.StopAtTools),
			ReasoningItemIDPolicy: built.ReasoningItemIDPolicy,
		},
		Model:   agents.ModelOptions{Provider: provider},
		Observe: agents.ObserveOptions{Tracer: tracer},
	})
	r.hub.setControl(runID, ctrl)
	res, streamedText, streamedReasoning, err := r.drainStream(stream, runID, built.HandoffToolNames, sendEvent)
	if err != nil {
		return failTurn(built.Agent.Model, "resume_error", err, streamedReasoning, streamedText)
	}

	// Title generation is not triggered here: the original run already fired it
	// in parallel at its start (even for an approval-gated first turn, which
	// pauses before finishing), so a resume never needs to.
	return r.finishResult(res, runID, sessionID, agentConfigID, sandboxID, sendEvent)
}

func (r *Runner) finishResult(res *agents.RunResult, runID, sessionID, agentConfigID, sandboxID string, sendEvent func(string, any)) *RunResult {
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
		return &RunResult{
			RunID:         runID,
			SessionID:     sessionID,
			AgentConfigID: agentConfigID,
			SandboxID:     sandboxID,
			Interrupted:   true,
			Interruptions: res.Interruptions,
			SDKState:      res.State,
		}
	}

	r.updateSessionMeta(sessionID, agentConfigID)

	finalText := res.FinalOutputString()
	sendEvent(protocol.EventRunOutput, protocol.RunOutput{RunID: runID, FinalOutput: finalText})
	return &RunResult{FinalText: finalText, RunID: runID, SessionID: sessionID, AgentConfigID: agentConfigID, SandboxID: sandboxID}
}

func (r *Runner) updateSessionMeta(sessionID, agentConfigID string) {
	if agentConfigID == "" {
		return
	}
	if _, err := r.db.NewUpdate().Model((*store.Session)(nil)).
		Set("agent_config_id = ?", agentConfigID).
		Where("id = ?", sessionID).
		Where("agent_config_id = '' OR agent_config_id IS NULL").
		Exec(context.Background()); err != nil {
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
	if log.GetLevel() == zerolog.Disabled {
		nop := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).With().Timestamp().Logger()
		log = &nop
	}

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

	// The run's own updateSessionMeta only ever sets agent_config_id, never the
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

// savePartialTurn records what the SDK cannot save itself when a run is
// cancelled or fails mid-stream. The SDK persists the user input and every
// completed turn incrementally (see agents.runner.persistSessionItems), so
// completed segments and tool calls survive on their own. This adds, all as
// display-only annotations that are never replayed:
// - the in-flight turn's streamed reasoning and text, so a cancel during the
// thinking phase (before that turn completed) still shows what the model was
// doing instead of vanishing;
// - a trailing marker for why the run stopped (annRole "cancelled"/"error",
// annMsg its optional detail; guardrail+stage, when set, tag an "error"
// marker as a guardrail block so a reload rebuilds the typed card);
//
// and, only when the run died before the SDK persisted anything under this run
// id (e.g. cancelled before the first turn completed), the prompt as a
// replayable fallback so it is not lost.
func (r *Runner) savePartialTurn(sessionID, runID, model, userInput, annRole, annMsg, partialReasoning, partialText, guardrail, stage string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	msgs := make([]store.Message, 0, 4)

	if userInput != "" && !r.runHasPersistedItems(ctx, sessionID, runID) {
		userItemJSON, _ := json.Marshal(map[string]any{
			"role":    "user",
			"content": userInput,
		})
		msgs = append(msgs, store.NewItemMessageRaw(sessionID, runID, model, userItemJSON))
	}

	// The in-flight turn's thinking and narration — display-only (a fabricated
	// reasoning item would be rejected on replay, and an abandoned turn should
	// not enter the model's history).
	if partialReasoning != "" {
		msgs = append(msgs, store.NewAnnotationMessage(sessionID, runID, "reasoning", partialReasoning))
	}
	if partialText != "" {
		msgs = append(msgs, store.NewAnnotationMessage(sessionID, runID, "assistant", partialText))
	}

	if annRole != "" {
		m := store.NewAnnotationMessage(sessionID, runID, annRole, annMsg)
		// A guardrail block carries its name + stage so a reload rebuilds the
		// typed "Blocked by guardrail X" card instead of a generic error.
		if guardrail != "" {
			m.Display, _ = json.Marshal(map[string]string{"guardrail": guardrail, "stage": stage})
		}
		msgs = append(msgs, m)
	}

	if len(msgs) == 0 {
		return
	}
	if _, err := r.db.NewInsert().Model(&msgs).Exec(ctx); err != nil {
		// The partial-turn save is the only durable record of a cancelled/failed
		// turn's prompt and in-flight thinking; a lost write means a reload shows
		// nothing. Best-effort, but never silent.
		zerolog.Ctx(r.hub.rootCtx).Warn().Err(err).Str("run_id", runID).Str("session_id", sessionID).
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
// duplicating the prompt the SDK's per-turn persistence normally saves.
func (r *Runner) runHasPersistedItems(ctx context.Context, sessionID, runID string) bool {
	exists, err := r.db.NewSelect().Model((*store.Message)(nil)).
		Where("session_id = ?", sessionID).
		Where("run_id = ?", runID).
		Where("kind = ?", store.MessageKindItem).
		Exists(ctx)
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
			if mo, ok := e.Item.(*agents.MessageOutputItem); ok {
				if text := mo.Text(); text != "" {
					send(protocol.EventRunMessage, protocol.RunMessage{RunID: runID, Text: text, ItemID: mo.Raw.ID})
				}
			}
		case "reasoning_item_created":
			// The completed thinking block, authoritative over the run.reasoning
			// deltas that previewed it — and the only thinking signal when the
			// backend streams no reasoning deltas or the segment was resumed.
			if ri, ok := e.Item.(*agents.ReasoningItem); ok {
				if text := ri.Text(); text != "" {
					send(protocol.EventRunReasoningItem, protocol.RunReasoningItem{RunID: runID, Text: text, ItemID: ri.Raw.ID})
				}
			}
		case "tool_called":
			if tc, ok := e.Item.(*agents.ToolCallItem); ok {
				fc := tc.FunctionCall()
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
			if to, ok := e.Item.(*agents.ToolCallOutputItem); ok {
				callID := ""
				if fco := to.Raw.OfFunctionCallOutput; fco != nil {
					callID = fco.CallID
				}
				send(protocol.EventRunToolResult, protocol.RunToolResult{
					RunID:      runID,
					ToolCallID: callID,
					Output:     fmt.Sprintf("%v", to.Output),
				})
			}
		case "handoff_requested":
			if hc, ok := e.Item.(*agents.HandoffCallItem); ok {
				send(protocol.EventRunHandoff, protocol.RunHandoff{
					RunID: runID,
					From:  hc.AgentRef().Name,
				})
			}
		case "handoff_occured":
			if ho, ok := e.Item.(*agents.HandoffOutputItem); ok {
				send(protocol.EventRunHandoff, protocol.RunHandoff{
					RunID: runID,
					From:  ho.SourceAgent.Name,
					To:    ho.TargetAgent.Name,
				})
			}
		}

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
