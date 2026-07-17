package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/sandbox"
)

// fakeTerminal echoes written bytes back as output; writing "exit\n" ends the
// shell with code 7.
type fakeTerminal struct {
	outR *io.PipeReader
	outW *io.PipeWriter

	mu         sync.Mutex
	cols, rows int
	exitCode   int
	closed     bool
}

func newFakeTerminal() *fakeTerminal {
	r, w := io.Pipe()
	return &fakeTerminal{outR: r, outW: w, exitCode: -1}
}

func (f *fakeTerminal) Read(p []byte) (int, error) { return f.outR.Read(p) }

func (f *fakeTerminal) Write(p []byte) (int, error) {
	if string(p) == "exit\n" {
		f.mu.Lock()
		f.exitCode = 7
		f.mu.Unlock()
		_ = f.outW.Close()
		return len(p), nil
	}
	return f.outW.Write(p)
}

func (f *fakeTerminal) Resize(cols, rows int) error {
	f.mu.Lock()
	f.cols, f.rows = cols, rows
	f.mu.Unlock()
	return nil
}

func (f *fakeTerminal) size() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cols, f.rows
}

func (f *fakeTerminal) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	_ = f.outW.Close()
	return nil
}

func (f *fakeTerminal) Wait() (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exitCode, nil
}

// fakeTerminalSandbox implements just enough of sandbox.Sandbox for the
// terminal path (only OpenTerminal is ever called; the embedded nil interface
// panics loudly if anything else is).
type fakeTerminalSandbox struct {
	sandbox.Sandbox
	term    *fakeTerminal
	openErr error
}

func (f *fakeTerminalSandbox) OpenTerminal(_ context.Context, opts sandbox.TerminalOptions) (sandbox.Terminal, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	_ = f.term.Resize(opts.EffectiveCols(), opts.EffectiveRows())
	return f.term, nil
}

type fakeProvider struct {
	sb  sandbox.Sandbox
	err error
}

func (p *fakeProvider) GetOrCreate(*store.SandboxConfig) (sandbox.Sandbox, error) {
	return p.sb, p.err
}

// terminalTestServer stands up /ws/terminal with a fake sandbox backend and a
// stored ssh sandbox config, returning the pieces tests need.
func terminalTestServer(t *testing.T, provider sandboxProvider) (*httptest.Server, *TerminalHandler, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	sandboxes := store.NewSandboxStore(db)
	cfg := &store.SandboxConfig{Name: "box", Type: "ssh", Config: json.RawMessage(`{"addr":"h","user":"u"}`)}
	if err := sandboxes.Create(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	th := NewTerminalHandler(sandboxes, provider)
	engine := gin.New()
	engine.GET("/ws/terminal", server.HandleWSWithAuth(th.Handle, testWSToken))
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)
	return srv, th, cfg.ID
}

// dialTerminal connects, authenticates and completes the terminal.open
// handshake, returning the connection after terminal.ready.
func dialTerminal(t *testing.T, srv *httptest.Server, sandboxID string) *websocket.Conn {
	t.Helper()
	conn := dialTerminalRaw(t, srv)
	openTerminal(t, conn, sandboxID)
	env := readTerminalEnvelope(t, conn)
	if env.Type != protocol.EventTerminalReady {
		t.Fatalf("handshake reply = %q, want %s", env.Type, protocol.EventTerminalReady)
	}
	return conn
}

func dialTerminalRaw(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/terminal"
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	defer resp.Body.Close()
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.WriteJSON(map[string]string{"type": protocol.EventAuth, "token": testWSToken}); err != nil {
		t.Fatalf("send auth: %v", err)
	}
	var ack protocol.Envelope
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := conn.ReadJSON(&ack); err != nil || ack.Type != protocol.EventAuthOK {
		t.Fatalf("auth ack: type=%q err=%v", ack.Type, err)
	}
	return conn
}

func openTerminal(t *testing.T, conn *websocket.Conn, sandboxID string) {
	t.Helper()
	if err := conn.WriteJSON(&protocol.Envelope{Type: protocol.EventTerminalOpen, Payload: mustJSON(protocol.TerminalOpen{
		SandboxID: sandboxID, Cols: 100, Rows: 30,
	})}); err != nil {
		t.Fatalf("send terminal.open: %v", err)
	}
}

// readTerminalEnvelope reads frames until the next text (control) frame and
// decodes it, skipping binary output frames.
func readTerminalEnvelope(t *testing.T, conn *websocket.Conn) protocol.Envelope {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read control frame: %v", err)
		}
		if mt != websocket.TextMessage {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("decode control frame: %v", err)
		}
		return env
	}
}

// readBinaryUntil accumulates binary frames until want appears in the stream.
func readBinaryUntil(t *testing.T, conn *websocket.Conn, want string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var got strings.Builder
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("waiting for output %q (got %q): %v", want, got.String(), err)
		}
		if mt != websocket.BinaryMessage {
			continue
		}
		got.Write(data)
		if strings.Contains(got.String(), want) {
			return
		}
	}
}

// The full happy path: handshake, echoed IO on binary frames, resize control,
// and an exit notification once the shell terminates.
func TestTerminalWS_EchoResizeExit(t *testing.T) {
	term := newFakeTerminal()
	srv, _, id := terminalTestServer(t, &fakeProvider{sb: &fakeTerminalSandbox{term: term}})
	conn := dialTerminal(t, srv, id)

	// The open handshake carried 100x30.
	if cols, rows := term.size(); cols != 100 || rows != 30 {
		t.Errorf("initial size = %dx%d, want 100x30", cols, rows)
	}

	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("echo hi\n")); err != nil {
		t.Fatal(err)
	}
	readBinaryUntil(t, conn, "echo hi")

	if err := conn.WriteJSON(&protocol.Envelope{Type: protocol.EventTerminalResize, Payload: mustJSON(protocol.TerminalResize{
		Cols: 120, Rows: 40,
	})}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if cols, rows := term.size(); cols == 120 && rows == 40 {
			break
		}
		if time.Now().After(deadline) {
			cols, rows := term.size()
			t.Fatalf("resize not applied: %dx%d, want 120x40", cols, rows)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("exit\n")); err != nil {
		t.Fatal(err)
	}
	env := readTerminalEnvelope(t, conn)
	if env.Type != protocol.EventTerminalExit {
		t.Fatalf("after exit: type = %q, want %s", env.Type, protocol.EventTerminalExit)
	}
	var exit protocol.TerminalExit
	if err := json.Unmarshal(env.Payload, &exit); err != nil {
		t.Fatal(err)
	}
	if exit.Code != 7 {
		t.Errorf("exit code = %d, want 7", exit.Code)
	}
}

func TestTerminalWS_UnknownSandboxRejected(t *testing.T) {
	srv, _, _ := terminalTestServer(t, &fakeProvider{sb: &fakeTerminalSandbox{term: newFakeTerminal()}})
	conn := dialTerminalRaw(t, srv)
	openTerminal(t, conn, "no-such-id")
	env := readTerminalEnvelope(t, conn)
	if env.Type != protocol.EventTerminalError {
		t.Fatalf("type = %q, want %s", env.Type, protocol.EventTerminalError)
	}
}

func TestTerminalWS_LocalSandboxRefused(t *testing.T) {
	term := newFakeTerminal()
	srv, th, _ := terminalTestServer(t, &fakeProvider{sb: &fakeTerminalSandbox{term: term}})
	cfg := &store.SandboxConfig{Name: "host", Type: "local"}
	if err := th.store.Create(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	conn := dialTerminalRaw(t, srv)
	openTerminal(t, conn, cfg.ID)
	env := readTerminalEnvelope(t, conn)
	if env.Type != protocol.EventTerminalError {
		t.Fatalf("type = %q, want %s", env.Type, protocol.EventTerminalError)
	}
	var msg protocol.TerminalError
	if err := json.Unmarshal(env.Payload, &msg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg.Message, "local") {
		t.Errorf("error message = %q, want a local-sandbox refusal", msg.Message)
	}
}

func TestTerminalWS_OpenErrorReported(t *testing.T) {
	srv, _, id := terminalTestServer(t, &fakeProvider{sb: &fakeTerminalSandbox{
		term:    newFakeTerminal(),
		openErr: errors.New("attach failed"),
	}})
	conn := dialTerminalRaw(t, srv)
	openTerminal(t, conn, id)
	env := readTerminalEnvelope(t, conn)
	if env.Type != protocol.EventTerminalError {
		t.Fatalf("type = %q, want %s", env.Type, protocol.EventTerminalError)
	}
}

// Deleting/updating a sandbox must tear down its live terminals: the client
// observes the shell dying (exit envelope and/or socket close).
func TestTerminalWS_CloseSandboxTerminals(t *testing.T) {
	term := newFakeTerminal()
	srv, th, id := terminalTestServer(t, &fakeProvider{sb: &fakeTerminalSandbox{term: term}})
	conn := dialTerminal(t, srv, id)

	th.CloseSandboxTerminals(id)

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			break // connection torn down — expected end state
		}
		if mt == websocket.TextMessage {
			var env protocol.Envelope
			if jerr := json.Unmarshal(data, &env); jerr == nil && env.Type == protocol.EventTerminalExit {
				continue // exit notification may precede the close
			}
		}
	}
	term.mu.Lock()
	closed := term.closed
	term.mu.Unlock()
	if !closed {
		t.Error("terminal not closed after CloseSandboxTerminals")
	}
}
