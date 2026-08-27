package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
	"github.com/zzir/agents-go/cmd/agents-server/internal/sandboxes"
	"github.com/zzir/agents-go/cmd/agents-server/internal/server"
	"github.com/zzir/agents-go/cmd/agents-server/internal/settings"
	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
	"github.com/zzir/agents-go/cmd/agents-server/internal/testdb"
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
	// releases counts how often the acquired reference was dropped — the
	// terminal must release exactly once when its connection ends.
	releases atomic.Int64
	// projects records what each Acquire was asked for.
	mu       sync.Mutex
	projects []string
}

func (p *fakeProvider) Acquire(spec sandboxes.Spec) (sandbox.Sandbox, func(), error) {
	if p.err != nil {
		return nil, nil, p.err
	}
	p.mu.Lock()
	p.projects = append(p.projects, spec.Project.ID)
	p.mu.Unlock()
	return p.sb, func() { p.releases.Add(1) }, nil
}

// terminalTestServer stands up /ws/terminal with a fake sandbox backend and a
// stored project, returning the pieces tests need.
func terminalTestServer(t *testing.T, provider sandboxProvider) (*httptest.Server, *TerminalHandler, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	projects := store.NewProjectStore(db)
	sandboxID := mkSandboxRow(t, db)
	proj := &store.Project{OwnerID: store.LocalUserID, SandboxID: sandboxID, Name: "p"}
	if err := projects.Create(t.Context(), proj); err != nil {
		t.Fatal(err)
	}
	th := NewTerminalHandler(store.NewSandboxStore(db), projects, provider, settings.NewReader(nil))
	engine := newTestEngine()
	engine.GET("/ws/terminal", server.HandleWSWithAuth(th.Handle, testAuthFunc(testWSToken), nil, nil))
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)
	return srv, th, proj.ID
}

// dialTerminal connects, authenticates and completes the terminal.open
// handshake, returning the connection after terminal.ready.
func dialTerminal(t *testing.T, srv *httptest.Server, projectID string) *websocket.Conn {
	t.Helper()
	conn := dialTerminalRaw(t, srv)
	openTerminal(t, conn, projectID)
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

func openTerminal(t *testing.T, conn *websocket.Conn, projectID string) {
	t.Helper()
	if err := conn.WriteJSON(&protocol.Envelope{Type: protocol.EventTerminalOpen, Payload: mustJSON(protocol.TerminalOpen{
		ProjectID: projectID, Cols: 100, Rows: 30,
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
	srv, _, pid := terminalTestServer(t, &fakeProvider{sb: &fakeTerminalSandbox{term: term}})
	conn := dialTerminal(t, srv, pid)

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

// A member opens a shell into their OWN project's container; a foreign
// project reads as absent and acquires nothing (decisions §5.28 — admins reach
// any project, which the other tests exercise via the local user).
func TestTerminalWS_ProjectOwnership(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	projects := store.NewProjectStore(db)
	sandboxID := mkSandboxRow(t, db)
	member := protocol.UserInfo{ID: store.NewID(), Email: "m@example.com", Role: store.RoleMember}
	own := &store.Project{OwnerID: member.ID, SandboxID: sandboxID, Name: "own"}
	foreign := &store.Project{OwnerID: store.NewID(), SandboxID: sandboxID, Name: "foreign"}
	for _, p := range []*store.Project{own, foreign} {
		if err := projects.Create(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	provider := &fakeProvider{sb: &fakeTerminalSandbox{term: newFakeTerminal()}}
	th := NewTerminalHandler(store.NewSandboxStore(db), projects, provider, settings.NewReader(nil))
	asMember := func(_ context.Context, bearer string) (protocol.UserInfo, error) {
		if bearer != testWSToken {
			return protocol.UserInfo{}, errors.New("unauthorized")
		}
		return member, nil
	}
	engine := newTestEngine()
	engine.GET("/ws/terminal", server.HandleWSWithAuth(th.Handle, asMember, nil, nil))
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)

	conn := dialTerminalRaw(t, srv)
	openTerminal(t, conn, own.ID)
	if env := readTerminalEnvelope(t, conn); env.Type != protocol.EventTerminalReady {
		t.Fatalf("member on their own project = %q, want %s", env.Type, protocol.EventTerminalReady)
	}
	acquired := len(provider.projects)

	conn2 := dialTerminalRaw(t, srv)
	openTerminal(t, conn2, foreign.ID)
	env := readTerminalEnvelope(t, conn2)
	var te protocol.TerminalError
	_ = json.Unmarshal(env.Payload, &te)
	if env.Type != protocol.EventTerminalError || !strings.Contains(te.Message, "not found") {
		t.Fatalf("member on a foreign project = %s %q, want a not-found refusal", env.Type, te.Message)
	}
	if len(provider.projects) != acquired {
		t.Fatal("a refused open acquired a sandbox")
	}
}

func TestTerminalWS_UnknownProjectRejected(t *testing.T) {
	srv, _, _ := terminalTestServer(t, &fakeProvider{sb: &fakeTerminalSandbox{term: newFakeTerminal()}})
	conn := dialTerminalRaw(t, srv)
	openTerminal(t, conn, store.NewID())
	env := readTerminalEnvelope(t, conn)
	if env.Type != protocol.EventTerminalError {
		t.Fatalf("type = %q, want %s", env.Type, protocol.EventTerminalError)
	}
}

func TestTerminalWS_OpenErrorReported(t *testing.T) {
	srv, _, pid := terminalTestServer(t, &fakeProvider{sb: &fakeTerminalSandbox{
		term:    newFakeTerminal(),
		openErr: errors.New("attach failed"),
	}})
	conn := dialTerminalRaw(t, srv)
	openTerminal(t, conn, pid)
	env := readTerminalEnvelope(t, conn)
	if env.Type != protocol.EventTerminalError {
		t.Fatalf("type = %q, want %s", env.Type, protocol.EventTerminalError)
	}
}

// A configuration change must tear down the project's live terminals: the
// client observes the shell dying (exit envelope and/or socket close).
func TestTerminalWS_CloseProjectTerminals(t *testing.T) {
	term := newFakeTerminal()
	srv, th, pid := terminalTestServer(t, &fakeProvider{sb: &fakeTerminalSandbox{term: term}})
	conn := dialTerminal(t, srv, pid)

	// The update's new generation retires everything below it (the test
	// project sits at generation 1).
	th.CloseProjectTerminals(pid, 2)

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
		t.Error("terminal not closed after CloseProjectTerminals")
	}
}

// terminal.open resolves its project and refuses anything that is not one the
// caller may reach: the shell must land in the exact project container the
// sessions bound to it use.
func TestTerminalOpen_ValidatesProject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	projects := store.NewProjectStore(db)
	sandboxID := mkSandboxRow(t, db)
	mine := &store.Project{OwnerID: store.LocalUserID, SandboxID: sandboxID, Name: "mine"}
	if err := projects.Create(t.Context(), mine); err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{sb: &fakeTerminalSandbox{term: newFakeTerminal()}}
	th := NewTerminalHandler(store.NewSandboxStore(db), projects, provider, settings.NewReader(nil))
	var auditMu sync.Mutex
	var audited []protocol.AuditRecord
	th.Audit = func(_ context.Context, r protocol.AuditRecord) {
		auditMu.Lock()
		defer auditMu.Unlock()
		audited = append(audited, r)
	}
	engine := newTestEngine()
	engine.GET("/ws/terminal", server.HandleWSWithAuth(th.Handle, testAuthFunc(testWSToken), nil, nil))
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)

	open := func(projectID string) protocol.Envelope {
		conn := dialTerminalRaw(t, srv)
		openTerminal(t, conn, projectID)
		return readTerminalEnvelope(t, conn)
	}

	// No project and an unknown one: refused.
	if env := open(""); env.Type != protocol.EventTerminalError {
		t.Fatalf("projectless open: %s, want %s", env.Type, protocol.EventTerminalError)
	}
	if env := open(store.NewID()); env.Type != protocol.EventTerminalError {
		t.Fatalf("unknown-project open: %s, want %s", env.Type, protocol.EventTerminalError)
	}
	// The caller's own project opens, and the manager saw exactly it.
	if env := open(mine.ID); env.Type != protocol.EventTerminalReady {
		t.Fatalf("valid open: %s, want %s", env.Type, protocol.EventTerminalReady)
	}
	provider.mu.Lock()
	got := append([]string(nil), provider.projects...)
	provider.mu.Unlock()
	if len(got) != 1 || got[0] != mine.ID {
		t.Fatalf("manager saw projects %q, want [%s]", got, mine.ID)
	}
	// Only the successful open leaves an audit line, and it names the project
	// and its owner.
	auditMu.Lock()
	records := append([]protocol.AuditRecord(nil), audited...)
	auditMu.Unlock()
	if len(records) != 1 || records[0].Resource != mine.ID {
		t.Fatalf("audit records = %+v, want one for project %s", records, mine.ID)
	}
	if d := records[0].Detail; !strings.Contains(d, mine.Name) || !strings.Contains(d, store.LocalUserID) {
		t.Fatalf("audit detail %q does not name the project and owner", d)
	}
}

// The instance reference lives exactly as long as the connection: closing the
// socket unwinds the pumps and drops the hold — once, not zero times (which
// would pin an evicted instance forever) and not twice.
func TestTerminalWS_ReleasesInstanceOnClose(t *testing.T) {
	p := &fakeProvider{sb: &fakeTerminalSandbox{term: newFakeTerminal()}}
	srv, _, pid := terminalTestServer(t, p)
	conn := dialTerminal(t, srv, pid)
	_ = conn.Close()

	deadline := time.Now().Add(5 * time.Second)
	for p.releases.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("releases = %d after close, want 1", p.releases.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The registration fence: a terminal that opened under a since-retired
// generation is refused at register — the sweep that retired it ran while
// this terminal was still dialing and could not see it.
func TestTerminalRegisterFence(t *testing.T) {
	th := NewTerminalHandler(nil, nil, nil, settings.NewReader(nil))
	th.CloseProjectTerminals("p1", 2)

	if ok, stale := th.register("p1", &liveTerminal{gen: 1}, 4); ok || !stale {
		t.Fatalf("stale-generation register: ok=%v stale=%v, want a stale refusal", ok, stale)
	}
	if ok, stale := th.register("p1", &liveTerminal{gen: 2}, 4); !ok || stale {
		t.Fatalf("current-generation register: ok=%v stale=%v, want accepted", ok, stale)
	}
	// The fence never regresses: an older sweep arriving late cannot reopen it.
	th.CloseProjectTerminals("p1", 1)
	if ok, stale := th.register("p1", &liveTerminal{gen: 1}, 4); ok || !stale {
		t.Fatalf("register after a late older sweep: ok=%v stale=%v, want still refused", ok, stale)
	}
	// A sibling project is its own fence.
	if ok, stale := th.register("p2", &liveTerminal{gen: 1}, 4); !ok || stale {
		t.Fatalf("sibling project: ok=%v stale=%v, want accepted", ok, stale)
	}
}

// perOpenTerminalSandbox hands each terminal its own PTY, so a sweep that
// closes one connection and spares another is observable.
type perOpenTerminalSandbox struct{ sandbox.Sandbox }

func (perOpenTerminalSandbox) OpenTerminal(context.Context, sandbox.TerminalOptions) (sandbox.Terminal, error) {
	return newFakeTerminal(), nil
}

// A project's sweep closes that project's terminals and leaves other
// projects' connected — they share a target, not a container.
func TestCloseProjectTerminalsSparesSiblings(t *testing.T) {
	srv, th, pid := terminalTestServer(t, &fakeProvider{sb: perOpenTerminalSandbox{}})
	first, err := th.projects.Get(t.Context(), pid)
	if err != nil {
		t.Fatal(err)
	}
	sibling := &store.Project{OwnerID: store.LocalUserID, SandboxID: first.SandboxID, Name: "sibling"}
	if err := th.projects.Create(t.Context(), sibling); err != nil {
		t.Fatal(err)
	}
	mine := dialTerminal(t, srv, pid)
	other := dialTerminal(t, srv, sibling.ID)

	// The project's environment changed: generations below 2 are retired.
	th.CloseProjectTerminals(pid, 2)

	_ = mine.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, _, err := mine.ReadMessage(); err != nil {
			break // torn down — the expected end state
		}
	}
	// The sibling is still connected: its read finds nothing to read rather
	// than a closed socket.
	_ = other.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, _, err = other.ReadMessage()
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Errorf("sibling terminal read = %v, want a timeout (connection still open)", err)
	}
}
