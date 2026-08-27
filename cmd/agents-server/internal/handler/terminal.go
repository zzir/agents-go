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
// binary frames carry the raw byte stream both ways while text envelopes
// carry control (resize, exit). Only a persistent container can host one
// (sandboxes.TerminalCapable).
//
// It also tracks live terminals per project so a configuration change can
// tear them down (a rebuilt container would otherwise leave orphaned sessions
// running against the old one).
type TerminalHandler struct {
	// Audit, when set, records every terminal opened: a shell on a sandbox
	// host is the act most worth a line. Wired at bootstrap.
	Audit     protocol.AuditFunc
	targets   *store.SandboxTargetStore
	templates *store.SandboxTemplateStore
	projects  *store.ProjectStore
	manager   sandboxProvider
	settings  *settings.Reader

	mu   sync.Mutex
	live map[string]map[*liveTerminal]struct{} // project id → open terminals
	// fence maps a project id to the lowest runtime generation still allowed
	// to register. A terminal reads its rows, dials and opens a PTY BEFORE
	// registering, so a configuration change (or a delete) can complete inside
	// that window — sweeping the registry misses it, and it would surface
	// afterwards as a live shell on retired credentials, a stale environment
	// or a project that is gone. CloseProjectTerminals moves the fence;
	// register checks it under the same lock, so the late arrival is refused
	// instead.
	fence map[string]int64
}

// sandboxProvider is the slice of sandboxes.Manager the terminal handler
// depends on; an interface so tests can inject a fake backend.
type sandboxProvider interface {
	// Acquire takes a reference on the instance for the terminal's lifetime;
	// the returned release drops it when the connection ends, so an instance
	// evicted meanwhile (config update, last bound session deleted) stays
	// alive under the open terminal and closes only after it.
	Acquire(spec sandboxes.Spec) (sandbox.Sandbox, func(), error)
}

var _ sandboxProvider = (*sandboxes.Manager)(nil)

// liveTerminal pairs a Terminal with its connection so a registry teardown
// can stop both pumps; gen records the config generation it opened under, so
// a teardown scoped to older generations leaves newer terminals running.
type liveTerminal struct {
	term      sandbox.Terminal
	conn      *server.WSConn
	gen       int64
	projectID string
}

// NewTerminalHandler returns a handler backed by the given stores and sandbox manager.
func NewTerminalHandler(targets *store.SandboxTargetStore, templates *store.SandboxTemplateStore, projects *store.ProjectStore, m sandboxProvider, cfg *settings.Reader) *TerminalHandler {
	return &TerminalHandler{
		targets: targets, templates: templates, projects: projects, manager: m, settings: cfg,
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
		// tree (decisions §5.28), and the log must answer whose data was
		// reached.
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
	// The instance reference lives exactly as long as this connection: a
	// forced teardown (CloseProjectTerminals) closes the conn, the pumps
	// return, and this defer drops the hold.
	defer release()
	lt := &liveTerminal{term: term, conn: conn, gen: proj.RuntimeGen, projectID: proj.ID}
	limit := h.settings.Int(conn.Context(), settings.KeyMaxTerminalsPerSandbox)
	if ok, stale := h.register(proj.ID, lt, limit); !ok {
		_ = term.Close()
		msg := fmt.Sprintf("too many open terminals for this project (max %d)", limit)
		if stale {
			// The project (or its target or template) was updated — or the
			// project deleted — while this terminal was dialing: its shell
			// would serve retired credentials, a stale environment, or a
			// project that is gone. Reconnect to open under the current one.
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

	// Output pump: PTY → binary frames. It owns the exit notification — when
	// the shell exits, Read returns EOF, the code is resolved and reported,
	// and closing the connection unblocks the input pump below.
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

// open reads the terminal.open handshake frame and builds the Terminal,
// returning the project whose tree the shell reaches — its id and runtime
// generation gate registration, and the audit line names it. The returned
// release drops the instance reference open acquired; the caller owns it once
// err is nil.
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
	// A member opens a shell into their OWN project's container; an admin
	// into any (the operator's escape hatch, recorded in decisions §5.28). A
	// foreign project reads as absent.
	if conn.User.Role != store.RoleAdmin && proj.OwnerID != conn.User.ID {
		return nil, nil, nil, fmt.Errorf("project %s: %w", msg.ProjectID, store.ErrNotFound)
	}
	spec, err := resolveSpec(ctx, h.targets, h.templates, proj)
	if err != nil {
		return nil, nil, nil, err
	}
	// From here no read happens until the shell is up — an ssh dial, a
	// first-time image pull — so the heartbeat's deadline is lifted for the
	// duration; otherwise a slow open ends at the first read with a deadline
	// no pong could have extended.
	conn.PauseHeartbeat()
	defer conn.ResumeHeartbeat()
	sb, release, err := h.manager.Acquire(spec)
	if err != nil {
		return nil, nil, nil, err
	}
	opener, ok := sb.(sandbox.TerminalOpener)
	if !ok {
		release()
		return nil, nil, nil, fmt.Errorf("%s sandbox: %w", spec.Target.Type, sandbox.ErrTerminalUnsupported)
	}
	term, err := opener.OpenTerminal(ctx, sandbox.TerminalOptions{Cols: msg.Cols, Rows: msg.Rows})
	if err != nil {
		release()
		return nil, nil, nil, err
	}
	return term, proj, release, nil
}

// register adds a live terminal, enforcing the per-project cap and the
// generation fence (see the fence field) — checked under the same lock the
// fence moves under. The two refusals are distinct answers: full is
// temporary, stale is final.
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
// register (see the fence field). Every configuration change that reaches a
// container arrives here as a project generation — its own environment, its
// template, its target (decisions §5.33) — and a delete passes maxTerminalGen:
// nothing may serve a project that is gone. A shell that keeps reading the old
// configuration is a person debugging against what the agent no longer sees.
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
func resolveSpec(ctx context.Context, targets *store.SandboxTargetStore, templates *store.SandboxTemplateStore, proj *store.Project) (sandboxes.Spec, error) {
	target, err := targets.Get(ctx, proj.TargetID)
	if err != nil {
		return sandboxes.Spec{}, fmt.Errorf("sandbox target %s: %w", proj.TargetID, err)
	}
	tpl, err := templates.Get(ctx, proj.TemplateID)
	if err != nil {
		return sandboxes.Spec{}, fmt.Errorf("sandbox template %s: %w", proj.TemplateID, err)
	}
	return sandboxes.Spec{Target: target, Template: tpl, Project: proj}, nil
}
