package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"

	agents "github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/tracing"
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
// filesystem the exec/read/write tools do. Multiple files change atomically:
// new content is computed entirely in memory first — any hunk that can't be
// located aborts before a single file is touched — and if a write fails
// mid-commit the already-applied operations are rolled back from a snapshot.
func ApplyPatchTool(sb Sandbox, cfg FileToolConfig) *agents.Tool {
	cfg = cfg.withDefaults()
	return agents.NewTool(
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
			span, ctx := tracing.StartSpanFrom(ctx, "sandbox.apply_patch", tracing.SpanTypeSandbox, nil)
			defer span.Finish()

			// A bad patch (parse / locate / missing file) is returned as text so
			// the model can correct itself, matching read_file / write_file.
			out, err := applyPatch(ctx, sb, args.Patch)
			if err != nil {
				span.SetError(err.Error(), nil)
				return "apply_patch failed: " + err.Error(), nil
			}
			return out, nil
		},
	)
}

// applyPatchSem serializes ALL apply_patch commits process-wide: a rollback's
// RemoveFile could otherwise delete a file a concurrent patch just wrote. A
// channel rather than a mutex so acquisition honors ctx.Done(); one global
// gate rather than one per Sandbox so nothing is keyed on a closed sandbox.
var applyPatchSem = make(chan struct{}, 1)

// fsOp is a single filesystem mutation paired with its inverse, so a failed
// multi-file commit can be rolled back from the in-memory snapshot.
type fsOp struct {
	do   func() error
	undo func(context.Context) error
	desc string
	// undoOnError marks an op whose undo is safe to run when its OWN do()
	// failed: WriteFile is not atomic (a truncate-then-write can leave the
	// file half-written), so the failing op is restored from the snapshot too.
	// Never set on CreateExclusive or Rename ops — their failure leaves the
	// target untouched, and for a create it is usually someone ELSE's file.
	undoOnError bool
}

// parkedName is where apply_patch parks a file it cannot snapshot while the
// patch commits: a dotfile beside it with a random suffix, on the same
// filesystem and impossible for a model-chosen path to collide with.
func parkedName(p string) string {
	var b [6]byte
	_, _ = rand.Read(b[:]) // never fails as of Go 1.24
	return path.Join(path.Dir(p), ".apply-patch."+path.Base(p)+"."+hex.EncodeToString(b[:]))
}

// rbLabel names an op in a rollback-failure report. The second half of a move
// carries no summary desc (the move is summarized by its first op), so fall back
// to a generic label rather than reporting a bare ": error".
func rbLabel(op fsOp) string {
	if op.desc == "" {
		return "(move: remove source)"
	}
	return op.desc
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
	// parked are the temp names of deleted files too large to snapshot; they
	// are removed once every op has landed (spec §2.7s).
	var parked []string
	for _, e := range edits {
		switch e.op {
		case opAdd:
			p, body := e.path, []byte(e.addBody)
			// Adding over an existing file is an error, not an overwrite;
			// CreateExclusive makes that race-free, and its undo can never
			// delete a file another patch already had.
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
				undo: func(ctx context.Context) error { return sb.RemoveFile(ctx, p) },
				desc: "A " + p,
			})
		case opDelete:
			p := e.path
			orig, rerr := sb.ReadFile(ctx, p)
			if errors.Is(rerr, ErrReadLimitExceeded) {
				// Too large to snapshot in memory: park it beside itself.
				// Rollback renames it back, commit removes it (spec §2.7s).
				tmp := parkedName(p)
				ops = append(ops, fsOp{
					do:   func() error { return sb.Rename(ctx, p, tmp) },
					undo: func(ctx context.Context) error { return sb.Rename(ctx, tmp, p) },
					desc: "D " + p,
				})
				parked = append(parked, tmp)
				continue
			}
			if rerr != nil {
				return "", fmt.Errorf("delete %s: %w", p, rerr)
			}
			ops = append(ops, fsOp{
				do:          func() error { return sb.RemoveFile(ctx, p) },
				undo:        func(ctx context.Context) error { return sb.WriteFile(ctx, p, orig) },
				desc:        "D " + p,
				undoOnError: true,
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
				// Renaming onto an existing file is an error; the exclusive
				// create's undo only ever deletes the file this move created.
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
						undo: func(ctx context.Context) error { return sb.RemoveFile(ctx, dst) },
						desc: "M " + src + " -> " + dst,
					},
					fsOp{
						do:          func() error { return sb.RemoveFile(ctx, src) },
						undo:        func(ctx context.Context) error { return sb.WriteFile(ctx, src, snap) },
						undoOnError: true,
					},
				)
			} else {
				snap := orig
				ops = append(ops, fsOp{
					do:          func() error { return sb.WriteFile(ctx, p, newContent) },
					undo:        func(ctx context.Context) error { return sb.WriteFile(ctx, p, snap) },
					desc:        "U " + p,
					undoOnError: true,
				})
			}
		}
	}

	// Commit phase: apply in order; on any failure roll back what was applied.
	var done []fsOp
	var summary []string
	for _, op := range ops {
		if derr := op.do(); derr != nil {
			// Detached context: a do() that failed on the caller's cancellation
			// must not have its undo fail on it too.
			rbCtx, cancelRB := detachedCtx(ctx)
			var rbErrs []string
			// The failing op itself may have partially applied — see undoOnError.
			if op.undoOnError {
				if uerr := op.undo(rbCtx); uerr != nil {
					rbErrs = append(rbErrs, rbLabel(op)+": "+uerr.Error())
				}
			}
			for _, d := range slices.Backward(done) {
				if uerr := d.undo(rbCtx); uerr != nil {
					rbErrs = append(rbErrs, rbLabel(d)+": "+uerr.Error())
				}
			}
			cancelRB()
			if len(rbErrs) > 0 {
				return "", fmt.Errorf("apply failed (%w); rollback INCOMPLETE, these changes remain: %s", derr, strings.Join(rbErrs, "; "))
			}
			return "", fmt.Errorf("apply aborted and rolled back: %w", derr)
		}
		done = append(done, op)
		if op.desc != "" {
			summary = append(summary, op.desc)
		}
	}
	out := "applied patch:\n" + strings.Join(summary, "\n")
	if len(parked) > 0 {
		// Every op has landed, so nothing rolls back over the parked copies;
		// one that will not go is reported rather than silently left behind.
		rmCtx, cancel := detachedCtx(ctx)
		defer cancel()
		for _, tmp := range parked {
			if err := sb.RemoveFile(rmCtx, tmp); err != nil {
				out += "\nwarning: the parked copy " + tmp + " was not removed: " + err.Error()
			}
		}
	}
	return out, nil
}

// detachedCtx bounds cleanup that must outlive the caller's cancellation.
func detachedCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}
