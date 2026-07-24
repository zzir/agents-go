package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
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
			// error, not a silent overwrite. Without this the undo (RemoveFile)
			// on a later rollback would delete the clobbered original outright.
			exists, terr := targetExists(ctx, sb, p)
			if terr != nil {
				return "", fmt.Errorf("add %s: %w", p, terr)
			}
			if exists {
				return "", fmt.Errorf("add %s: file already exists", p)
			}
			ops = append(ops, fsOp{
				do:   func() error { return sb.WriteFile(ctx, p, body) },
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
				// an error. Otherwise the two-op move would clobber the
				// destination and its rollback (undo = RemoveFile dst) would
				// delete the pre-existing file outright.
				exists, terr := targetExists(ctx, sb, e.movePath)
				if terr != nil {
					return "", fmt.Errorf("move %s -> %s: %w", p, e.movePath, terr)
				}
				if exists {
					return "", fmt.Errorf("move %s -> %s: destination already exists", p, e.movePath)
				}
				dst, src, snap := e.movePath, p, orig
				ops = append(ops,
					fsOp{
						do:   func() error { return sb.WriteFile(ctx, dst, newContent) },
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

// targetExists reports whether p already names an entry in the sandbox. It
// lists p's parent directory rather than reading p, so a large file (which
// ReadFile would reject with ErrReadLimitExceeded) is still detected and a
// directory is not slurped into memory.
//
// A missing parent (fs.ErrNotExist) means p can't exist yet — a legitimate add
// into a new directory — so it returns (false, nil). Any other ListDir error
// (permission denied, a transient backend failure) is returned so the caller
// fails closed instead of treating an unreadable parent as "absent" and
// clobbering a file that may well be there.
//
// This closes the fail-open hole but not the check-then-write TOCTOU: two
// concurrent adds of the same path can both observe "absent". Fully preventing
// that needs an exclusive-create/rename primitive on the Sandbox interface.
func targetExists(ctx context.Context, sb Sandbox, p string) (bool, error) {
	entries, err := sb.ListDir(ctx, path.Dir(p))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	base := path.Base(p)
	for _, e := range entries {
		if e.Name == base {
			return true, nil
		}
	}
	return false, nil
}
