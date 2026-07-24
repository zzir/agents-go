package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
)

// RunHandler exposes the REST surface for starting and observing agent runs.
// It shares the runner (and its hub) with the WebSocket handler, so a run
// started over either transport is observable over both.
type RunHandler struct {
	runner *bridge.Runner
}

// NewRunHandler returns a run handler backed by the given runner.
func NewRunHandler(runner *bridge.Runner) *RunHandler {
	return &RunHandler{runner: runner}
}

type createRunReq struct {
	Input         string `json:"input"`
	AgentConfigID string `json:"agent_config_id"`
	SandboxID     string `json:"sandbox_id"`
}

type createRunResp struct {
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
}

// Create starts a run for the session identified by the id path parameter.
// With ?wait=true it blocks until the run terminates and returns the final
// output; otherwise it returns 201 immediately with the run id (stream events
// via GET /runs/{id}/events).
//
//	@Summary Start run
//	@Description	Starts an agent run on the session. Default returns 201 with a run id; pass wait=true to block until the run ends and receive the final output — or status "interrupted" when it pauses for tool approval (act via /sessions/{id}/approvals). Fails 409 if the session already has an active run.
//	@Tags runs
//	@Accept json
//	@Produce json
//	@Param id path string true	"Session ID"
//	@Param wait	query bool false	"Block until the run finishes"
//	@Param run body createRunReq	true	"Run input"
//	@Success 201 {object}	createRunResp
//	@Success 200 {object}	map[string]interface{}	"Final result (wait=true)"
//	@Failure 400 {object}	ErrorResponse
//	@Failure 404 {object}	ErrorResponse
//	@Failure 409 {object}	ErrorResponse	"session already has an active run"
//	@Security BearerAuth
//	@Router /sessions/{id}/runs [post]
func (h *RunHandler) Create(c *gin.Context) {
	sessionID := c.Param("id")
	var req createRunReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}

	wait, _ := strconv.ParseBool(c.Query("wait"))
	if wait {
		h.createAndWait(c, sessionID, req)
		return
	}

	runID, err := h.runner.StartRun(sessionID, req.AgentConfigID, req.SandboxID, req.Input, nil)
	if err != nil {
		h.startError(c, err)
		return
	}
	c.JSON(http.StatusCreated, createRunResp{RunID: runID, SessionID: sessionID, Status: string(bridge.RunRunning)})
}

// createAndWait starts a run and blocks until it terminates, collecting the
// final output (or error / interruption) to return synchronously.
func (h *RunHandler) createAndWait(c *gin.Context, sessionID string, req createRunReq) {
	type outcome struct {
		final       string
		code        string
		msg         string
		interrupted bool
	}
	done := make(chan outcome, 1)
	report := func(o outcome) {
		select {
		case done <- o:
		default:
		}
	}
	sink := func(env *protocol.Envelope) {
		switch env.Type {
		case protocol.EventRunOutput:
			var p protocol.RunOutput
			_ = json.Unmarshal(env.Payload, &p)
			report(outcome{final: p.FinalOutput})
		case protocol.EventRunError:
			var p protocol.RunError
			_ = json.Unmarshal(env.Payload, &p)
			report(outcome{code: p.Code, msg: p.Message})
		case protocol.EventRunCancelled:
			report(outcome{code: "cancelled", msg: "run cancelled"})
		case protocol.EventRunInterrupted:
			report(outcome{interrupted: true})
		}
	}

	runID, err := h.runner.StartRun(sessionID, req.AgentConfigID, req.SandboxID, req.Input, nil)
	if err != nil {
		h.startError(c, err)
		return
	}
	// Subscribe from 0 so a terminal event that fired before we attached is
	// still replayed from the buffer.
	subID, ok := h.runner.Hub().Subscribe(runID, 0, sink)
	if !ok {
		internalError(c, io.ErrUnexpectedEOF)
		return
	}
	defer h.runner.Hub().Unsubscribe(runID, subID)

	select {
	case <-c.Request.Context().Done():
		// Client hung up; the run keeps executing in the hub.
		return
	case res := <-done:
		switch {
		case res.interrupted:
			// The run paused for tool approval: waiting any longer would
			// hang until a human acts. Report the state; the caller lists
			// GET /sessions/{id}/approvals and decides, which resumes the
			// run under the same run id.
			c.JSON(http.StatusOK, gin.H{"run_id": runID, "session_id": sessionID, "status": string(bridge.RunInterrupted)})
		case res.code != "":
			abortError(c, http.StatusBadGateway, CodeUpstream, res.msg)
		default:
			c.JSON(http.StatusOK, gin.H{"run_id": runID, "session_id": sessionID, "status": string(bridge.RunCompleted), "final_output": res.final})
		}
	}
}

func (h *RunHandler) startError(c *gin.Context, err error) {
	var busy bridge.ErrSessionBusy
	if errors.As(err, &busy) {
		conflict(c, "session already has an active run: "+busy.RunID)
		return
	}
	// The session exists but is already at its live-task cap: a state conflict
	// (409), not a missing resource.
	var limit bridge.ErrTaskLimit
	if errors.As(err, &limit) {
		conflict(c, limit.Error())
		return
	}
	// The remaining failures come from the session lookup StartRun does first:
	// an unknown session -> 404, any other DB error -> 500. Folding the latter
	// into 404 would mislabel a transient failure as "session not found".
	storeError(c, err)
}

// Get reports the current status of the run identified by the id path parameter.
//
//	@Summary	Get run status
//	@Tags runs
//	@Produce	json
//	@Param id	path string	true	"Run ID"
//	@Success	200	{object}	bridge.RunInfo
//	@Failure	404	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router /runs/{id} [get]
func (h *RunHandler) Get(c *gin.Context) {
	info, ok := h.runner.Hub().Info(c.Param("id"))
	if !ok {
		notFound(c)
		return
	}
	c.JSON(http.StatusOK, info)
}

// Cancel cancels the run identified by the id path parameter.
//
//	@Summary	Cancel run
//	@Tags runs
//	@Param id path	string	true	"Run ID"
//	@Param mode	query	string	false	"graceful = stop after the current turn; default aborts immediately"
//	@Success	204 "cancelling"
//	@Failure	404 {object}	ErrorResponse
//	@Security	BearerAuth
//	@Router /runs/{id}/cancel [post]
func (h *RunHandler) Cancel(c *gin.Context) {
	if _, ok := h.runner.Hub().Info(c.Param("id")); !ok {
		notFound(c)
		return
	}
	if c.Query("mode") == "graceful" {
		h.runner.StopRunAfterTurn(c.Param("id"))
	} else {
		h.runner.CancelRun(c.Param("id"))
	}
	c.Status(http.StatusNoContent)
}

// Events streams the run's events as Server-Sent Events. Each event's id is
// its hub sequence number; a reconnect with Last-Event-ID (or ?from_seq)
// resumes without loss. The stream ends after a terminal event.
//
//	@Summary Stream run events (SSE)
//	@Description	Server-Sent Events stream. Each event id is the hub sequence number; reconnect with the Last-Event-ID header or from_seq to resume. The stream closes after a terminal event: run.output, run.error, run.cancelled, or run.interrupted (paused for approval — deciding via /approvals resumes the SAME run id; reconnect with Last-Event-ID to continue the stream).
//	@Tags runs
//	@Produce text/event-stream
//	@Param id path string	true	"Run ID"
//	@Param from_seq	query int false	"Resume after this sequence number"
//	@Success 200 {string}	string	"SSE stream"
//	@Failure 404 {object}	ErrorResponse
//	@Security BearerAuth
//	@Router /runs/{id}/events [get]
func (h *RunHandler) Events(c *gin.Context) {
	runID := c.Param("id")
	fromSeq := 0
	if lastID := c.GetHeader("Last-Event-ID"); lastID != "" {
		fromSeq, _ = strconv.Atoi(lastID)
	} else if q := c.Query("from_seq"); q != "" {
		fromSeq, _ = strconv.Atoi(q)
	}

	// The events buffer matches the hub's replay buffer so replaying a full
	// history is lossless; live events beyond that are dropped under
	// backpressure (client backfills via from_seq after reconnecting).
	// Only a FINAL event (output/error/cancelled) closes the stream, via its
	// own guaranteed channel. run.interrupted is NOT final under same-id
	// resume — a replayed historical interrupt must flow as an ordinary event,
	// or a late subscriber to a resumed+completed run would be cut off at the
	// old pause and miss the real output.
	events := make(chan bridge.SeqEnvelope, bridge.EventBufferCap)
	terminal := make(chan bridge.SeqEnvelope, 1)
	sink := func(item bridge.SeqEnvelope) {
		if bridge.IsFinalRunEvent(item.Env.Type) {
			select {
			case terminal <- item:
			default:
			}
			return
		}
		select {
		case events <- item:
		default:
		}
	}

	subID, ok := h.runner.Hub().SubscribeSeq(runID, fromSeq, sink)
	if !ok {
		notFound(c)
		return
	}
	defer h.runner.Hub().Unsubscribe(runID, subID)

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	ctx := c.Request.Context()
	// A heartbeat keeps intermediaries from idling the connection out.
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	// id is the hub sequence number, enabling Last-Event-ID resume; event
	// carries the envelope type.
	write := func(w io.Writer, item bridge.SeqEnvelope) bool {
		data, _ := json.Marshal(item.Env)
		_, err := io.WriteString(w, "id: "+strconv.Itoa(item.Seq)+"\nevent: "+item.Env.Type+"\ndata: "+string(data)+"\n\n")
		return err == nil
	}

	c.Stream(func(w io.Writer) bool {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			_, err := io.WriteString(w, ": ping\n\n")
			return err == nil
		case item := <-events:
			return write(w, item)
		case item := <-terminal:
			// Flush any queued non-terminal events first (their seqs precede
			// the terminal's), then deliver the ending and close the stream.
			for {
				select {
				case queued := <-events:
					if !write(w, queued) {
						return false
					}
				default:
					_ = write(w, item)
					return false
				}
			}
		}
	})
}
