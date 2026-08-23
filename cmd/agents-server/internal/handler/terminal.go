package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/gorilla/websocket"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
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
// carry control (resize, exit). Local sandboxes are refused by design: a web
// terminal on the host process is a bigger grant than --allow-local-sandbox
// implies.
//
// It also tracks live terminals per sandbox config so config updates and
// deletes can tear them down (an SSH/docker rebuild would otherwise leave
// orphaned sessions running against the old config).
type TerminalHandler struct {
	// Audit, when set, records every terminal opened: a shell on a sandbox
	// host is the act most worth a line. Wired at bootstrap.
	Audit    protocol.AuditFunc
	store    *store.SandboxStore
	manager  sandboxProvider
	settings *settings.Reader

	mu   sync.Mutex
	live map[string]map[*liveTerminal]struct{} // sandbox config id → open terminals
	// fence maps a config id to the lowest runtime generation still allowed
	// to register. A terminal reads its config, dials and opens a PTY BEFORE
	// registering, so a config update (or delete) can complete inside that
	// window — sweeping the registry misses it, and it would surface
	// afterwards as a live shell on retired credentials (or on a deleted
	// sandbox). CloseSandboxTerminals moves the fence; register checks it
	// under the same lock, so the late arrival is refused instead.
	fence map[string]int64
}

// sandboxProvider is the slice of sandboxes.Manager the terminal handler
// depends on; an interface so tests can inject a fake backend.
type sandboxProvider interface {
	// Acquire takes a reference on the instance for the terminal's lifetime;
	// the returned release drops it when the connection ends, so an instance
	// evicted meanwhile (config update, last bound session deleted) stays
	// alive under the open terminal and closes only after it.
	Acquire(cfg *store.SandboxConfig, workDir string) (sandbox.Sandbox, func(), error)
}

var _ sandboxProvider = (*sandboxes.Manager)(nil)

// liveTerminal pairs a Terminal with its connection so a registry teardown
// can stop both pumps; gen records the config generation it opened under, so
// a teardown scoped to older generations leaves newer terminals running.
type liveTerminal struct {
	term sandbox.Terminal
	conn *server.WSConn
	gen  int64
}

// NewTerminalHandler returns a handler backed by the given store and sandbox manager.
func NewTerminalHandler(s *store.SandboxStore, m sandboxProvider, cfg *settings.Reader) *TerminalHandler {
	return &TerminalHandler{store: s, manager: m, settings: cfg, live: map[string]map[*liveTerminal]struct{}{}, fence: map[string]int64{}}
}

// Handle runs one terminal session on an authenticated WebSocket connection.
func (h *TerminalHandler) Handle(conn *server.WSConn) {
	log := logging.Ctx(conn.Context())
	// A terminal is a shell on a sandbox host with the server's stored
	// credentials — admin-only, like every other write to what runs where.
	if !conn.Recheck() {
		return
	}
	if conn.User.Role != store.RoleAdmin {
		_ = conn.WriteJSON(&protocol.Envelope{Type: protocol.EventTerminalError, Payload: mustJSON(protocol.TerminalError{
			Message: "the terminal requires the admin role",
		})})
		return
	}

	term, opened, release, err := h.open(conn)
	if err == nil && h.Audit != nil {
		h.Audit(context.WithoutCancel(conn.Context()), protocol.AuditRecord{
			Actor: conn.User, Action: "terminal.open", Resource: opened.ID,
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
	sandboxID := opened.ID
	// The instance reference lives exactly as long as this connection: a
	// forced teardown (CloseSandboxTerminals) closes the conn, the pumps
	// return, and this defer drops the hold.
	defer release()
	lt := &liveTerminal{term: term, conn: conn, gen: opened.RuntimeGen}
	limit := h.settings.Int(conn.Context(), settings.KeyMaxTerminalsPerSandbox)
	if ok, stale := h.register(sandboxID, lt, limit); !ok {
		_ = term.Close()
		msg := fmt.Sprintf("too many open terminals for this sandbox (max %d)", limit)
		if stale {
			// The config was updated or deleted while this terminal was
			// dialing: its shell would serve retired credentials (or a
			// sandbox that is gone). Reconnect to open under the current one.
			msg = "this sandbox changed while the terminal was opening; reconnect"
		}
		_ = conn.WriteJSON(&protocol.Envelope{Type: protocol.EventTerminalError, Payload: mustJSON(protocol.TerminalError{
			Message: msg,
		})})
		return
	}
	defer h.unregister(sandboxID, lt)
	defer term.Close()

	if err := conn.WriteJSON(&protocol.Envelope{Type: protocol.EventTerminalReady}); err != nil {
		return
	}
	log.Debug("terminal opened", "sandbox_id", sandboxID)

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
// returning the config it opened under (its id and runtime generation gate
// registration). The returned release drops the instance reference open
// acquired; the caller owns it once err is nil.
func (h *TerminalHandler) open(conn *server.WSConn) (sandbox.Terminal, *store.SandboxConfig, func(), error) {
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
	if msg.SandboxID == "" {
		return nil, nil, nil, errors.New("terminal.open requires sandbox_id")
	}

	ctx := conn.Context()
	cfg, err := h.store.Get(ctx, msg.SandboxID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("sandbox %s: %w", msg.SandboxID, err)
	}
	if cfg.Type == "local" {
		return nil, nil, nil, errors.New("local sandboxes do not support web terminals")
	}
	// A NON-empty work_dir passes the same validation a binding does, and the
	// canonical form is what keys the instance — a value the backend would
	// silently rewrite (a docker path outside /workspace landing in the
	// default) must be refused, not displayed as one directory while the
	// shell runs in another. An empty work_dir stays valid here even where a
	// binding would refuse it (ssh with no configured default): a terminal is
	// an interactive shell with no session-files promise, and empty honestly
	// means the sandbox's own default.
	workDir := msg.WorkDir
	if workDir != "" {
		workDir, err = bridge.ResolveBindingWorkDir(cfg, workDir)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	// From here no read happens until the shell is up — an ssh dial, a
	// first-time image pull — so the heartbeat's deadline is lifted for the
	// duration; otherwise a slow open ends at the first read with a deadline
	// no pong could have extended.
	conn.PauseHeartbeat()
	defer conn.ResumeHeartbeat()
	sb, release, err := h.manager.Acquire(cfg, workDir)
	if err != nil {
		return nil, nil, nil, err
	}
	opener, ok := sb.(sandbox.TerminalOpener)
	if !ok {
		release()
		return nil, nil, nil, fmt.Errorf("%s sandbox: %w", cfg.Type, sandbox.ErrTerminalUnsupported)
	}
	term, err := opener.OpenTerminal(ctx, sandbox.TerminalOptions{Cols: msg.Cols, Rows: msg.Rows})
	if err != nil {
		release()
		return nil, nil, nil, err
	}
	return term, cfg, release, nil
}

// register adds a live terminal, enforcing the per-sandbox cap and the
// generation fence (see the fence field) — checked under the same lock the
// fence moves under. The two refusals are distinct answers: full is
// temporary, stale is final.
func (h *TerminalHandler) register(sandboxID string, lt *liveTerminal, limit int) (ok, stale bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if lt.gen < h.fence[sandboxID] {
		return false, true
	}
	set := h.live[sandboxID]
	if len(set) >= limit {
		return false, false
	}
	if set == nil {
		set = map[*liveTerminal]struct{}{}
		h.live[sandboxID] = set
	}
	set[lt] = struct{}{}
	return true, false
}

func (h *TerminalHandler) unregister(sandboxID string, lt *liveTerminal) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.live[sandboxID]
	delete(set, lt)
	if len(set) == 0 {
		delete(h.live, sandboxID)
	}
}

// CloseSandboxTerminals tears down every live terminal for a sandbox config
// that opened under a generation below minGen, and moves the registration
// fence there so a terminal still dialing is refused at register (see the
// fence field). A config update passes the new runtime generation; a delete
// passes math.MaxInt64: nothing may serve a config that is gone. Sandbox
// Update/Delete call it alongside SandboxManager.Retire/Remove.
func (h *TerminalHandler) CloseSandboxTerminals(sandboxID string, minGen int64) {
	h.mu.Lock()
	if h.fence[sandboxID] < minGen {
		h.fence[sandboxID] = minGen
	}
	terminals := make([]*liveTerminal, 0, len(h.live[sandboxID]))
	for lt := range h.live[sandboxID] {
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
