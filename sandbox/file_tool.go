package sandbox

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	agents "github.com/zzir/agents-go/agents"
)

// FileToolConfig configures the file operation tools.
type FileToolConfig struct {
	// Timeout bounds each file operation. Zero means DefaultTimeout.
	Timeout time.Duration
	// MaxOutputBytes truncates the content returned to the model. Defaults
	// to CodeToolConfig's default (8192).
	MaxOutputBytes int
}

func (c FileToolConfig) withDefaults() FileToolConfig {
	if c.MaxOutputBytes <= 0 {
		c.MaxOutputBytes = 8192
	}
	return c
}

func (c FileToolConfig) effectiveTimeout() time.Duration {
	if c.Timeout <= 0 {
		return DefaultTimeout
	}
	return c.Timeout
}

// fileToolError renders a backend error for the model without leaking
// host/remote absolute paths: only the operation, the path the model asked
// for and the error kind survive. Raw backend errors routinely embed the
// host-side working directory (a *fs.PathError, a daemon's message), which
// the model has no business seeing.
func fileToolError(op, reqPath string, err error) string {
	var kind string
	pathErr, isPathErr := errors.AsType[*fs.PathError](err)
	switch {
	case errors.Is(err, ErrReadLimitExceeded):
		kind = "file exceeds read limit"
	case errors.Is(err, ErrNoWorkDir):
		kind = "no persistent working directory configured"
	case errors.Is(err, ErrOutsideWorkDir):
		kind = "outside the working directory"
	case errors.Is(err, fs.ErrNotExist):
		kind = "not found"
	case errors.Is(err, fs.ErrPermission):
		kind = "permission denied"
	case errors.Is(err, fs.ErrExist):
		kind = "already exists"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		kind = "timed out"
	case isPathErr:
		// Keep the underlying errno text ("is a directory", ...) which carries
		// no path, and drop the path-bearing wrapper.
		kind = pathErr.Err.Error()
	default:
		kind = "operation failed"
	}
	return fmt.Sprintf("error: %s %s: %s", op, reqPath, kind)
}

// FileTools returns read_file, write_file and list_files tools backed by the
// given sandbox. These complement CodeTool by giving the model structured file
// I/O instead of piping everything through shell commands.
func FileTools(sb Sandbox, cfg FileToolConfig) []*agents.Tool {
	cfg = cfg.withDefaults()
	return []*agents.Tool{
		ReadFileTool(sb, cfg),
		WriteFileTool(sb, cfg),
		ListFilesTool(sb, cfg),
	}
}

type readFileArgs struct {
	Path string `json:"path" jsonschema:"file path to read (absolute, or relative to the working directory)"`
}

// ReadFileTool returns a tool that reads a file from the sandbox. The file
// content is returned as a string, truncated to MaxOutputBytes.
func ReadFileTool(sb Sandbox, cfg FileToolConfig) *agents.Tool {
	cfg = cfg.withDefaults()
	t := agents.NewTool(
		"read_file",
		"Read the contents of a file in the sandbox. Returns the file content as text.",
		func(ctx context.Context, _ *agents.ToolContext, args readFileArgs) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, cfg.effectiveTimeout())
			defer cancel()
			data, err := sb.ReadFile(ctx, args.Path)
			if err != nil {
				return fileToolError("read", args.Path, err), nil
			}
			return truncateWithInfo(string(data), cfg.MaxOutputBytes), nil
		},
	)
	t.ReadOnly = true
	return t
}

type writeFileArgs struct {
	Path    string `json:"path"    jsonschema:"file path to write (absolute, or relative to the working directory)"`
	Content string `json:"content" jsonschema:"file content to write"`
}

// WriteFileTool returns a tool that writes a file into the sandbox.
func WriteFileTool(sb Sandbox, cfg FileToolConfig) *agents.Tool {
	cfg = cfg.withDefaults()
	return agents.NewTool(
		"write_file",
		"Write content to a file in the sandbox. Creates parent directories as needed. Overwrites any existing file.",
		func(ctx context.Context, _ *agents.ToolContext, args writeFileArgs) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, cfg.effectiveTimeout())
			defer cancel()
			if err := sb.WriteFile(ctx, args.Path, []byte(args.Content)); err != nil {
				return fileToolError("write", args.Path, err), nil
			}
			return fmt.Sprintf("wrote %d bytes to %s", len(args.Content), args.Path), nil
		},
	)
}

type listFilesArgs struct {
	Path string `json:"path" jsonschema:"directory path to list; empty uses the working directory"`
}

// ListFilesTool returns a tool that lists files in a sandbox directory, sorted
// by name so every backend answers in one order.
func ListFilesTool(sb Sandbox, cfg FileToolConfig) *agents.Tool {
	cfg = cfg.withDefaults()
	t := agents.NewTool(
		"list_files",
		"List files and directories in the sandbox. Returns name, size and type for each entry.",
		func(ctx context.Context, _ *agents.ToolContext, args listFilesArgs) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, cfg.effectiveTimeout())
			defer cancel()
			dir := args.Path
			dir = cmp.Or(dir, ".")
			entries, err := sb.ListDir(ctx, dir)
			if err != nil {
				return fileToolError("list", dir, err), nil
			}
			slices.SortFunc(entries, func(a, b DirEntry) int { return strings.Compare(a.Name, b.Name) })
			var b strings.Builder
			for _, e := range entries {
				typ := "file"
				if e.IsDir {
					typ = "dir "
				}
				fmt.Fprintf(&b, "%s %8d  %s\n", typ, e.Size, e.Name)
			}
			return truncateWithInfo(b.String(), cfg.MaxOutputBytes), nil
		},
	)
	t.ReadOnly = true
	return t
}
