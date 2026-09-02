package e2b_test

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeService is an E2B-shaped server backed by a directory on this machine.
// It exists to exercise the CLIENT — the Connect framing, the JSON shapes, the
// Sandbox contract — end to end without credentials. It is not a claim about
// what the real services do; that is what the protocol probe is for
// (decisions §5.34).
type fakeService struct {
	t   *testing.T
	srv *httptest.Server

	root string
	// numericEnums renders FileType as the enum's number and size as a
	// number — the way Alibaba Cloud's older envd does. The default is E2B's
	// spelling. Both are real; a client that handles one is broken.
	numericEnums bool
	mu           sync.Mutex
	boxes        map[string]*fakeBox
	nextID       int
	nextPID      uint32
	procs        map[uint32]*fakeProc
	// createCalls counts provisioning, so a test can assert a client does not
	// create a second sandbox for one project.
	createCalls int
	// timeoutCalls records each /timeout request's requested TTL in seconds,
	// so a test can assert a long operation extended the lease enough.
	timeoutCalls []int
	// signalCalls counts SendSignal RPCs — the client's cleanup of a process
	// whose stream it abandoned.
	signalCalls int
	// failDeletes makes the next n control-plane DELETEs answer 500 — a
	// transient kill failure.
	failDeletes int
	// failUploads makes the next n /files uploads answer 500.
	failUploads int
	// makeDirHook, when set, runs in the MakeDir handler; a non-empty code
	// answers the RPC with that error instead of touching the filesystem.
	makeDirHook func(path string) (code, msg string)
}

type fakeBox struct {
	id     string
	paused bool
	token  string
}

type fakeProc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
}

// newFakeService serves root as the sandbox's filesystem: the test builds its
// client with root as the working directory, so a sandbox-absolute path IS a
// host path and no translation is needed — which is also what envd does.
func newFakeService(t *testing.T, root string) *fakeService {
	t.Helper()
	f := &fakeService{t: t, root: root, boxes: map[string]*fakeBox{}, procs: map[uint32]*fakeProc{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.route))
	t.Cleanup(f.srv.Close)
	return f
}

// URL is both the control-plane base and — because the client builds envd
// hosts from a domain — the address the test rewrites those to.
func (f *fakeService) URL() string { return f.srv.URL }

func (f *fakeService) route(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/sandboxes"):
		f.control(w, r)
	case r.URL.Path == "/files":
		f.files(w, r)
	case strings.HasPrefix(r.URL.Path, "/process.Process/"),
		strings.HasPrefix(r.URL.Path, "/filesystem.Filesystem/"):
		f.rpc(w, r)
	default:
		http.NotFound(w, r)
	}
}

/* ---------- control plane ---------- */

func (f *fakeService) control(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-API-Key") == "" {
		http.Error(w, `{"message":"no api key"}`, http.StatusUnauthorized)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/sandboxes")
	if rest == "" || rest == "/" {
		if r.Method != http.MethodPost {
			http.Error(w, `{"message":"bad method"}`, http.StatusMethodNotAllowed)
			return
		}
		f.mu.Lock()
		f.nextID++
		f.createCalls++
		box := &fakeBox{id: "sb" + strconv.Itoa(f.nextID), token: "tok" + strconv.Itoa(f.nextID)}
		if req, _ := body(r); req["secure"] != true {
			// Every create this client makes must ask for a credentialed
			// daemon; without it anyone knowing the id reaches the sandbox.
			f.t.Error("create did not ask for a secure sandbox")
		}
		f.boxes[box.id] = box
		f.mu.Unlock()
		writeJSON(w, map[string]any{"sandboxID": box.id, "envdAccessToken": box.token, "state": "running"})
		return
	}
	parts := strings.Split(strings.TrimPrefix(rest, "/"), "/")
	box := f.box(parts[0])
	if box == nil {
		http.Error(w, `{"message":"gone"}`, http.StatusNotFound)
		return
	}
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	switch {
	case r.Method == http.MethodGet && action == "":
		writeJSON(w, f.infoOf(box))
	case r.Method == http.MethodDelete && action == "":
		f.mu.Lock()
		if f.failDeletes > 0 {
			f.failDeletes--
			f.mu.Unlock()
			http.Error(w, `{"message":"transient"}`, http.StatusInternalServerError)
			return
		}
		delete(f.boxes, box.id)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case action == "pause":
		f.setPaused(box, true)
		w.WriteHeader(http.StatusNoContent)
	case action == "connect":
		f.setPaused(box, false)
		writeJSON(w, f.infoOf(box))
	case action == "timeout":
		req, _ := body(r)
		f.mu.Lock()
		f.timeoutCalls = append(f.timeoutCalls, int(num(req["timeout"])))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, `{"message":"unknown action"}`, http.StatusNotFound)
	}
}

func (f *fakeService) infoOf(b *fakeBox) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := "running"
	if b.paused {
		state = "paused"
	}
	return map[string]any{"sandboxID": b.id, "envdAccessToken": b.token, "state": state}
}

func (f *fakeService) setPaused(b *fakeBox, v bool) {
	f.mu.Lock()
	b.paused = v
	f.mu.Unlock()
}

func (f *fakeService) box(id string) *fakeBox {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.boxes[id]
}

// only resolves the single sandbox these tests use. The client addresses envd
// by HOST, which httptest cannot serve, so the test rewrites those requests
// here and the box is found by being the only one.
func (f *fakeService) only() *fakeBox {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.boxes {
		return b
	}
	return nil
}

/* ---------- envd: /files ---------- */

func (f *fakeService) files(w http.ResponseWriter, r *http.Request) {
	box := f.only()
	if box == nil {
		http.Error(w, "no sandbox", http.StatusNotFound)
		return
	}
	full, ok := f.hostPath(r.URL.Query().Get("path"))
	if !ok {
		http.Error(w, "outside the sandbox", http.StatusForbidden)
		return
	}
	_ = box
	switch r.Method {
	case http.MethodGet:
		data, err := os.ReadFile(full)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = w.Write(data)
	case http.MethodPost:
		f.mu.Lock()
		fail := f.failUploads > 0
		if fail {
			f.failUploads--
		}
		f.mu.Unlock()
		if fail {
			http.Error(w, `{"message":"disk full"}`, http.StatusInternalServerError)
			return
		}
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, `{"message":"bad form"}`, http.StatusBadRequest)
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, `{"message":"no file part"}`, http.StatusBadRequest)
			return
		}
		defer file.Close()
		data, _ := io.ReadAll(file)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			http.Error(w, `{"message":"mkdir"}`, http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			http.Error(w, `{"message":"write"}`, http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"path": r.URL.Query().Get("path")})
	default:
		http.Error(w, "bad method", http.StatusMethodNotAllowed)
	}
}

// hostPath refuses anything outside the served root: a client bug must not be
// able to write to the machine running the tests.
func (f *fakeService) hostPath(p string) (string, bool) {
	full := filepath.Clean(filepath.FromSlash(p))
	if full != f.root && !strings.HasPrefix(full, f.root+string(filepath.Separator)) {
		return "", false
	}
	return full, true
}

/* ---------- envd: Connect RPC ---------- */

func (f *fakeService) rpc(w http.ResponseWriter, r *http.Request) {
	box := f.only()
	if box == nil {
		connectErr(w, http.StatusNotFound, "not_found", "no sandbox")
		return
	}
	streaming := strings.Contains(r.Header.Get("Content-Type"), "connect+")
	body, _ := io.ReadAll(r.Body)
	if streaming {
		var err error
		if body, err = firstFrame(body); err != nil {
			connectErr(w, http.StatusBadRequest, "invalid_argument", err.Error())
			return
		}
	}
	var req map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			connectErr(w, http.StatusBadRequest, "invalid_argument", err.Error())
			return
		}
	}
	switch r.URL.Path {
	case "/process.Process/Start":
		f.start(w, req)
	case "/process.Process/SendInput":
		f.sendInput(w, req)
	case "/process.Process/SendSignal":
		// Counted, and honored like real envd's: the process is killed.
		sel, _ := req["process"].(map[string]any)
		f.mu.Lock()
		f.signalCalls++
		proc := f.procs[uint32(num(sel["pid"]))]
		f.mu.Unlock()
		if proc != nil && proc.cmd.Process != nil {
			_ = proc.cmd.Process.Kill()
		}
		writeJSON(w, map[string]any{})
	case "/process.Process/Update":
		writeJSON(w, map[string]any{})
	case "/filesystem.Filesystem/ListDir":
		f.listDir(w, req)
	case "/filesystem.Filesystem/MakeDir":
		f.makeDir(w, req)
	case "/filesystem.Filesystem/Move":
		f.move(w, req)
	case "/filesystem.Filesystem/Remove":
		f.remove(w, req)
	case "/filesystem.Filesystem/Stat":
		f.stat(w, req)
	default:
		connectErr(w, http.StatusNotFound, "unimplemented", r.URL.Path)
	}
}

func (f *fakeService) listDir(w http.ResponseWriter, req map[string]any) {
	full, ok := f.hostPath(str(req["path"]))
	if !ok {
		connectErr(w, http.StatusForbidden, "permission_denied", "outside the sandbox")
		return
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		connectErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		var size int64
		if info, ierr := e.Info(); ierr == nil {
			size = info.Size()
		}
		var typ, sz any
		if f.numericEnums {
			typ, sz = 1, size
			if e.IsDir() {
				typ = 2
			}
		} else {
			typ, sz = "FILE_TYPE_FILE", strconv.FormatInt(size, 10)
			if e.IsDir() {
				typ = "FILE_TYPE_DIRECTORY"
			}
		}
		out = append(out, map[string]any{"name": e.Name(), "type": typ, "size": sz})
	}
	writeJSON(w, map[string]any{"entries": out})
}

func (f *fakeService) makeDir(w http.ResponseWriter, req map[string]any) {
	f.mu.Lock()
	hook := f.makeDirHook
	f.mu.Unlock()
	if hook != nil {
		if code, msg := hook(str(req["path"])); code != "" {
			connectErr(w, http.StatusInternalServerError, code, msg)
			return
		}
	}
	full, ok := f.hostPath(str(req["path"]))
	if !ok {
		connectErr(w, http.StatusForbidden, "permission_denied", "outside the sandbox")
		return
	}
	if _, err := os.Stat(full); err == nil {
		connectErr(w, http.StatusConflict, "already_exists", "exists")
		return
	}
	if err := os.MkdirAll(full, 0o755); err != nil {
		connectErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, map[string]any{})
}

func (f *fakeService) move(w http.ResponseWriter, req map[string]any) {
	src, okSrc := f.hostPath(str(req["source"]))
	dst, okDst := f.hostPath(str(req["destination"]))
	if !okSrc || !okDst {
		connectErr(w, http.StatusForbidden, "permission_denied", "outside the sandbox")
		return
	}
	if _, err := os.Stat(src); err != nil {
		connectErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := os.Rename(src, dst); err != nil {
		connectErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, map[string]any{})
}

// remove is IDEMPOTENT, like real envd's: a missing path answers OK. The
// client's stat-first guard is what turns that into fs.ErrNotExist.
func (f *fakeService) remove(w http.ResponseWriter, req map[string]any) {
	full, ok := f.hostPath(str(req["path"]))
	if !ok {
		connectErr(w, http.StatusForbidden, "permission_denied", "outside the sandbox")
		return
	}
	if err := os.RemoveAll(full); err != nil {
		connectErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, map[string]any{})
}

// stat answers whether a path exists — what RemoveFile checks first, because
// envd's own Remove is idempotent and would hide the absence.
func (f *fakeService) stat(w http.ResponseWriter, req map[string]any) {
	full, ok := f.hostPath(str(req["path"]))
	if !ok {
		connectErr(w, http.StatusForbidden, "permission_denied", "outside the sandbox")
		return
	}
	if _, err := os.Stat(full); err != nil {
		connectErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, map[string]any{"entry": map[string]any{"name": filepath.Base(full)}})
}

// start runs the command on this machine and streams its events back in
// Connect envelopes — the client's framing is what this exercises.
func (f *fakeService) start(w http.ResponseWriter, req map[string]any) {
	proc, _ := req["process"].(map[string]any)
	name := str(proc["cmd"])
	var args []string
	if raw, ok := proc["args"].([]any); ok {
		for _, a := range raw {
			args = append(args, str(a))
		}
	}
	cmd := exec.Command(name, args...) //nolint:gosec // a test fake, running what the test asked for
	// Honor the requested cwd like real envd: a command whose cwd does not
	// exist must fail, not silently run somewhere else.
	cmd.Dir = f.root
	if cwd, ok := f.hostPath(str(proc["cwd"])); ok && str(proc["cwd"]) != "" {
		cmd.Dir = cwd
	}
	if envs, ok := proc["envs"].(map[string]any); ok {
		cmd.Env = append(os.Environ(), envSlice(envs)...)
	}
	pty := req["pty"] != nil

	w.Header().Set("Content-Type", "application/connect+json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	f.mu.Lock()
	f.nextPID++
	pid := f.nextPID
	f.mu.Unlock()

	var stdin io.WriteCloser
	if pty {
		stdin, _ = cmd.StdinPipe()
	}
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		writeFrame(w, endStreamFlagTest, map[string]any{"error": map[string]any{"code": "internal", "message": err.Error()}})
		return
	}
	f.mu.Lock()
	f.procs[pid] = &fakeProc{cmd: cmd, stdin: stdin}
	f.mu.Unlock()
	writeFrame(w, 0, map[string]any{"event": map[string]any{"start": map[string]any{"pid": pid}}})
	if flusher != nil {
		flusher.Flush()
	}

	field := "stdout"
	if pty {
		field = "pty"
	}
	var wg sync.WaitGroup
	var sendMu sync.Mutex
	pump := func(r io.Reader, key string) {
		defer wg.Done()
		br := bufio.NewReader(r)
		buf := make([]byte, 4096)
		for {
			n, err := br.Read(buf)
			if n > 0 {
				sendMu.Lock()
				writeFrame(w, 0, map[string]any{"event": map[string]any{"data": map[string]any{
					key: base64.StdEncoding.EncodeToString(buf[:n]),
				}}})
				if flusher != nil {
					flusher.Flush()
				}
				sendMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go pump(stdout, field)
	go pump(stderr, map[bool]string{true: "pty", false: "stderr"}[pty])
	wg.Wait()

	code := 0
	if err := cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	sendMu.Lock()
	writeFrame(w, 0, map[string]any{"event": map[string]any{"end": map[string]any{"exitCode": code, "exited": true}}})
	writeFrame(w, endStreamFlagTest, map[string]any{})
	sendMu.Unlock()
	f.mu.Lock()
	delete(f.procs, pid)
	f.mu.Unlock()
}

func (f *fakeService) sendInput(w http.ResponseWriter, req map[string]any) {
	sel, _ := req["process"].(map[string]any)
	pid := uint32(num(sel["pid"]))
	f.mu.Lock()
	proc := f.procs[pid]
	f.mu.Unlock()
	if proc == nil {
		connectErr(w, http.StatusNotFound, "not_found", "no such process")
		return
	}
	in, _ := req["input"].(map[string]any)
	raw, _ := base64.StdEncoding.DecodeString(str(in["pty"]))
	if proc.stdin != nil {
		_, _ = proc.stdin.Write(raw)
	}
	writeJSON(w, map[string]any{})
}

/* ---------- wire helpers ---------- */

const endStreamFlagTest = 0x02

// body decodes a JSON request body without consuming it for later handlers.
func body(r *http.Request) (map[string]any, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func connectErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": msg})
}

func writeFrame(w io.Writer, flag byte, v any) {
	payload, _ := json.Marshal(v)
	head := make([]byte, 5)
	head[0] = flag
	binary.BigEndian.PutUint32(head[1:], uint32(len(payload)))
	_, _ = w.Write(head)
	_, _ = w.Write(payload)
}

func firstFrame(body []byte) ([]byte, error) {
	if len(body) < 5 {
		return nil, fmt.Errorf("short frame")
	}
	n := binary.BigEndian.Uint32(body[1:5])
	if int(n)+5 > len(body) {
		return nil, fmt.Errorf("truncated frame")
	}
	return body[5 : 5+n], nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

func envSlice(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+str(v))
	}
	return out
}
