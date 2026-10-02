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
	// MaxOutputBytes caps what one call returns to the model: a read_file
	// page ends there, list_files output is truncated. Defaults to 8192.
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

// fileToolError renders a backend error for the model without leaking host or
// remote absolute paths: only the operation, the requested path and the kind survive.
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
	Path   string `json:"path"   jsonschema:"file path to read (absolute, or relative to the working directory)"`
	Offset int    `json:"offset" jsonschema:"first line to return, counting from 1; 0 starts at the first line"`
	Limit  int    `json:"limit"  jsonschema:"how many lines to return; 0 reads to the end, as far as the output limit allows"`
}

// ReadFileTool returns a tool that reads a file from the sandbox, a page of
// numbered lines at a time — see spec §2.7t.
func ReadFileTool(sb Sandbox, cfg FileToolConfig) *agents.Tool {
	cfg = cfg.withDefaults()
	t := agents.NewTool(
		"read_file",
		"Read a file in the sandbox as numbered lines. The first line says which lines came back of how many; when more remain, call again with offset set to the next line.",
		func(ctx context.Context, _ *agents.ToolContext, args readFileArgs) (string, error) {
			if args.Offset < 0 || args.Limit < 0 {
				return fmt.Sprintf("error: read %s: offset and limit must not be negative", args.Path), nil
			}
			ctx, cancel := context.WithTimeout(ctx, cfg.effectiveTimeout())
			defer cancel()
			data, err := sb.ReadFile(ctx, args.Path)
			if err != nil {
				return fileToolError("read", args.Path, err), nil
			}
			return pageLines(string(data), args.Offset, args.Limit, cfg.MaxOutputBytes), nil
		},
	)
	t.ReadOnly = true
	return t
}

// pageLines renders lines [offset, offset+limit) of content, numbered, under a
// header naming the page; offset 0 is 1 and limit 0 is to the end. The page
// stops early at the byte budget, header included, and says how to continue.
func pageLines(content string, offset, limit, budget int) string {
	if content == "" {
		return "(empty file)"
	}
	const headerReserve = 96
	if budget > 2*headerReserve {
		budget -= headerReserve
	}
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	total := len(lines)
	first := max(offset, 1)
	if first > total {
		return fmt.Sprintf("lines %d-%d of %d: offset %d is past the end", 0, 0, total, offset)
	}
	last := total
	if limit > 0 {
		last = min(first-1+limit, total)
	}
	width := len(fmt.Sprint(last))
	var b strings.Builder
	n := first
	for ; n <= last; n++ {
		row := fmt.Sprintf("%*d\t%s\n", width, n, lines[n-1])
		// The first line always comes back, cut if it alone is over budget.
		if n == first && len(row) > budget {
			b.WriteString(truncateWithInfo(row, budget))
			n++
			break
		}
		if b.Len()+len(row) > budget {
			break
		}
		b.WriteString(row)
	}
	shown := n - 1
	header := fmt.Sprintf("lines %d-%d of %d", first, shown, total)
	if shown < last {
		header += fmt.Sprintf(" (stopped at the output limit; continue with offset=%d)", shown+1)
	} else if shown < total {
		header += fmt.Sprintf(" (continue with offset=%d)", shown+1)
	}
	return header + "\n" + b.String()
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
