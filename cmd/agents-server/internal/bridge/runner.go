package bridge

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

// EventSink receives protocol envelopes emitted during a streamed run.
type EventSink func(env *protocol.Envelope)

// Runner executes and tracks streamed agent runs, keyed by run id for cancellation.
type Runner struct {
	db   *bun.DB
	Deps *AgentDeps

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

// NewRunner creates a Runner backed by the given database and agent dependencies.
func NewRunner(db *bun.DB, deps *AgentDeps) *Runner {
	return &Runner{
		db:      db,
		Deps:    deps,
		cancels: make(map[string]context.CancelFunc),
	}
}

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

// RunStreamed runs the agent for the given session, streaming events to the sink and returning the run outcome.
func (r *Runner) RunStreamed(ctx context.Context, sessionID, agentConfigID, sandboxID, input string, sink EventSink) *RunResult {
	runID := store.NewID()
	ctx, cancel := context.WithCancel(ctx)

	r.mu.Lock()
	r.cancels[runID] = cancel
	r.mu.Unlock()

	defer func() {
		cancel()
		r.mu.Lock()
		delete(r.cancels, runID)
		r.mu.Unlock()
	}()

	log := zerolog.Ctx(ctx)

	sendEvent := func(typ string, payload any) {
		env, err := protocol.NewEnvelope(typ, payload)
		if err != nil {
			log.Error().Err(err).Str("type", typ).Msg("marshal event")
			return
		}
		sink(env)
	}

	sendEvent("run.started", protocol.RunStarted{RunID: runID})

	mkResult := func() *RunResult {
		return &RunResult{RunID: runID, SessionID: sessionID, AgentConfigID: agentConfigID, SandboxID: sandboxID}
	}

	// Build fully configured agent from DB config
	built, err := BuildFullAgent(ctx, r.Deps, agentConfigID, sandboxID)
	if err != nil {
		sendEvent("run.error", protocol.RunError{
			Code:    "config_error",
			Message: err.Error(),
		})
		return mkResult()
	}

	agent := built.Agent
	provider := built.Provider
	if provider == nil {
		sendEvent("run.error", protocol.RunError{
			Code:    "config_error",
			Message: "no API key configured for this agent",
		})
		return mkResult()
	}

	// Wrap with router provider if routes exist
	provider = BuildRouterProvider(ctx, r.Deps, provider)

	sendEvent("run.agent_start", protocol.RunAgentStart{AgentName: agent.Name})

	session := store.NewSessionAdapter(r.db, sessionID)

	hooks := newWSRunHooks(sendEvent, r.Deps.Traces, sessionID, runID)
	tracer := newTracer(sendEvent, r.Deps.Traces, sessionID, runID)

	opts := agents.RunOptions{
		Session:               session,
		ModelProvider:         provider,
		MaxTurns:              built.MaxTurns,
		Hooks:                 hooks,
		Tracer:                tracer,
		UsePreviousResponseID: built.UsePreviousResponseID,
		MaxToolConcurrency:    built.MaxToolConcurrency,
	}
	if built.HandoffInputFilter == "nest_history" {
		opts.HandoffInputFilter = agents.NestHandoffHistory(agents.NestHistoryOptions{})
	}
	opts.ToolNotFoundBehavior = agents.ParseToolNotFoundBehavior(built.ToolNotFoundBehavior)

	sr := agents.RunStreamed(ctx, agent, input, opts)

	for event, err := range sr.Events() {
		if err != nil {
			if ctx.Err() != nil {
				sendEvent("run.cancelled", nil)
			} else {
				sendEvent("run.error", protocol.RunError{
					Code:    "stream_error",
					Message: err.Error(),
				})
			}
			return mkResult()
		}
		r.handleStreamEvent(event, sendEvent)
	}

	result := r.processResult(sr, runID, sessionID, agentConfigID, sandboxID, sendEvent)
	if result.FinalText != "" {
		go r.maybeGenerateTitle(sessionID, agentConfigID, input, sendEvent)
	}
	return result
}

// ResumeStreamed continues a paused run after HITL approval/rejection.
func (r *Runner) ResumeStreamed(ctx context.Context, state *agents.RunState, sessionID, agentConfigID, sandboxID string, sink EventSink) *RunResult {
	runID := store.NewID()
	ctx, cancel := context.WithCancel(ctx)

	r.mu.Lock()
	r.cancels[runID] = cancel
	r.mu.Unlock()

	defer func() {
		cancel()
		r.mu.Lock()
		delete(r.cancels, runID)
		r.mu.Unlock()
	}()

	log := zerolog.Ctx(ctx)

	sendEvent := func(typ string, payload any) {
		env, err := protocol.NewEnvelope(typ, payload)
		if err != nil {
			log.Error().Err(err).Str("type", typ).Msg("marshal event")
			return
		}
		sink(env)
	}

	mkResult := func() *RunResult {
		return &RunResult{RunID: runID, SessionID: sessionID, AgentConfigID: agentConfigID, SandboxID: sandboxID}
	}

	sendEvent("run.started", protocol.RunStarted{RunID: runID})

	built, err := BuildFullAgent(ctx, r.Deps, agentConfigID, sandboxID)
	if err != nil {
		sendEvent("run.error", protocol.RunError{
			Code:    "config_error",
			Message: err.Error(),
		})
		return mkResult()
	}
	provider := built.Provider
	if provider == nil {
		sendEvent("run.error", protocol.RunError{
			Code:    "config_error",
			Message: "no API key configured for this agent",
		})
		return mkResult()
	}

	res, err := agents.ResumeRun(ctx, state, agents.RunOptions{
		ModelProvider: provider,
	})
	if err != nil {
		sendEvent("run.error", protocol.RunError{
			Code:    "resume_error",
			Message: err.Error(),
		})
		return mkResult()
	}

	return r.finishResult(res, runID, sessionID, agentConfigID, sandboxID, sendEvent)
}

func (r *Runner) processResult(sr *agents.StreamedResult, runID, sessionID, agentConfigID, sandboxID string, sendEvent func(string, any)) *RunResult {
	res, err := sr.FinalResult()
	if err != nil {
		sendEvent("run.error", protocol.RunError{
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
				ToolCallID:    item.CallID,
				ToolName:      item.ToolName,
				Arguments:     item.Arguments,
				NeedsApproval: true,
			})
		}
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
	sendEvent("run.output", protocol.RunOutput{FinalOutput: finalText})
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

func (r *Runner) maybeGenerateTitle(sessionID, agentConfigID, userInput string, sendEvent func(string, any)) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

// CancelRun cancels the in-flight run with the given run id, if one is active.
func (r *Runner) CancelRun(runID string) {
	r.mu.Lock()
	cancel, ok := r.cancels[runID]
	r.mu.Unlock()
	if ok {
		cancel()
	}
}

func (r *Runner) handleStreamEvent(event agents.StreamEvent, send func(string, any)) {
	switch e := event.(type) {
	case *agents.RawResponsesStreamEvent:
		if e.Data == nil {
			return
		}
		delta := extractDelta(e.Data)
		if delta != "" {
			send("run.step", protocol.RunStep{Delta: delta})
		}

	case *agents.RunItemStreamEvent:
		switch e.Name {
		case "tool_called":
			if tc, ok := e.Item.(*agents.ToolCallItem); ok {
				fc := tc.FunctionCall()
				send("run.tool_call", protocol.RunToolCall{
					ToolCallID: fc.CallID,
					ToolName:   fc.Name,
					Arguments:  fc.Arguments,
				})
			}
		case "tool_output":
			if to, ok := e.Item.(*agents.ToolCallOutputItem); ok {
				send("run.tool_result", protocol.RunToolResult{
					ToolCallID: "",
					Output:     fmt.Sprintf("%v", to.Output),
				})
			}
		case "handoff_requested":
			if hc, ok := e.Item.(*agents.HandoffCallItem); ok {
				send("run.handoff", protocol.RunHandoff{
					From: hc.AgentRef().Name,
				})
			}
		case "handoff_occured":
			if ho, ok := e.Item.(*agents.HandoffOutputItem); ok {
				send("run.handoff", protocol.RunHandoff{
					From: ho.SourceAgent.Name,
					To:   ho.TargetAgent.Name,
				})
			}
		}

	case *agents.AgentUpdatedStreamEvent:
		send("run.agent_start", protocol.RunAgentStart{
			AgentName: e.NewAgent.Name,
		})
	}
}

func extractDelta(event *agents.TResponseStreamEvent) string {
	if event.Type == "response.output_text.delta" {
		return event.Delta
	}
	return ""
}
