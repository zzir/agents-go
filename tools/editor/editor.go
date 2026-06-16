// Package editor provides provider-agnostic file-editing tools for an agent,
// following the str_replace editor pattern (view / create / str_replace / insert)
// rather than a hosted diff tool. Every edit is plain string manipulation — no
// diff parser, no third-party dependency — and all reads and writes are confined
// to a working directory via os.Root, so "../" traversal and symlink escapes are
// rejected.
//
// The tools are ordinary FunctionTools, so they work against any model backend.
package editor

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/zzir/agents-go/agents"
)

// maxFileBytes caps a single read/edit so a huge file cannot blow up context.
const maxFileBytes = 1 << 20 // 1 MiB

// NewTools returns the file-editing tools (view_file, create_file, str_replace,
// insert_text) scoped to rootDir. Give them to an agent via Agent.Tools.
func NewTools(rootDir string) []agents.Tool {
	return []agents.Tool{
		viewTool(rootDir),
		createTool(rootDir),
		strReplaceTool(rootDir),
		insertTool(rootDir),
	}
}

func withRoot[T any](rootDir string, fn func(*os.Root) (T, error)) (T, error) {
	var zero T
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return zero, fmt.Errorf("opening working directory: %w", err)
	}
	defer root.Close()
	return fn(root)
}

func readFile(root *os.Root, path string) (string, error) {
	f, err := root.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func writeExisting(root *os.Root, path, content string) error {
	f, err := root.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(content)
	return err
}

type viewArgs struct {
	Path string `json:"path" jsonschema:"path to a file or directory under the working directory"`
}

func viewTool(rootDir string) agents.Tool {
	return agents.NewFunctionTool("view_file",
		"View a file's contents (with line numbers) or list a directory's entries.",
		func(_ context.Context, _ *agents.ToolContext, args viewArgs) (string, error) {
			return withRoot(rootDir, func(root *os.Root) (string, error) {
				f, err := root.Open(args.Path)
				if err != nil {
					return "", fmt.Errorf("view %q: %w", args.Path, err)
				}
				defer f.Close()
				info, err := f.Stat()
				if err != nil {
					return "", err
				}
				if info.IsDir() {
					entries, err := f.ReadDir(-1)
					if err != nil {
						return "", err
					}
					var b strings.Builder
					for _, e := range entries {
						name := e.Name()
						if e.IsDir() {
							name += "/"
						}
						b.WriteString(name + "\n")
					}
					return b.String(), nil
				}
				data, err := io.ReadAll(io.LimitReader(f, maxFileBytes))
				if err != nil {
					return "", err
				}
				return withLineNumbers(string(data)), nil
			})
		})
}

type createArgs struct {
	Path    string `json:"path" jsonschema:"path of the new file to create"`
	Content string `json:"content" jsonschema:"the file's contents"`
}

func createTool(rootDir string) agents.Tool {
	return agents.NewFunctionTool("create_file",
		"Create a new file with the given contents. Fails if the file already exists (edit it with str_replace instead).",
		func(_ context.Context, _ *agents.ToolContext, args createArgs) (string, error) {
			return withRoot(rootDir, func(root *os.Root) (string, error) {
				f, err := root.OpenFile(args.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
				if err != nil {
					return "", fmt.Errorf("create %q: %w", args.Path, err)
				}
				defer f.Close()
				if _, err := f.WriteString(args.Content); err != nil {
					return "", err
				}
				return "created " + args.Path, nil
			})
		})
}

type replaceArgs struct {
	Path   string `json:"path" jsonschema:"path of the file to edit"`
	OldStr string `json:"old_str" jsonschema:"exact text to replace; must occur exactly once in the file"`
	NewStr string `json:"new_str" jsonschema:"replacement text"`
}

func strReplaceTool(rootDir string) agents.Tool {
	return agents.NewFunctionTool("str_replace",
		"Replace a unique snippet of text in a file. old_str must match exactly once; include surrounding context to make it unique.",
		func(_ context.Context, _ *agents.ToolContext, args replaceArgs) (string, error) {
			return withRoot(rootDir, func(root *os.Root) (string, error) {
				content, err := readFile(root, args.Path)
				if err != nil {
					return "", fmt.Errorf("str_replace %q: %w", args.Path, err)
				}
				switch strings.Count(content, args.OldStr) {
				case 0:
					return "", fmt.Errorf("str_replace %q: old_str not found", args.Path)
				case 1:
					// ok
				default:
					n := strings.Count(content, args.OldStr)
					return "", fmt.Errorf("str_replace %q: old_str matched %d times; include more context to make it unique", args.Path, n)
				}
				if err := writeExisting(root, args.Path, strings.Replace(content, args.OldStr, args.NewStr, 1)); err != nil {
					return "", err
				}
				return "edited " + args.Path, nil
			})
		})
}

type insertArgs struct {
	Path string `json:"path" jsonschema:"path of the file to edit"`
	Line int    `json:"line" jsonschema:"1-based line number to insert after; 0 inserts at the start of the file"`
	Text string `json:"text" jsonschema:"text to insert (without a trailing newline)"`
}

func insertTool(rootDir string) agents.Tool {
	return agents.NewFunctionTool("insert_text",
		"Insert a line of text after the given line number in a file (line 0 inserts at the start).",
		func(_ context.Context, _ *agents.ToolContext, args insertArgs) (string, error) {
			return withRoot(rootDir, func(root *os.Root) (string, error) {
				content, err := readFile(root, args.Path)
				if err != nil {
					return "", fmt.Errorf("insert_text %q: %w", args.Path, err)
				}
				lines := strings.Split(content, "\n")
				if args.Line < 0 || args.Line > len(lines) {
					return "", fmt.Errorf("insert_text %q: line %d out of range (0-%d)", args.Path, args.Line, len(lines))
				}
				out := make([]string, 0, len(lines)+1)
				out = append(out, lines[:args.Line]...)
				out = append(out, args.Text)
				out = append(out, lines[args.Line:]...)
				if err := writeExisting(root, args.Path, strings.Join(out, "\n")); err != nil {
					return "", err
				}
				return "inserted into " + args.Path, nil
			})
		})
}

func withLineNumbers(content string) string {
	if content == "" {
		return "(empty file)"
	}
	lines := strings.Split(content, "\n")
	var b strings.Builder
	for i, ln := range lines {
		fmt.Fprintf(&b, "%d\t%s\n", i+1, ln)
	}
	return b.String()
}
