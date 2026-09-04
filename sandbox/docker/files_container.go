package docker

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zzir/agents-go/sandbox"
)

// The container side of the file operations (persistent mode, no bind mount):
// everything goes through exec, so the tools see every mount (decisions §5.14).

// Exit codes the in-container scripts use for outcomes a caller branches on —
// a code, not a phrase: stderr wording is the image's shell and locale to choose.
const (
	exitIsDir      = 21
	exitOpenFailed = 22
	exitTooLarge   = 23
	exitNotFound   = 24
	exitNotDir     = 25
)

// readFileScript writes full as base64 on stdout, size-guarded by limit. The
// redirect follows symlinks and fails on a missing file.
func readFileScript(full string, limit int64) string {
	return fmt.Sprintf(
		"f=%s\n"+
			"if [ -d \"$f\" ]; then exit %d; fi\n"+
			"sz=$(wc -c < \"$f\") || exit %d\n"+
			"if [ \"$((sz))\" -gt %d ]; then exit %d; fi\n"+
			"exec base64 < \"$f\"",
		sandbox.ShellQuote(full), exitIsDir, exitOpenFailed, limit, exitTooLarge)
}

func (s *Sandbox) readFileContainer(ctx context.Context, p string) ([]byte, error) {
	if err := s.ensureImage(ctx); err != nil {
		return nil, err
	}
	limit := s.opts.MaxReadFileBytes
	if limit <= 0 {
		limit = sandbox.DefaultMaxReadFileBytes
	}
	// The exec cap sits above base64's 4/3 inflation so a file at the limit is
	// never cut mid-stream; the timeout grows with the limit for remote daemons.
	timeout := sandbox.DefaultTimeout + time.Duration(limit>>20)*time.Second
	res, err := s.Exec(ctx, sandbox.ExecRequest{
		Cmd:            []string{"sh", "-c", readFileScript(s.containerPath(p), limit)},
		Timeout:        timeout,
		MaxOutputBytes: limit + limit/2 + 1024,
	})
	if err != nil {
		return nil, err
	}
	if res.TimedOut {
		return nil, fmt.Errorf("docker sandbox: read %q: timed out after %s: %w", p, timeout, context.DeadlineExceeded)
	}
	switch res.ExitCode {
	case 0:
		// Strip the wrapping GNU base64 inserts every 76 columns (busybox emits
		// none); the StdEncoding decoder rejects embedded newlines.
		clean := strings.NewReplacer("\n", "", "\r", "").Replace(res.Stdout)
		data, derr := base64.StdEncoding.DecodeString(clean)
		if derr != nil {
			return nil, fmt.Errorf("docker sandbox: read %q: decoding output: %w", p, derr)
		}
		if int64(len(data)) > limit {
			return nil, fmt.Errorf("docker sandbox: read %q: %w", p, sandbox.ErrReadLimitExceeded)
		}
		return data, nil
	case exitIsDir:
		return nil, &fs.PathError{Op: "read", Path: p, Err: syscall.EISDIR}
	case exitTooLarge:
		return nil, fmt.Errorf("docker sandbox: read %q: %w", p, sandbox.ErrReadLimitExceeded)
	default:
		// exitOpenFailed (the redirect failed) and anything else carry the
		// shell's message, which tells absence from permission from a loop.
		low := strings.ToLower(res.Stderr)
		if strings.Contains(low, "no such file") {
			return nil, fmt.Errorf("docker sandbox: read %q: %w", p, fs.ErrNotExist)
		}
		if strings.Contains(low, "permission denied") {
			return nil, fmt.Errorf("docker sandbox: read %q: %w", p, fs.ErrPermission)
		}
		if strings.Contains(low, "too many levels of symbolic links") {
			return nil, fmt.Errorf("docker sandbox: read %s: too many levels of symbolic links", p)
		}
		return nil, fmt.Errorf("docker sandbox: read %q: %s", p, strings.TrimSpace(res.Stderr))
	}
}

func (s *Sandbox) writeFileContainer(ctx context.Context, p string, content []byte) error {
	if err := s.ensureImage(ctx); err != nil {
		return err
	}
	full := s.containerPath(p)
	if full == "/" {
		return fmt.Errorf("docker sandbox: invalid file path %q", p)
	}
	// Stage the bytes via ExecRequest.Files (tar: binary-safe, size-unbounded),
	// then move them into place with exec, which reaches mounts the archive API cannot.
	buf := make([]byte, 8)
	_, _ = rand.Read(buf) // never fails as of Go 1.24
	stageName := ".agents-write." + hex.EncodeToString(buf)
	stageAbs := path.Join(workDir, stageName)
	script := fmt.Sprintf("mkdir -p -- %s && cat -- %s > %s; rc=$?; rm -f -- %s; exit $rc",
		sandbox.ShellQuote(path.Dir(full)), sandbox.ShellQuote(stageAbs), sandbox.ShellQuote(full), sandbox.ShellQuote(stageAbs))
	res, err := s.Exec(ctx, sandbox.ExecRequest{Files: map[string]string{stageName: string(content)}, Cmd: []string{"sh", "-c", script}})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		if strings.Contains(res.Stderr, "Permission denied") {
			return fmt.Errorf("docker sandbox: write %q: %w", p, fs.ErrPermission)
		}
		return fmt.Errorf("docker sandbox: write file %q: %s", p, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func (s *Sandbox) createExclusiveContainer(ctx context.Context, p string, content []byte) error {
	if err := s.ensureImage(ctx); err != nil {
		return err
	}
	full := s.containerPath(p)
	if full == "/" {
		return fmt.Errorf("docker sandbox: invalid file path %q", p)
	}
	b64 := base64.StdEncoding.EncodeToString(content)
	buf := make([]byte, 8)
	_, _ = rand.Read(buf) // never fails as of Go 1.24
	tmpPath := path.Join(path.Dir(full), ".ap."+hex.EncodeToString(buf))
	createScript, cleanupScript, rmTmpScript := exclusiveCreateScripts(full, tmpPath, b64)
	res, err := s.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"sh", "-c", createScript}})
	cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelCleanup()
	// A hard error or a timeout is outcome-unknown: ln may have published the
	// target. The inode-checked cleanup makes a returned error mean "not created".
	if err != nil || res.TimedOut {
		cres, cerr := s.Exec(cleanupCtx, sandbox.ExecRequest{Cmd: []string{"sh", "-c", cleanupScript}})
		if cerr != nil || cres.TimedOut || cres.ExitCode != 0 {
			return fmt.Errorf("docker sandbox: create %q outcome unknown; cleanup unconfirmed, the target may still exist", p)
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("docker sandbox: create %q timed out", p)
	}
	// Exec ran to completion; drop the temp link (a published target keeps its
	// own hard link). A tmp-remove failure only leaks the temp file.
	_, _ = s.Exec(cleanupCtx, sandbox.ExecRequest{Cmd: []string{"sh", "-c", rmTmpScript}})
	if res.ExitCode != 0 {
		if res.ExitCode == exitTargetExists {
			return fmt.Errorf("docker sandbox: create %q: %w", p, fs.ErrExist)
		}
		return fmt.Errorf("docker sandbox: create %q: %s", p, res.Stderr)
	}
	return nil
}

// removeFileScript removes full, reporting absence by exit code. -L as well as
// -e: a dangling symlink is there to remove.
func removeFileScript(full string) string {
	return fmt.Sprintf(
		"f=%s\n"+
			"if [ ! -e \"$f\" ] && [ ! -L \"$f\" ]; then exit %d; fi\n"+
			"exec rm -- \"$f\"",
		sandbox.ShellQuote(full), exitNotFound)
}

func (s *Sandbox) removeFileContainer(ctx context.Context, p string) error {
	res, err := s.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"sh", "-c", removeFileScript(s.containerPath(p))}})
	if err != nil {
		return err
	}
	switch res.ExitCode {
	case 0:
		return nil
	case exitNotFound:
		return fmt.Errorf("docker sandbox: rm %q: %w", p, fs.ErrNotExist)
	default:
		return fmt.Errorf("docker sandbox: rm %q: %s", p, strings.TrimSpace(res.Stderr))
	}
}

func (s *Sandbox) renameContainer(ctx context.Context, oldPath, newPath string) error {
	oc := s.containerPath(oldPath)
	nc := s.containerPath(newPath)
	if parent := path.Dir(nc); parent != "/" {
		if res, err := s.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"mkdir", "-p", "--", parent}}); err != nil {
			return err
		} else if res.ExitCode != 0 {
			return fmt.Errorf("docker sandbox: mkdir %s: %s", parent, res.Stderr)
		}
	}
	res, err := s.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"mv", "--", oc, nc}})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("docker sandbox: mv %q -> %q: %s", oldPath, newPath, res.Stderr)
	}
	return nil
}

// listDirScript lists dir one level deep, NUL-terminated "type\tsize\tname" records;
// absence/non-dir are exit codes. Portable sh; wc -c on regular files only (fifo blocks).
func listDirScript(dir string) string {
	return fmt.Sprintf(
		"d=%s\n"+
			"if [ ! -e \"$d\" ]; then exit %d; fi\n"+
			"if [ ! -d \"$d\" ]; then exit %d; fi\n"+
			"exec find \"$d\" -maxdepth 1 -mindepth 1 -exec sh -c %s _ {} +",
		sandbox.ShellQuote(dir), exitNotFound, exitNotDir,
		sandbox.ShellQuote(`for f; do if [ -d "$f" ]; then printf "d\t0\t%s\0" "${f##*/}"; elif [ -f "$f" ]; then printf "f\t%s\t%s\0" "$(($(wc -c < "$f")))" "${f##*/}"; else printf "f\t0\t%s\0" "${f##*/}"; fi; done`))
}

func (s *Sandbox) listDirContainer(ctx context.Context, p string) ([]sandbox.DirEntry, error) {
	if err := s.ensureImage(ctx); err != nil {
		return nil, err
	}
	res, err := s.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"sh", "-c", listDirScript(s.containerPath(p))}})
	if err != nil {
		return nil, err
	}
	switch res.ExitCode {
	case 0:
		return parseFindEntries(res.Stdout), nil
	case exitNotFound:
		return nil, fmt.Errorf("docker sandbox: list dir %q: %w", p, fs.ErrNotExist)
	case exitNotDir:
		return nil, &fs.PathError{Op: "list", Path: p, Err: syscall.ENOTDIR}
	default:
		return nil, fmt.Errorf("docker sandbox: list dir %q: %s", p, strings.TrimSpace(res.Stderr))
	}
}

// containerPath maps a model-supplied path with shell semantics, mirroring
// exec: absolute as-is, relative under /workspace (spec §2.7t).
func (s *Sandbox) containerPath(p string) string {
	if path.IsAbs(p) {
		return path.Clean(p)
	}
	return path.Join(workDir, p)
}

// exitTargetExists is the create script's exit code for "the target is already
// there": EEXIST's number, picked only because it is recognizable.
const exitTargetExists = 17

// exclusiveCreateScripts builds the atomic exclusive-create of absolute fullPath via
// a temp hard link: create (ln fails on an existing target), cleanup by inode, rmTmp.
func exclusiveCreateScripts(fullPath, tmpPath, b64 string) (create, cleanup, rmTmp string) {
	target := sandbox.ShellQuote(fullPath)
	dirQ := sandbox.ShellQuote(path.Dir(fullPath))
	tmpQ := sandbox.ShellQuote(tmpPath)
	// "Already there" leaves as an exit CODE, not a phrase in ln's stderr. -L as
	// well as -e: a dangling symlink, which ln refuses, would otherwise read as absent.
	create = "mkdir -p " + dirQ + " || exit 1\n" +
		"printf %s " + sandbox.ShellQuote(b64) + " | base64 -d > " + tmpQ + " || exit 1\n" +
		"if ln " + tmpQ + " " + target + "; then exit 0; fi\n" +
		"if [ -e " + target + " ] || [ -L " + target + " ]; then exit " + strconv.Itoa(exitTargetExists) + "; fi\n" +
		"exit 1"
	cleanup = "rc=0; if [ " + target + " -ef " + tmpQ + " ]; then rm -f " + target + " || rc=1; fi; rm -f " + tmpQ + " || rc=1; exit $rc"
	rmTmp = "rm -f " + tmpQ
	return
}

// parseFindEntries parses listDirScript's NUL-separated output. A trailing
// NUL yields an empty final record, which is skipped.
func parseFindEntries(out string) []sandbox.DirEntry {
	records := strings.Split(out, "\x00")
	entries := make([]sandbox.DirEntry, 0, len(records))
	for _, rec := range records {
		if rec == "" {
			continue
		}
		parts := strings.SplitN(rec, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		size, _ := strconv.ParseInt(parts[1], 10, 64)
		entries = append(entries, sandbox.DirEntry{
			Name:  parts[2],
			IsDir: parts[0] == "d",
			Size:  size,
		})
	}
	return entries
}
