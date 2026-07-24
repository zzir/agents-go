package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	agents "github.com/zzir/agents-go/agents"
)

type applyPatchArgs struct {
	Patch string `json:"patch" jsonschema:"a Codex-style patch delimited by *** Begin Patch / *** End Patch"`
}

const applyPatchDesc = "Apply a patch to one or more files in the sandbox. Format:\n" +
	"*** Begin Patch\n" +
	"*** Update File: path/to/file\n" +
	"@@ optional line to anchor near\n" +
	" unchanged context line (leading space)\n" +
	"-line to remove\n" +
	"+line to add\n" +
	"*** Add File: path/to/new\n" +
	"+new file line\n" +
	"*** Delete File: path/to/old\n" +
	"*** End Patch\n" +
	"Prefix context lines with a space, removals with '-', additions with '+'. " +
	"Include enough surrounding context to locate each change. To rename, put " +
	"'*** Move to: new/path' right after '*** Update File:'. All files in one " +
	"patch apply atomically — a change that can't be located changes nothing."

// ApplyPatchTool returns a tool that applies a Codex-style patch to files in the
// sandbox. It edits through the Sandbox abstraction, so it targets the same
// filesystem the exec/read/write tools do (local dir, bind-mounted container, or
// remote host over SFTP). Multiple files change atomically: new content is
// computed entirely in memory first — any hunk that can't be located aborts
// before a single file is touched — and if a write fails mid-commit the
// already-applied operations are rolled back from an in-memory snapshot.
func ApplyPatchTool(sb Sandbox, cfg FileToolConfig) agents.Tool {
	cfg = cfg.withDefaults()
	return agents.NewFunctionTool(
		"apply_patch",
		applyPatchDesc,
		func(ctx context.Context, _ *agents.ToolContext, args applyPatchArgs) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, cfg.effectiveTimeout())
			defer cancel()
			// Serialize all apply_patch commits process-wide (see applyPatchSem).
			// Acquire via select so the timeout covers queueing time and a
			// cancelled run doesn't block behind a slow patch on another sandbox.
			select {
			case applyPatchSem <- struct{}{}:
				defer func() { <-applyPatchSem }()
			case <-ctx.Done():
				return "apply_patch failed: " + ctx.Err().Error(), nil
			}
			// A bad patch (parse / locate / missing file) is returned as text so
			// the model can correct itself, matching read_file / write_file.
			out, err := applyPatch(ctx, sb, args.Patch)
			if err != nil {
				return "apply_patch failed: " + err.Error(), nil
			}
			return out, nil
		},
	)
}

// applyPatchSem is a global cap-1 semaphore serializing ALL apply_patch commits
// process-wide. A rollback's RemoveFile could otherwise delete a file a
// concurrent patch just wrote on the same filesystem; exclusive-create only
// stops two Adds racing, not a rollback racing an Update. It is a channel (not a
// sync.Mutex) so acquisition honors ctx.Done() — a cancelled/timed-out run
// queued behind a slow patch unblocks instead of hanging. A single global gate
// (rather than one keyed on the Sandbox) avoids requiring Sandbox to be
// comparable and avoids leaking an entry per closed sandbox, at the cost of
// serializing applies across sandboxes — fine for a low-frequency op.
var applyPatchSem = make(chan struct{}, 1)

// fsOp is a single filesystem mutation paired with its inverse, so a failed
// multi-file commit can be rolled back from the in-memory snapshot.
type fsOp struct {
	do   func() error
	undo func() error
	desc string
}

// applyPatch runs the two-phase apply: plan (pure, in-memory) then commit
// (write, with rollback). Split out from the tool wrapper so it is unit-testable
// against any Sandbox.
func applyPatch(ctx context.Context, sb Sandbox, patch string) (string, error) {
	edits, err := parsePatch(patch)
	if err != nil {
		return "", err
	}

	// Plan phase: read originals, compute new content, build reversible ops.
	// Nothing is written yet, so a hunk that can't be located fails here, before
	// any file is touched (validation atomicity).
	var ops []fsOp
	for _, e := range edits {
		switch e.op {
		case opAdd:
			p, body := e.path, []byte(e.addBody)
			// Codex apply_patch semantics: adding over an existing file is an
			// error, not a silent overwrite. CreateExclusive is atomic (O_EXCL),
			// so concurrent adds of the same path can't both succeed, and because
			// it only creates when the file is absent, the undo (RemoveFile) can
			// never delete a file another patch already had.
			ops = append(ops, fsOp{
				do: func() error {
					if err := sb.CreateExclusive(ctx, p, body); err != nil {
						if errors.Is(err, fs.ErrExist) {
							return fmt.Errorf("add %s: file already exists", p)
						}
						return err
					}
					return nil
				},
				undo: func() error { return sb.RemoveFile(ctx, p) },
				desc: "A " + p,
			})
		case opDelete:
			p := e.path
			orig, rerr := sb.ReadFile(ctx, p)
			if rerr != nil {
				return "", fmt.Errorf("delete %s: %w", p, rerr)
			}
			ops = append(ops, fsOp{
				do:   func() error { return sb.RemoveFile(ctx, p) },
				undo: func() error { return sb.WriteFile(ctx, p, orig) },
				desc: "D " + p,
			})
		case opUpdate:
			p := e.path
			orig, rerr := sb.ReadFile(ctx, p)
			if rerr != nil {
				return "", fmt.Errorf("update %s: %w", p, rerr)
			}
			nc, aerr := applyHunks(string(orig), e.hunks)
			if aerr != nil {
				return "", fmt.Errorf("%s: %w", p, aerr)
			}
			newContent := []byte(nc)
			if e.movePath != "" && e.movePath != p {
				// Codex apply_patch semantics: renaming onto an existing file is
				// an error. CreateExclusive is atomic, so the move can't clobber a
				// destination that appeared concurrently, and its rollback (undo =
				// RemoveFile dst) only ever deletes the file this move created.
				dst, src, snap := e.movePath, p, orig
				ops = append(ops,
					fsOp{
						do: func() error {
							if err := sb.CreateExclusive(ctx, dst, newContent); err != nil {
								if errors.Is(err, fs.ErrExist) {
									return fmt.Errorf("move %s -> %s: destination already exists", src, dst)
								}
								return err
							}
							return nil
						},
						undo: func() error { return sb.RemoveFile(ctx, dst) },
						desc: "M " + src + " -> " + dst,
					},
					fsOp{
						do:   func() error { return sb.RemoveFile(ctx, src) },
						undo: func() error { return sb.WriteFile(ctx, src, snap) },
					},
				)
			} else {
				snap := orig
				ops = append(ops, fsOp{
					do:   func() error { return sb.WriteFile(ctx, p, newContent) },
					undo: func() error { return sb.WriteFile(ctx, p, snap) },
					desc: "U " + p,
				})
			}
		}
	}

	// Commit phase: apply in order; on any failure roll back what was applied.
	var done []fsOp
	var summary []string
	for _, op := range ops {
		if derr := op.do(); derr != nil {
			for i := len(done) - 1; i >= 0; i-- {
				_ = done[i].undo()
			}
			return "", fmt.Errorf("apply aborted and rolled back: %w", derr)
		}
		done = append(done, op)
		if op.desc != "" {
			summary = append(summary, op.desc)
		}
	}
	return "applied patch:\n" + strings.Join(summary, "\n"), nil
}
