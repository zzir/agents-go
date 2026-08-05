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
//	@Summary		Start run
//	@Description	Starts an agent run on the session. Default returns 201 with a run id; pass wait=true to block until the run ends and receive the final output — or status "interrupted" when it pauses for tool approval (act via /sessions/{id}/approvals). Fails 409 if the session already has an active run.
//	@Tags			runs
//	@Accept			json
//	@Produce		json
//	@Param			id		path		string			true	"Session ID"
//	@Param			wait	query		bool			false	"Block until the run finishes"
//	@Param			run		body		createRunReq	true	"Run input"
//	@Success		201		{object}	createRunResp
//	@Success		200		{object}	map[string]interface{}	"Final result (wait=true)"
//	@Failure		400		{object}	ErrorResponse
//	@Failure		404		{object}	ErrorResponse
//	@Failure		409		{object}	ErrorResponse	"session already has an active run"
//	@Security		BearerAuth
//	@Router			/sessions/{id}/runs [post]
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
	// StartRun's onDone delivers the typed outcome directly — no need to
	// subscribe to our own broadcast and decode the envelopes this process
	// just marshaled. Buffered so the callback never blocks if the client
	// hangs up first.
	done := make(chan *bridge.RunOutcome, 1)
	runID, err := h.runner.StartRun(sessionID, req.AgentConfigID, req.SandboxID, req.Input, func(res *bridge.RunOutcome) {
		done <- res
	})
	if err != nil {
		h.startError(c, err)
		return
	}

	select {
	case <-c.Request.Context().Done():
		// Client hung up; the run keeps executing in the hub.
		return
	case res := <-done:
		switch {
		case res.Interrupted:
			// The run paused for tool approval: waiting any longer would
			// hang until a human acts. Report the state; the caller lists
			// GET /sessions/{id}/approvals and decides, which resumes the
			// run under the same run id.
			c.JSON(http.StatusOK, gin.H{"run_id": runID, "session_id": sessionID, "status": string(bridge.RunInterrupted)})
		case res.Cancelled:
			abortError(c, http.StatusBadGateway, protocol.CodeUpstream, "run cancelled")
		case res.ErrCode != "":
			abortError(c, http.StatusBadGateway, protocol.CodeUpstream, res.ErrMessage)
		default:
			c.JSON(http.StatusOK, gin.H{"run_id": runID, "session_id": sessionID, "status": string(bridge.RunCompleted), "final_output": res.FinalText})
		}
	}
}

func (h *RunHandler) startError(c *gin.Context, err error) {
	if busy, ok := errors.AsType[bridge.ErrSessionBusy](err); ok {
		conflict(c, "session already has an active run: "+busy.RunID)
		return
	}
	// The session exists but is already at its live-task cap: a state conflict
	// (409), not a missing resource.
	if limit, ok := errors.AsType[bridge.ErrTaskLimit](err); ok {
		conflict(c, limit.Error())
		return
	}
	// The session's delete cascade is in progress: a state conflict (409), not a
	// missing session.
	if deleting, ok := errors.AsType[bridge.ErrSessionDeleting](err); ok {
		conflict(c, deleting.Error())
		return
	}
	// The server is draining: 503, not 500. The request was fine; the answer
	// is to retry against the process that comes back.
	if down, ok := errors.AsType[bridge.ErrShuttingDown](err); ok {
		unavailable(c, down.Error())
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
//	@Tags		runs
//	@Produce	json
//	@Param		id	path		string	true	"Run ID"
//	@Success	200	{object}	bridge.RunInfo
//	@Failure	404	{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/runs/{id} [get]
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
//	@Tags		runs
//	@Param		id		path	string	true	"Run ID"
//	@Param		mode	query	string	false	"graceful = stop after the current turn; default aborts immediately"
//	@Success	204		"cancelling"
//	@Failure	404		{object}	ErrorResponse
//	@Security	BearerAuth
//	@Router		/runs/{id}/cancel [post]
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
// resumes without loss. The stream ends after a FINAL event; run.interrupted
// only pauses the run, and the same-id resume continues on the open stream.
//
//	@Summary		Stream run events (SSE)
//	@Description	Server-Sent Events stream. Each event id is the hub sequence number; reconnect with the Last-Event-ID header or from_seq to resume. The stream closes after a final event: run.output, run.error or run.cancelled. run.interrupted (paused for approval) does not close a live stream — deciding via /approvals resumes the SAME run id and its events continue on the open connection; a disconnected client reconnects with Last-Event-ID.
//	@Tags			runs
//	@Produce		text/event-stream
//	@Param			id			path		string	true	"Run ID"
//	@Param			from_seq	query		int		false	"Resume after this sequence number"
//	@Success		200			{string}	string	"SSE stream"
//	@Failure		404			{object}	ErrorResponse
//	@Security		BearerAuth
//	@Router			/runs/{id}/events [get]
func (h *RunHandler) Events(c *gin.Context) {
	runID := c.Param("id")
	fromSeq := 0
	if lastID := c.GetHeader("Last-Event-ID"); lastID != "" {
		fromSeq, _ = strconv.Atoi(lastID)
	} else if q := c.Query("from_seq"); q != "" {
		fromSeq, _ = strconv.Atoi(q)
	}

	ctx := c.Request.Context()

	// This buffer hands events from the subscriber goroutine to the HTTP
	// writer. It does NOT drop: the sink blocks, which pushes back into the
	// hub's per-subscriber buffer, and THAT is where a slow client is dropped —
	// with a run.gap telling it what it missed. A second silent drop here would
	// lose events the hub believes it delivered.
	//
	// Blocking is only safe because the sink runs on its own goroutine; it used
	// to run on the publishing goroutine, where it had to be non-blocking.
	//
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
		case <-ctx.Done():
		}
	}

	cancel, ok := h.runner.Hub().SubscribeSeq(runID, fromSeq, sink)
	if !ok {
		notFound(c)
		return
	}
	defer cancel()

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

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
