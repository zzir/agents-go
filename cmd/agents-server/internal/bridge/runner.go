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
	db   *bun.DB
	Deps *AgentDeps
	hub  *RunHub
}

// compactionNotifier drives the chat UI's live indicator with transient
// run.compaction status events. Trace recording is the compaction span's job
// (opened by the SDK runner via CompactionArgs.StartSpan), not the notifier's.
func compactionNotifier(send func(string, any), runID string) store.CompactionNotifier {
	return store.CompactionNotifier{
		OnStart: func() {
			send("run.compaction", protocol.RunCompaction{RunID: runID, Phase: "started"})
		},
		OnDone: func(before, after int) {
			send("run.compaction", protocol.RunCompaction{
				RunID:  runID,
				Phase:  "finished",
				Detail: fmt.Sprintf("compacted %d→%d items", before, after),
			})
		},
	}
}

// wrapCompaction wraps sa with the compaction adapter when the agent config
// enables it. An empty summary model falls back to the agent's own model, so
// leaving the field blank does not silently disable compaction.
func wrapCompaction(sa *store.SessionAdapter, built *BuildResult, provider agents.ModelProvider, send func(string, any), runID string) agents.Session {
	if !built.CompactionEnabled || provider == nil {
		return sa
	}
	modelName := built.CompactionModel
	if modelName == "" {
		modelName = built.Agent.Model
	}
	summaryModel, err := provider.GetModel(modelName)
	if err != nil || summaryModel == nil {
		return sa
	}
	return store.NewCompactionAdapter(sa, summaryModel,
		built.CompactionThreshold, built.CompactionWindow, built.CompactionPrompt,
		compactionNotifier(send, runID),
	)
}

// NewRunner creates a Runner backed by the given database and agent
// dependencies. rootCtx scopes every run's lifetime (see RunHub); cancelling
// it stops all in-flight runs.
func NewRunner(rootCtx context.Context, db *bun.DB, deps *AgentDeps) *Runner {
	return &Runner{
		db:   db,
		Deps: deps,
		hub:  NewRunHub(rootCtx),
	}
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
	// Reject unknown sessions up front so we never register a run (or write
	// orphaned messages) against a non-existent session.
	if _, err := r.Deps.Sessions.Get(r.hub.rootCtx, sessionID); err != nil {
		return "", err
	}
	runID := store.NewID()
	_, ctx, err := r.hub.register(runID, sessionID, agentConfigID, sandboxID)
	if err != nil {
		return "", err
	}
	go func() {
		result := r.runStreamed(ctx, runID, sessionID, agentConfigID, sandboxID, input)
		r.hub.finish(runID, result.Interrupted)
		r.afterRun(runID, result)
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

	sendEvent("run.started", protocol.RunStarted{RunID: runID, SessionID: sessionID})

	mkResult := func() *RunResult {
		return &RunResult{RunID: runID, SessionID: sessionID, AgentConfigID: agentConfigID, SandboxID: sandboxID}
	}

	// Refuse to run against a session that doesn't exist — otherwise the run
	// would write orphaned messages under an arbitrary session id.
	if _, err := r.Deps.Sessions.Get(ctx, sessionID); err != nil {
		sendEvent("run.error", protocol.RunError{
			RunID:   runID,
			Code:    "session_not_found",
			Message: "session not found: " + sessionID,
		})
		return mkResult()
	}

	// Build fully configured agent from DB config
	built, err := BuildFullAgent(ctx, r.Deps, agentConfigID, sandboxID)
	if err != nil {
		sendEvent("run.error", protocol.RunError{
			RunID:   runID,
			Code:    "config_error",
			Message: err.Error(),
		})
		return mkResult()
	}

	agent := built.Agent
	provider := built.Provider
	if provider == nil {
		sendEvent("run.error", protocol.RunError{
			RunID:   runID,
			Code:    "config_error",
			Message: "no API key configured for this agent",
		})
		return mkResult()
	}

	// Wrap with router provider if routes exist
	provider = BuildRouterProvider(ctx, r.Deps, provider)

	sendEvent("run.agent_start", protocol.RunAgentStart{RunID: runID, AgentName: agent.Name})

	sa := store.NewSessionAdapter(r.db, sessionID)
	sa.SetRunID(runID)
	sa.SetModel(agent.Model)
	tracer := newTracer(sendEvent, r.Deps.Traces, sessionID, runID)

	runSession := wrapCompaction(sa, built, provider, sendEvent, runID)

	opts := agents.RunOptions{
		Session:               runSession,
		ModelProvider:         provider,
		MaxTurns:              built.MaxTurns,
		Tracer:                tracer,
		UsePreviousResponseID: built.UsePreviousResponseID,
		MaxToolConcurrency:    built.MaxToolConcurrency,
	}
	if built.HandoffInputFilter == "nest_history" {
		opts.HandoffInputFilter = agents.NestHandoffHistory(agents.NestHistoryOptions{})
	}
	opts.ToolNotFoundBehavior = agents.ParseToolNotFoundBehavior(built.ToolNotFoundBehavior)

	sr := agents.RunStreamed(ctx, agent, input, opts)

	var streamedText, streamedReasoning strings.Builder
	for event, err := range sr.Events() {
		if err != nil {
			if ctx.Err() != nil {
				r.savePartialTurn(sessionID, runID, agent.Model, input, streamedText.String(), streamedReasoning.String(), "")
				sendEvent("run.cancelled", protocol.RunCancelled{RunID: runID})
			} else {
				r.savePartialTurn(sessionID, runID, agent.Model, input, streamedText.String(), streamedReasoning.String(), err.Error())
				sendEvent("run.error", protocol.RunError{
					RunID:   runID,
					Code:    "stream_error",
					Message: err.Error(),
				})
			}
			return mkResult()
		}
		if raw, ok := event.(*agents.RawResponsesStreamEvent); ok && raw.Data != nil {
			switch raw.Data.Type {
			case "response.output_text.delta":
				streamedText.WriteString(raw.Data.Delta)
			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				streamedReasoning.WriteString(raw.Data.Delta)
			}
		}
		r.handleStreamEvent(event, runID, sendEvent)
	}

	result := r.processResult(sr, runID, sessionID, agentConfigID, sandboxID, sendEvent)
	if result.FinalText != "" {
		go r.maybeGenerateTitle(r.hub.rootCtx, sessionID, agentConfigID, sendEvent)
	}
	return result
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
	ctx, err := r.hub.resume(runID, sessionID, agentConfigID, sandboxID)
	if err != nil {
		return "", err
	}
	go func() {
		result := r.resumeStreamed(ctx, runID, state, sessionID, agentConfigID, sandboxID)
		r.hub.finish(runID, result.Interrupted)
		r.afterRun(runID, result)
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

	sendEvent("run.started", protocol.RunStarted{RunID: runID, SessionID: sessionID})

	// Any failed continuation must persist the user's prompt (and the error):
	// the pending-approval row was consumed as the resume's claim and the SDK
	// saves the turn only on success — without this, a failed resume would
	// lose the whole turn from durable state. Mirrors runStreamed's
	// partial-turn save.
	failTurn := func(model, code string, err error) *RunResult {
		r.savePartialTurn(sessionID, runID, model, userInputText(state.UserInput), "", "", err.Error())
		sendEvent("run.error", protocol.RunError{RunID: runID, Code: code, Message: err.Error()})
		return mkResult()
	}

	built, err := BuildFullAgent(ctx, r.Deps, agentConfigID, sandboxID)
	if err != nil {
		return failTurn("", "config_error", err)
	}
	provider := built.Provider
	if provider == nil {
		return failTurn(built.Agent.Model, "config_error", errors.New("no API key configured for this agent"))
	}

	provider = BuildRouterProvider(ctx, r.Deps, provider)

	resumeSA := store.NewSessionAdapter(r.db, sessionID)
	resumeSA.SetRunID(runID)
	resumeSA.SetModel(built.Agent.Model)
	tracer := newTracer(sendEvent, r.Deps.Traces, sessionID, runID)

	resumeSession := wrapCompaction(resumeSA, built, provider, sendEvent, runID)

	res, err := agents.ResumeRun(ctx, state, agents.RunOptions{
		Session:               resumeSession,
		ModelProvider:         provider,
		MaxTurns:              built.MaxTurns,
		Tracer:                tracer,
		UsePreviousResponseID: built.UsePreviousResponseID,
		MaxToolConcurrency:    built.MaxToolConcurrency,
	})
	if err != nil {
		return failTurn(built.Agent.Model, "resume_error", err)
	}

	result := r.finishResult(res, runID, sessionID, agentConfigID, sandboxID, sendEvent)
	// A resumed run that reaches a final answer is where an approval-gated
	// first turn actually completes, so title generation must fire here too —
	// the initial (interrupted) segment had no final output to trigger it.
	if result.FinalText != "" {
		go r.maybeGenerateTitle(r.hub.rootCtx, sessionID, agentConfigID, sendEvent)
	}
	return result
}

func (r *Runner) processResult(sr *agents.StreamedResult, runID, sessionID, agentConfigID, sandboxID string, sendEvent func(string, any)) *RunResult {
	res, err := sr.FinalResult()
	if err != nil {
		sendEvent("run.error", protocol.RunError{
			RunID:   runID,
			Code:    "run_error",
			Message: err.Error(),
		})
		return &RunResult{RunID: runID, SessionID: sessionID, AgentConfigID: agentConfigID, SandboxID: sandboxID}
	}

	return r.finishResult(res, runID, sessionID, agentConfigID, sandboxID, sendEvent)
}

func (r *Runner) finishResult(res *agents.RunResult, runID, sessionID, agentConfigID, sandboxID string, sendEvent func(string, any)) *RunResult {
	if len(res.Interruptions) > 0 {
		for _, item := range res.Interruptions {
			sendEvent("run.tool_call", protocol.RunToolCall{
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
		sendEvent("run.interrupted", protocol.RunInterrupted{RunID: runID})
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
	sendEvent("run.output", protocol.RunOutput{RunID: runID, FinalOutput: finalText})
	return &RunResult{FinalText: finalText, RunID: runID, SessionID: sessionID, AgentConfigID: agentConfigID, SandboxID: sandboxID}
}

func (r *Runner) updateSessionMeta(sessionID, agentConfigID string) {
	if agentConfigID == "" {
		return
	}
	_, _ = r.db.NewUpdate().Model((*store.Session)(nil)).
		Set("agent_config_id = ?", agentConfigID).
		Where("id = ?", sessionID).
		Where("agent_config_id = '' OR agent_config_id IS NULL").
		Exec(context.Background())
}

// maybeGenerateTitle names a still-default ("New Chat") session from its first
// user message. It sources that message from the database rather than a passed
// input so both the initial-run and the HITL-resume completion paths can call
// it — the SDK persists the session (input included) on successful completion,
// which is exactly when this fires.
func (r *Runner) maybeGenerateTitle(parentCtx context.Context, sessionID, agentConfigID string, sendEvent func(string, any)) {
	ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel()
	log := zerolog.Ctx(ctx)
	if log.GetLevel() == zerolog.Disabled {
		nop := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).With().Timestamp().Logger()
		log = &nop
	}

	sess, err := r.Deps.Sessions.Get(ctx, sessionID)
	if err != nil || sess.Name != "New Chat" {
		return
	}

	userInput := r.firstUserMessage(ctx, sessionID)
	if userInput == "" {
		return
	}

	built, err := BuildFullAgent(ctx, r.Deps, agentConfigID, "")
	if err != nil {
		log.Warn().Err(err).Msg("title gen: build agent failed")
		return
	}
	if built.Provider == nil {
		log.Warn().Msg("title gen: no provider available")
		return
	}

	titleAgent := &agents.Agent{
		Name:         "title_gen",
		Model:        built.Agent.Model,
		Instructions: agents.StaticInstructions("You generate concise chat titles. Reply with ONLY the title text, nothing else. No quotes. Under 30 characters."),
	}
	prompt := "Generate a short title for this chat:\n\n" + userInput
	sr := agents.RunStreamed(ctx, titleAgent, prompt, agents.RunOptions{
		ModelProvider: built.Provider,
		MaxTurns:      1,
	})
	for _, err := range sr.Events() {
		if err != nil {
			log.Warn().Err(err).Msg("title gen: stream error")
			return
		}
	}
	res, err := sr.FinalResult()
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

	if err := r.Deps.Sessions.Update(ctx, sessionID, title); err != nil {
		log.Warn().Err(err).Msg("title gen: save failed")
		return
	}
	sendEvent("session.title_updated", protocol.SessionTitleUpdated{
		SessionID: sessionID,
		Title:     title,
	})
}

// firstUserMessage returns the text content of the earliest user message in a
// session, or "" if there is none. Used to seed title generation.
func (r *Runner) firstUserMessage(ctx context.Context, sessionID string) string {
	var msg store.Message
	err := r.db.NewSelect().Model(&msg).
		Column("content").
		Where("session_id = ?", sessionID).
		Where("role = ?", "user").
		Order("id ASC").
		Limit(1).
		Scan(ctx)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(msg.Content)
}

// savePartialTurn persists the user input and any partial assistant response
// (reasoning + text) to the session when a run is cancelled or fails
// mid-stream. Without this, the entire turn is lost on reload because the SDK
// only saves the session on successful completion. The user input and partial
// text go through the store's item chokepoint (canonical wire JSON,
// replayable); the partial reasoning and error are annotations — shown in the
// UI but never replayed (a fabricated reasoning item would be rejected by the
// API).
func (r *Runner) savePartialTurn(sessionID, runID, model, userInput, partialText, partialReasoning, errMsg string) {
	if userInput == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	msgs := make([]store.Message, 0, 4)

	userItemJSON, _ := json.Marshal(map[string]any{
		"role":    "user",
		"content": userInput,
	})
	msgs = append(msgs, store.NewItemMessageRaw(sessionID, runID, model, userItemJSON))

	if partialReasoning != "" {
		msgs = append(msgs, store.NewAnnotationMessage(sessionID, runID, "reasoning", partialReasoning))
	}

	if partialText != "" {
		assistantItemJSON, _ := json.Marshal(map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []map[string]string{
				{"type": "output_text", "text": partialText},
			},
		})
		msgs = append(msgs, store.NewItemMessageRaw(sessionID, runID, model, assistantItemJSON))
	}

	if errMsg != "" {
		msgs = append(msgs, store.NewAnnotationMessage(sessionID, runID, "error", errMsg))
	}

	_, _ = r.db.NewInsert().Model(&msgs).Exec(ctx)
}

// CancelRun cancels the in-flight run with the given run id, if one is active.
func (r *Runner) CancelRun(runID string) {
	r.hub.Cancel(runID)
}

func (r *Runner) handleStreamEvent(event agents.StreamEvent, runID string, send func(string, any)) {
	switch e := event.(type) {
	case *agents.RawResponsesStreamEvent:
		if e.Data == nil {
			return
		}
		switch e.Data.Type {
		case "response.output_text.delta":
			if e.Data.Delta != "" {
				send("run.step", protocol.RunStep{RunID: runID, Delta: e.Data.Delta})
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if e.Data.Delta != "" {
				send("run.reasoning", protocol.RunReasoning{RunID: runID, Delta: e.Data.Delta})
			}
		}

	case *agents.RunItemStreamEvent:
		switch e.Name {
		case "tool_called":
			if tc, ok := e.Item.(*agents.ToolCallItem); ok {
				fc := tc.FunctionCall()
				send("run.tool_call", protocol.RunToolCall{
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
				send("run.tool_result", protocol.RunToolResult{
					RunID:      runID,
					ToolCallID: callID,
					Output:     fmt.Sprintf("%v", to.Output),
				})
			}
		case "handoff_requested":
			if hc, ok := e.Item.(*agents.HandoffCallItem); ok {
				send("run.handoff", protocol.RunHandoff{
					RunID: runID,
					From:  hc.AgentRef().Name,
				})
			}
		case "handoff_occured":
			if ho, ok := e.Item.(*agents.HandoffOutputItem); ok {
				send("run.handoff", protocol.RunHandoff{
					RunID: runID,
					From:  ho.SourceAgent.Name,
					To:    ho.TargetAgent.Name,
				})
			}
		}

	case *agents.AgentUpdatedStreamEvent:
		send("run.agent_start", protocol.RunAgentStart{
			RunID:     runID,
			AgentName: e.NewAgent.Name,
		})
	}
}
