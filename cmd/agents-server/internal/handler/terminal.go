package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/gorilla/websocket"

	"github.com/zzir/agents-go/cmd/agents-server/internal/logging"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/sandbox"
)

const (
	// terminalReadChunk caps one binary frame of terminal output. Reads block
	// on the PTY, so this also bounds per-terminal buffer memory.
	terminalReadChunk = 32 << 10
)

// TerminalHandler serves /ws/terminal: one interactive sandbox terminal per
// WebSocket connection. The client opens with a terminal.open envelope, then
// binary frames carry the byte stream both ways while text envelopes carry
// control (resize, exit). Only a persistent container can host one
// (sandbox.TerminalOpener). Live terminals are tracked per project so a
// configuration change can tear them down.
type TerminalHandler struct {
	// Audit, when set, records every terminal opened: a shell on a sandbox
	// host is the act most worth a line. Wired at bootstrap.
	Audit     protocol.AuditFunc
	sandboxes *store.SandboxStore
	projects  *store.ProjectStore
	manager   sandboxProvider
	settings  *settings.Reader

	mu   sync.Mutex
	live map[string]map[*liveTerminal]struct{} // project id → open terminals
	// fence maps a project id to the lowest runtime generation still allowed
	// to register: a terminal dials BEFORE registering, so a change in between is refused there.
	fence map[string]int64
}

// sandboxProvider is the slice of sandboxes.Manager the terminal handler
// depends on; an interface so tests can inject a fake backend.
type sandboxProvider interface {
	// Acquire takes a reference on the instance for the terminal's lifetime,
	// so an instance evicted meanwhile closes only after the terminal.
	Acquire(spec sandboxes.Spec) (sandbox.Sandbox, func(), error)
}

var _ sandboxProvider = (*sandboxes.Manager)(nil)

// liveTerminal pairs a Terminal with its connection so a teardown can stop
// both pumps; gen is the config generation it opened under.
type liveTerminal struct {
	term      sandbox.Terminal
	conn      *server.WSConn
	gen       int64
	projectID string
}

// NewTerminalHandler returns a handler backed by the given stores and sandbox manager.
func NewTerminalHandler(sbs *store.SandboxStore, projects *store.ProjectStore, m sandboxProvider, cfg *settings.Reader) *TerminalHandler {
	return &TerminalHandler{
		sandboxes: sbs, projects: projects, manager: m, settings: cfg,
		live: map[string]map[*liveTerminal]struct{}{}, fence: map[string]int64{},
	}
}

// Handle runs one terminal session on an authenticated WebSocket connection.
func (h *TerminalHandler) Handle(conn *server.WSConn) {
	log := logging.Ctx(conn.Context())
	if !conn.Recheck() {
		return
	}

	term, proj, release, err := h.open(conn)
	if err == nil && h.Audit != nil {
		// Detail names the owner: an admin may open a shell into a member's
		// tree (decisions §5.28), and the log must say whose.
		h.Audit(context.WithoutCancel(conn.Context()), protocol.AuditRecord{
			Actor: conn.User, Action: "terminal.open", Resource: proj.ID,
			Detail: "project " + proj.Name + " (owner " + proj.OwnerID + ")",
		})
	}
	if err != nil {
		// Client-caused failures (bad first frame, unknown sandbox, unsupported
		// backend) are reported on the wire and logged at debug only.
		log.Debug("terminal open failed", "error", err)
		_ = conn.WriteJSON(&protocol.Envelope{Type: protocol.EventTerminalError, Payload: mustJSON(protocol.TerminalError{
			Message: err.Error(),
		})})
		return
	}
	// The instance reference lives exactly as long as this connection.
	defer release()
	lt := &liveTerminal{term: term, conn: conn, gen: proj.RuntimeGen, projectID: proj.ID}
	limit := h.settings.Int(conn.Context(), settings.KeyMaxTerminalsPerSandbox)
	if ok, stale := h.register(proj.ID, lt, limit); !ok {
		_ = term.Close()
		msg := fmt.Sprintf("too many open terminals for this project (max %d)", limit)
		if stale {
			// The project changed (or was deleted) while this terminal was
			// dialing; reconnect to open under the current one.
			msg = "this project changed while the terminal was opening; reconnect"
		}
		_ = conn.WriteJSON(&protocol.Envelope{Type: protocol.EventTerminalError, Payload: mustJSON(protocol.TerminalError{
			Message: msg,
		})})
		return
	}
	defer h.unregister(proj.ID, lt)
	defer term.Close()

	if err := conn.WriteJSON(&protocol.Envelope{Type: protocol.EventTerminalReady}); err != nil {
		return
	}
	log.Debug("terminal opened", "project_id", proj.ID)

	// Output pump: PTY → binary frames. It owns the exit notification, and
	// closing the connection unblocks the input pump below.
	go func() {
		buf := make([]byte, terminalReadChunk)
		for {
			n, rerr := term.Read(buf)
			if n > 0 {
				if werr := conn.WriteBinary(buf[:n]); werr != nil {
					_ = term.Close()
					return
				}
			}
			if rerr != nil {
				code, _ := term.Wait()
				_ = conn.WriteJSON(&protocol.Envelope{Type: protocol.EventTerminalExit, Payload: mustJSON(protocol.TerminalExit{
					Code: code,
				})})
				conn.Close()
				return
			}
		}
	}()

	// Input pump (this goroutine): frames → PTY. Returning tears the terminal
	// down via the deferred Close, which in turn EOFs the output pump.
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			if !server.IsNormalClose(err) {
				log.Debug("terminal ws read error", "error", err)
			}
			return
		}
		switch mt {
		case websocket.BinaryMessage:
			if _, err := term.Write(data); err != nil {
				return
			}
		case websocket.TextMessage:
			var env protocol.Envelope
			if err := json.Unmarshal(data, &env); err != nil {
				log.Debug("terminal control frame not JSON", "error", err)
				continue
			}
			if env.Type != protocol.EventTerminalResize {
				log.Warn("unknown terminal control type", "type", env.Type)
				continue
			}
			var msg protocol.TerminalResize
			if err := json.Unmarshal(env.Payload, &msg); err != nil {
				continue
			}
			if msg.Cols > 0 && msg.Rows > 0 {
				_ = term.Resize(msg.Cols, msg.Rows)
			}
		}
	}
}

// open reads the terminal.open handshake and builds the Terminal, returning
// the project whose tree the shell reaches. The caller owns release once err is nil.
func (h *TerminalHandler) open(conn *server.WSConn) (sandbox.Terminal, *store.Project, func(), error) {
	var env protocol.Envelope
	if err := conn.ReadJSON(&env); err != nil {
		return nil, nil, nil, fmt.Errorf("read terminal.open: %w", err)
	}
	if env.Type != protocol.EventTerminalOpen {
		return nil, nil, nil, fmt.Errorf("expected %s as first message, got %q", protocol.EventTerminalOpen, env.Type)
	}
	var msg protocol.TerminalOpen
	if err := json.Unmarshal(env.Payload, &msg); err != nil {
		return nil, nil, nil, fmt.Errorf("invalid terminal.open payload: %w", err)
	}
	if msg.ProjectID == "" {
		return nil, nil, nil, errors.New("terminal.open requires project_id")
	}

	ctx := conn.Context()
	proj, err := h.projects.Get(ctx, msg.ProjectID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("project %s: %w", msg.ProjectID, err)
	}
	// A member opens a shell into their OWN project; an admin into any
	// (decisions §5.28). A foreign project reads as absent.
	if conn.User.Role != store.RoleAdmin && proj.OwnerID != conn.User.ID {
		return nil, nil, nil, fmt.Errorf("project %s: %w", msg.ProjectID, store.ErrNotFound)
	}
	spec, err := resolveSpec(ctx, h.sandboxes, proj)
	if err != nil {
		return nil, nil, nil, err
	}
	// No read happens until the shell is up (an ssh dial, an image pull), so
	// the heartbeat's deadline is lifted for the duration.
	conn.PauseHeartbeat()
	defer conn.ResumeHeartbeat()
	sb, release, err := h.manager.Acquire(spec)
	if err != nil {
		return nil, nil, nil, err
	}
	opener, ok := sb.(sandbox.TerminalOpener)
	if !ok {
		release()
		return nil, nil, nil, fmt.Errorf("%s sandbox: %w", spec.Sandbox.Type, sandbox.ErrTerminalUnsupported)
	}
	term, err := opener.OpenTerminal(ctx, sandbox.TerminalOptions{Cols: msg.Cols, Rows: msg.Rows})
	if err != nil {
		release()
		return nil, nil, nil, err
	}
	return term, proj, release, nil
}

// register adds a live terminal, enforcing the per-project cap and the
// generation fence under one lock. full is temporary, stale is final.
func (h *TerminalHandler) register(projectID string, lt *liveTerminal, limit int) (ok, stale bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if lt.gen < h.fence[projectID] {
		return false, true
	}
	set := h.live[projectID]
	if len(set) >= limit {
		return false, false
	}
	if set == nil {
		set = map[*liveTerminal]struct{}{}
		h.live[projectID] = set
	}
	set[lt] = struct{}{}
	return true, false
}

func (h *TerminalHandler) unregister(projectID string, lt *liveTerminal) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.live[projectID]
	delete(set, lt)
	if len(set) == 0 {
		delete(h.live, projectID)
	}
}

// CloseProjectTerminals severs the terminals a project opened before minGen
// and fences that generation off, so a terminal still dialing is refused at
// register. Every configuration change arrives as a project generation
// (decisions §5.33); a delete passes maxTerminalGen.
func (h *TerminalHandler) CloseProjectTerminals(projectID string, minGen int64) {
	h.mu.Lock()
	if h.fence[projectID] < minGen {
		h.fence[projectID] = minGen
	}
	terminals := make([]*liveTerminal, 0, len(h.live[projectID]))
	for lt := range h.live[projectID] {
		if lt.gen < minGen {
			terminals = append(terminals, lt)
		}
	}
	h.mu.Unlock()
	// Close outside the lock: pump teardown re-enters unregister.
	for _, lt := range terminals {
		_ = lt.term.Close()
		lt.conn.Close()
	}
}

// maxTerminalGen fences a project id permanently — the delete's minGen.
const maxTerminalGen = int64(1) << 62

// resolveSpec loads the target and template a project names, so a caller
// holding only the project row can build or acquire its sandbox.
func resolveSpec(ctx context.Context, sbs *store.SandboxStore, proj *store.Project) (sandboxes.Spec, error) {
	sb, err := sbs.Get(ctx, proj.SandboxID)
	if err != nil {
		return sandboxes.Spec{}, fmt.Errorf("sandbox %s: %w", proj.SandboxID, err)
	}
	return sandboxes.Spec{Sandbox: sb, Project: proj}, nil
}
