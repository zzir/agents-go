// Package sandboxtest is the conformance suite every sandbox.Sandbox
// implementation must pass — the shared definition of what a backend means,
// exercised against the real thing. It is exported because the backends live
// in their own packages and modules (as models/modelkit/conformancetest). A
// backend's test calls Run with a factory; optional capabilities are detected
// by type assertion, so a backend implementing none still passes the core.
package sandboxtest

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"slices"
	"strings"
	"testing"

	"github.com/zzir/agents-go/sandbox"
)

// Backend is what a suite run needs: a fresh sandbox per subtest, and the
// caller's own teardown. Returning a nil Sandbox skips the run — a backend
// whose daemon is unreachable reports that itself.
type Backend func(t *testing.T) sandbox.Sandbox

// Run executes the whole suite. Subtests are independent: each takes its own
// sandbox from the factory, so a backend may hand out one shared instance or a
// new one per call.
func Run(t *testing.T, open Backend) {
	t.Helper()
	t.Run("Exec", func(t *testing.T) { testExec(t, open) })
	t.Run("ExecFailure", func(t *testing.T) { testExecFailure(t, open) })
	t.Run("Files", func(t *testing.T) { testFiles(t, open) })
	t.Run("CreateExclusive", func(t *testing.T) { testCreateExclusive(t, open) })
	t.Run("RenameRemove", func(t *testing.T) { testRenameRemove(t, open) })
	t.Run("ExecSeesFiles", func(t *testing.T) { testExecSeesFiles(t, open) })
	t.Run("Lifecycle", func(t *testing.T) { testLifecycle(t, open) })
	t.Run("Terminal", func(t *testing.T) { testTerminal(t, open) })
	t.Run("Export", func(t *testing.T) { testExport(t, open) })
}

func sb(t *testing.T, open Backend) sandbox.Sandbox {
	t.Helper()
	s := open(t)
	if s == nil {
		t.Skip("backend unavailable")
	}
	return s
}

// A command's stdout, stderr and exit code come back as the sandbox saw them.
//
//nolint:thelper // a subtest BODY, not a helper: t.Helper() would attribute every failure to the caller's dispatch line instead of the assertion that failed.
func testExec(t *testing.T, open Backend) {
	s := sb(t, open)
	res, err := s.Exec(t.Context(), sandbox.ExecRequest{Cmd: []string{"sh", "-c", "echo out; echo err >&2"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "out") {
		t.Errorf("stdout = %q, want it to carry \"out\"", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "err") {
		t.Errorf("stderr = %q, want it to carry \"err\"", res.Stderr)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
	if res.TimedOut {
		t.Error("TimedOut on a command that finished")
	}
}

// A non-zero exit is a RESULT, not an error: the model has to see it to
// correct itself, so the call must not fail.
//
//nolint:thelper // a subtest BODY, not a helper: t.Helper() would attribute every failure to the caller's dispatch line instead of the assertion that failed.
func testExecFailure(t *testing.T, open Backend) {
	s := sb(t, open)
	res, err := s.Exec(t.Context(), sandbox.ExecRequest{Cmd: []string{"sh", "-c", "exit 3"}})
	if err != nil {
		t.Fatalf("Exec of a failing command returned an error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
}

// Write, read back, list. A missing file is fs.ErrNotExist, not a generic
// failure — the file tools map it to a message the model can act on.
//
//nolint:thelper // a subtest BODY, not a helper: t.Helper() would attribute every failure to the caller's dispatch line instead of the assertion that failed.
func testFiles(t *testing.T, open Backend) {
	s := sb(t, open)
	ctx := t.Context()
	if err := s.WriteFile(ctx, "dir/hello.txt", []byte("hi\n")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := s.ReadFile(ctx, "dir/hello.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hi\n" {
		t.Errorf("ReadFile = %q, want %q", got, "hi\n")
	}
	entries, err := s.ListDir(ctx, "dir")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if !slices.ContainsFunc(entries, func(e sandbox.DirEntry) bool { return e.Name == "hello.txt" && !e.IsDir }) {
		t.Errorf("ListDir = %+v, want it to carry hello.txt", entries)
	}
	if _, err := s.ReadFile(ctx, "dir/absent.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadFile of a missing file = %v, want fs.ErrNotExist", err)
	}
}

// CreateExclusive never overwrites: the second call fails with fs.ErrExist,
// which is what makes apply_patch's Add safe against a concurrent tool call.
//
//nolint:thelper // a subtest BODY, not a helper: t.Helper() would attribute every failure to the caller's dispatch line instead of the assertion that failed.
func testCreateExclusive(t *testing.T, open Backend) {
	s := sb(t, open)
	ctx := t.Context()
	if err := s.CreateExclusive(ctx, "excl/new.txt", []byte("first")); err != nil {
		t.Fatalf("CreateExclusive: %v", err)
	}
	if err := s.CreateExclusive(ctx, "excl/new.txt", []byte("second")); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second CreateExclusive = %v, want fs.ErrExist", err)
	}
	got, err := s.ReadFile(ctx, "excl/new.txt")
	if err != nil || string(got) != "first" {
		t.Errorf("content = %q, %v; want the first write to stand", got, err)
	}
}

// Rename moves (creating parents), Remove deletes, and both report a missing
// source as fs.ErrNotExist.
//
//nolint:thelper // a subtest BODY, not a helper: t.Helper() would attribute every failure to the caller's dispatch line instead of the assertion that failed.
func testRenameRemove(t *testing.T, open Backend) {
	s := sb(t, open)
	ctx := t.Context()
	if err := s.WriteFile(ctx, "mv/a.txt", []byte("x")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := s.Rename(ctx, "mv/a.txt", "mv/deep/b.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := s.ReadFile(ctx, "mv/a.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("source after Rename = %v, want fs.ErrNotExist", err)
	}
	if got, err := s.ReadFile(ctx, "mv/deep/b.txt"); err != nil || string(got) != "x" {
		t.Errorf("destination after Rename = %q, %v", got, err)
	}
	if err := s.RemoveFile(ctx, "mv/deep/b.txt"); err != nil {
		t.Fatalf("RemoveFile: %v", err)
	}
	if err := s.RemoveFile(ctx, "mv/deep/b.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("second RemoveFile = %v, want fs.ErrNotExist", err)
	}
}

// The file operations and exec share ONE filesystem (decisions §5.14).
//
//nolint:thelper // a subtest BODY, not a helper: t.Helper() would attribute every failure to the caller's dispatch line instead of the assertion that failed.
func testExecSeesFiles(t *testing.T, open Backend) {
	s := sb(t, open)
	ctx := t.Context()
	if err := s.WriteFile(ctx, "shared.txt", []byte("from-the-file-tool")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	res, err := s.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"sh", "-c", "cat shared.txt"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "from-the-file-tool") {
		t.Errorf("exec cannot see the written file: stdout = %q, stderr = %q", res.Stdout, res.Stderr)
	}
	if _, err := s.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"sh", "-c", "echo from-exec > back.txt"}}); err != nil {
		t.Fatalf("Exec write: %v", err)
	}
	got, err := s.ReadFile(ctx, "back.txt")
	if err != nil || !strings.Contains(string(got), "from-exec") {
		t.Errorf("the file tool cannot see what exec wrote: %q, %v", got, err)
	}
}

// Stop keeps the FILESYSTEM and nothing more; Start makes it usable again.
// A backend that implements no Lifecycle skips this.
//
//nolint:thelper // a subtest BODY, not a helper: t.Helper() would attribute every failure to the caller's dispatch line instead of the assertion that failed.
func testLifecycle(t *testing.T, open Backend) {
	s := sb(t, open)
	lc, ok := s.(sandbox.Lifecycle)
	if !ok {
		t.Skip("backend has no Lifecycle")
	}
	ctx := t.Context()
	if err := s.WriteFile(ctx, "survivor.txt", []byte("kept")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := lc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got, err := lc.Status(ctx); err != nil || got != sandbox.StateRunning {
		t.Fatalf("Status after Start = %v, %v; want running", got, err)
	}
	if err := lc.Start(ctx); err != nil {
		t.Errorf("Start on a running sandbox = %v, want a no-op", err)
	}
	if err := lc.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got, err := lc.Status(ctx); err != nil || got == sandbox.StateRunning {
		t.Fatalf("Status after Stop = %v, %v; want not-running", got, err)
	}
	if err := lc.Stop(ctx); err != nil {
		t.Errorf("Stop on a stopped sandbox = %v, want a no-op", err)
	}
	// The filesystem is the one thing Stop promises.
	if got, err := s.ReadFile(ctx, "survivor.txt"); err != nil || string(got) != "kept" {
		t.Errorf("file after a stop/start cycle = %q, %v; want it kept", got, err)
	}
}

// A terminal echoes what is typed and reports the shell's exit.
//
//nolint:thelper // a subtest BODY, not a helper: t.Helper() would attribute every failure to the caller's dispatch line instead of the assertion that failed.
func testTerminal(t *testing.T, open Backend) {
	s := sb(t, open)
	opener, ok := s.(sandbox.TerminalOpener)
	if !ok {
		t.Skip("backend has no TerminalOpener")
	}
	term, err := opener.OpenTerminal(t.Context(), sandbox.TerminalOptions{Cols: 100, Rows: 30})
	if err != nil {
		if errors.Is(err, sandbox.ErrTerminalUnsupported) {
			t.Skip("terminals unsupported in this configuration")
		}
		t.Fatalf("OpenTerminal: %v", err)
	}
	defer term.Close()
	if err := term.Resize(120, 40); err != nil {
		t.Errorf("Resize: %v", err)
	}
	if _, err := term.Write([]byte("echo conformance-marker\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !readUntil(t, term, "conformance-marker") {
		t.Fatal("the terminal never echoed the marker back")
	}
	if _, err := term.Write([]byte("exit 0\n")); err != nil {
		t.Fatalf("Write exit: %v", err)
	}
	// Drain to EOF, then the code resolves.
	_, _ = io.Copy(io.Discard, term)
	if _, err := term.Wait(); err != nil {
		t.Errorf("Wait: %v", err)
	}
}

// The export is a tar of the working tree, carrying what was written into it.
//
//nolint:thelper // a subtest BODY, not a helper: t.Helper() would attribute every failure to the caller's dispatch line instead of the assertion that failed.
func testExport(t *testing.T, open Backend) {
	s := sb(t, open)
	ex, ok := s.(sandbox.Exporter)
	if !ok {
		t.Skip("backend has no Exporter")
	}
	ctx := t.Context()
	if err := s.WriteFile(ctx, "exported.txt", []byte("in-the-archive")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	rc, err := ex.ExportTar(ctx)
	if err != nil {
		t.Fatalf("ExportTar: %v", err)
	}
	defer rc.Close()
	// Read a bounded prefix: the assertion is that the archive carries the
	// file, not that the whole tree fits in memory.
	buf, err := io.ReadAll(io.LimitReader(rc, 4<<20))
	if err != nil {
		t.Fatalf("reading the archive: %v", err)
	}
	if !bytes.Contains(buf, []byte("exported.txt")) {
		t.Error("the archive does not name the exported file")
	}
}

// readUntil reads from term until want appears or the reads stop producing it,
// bounded by the test's own deadline: a hang ends as a test timeout, which names it.
func readUntil(t *testing.T, r io.Reader, want string) bool {
	t.Helper()
	var acc []byte
	buf := make([]byte, 4096)
	for range 200 {
		n, err := r.Read(buf)
		acc = append(acc, buf[:n]...)
		if bytes.Contains(acc, []byte(want)) {
			return true
		}
		if err != nil {
			return false
		}
	}
	return false
}
