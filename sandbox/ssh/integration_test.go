//go:build ssh_integration

// Run with: go test -tags ssh_integration ./sandbox/ssh
//
// Requires a reachable SSH server with a POSIX shell and SFTP enabled.
// Configure via environment variables:
//
//	SSH_TEST_ADDR   host or host:port (required)
//	SSH_TEST_USER   username (required)
//	SSH_TEST_KEY    path to a private key file (optional)
//	SSH_TEST_PASS   password (optional; used if no key)
//
// Host-key verification is disabled for these tests.
package ssh

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zzir/agents-go/sandbox"
)

func testSandbox(t *testing.T) *Sandbox {
	t.Helper()
	addr := os.Getenv("SSH_TEST_ADDR")
	user := os.Getenv("SSH_TEST_USER")
	if addr == "" || user == "" {
		t.Skip("SSH_TEST_ADDR and SSH_TEST_USER must be set")
	}
	sb, err := New(Options{
		Addr:    addr,
		User:    user,
		Auth:    AuthConfig{KeyFile: os.Getenv("SSH_TEST_KEY"), Password: os.Getenv("SSH_TEST_PASS")},
		HostKey: HostKeyConfig{InsecureIgnoreHostKey: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	return sb
}

func TestSSHSandbox_RunsCommand(t *testing.T) {
	sb := testSandbox(t)
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Cmd: []string{"echo", "hello from ssh"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, stderr = %q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "hello from ssh") {
		t.Errorf("stdout = %q", res.Stdout)
	}
}

func TestSSHSandbox_FilesWritten(t *testing.T) {
	sb := testSandbox(t)
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Files: map[string]string{"sub/data.txt": "payload"},
		Cmd:   []string{"cat", "sub/data.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, stderr = %q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "payload") {
		t.Errorf("stdout = %q", res.Stdout)
	}
}

func TestSSHSandbox_NonZeroExit(t *testing.T) {
	sb := testSandbox(t)
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Cmd: []string{"sh", "-c", "exit 3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", res.ExitCode)
	}
}

func TestSSHSandbox_Stdin(t *testing.T) {
	sb := testSandbox(t)
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Cmd:   []string{"cat"},
		Stdin: "piped input",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, "piped input") {
		t.Errorf("stdout = %q", res.Stdout)
	}
}

func TestSSHSandbox_Env(t *testing.T) {
	sb := testSandbox(t)
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Cmd: []string{"sh", "-c", "echo $FOO"},
		Env: map[string]string{"FOO": "bar123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, "bar123") {
		t.Errorf("stdout = %q", res.Stdout)
	}
}

func TestSSHSandbox_Timeout(t *testing.T) {
	sb := testSandbox(t)
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Cmd:     []string{"sh", "-c", "echo started; sleep 30"},
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.ExitCode != -1 {
		t.Errorf("TimedOut = %v, exit = %d; want true, -1", res.TimedOut, res.ExitCode)
	}
}

func TestSSHSandbox_Cleanup(t *testing.T) {
	sb := testSandbox(t)
	// Write a file, then verify nothing under the base dir survives. We probe by
	// listing the temp directory naming pattern; after Exec it must be gone.
	_, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Files: map[string]string{"f.txt": "x"},
		Cmd:   []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	matches, err := sb.sftp.Glob("/tmp/agents-sandbox-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("temp dirs left behind: %v", matches)
	}
}

func TestSSHSandbox_OutputCap(t *testing.T) {
	sb := testSandbox(t)
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{
		Cmd:            []string{"sh", "-c", "yes x | head -c 100000"},
		MaxOutputBytes: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) != 4096 {
		t.Errorf("stdout = %d bytes, want capped at 4096", len(res.Stdout))
	}
}
