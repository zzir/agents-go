package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"

	"github.com/zzir/agents-go/cmd/agents-server/internal/bridge"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/sandbox"
)

const (
	// terminalReadChunk caps one binary frame of terminal output. Reads block
	// on the PTY, so this also bounds per-terminal buffer memory.
	terminalReadChunk = 32 << 10
	// maxTerminalsPerSandbox bounds concurrent terminals per sandbox config —
	// a fat-finger guard, not a scheduler.
	maxTerminalsPerSandbox = 4
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
	store   *store.SandboxStore
	manager sandboxProvider

	mu   sync.Mutex
	live map[string]map[*liveTerminal]struct{} // sandbox config id → open terminals
}

// sandboxProvider is the slice of bridge.SandboxManager the terminal handler
// depends on; an interface so tests can inject a fake backend.
type sandboxProvider interface {
	GetOrCreate(cfg *store.SandboxConfig) (sandbox.Sandbox, error)
}

var _ sandboxProvider = (*bridge.SandboxManager)(nil)

// liveTerminal pairs a Terminal with its connection so a registry teardown
// can stop both pumps.
type liveTerminal struct {
	term sandbox.Terminal
	conn *server.WSConn
}

// NewTerminalHandler returns a handler backed by the given store and sandbox manager.
func NewTerminalHandler(s *store.SandboxStore, m sandboxProvider) *TerminalHandler {
	return &TerminalHandler{store: s, manager: m, live: map[string]map[*liveTerminal]struct{}{}}
}

// Handle runs one terminal session on an authenticated WebSocket connection.
func (h *TerminalHandler) Handle(conn *server.WSConn) {
	log := zerolog.Ctx(conn.Context())

	term, sandboxID, err := h.open(conn)
	if err != nil {
		// Client-caused failures (bad first frame, unknown sandbox, unsupported
		// backend) are reported on the wire and logged at debug only.
		log.Debug().Err(err).Msg("terminal open failed")
		_ = conn.WriteJSON(&protocol.Envelope{Type: protocol.EventTerminalError, Payload: mustJSON(protocol.TerminalError{
			Message: err.Error(),
		})})
		return
	}
	lt := &liveTerminal{term: term, conn: conn}
	if !h.register(sandboxID, lt) {
		_ = term.Close()
		_ = conn.WriteJSON(&protocol.Envelope{Type: protocol.EventTerminalError, Payload: mustJSON(protocol.TerminalError{
			Message: fmt.Sprintf("too many open terminals for this sandbox (max %d)", maxTerminalsPerSandbox),
		})})
		return
	}
	defer h.unregister(sandboxID, lt)
	defer term.Close()

	if err := conn.WriteJSON(&protocol.Envelope{Type: protocol.EventTerminalReady}); err != nil {
		return
	}
	log.Debug().Str("sandbox_id", sandboxID).Msg("terminal opened")

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
				log.Debug().Err(err).Msg("terminal ws read error")
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
				log.Debug().Err(err).Msg("terminal control frame not JSON")
				continue
			}
			if env.Type != protocol.EventTerminalResize {
				log.Warn().Str("type", env.Type).Msg("unknown terminal control type")
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

// open reads the terminal.open handshake frame and builds the Terminal.
func (h *TerminalHandler) open(conn *server.WSConn) (sandbox.Terminal, string, error) {
	var env protocol.Envelope
	if err := conn.ReadJSON(&env); err != nil {
		return nil, "", fmt.Errorf("read terminal.open: %w", err)
	}
	if env.Type != protocol.EventTerminalOpen {
		return nil, "", fmt.Errorf("expected %s as first message, got %q", protocol.EventTerminalOpen, env.Type)
	}
	var msg protocol.TerminalOpen
	if err := json.Unmarshal(env.Payload, &msg); err != nil {
		return nil, "", fmt.Errorf("invalid terminal.open payload: %w", err)
	}
	if msg.SandboxID == "" {
		return nil, "", errors.New("terminal.open requires sandbox_id")
	}

	ctx := conn.Context()
	cfg, err := h.store.Get(ctx, msg.SandboxID)
	if err != nil {
		return nil, "", fmt.Errorf("sandbox %s: %w", msg.SandboxID, err)
	}
	if cfg.Type == "local" {
		return nil, "", errors.New("local sandboxes do not support web terminals")
	}
	sb, err := h.manager.GetOrCreate(cfg)
	if err != nil {
		return nil, "", err
	}
	opener, ok := sb.(sandbox.TerminalOpener)
	if !ok {
		return nil, "", fmt.Errorf("%s sandbox: %w", cfg.Type, sandbox.ErrTerminalUnsupported)
	}
	term, err := opener.OpenTerminal(ctx, sandbox.TerminalOptions{Cols: msg.Cols, Rows: msg.Rows})
	if err != nil {
		return nil, "", err
	}
	return term, cfg.ID, nil
}

// register adds a live terminal, enforcing the per-sandbox cap.
func (h *TerminalHandler) register(sandboxID string, lt *liveTerminal) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.live[sandboxID]
	if len(set) >= maxTerminalsPerSandbox {
		return false
	}
	if set == nil {
		set = map[*liveTerminal]struct{}{}
		h.live[sandboxID] = set
	}
	set[lt] = struct{}{}
	return true
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

// CloseSandboxTerminals tears down every live terminal for a sandbox config.
// Sandbox Update/Delete call it alongside SandboxManager.Remove so no session
// keeps running against a stale or deleted config.
func (h *TerminalHandler) CloseSandboxTerminals(sandboxID string) {
	h.mu.Lock()
	terminals := make([]*liveTerminal, 0, len(h.live[sandboxID]))
	for lt := range h.live[sandboxID] {
		terminals = append(terminals, lt)
	}
	h.mu.Unlock()
	// Close outside the lock: pump teardown re-enters unregister.
	for _, lt := range terminals {
		_ = lt.term.Close()
		lt.conn.Close()
	}
}
