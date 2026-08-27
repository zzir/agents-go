package docker

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/zzir/agents-go/sandbox"
)

// The container side of the file operations, used in persistent mode without a
// bind mount: the working directory lives inside the long-lived container, so
// these reach it the same way a command does — with tar over the daemon API for
// bulk content and with exec for everything the API does not expose.

func (s *Sandbox) readFileContainer(ctx context.Context, p string) ([]byte, error) {
	if err := s.ensureImage(ctx); err != nil {
		return nil, err
	}
	id, err := s.ensureContainer(ctx)
	if err != nil {
		return nil, err
	}
	result, err := s.cli.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{
		SourcePath: s.containerPath(p),
	})
	if err != nil {
		// The daemon reports a missing path as its own not-found; map it so
		// callers see fs.ErrNotExist, as every other backend gives them.
		if cerrdefs.IsNotFound(err) || strings.Contains(err.Error(), "Could not find the file") {
			return nil, fmt.Errorf("docker sandbox: read file %q: %w", p, fs.ErrNotExist)
		}
		return nil, fmt.Errorf("docker sandbox: read file: %w", err)
	}
	defer result.Content.Close()
	tr := tar.NewReader(result.Content)
	if _, err := tr.Next(); err != nil {
		return nil, fmt.Errorf("docker sandbox: read file: %w", err)
	}
	return sandbox.ReadAllLimited(tr, s.opts.MaxReadFileBytes)
}

func (s *Sandbox) writeFileContainer(ctx context.Context, p string, content []byte) error {
	if err := s.ensureImage(ctx); err != nil {
		return err
	}
	id, err := s.ensureContainer(ctx)
	if err != nil {
		return err
	}
	// The tar is built with root-relative names and extracted at "/", so an
	// absolute in-container path works the same as a workdir-relative one.
	tarball, terr := buildTar(map[string]string{s.containerPath(p)[1:]: string(content)})
	if terr != nil {
		return terr
	}
	if _, err := s.cli.CopyToContainer(ctx, id, client.CopyToContainerOptions{DestinationPath: "/", Content: tarball}); err != nil {
		return fmt.Errorf("docker sandbox: write file: %w", err)
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
	// As of Go 1.24 crypto/rand.Read never fails; it aborts the program if
	// the OS source is unavailable.
	_, _ = rand.Read(buf)
	tmpPath := path.Join(path.Dir(full), ".ap."+hex.EncodeToString(buf))
	createScript, cleanupScript, rmTmpScript := exclusiveCreateScripts(full, tmpPath, b64)
	res, err := s.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"sh", "-c", createScript}})
	cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelCleanup()
	// Both a hard error and a timeout (Exec returns TimedOut:true, ExitCode:-1,
	// err:nil) are outcome-unknown: ln may have published the target before Exec
	// stopped, and the temp link is about to be removed. Run the inode-checked
	// cleanup so a returned error always means "target not created" — unless the
	// cleanup itself can't be confirmed (same daemon failure / timeout), in
	// which case we must NOT claim the target is gone.
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
	// Exec ran to completion; drop our temp link (the target, if published,
	// keeps its own independent hard link). A tmp-remove failure only leaks the
	// temp file, so it's non-fatal.
	_, _ = s.Exec(cleanupCtx, sandbox.ExecRequest{Cmd: []string{"sh", "-c", rmTmpScript}})
	if res.ExitCode != 0 {
		if res.ExitCode == exitTargetExists {
			return fmt.Errorf("docker sandbox: create %q: %w", p, fs.ErrExist)
		}
		return fmt.Errorf("docker sandbox: create %q: %s", p, res.Stderr)
	}
	return nil
}

func (s *Sandbox) removeFileContainer(ctx context.Context, p string) error {
	res, err := s.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"rm", "--", s.containerPath(p)}})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		// "Absent" must be distinguishable from a real failure — the file
		// tools render the two differently, and apply_patch's rollback
		// depends on it.
		if strings.Contains(res.Stderr, "No such file") {
			return fmt.Errorf("docker sandbox: rm %q: %w", p, fs.ErrNotExist)
		}
		return fmt.Errorf("docker sandbox: rm %q: %s", p, res.Stderr)
	}
	return nil
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

func (s *Sandbox) listDirContainer(ctx context.Context, p string) ([]sandbox.DirEntry, error) {
	if err := s.ensureImage(ctx); err != nil {
		return nil, err
	}
	dir := s.containerPath(p)
	// NUL-terminate each record instead of newline: a filename may contain
	// a newline (or a tab), which would otherwise split into a phantom
	// entry or corrupt the next line. The name is the final \t-field, so a
	// tab inside it is preserved by the 3-way split; NUL can never appear
	// in a filename, so records stay unambiguous.
	//
	// The formatting is a batched `sh` rather than find's `-printf`: that
	// flag is GNU-only, and busybox find — every alpine-based image — fails
	// the whole listing on it. `-exec … +` runs ONE shell for the entire
	// directory, so the cost is one `wc -c` per regular file.
	cmd := fmt.Sprintf("find %s -maxdepth 1 -mindepth 1 -exec sh -c %s _ {} +",
		sandbox.ShellQuote(dir),
		sandbox.ShellQuote(`for f; do if [ -d "$f" ]; then printf "d\t0\t%s\0" "${f##*/}"; else printf "f\t%s\t%s\0" "$(wc -c < "$f")" "${f##*/}"; fi; done`))
	res, err := s.Exec(ctx, sandbox.ExecRequest{Cmd: []string{"sh", "-c", cmd}})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		// A missing directory must surface as fs.ErrNotExist so callers can
		// tell "absent" from a real failure, uniform with the bind-mount and
		// local/ssh backends.
		if strings.Contains(res.Stderr, "No such file") {
			return nil, fmt.Errorf("docker sandbox: list dir %q: %w", p, fs.ErrNotExist)
		}
		return nil, fmt.Errorf("docker sandbox: list dir: %s", res.Stderr)
	}
	return parseFindEntries(res.Stdout), nil
}

// containerPath maps a model-supplied path onto the container filesystem with
// shell semantics, mirroring exec (whose cwd is the working directory): an
// absolute path is used as-is, a relative one resolves under the container's
// working directory (ContainerWorkDir — the directory exec runs in). The
// container itself is the isolation boundary — exec already reaches the whole
// container filesystem — so persistent-mode file operations share exec's view
// rather than pretending to a narrower one.
func (s *Sandbox) containerPath(p string) string {
	if path.IsAbs(p) {
		return path.Clean(p)
	}
	return path.Join(s.containerWorkDir(), p)
}

// exclusiveCreateScripts builds the in-container shell scripts for an atomic
// exclusive-create of fullPath (absolute in-container) via a temp hard link.
// Absolute paths start with "/", so mkdir/ln/rm can never mistake one for an
// option (e.g. a "-f" filename) — sandbox.ShellQuote stops shell expansion,
// not option parsing. The temp file has a Go-generated unpredictable name (not
// a shell glob), so we never touch a user file. Returns:
//   - create: write tmp from b64, then hard-link it onto the target. ln fails
//     with EEXIST if the target exists — the atomic exclusive create.
//   - cleanup: run when the outcome is unknown (Exec error or timeout). Undo a
//     target that ln may have published (same inode as tmp), then drop tmp,
//     propagating any rm failure to the exit code.
//   - rmTmp: drop just the temp link after a completed Exec (non-fatal).
//
// exitTargetExists is the create script's exit code for "the target is already
// there": EEXIST's number, picked only because it is recognizable.
const exitTargetExists = 17

func exclusiveCreateScripts(fullPath, tmpPath, b64 string) (create, cleanup, rmTmp string) {
	target := sandbox.ShellQuote(fullPath)
	dirQ := sandbox.ShellQuote(path.Dir(fullPath))
	tmpQ := sandbox.ShellQuote(tmpPath)
	// "Already there" leaves as an exit CODE, not as a phrase in ln's stderr:
	// the wording is the image's to choose (GNU ln, BusyBox ln, any locale),
	// and a create that silently stopped reporting fs.ErrExist would turn
	// apply_patch's Add-over-an-existing-file conflict into a generic failure.
	// -L as well as -e, or a dangling symlink — which ln refuses and -e does
	// not see — would read as "not there".
	create = "mkdir -p " + dirQ + " || exit 1\n" +
		"printf %s " + sandbox.ShellQuote(b64) + " | base64 -d > " + tmpQ + " || exit 1\n" +
		"if ln " + tmpQ + " " + target + "; then exit 0; fi\n" +
		"if [ -e " + target + " ] || [ -L " + target + " ]; then exit " + strconv.Itoa(exitTargetExists) + "; fi\n" +
		"exit 1"
	cleanup = "rc=0; if [ " + target + " -ef " + tmpQ + " ]; then rm -f " + target + " || rc=1; fi; rm -f " + tmpQ + " || rc=1; exit $rc"
	rmTmp = "rm -f " + tmpQ
	return
}

// parseFindEntries parses the NUL-separated output of the persistent-mode
// ListDir "find" command. Each record is "%y\t%s\t%f" — type char, size,
// filename — and records are separated by NUL so a filename containing a tab or
// newline cannot corrupt the listing. A trailing NUL yields an empty final
// record, which is skipped.
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
