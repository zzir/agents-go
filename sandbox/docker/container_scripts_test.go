package docker

import (
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runScript runs an in-container script under the host's /bin/sh — the only
// daemon-free way to see what it reports — returning its exit code and stdout.
func runScript(t *testing.T, script string) (int, string) {
	t.Helper()
	out, err := exec.Command("/bin/sh", "-c", script).Output()
	if err == nil {
		return 0, string(out)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("running the script: %v", err)
	return -1, ""
}

// Absence, a directory and a non-directory travel as exit codes, never as a
// phrase sniffed out of stderr: the wording is the image's to choose, and a
// miss would turn "not found" into a generic failure the file tools render
// differently and apply_patch's rollback cannot tell apart.
func TestContainerScriptExitCodes(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("read", func(t *testing.T) {
		if code, out := runScript(t, readFileScript(file, 1024)); code != 0 {
			t.Fatalf("reading a file: exit %d", code)
		} else if data, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(out)); string(data) != "hello" {
			t.Fatalf("content = %q", data)
		}
		if code, _ := runScript(t, readFileScript(sub, 1024)); code != exitIsDir {
			t.Errorf("reading a directory: exit %d, want %d", code, exitIsDir)
		}
		if code, _ := runScript(t, readFileScript(file, 2)); code != exitTooLarge {
			t.Errorf("reading past the limit: exit %d, want %d", code, exitTooLarge)
		}
		if code, _ := runScript(t, readFileScript(filepath.Join(dir, "absent"), 1024)); code != exitOpenFailed {
			t.Errorf("reading a missing file: exit %d, want %d", code, exitOpenFailed)
		}
	})

	t.Run("remove", func(t *testing.T) {
		if code, _ := runScript(t, removeFileScript(filepath.Join(dir, "absent"))); code != exitNotFound {
			t.Errorf("removing a missing file: exit %d, want %d", code, exitNotFound)
		}
		link := filepath.Join(dir, "dangling")
		if err := os.Symlink(filepath.Join(dir, "nowhere"), link); err != nil {
			t.Fatal(err)
		}
		if code, _ := runScript(t, removeFileScript(link)); code != 0 {
			t.Errorf("removing a dangling symlink: exit %d", code)
		}
		gone := filepath.Join(dir, "gone.txt")
		if err := os.WriteFile(gone, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if code, _ := runScript(t, removeFileScript(gone)); code != 0 {
			t.Errorf("removing a file: exit %d", code)
		}
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the file survived its removal: %v", err)
		}
	})

	t.Run("list", func(t *testing.T) {
		if code, _ := runScript(t, listDirScript(filepath.Join(dir, "absent"))); code != exitNotFound {
			t.Errorf("listing a missing directory: exit %d, want %d", code, exitNotFound)
		}
		if code, _ := runScript(t, listDirScript(file)); code != exitNotDir {
			t.Errorf("listing a file: exit %d, want %d", code, exitNotDir)
		}
		code, out := runScript(t, listDirScript(dir))
		if code != 0 {
			t.Fatalf("listing a directory: exit %d", code)
		}
		var names []string
		for _, e := range parseFindEntries(out) {
			names = append(names, e.Name)
			if e.Name == "f.txt" && (e.IsDir || e.Size != 5) {
				t.Errorf("f.txt = %+v, want a 5-byte file", e)
			}
			if e.Name == "sub" && !e.IsDir {
				t.Errorf("sub = %+v, want a directory", e)
			}
		}
		if len(names) != 2 {
			t.Errorf("entries = %v, want f.txt and sub", names)
		}
	})
}
