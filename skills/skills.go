// Package skills implements the open Agent Skills format
// (https://github.com/agentskills/agentskills) for the agents SDK. A skill is a
// directory with a SKILL.md file (YAML frontmatter + Markdown instructions),
// optionally bundling scripts/, references/ and assets/.
//
// It maps the format's progressive disclosure onto plain SDK primitives, with no
// dependency on the sandbox:
//
//   - Discovery: RenderIndex builds an instructions section (name + description +
//     path for each skill) to add to an agent's instructions.
//   - Activation/Execution: ReadFileTool lets the model read a skill's SKILL.md
//     body, references and assets on demand, confined to the skills directory.
//
// It is a separate module so the YAML dependency stays out of the core SDK.
package skills

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/zzir/agents-go/agents"
)

// maxSkillFileBytes caps a single ReadFileTool read so a large bundled file
// cannot blow up the model's context.
const maxSkillFileBytes = 256 << 10 // 256 KiB

// nameRe enforces the spec's name rules: lowercase alphanumerics in
// hyphen-separated groups, so it cannot start/end with a hyphen or contain "--".
var nameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Skill is a parsed SKILL.md: its frontmatter metadata and on-disk location. The
// Markdown body is not loaded here — the model reads it via ReadFileTool during
// activation (progressive disclosure).
type Skill struct {
	Name          string
	Description   string
	License       string
	Compatibility string
	Metadata      map[string]string
	AllowedTools  []string
	// Dir and Path are relative to the skills root passed to Load, so Path is
	// exactly what the model passes to ReadFileTool (e.g. "pdf/SKILL.md").
	Dir  string
	Path string
}

type frontmatter struct {
	Name          string            `yaml:"name"`
	Description   string            `yaml:"description"`
	License       string            `yaml:"license"`
	Compatibility string            `yaml:"compatibility"`
	Metadata      map[string]string `yaml:"metadata"`
	AllowedTools  string            `yaml:"allowed-tools"`
}

// Load scans rootDir for immediate subdirectories containing a SKILL.md, parses
// each, and returns the skills sorted by name. Subdirectories without a SKILL.md
// are skipped; a present-but-invalid SKILL.md is an error.
func Load(rootDir string) ([]Skill, error) {
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		return nil, fmt.Errorf("skills: reading %s: %w", rootDir, err)
	}
	var out []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mdPath := path.Join(e.Name(), "SKILL.md")
		data, err := os.ReadFile(path.Join(rootDir, e.Name(), "SKILL.md"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("skills: reading %s: %w", mdPath, err)
		}
		fm, err := parseFrontmatter(data)
		if err != nil {
			return nil, fmt.Errorf("skills: %s: %w", mdPath, err)
		}
		if err := validate(fm, e.Name()); err != nil {
			return nil, fmt.Errorf("skills: %s: %w", mdPath, err)
		}
		out = append(out, Skill{
			Name:          fm.Name,
			Description:   fm.Description,
			License:       fm.License,
			Compatibility: fm.Compatibility,
			Metadata:      fm.Metadata,
			AllowedTools:  strings.Fields(fm.AllowedTools),
			Dir:           e.Name(),
			Path:          mdPath,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// parseFrontmatter splits a SKILL.md into its YAML frontmatter and decodes it.
func parseFrontmatter(content []byte) (frontmatter, error) {
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return frontmatter{}, fmt.Errorf("must start with YAML frontmatter (---)")
	}
	rest := text[len("---\n"):]
	yamlPart, _, found := strings.Cut(rest, "\n---")
	if !found {
		return frontmatter{}, fmt.Errorf("unterminated frontmatter (missing closing ---)")
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(yamlPart), &fm); err != nil {
		return frontmatter{}, fmt.Errorf("parsing frontmatter: %w", err)
	}
	return fm, nil
}

func validate(fm frontmatter, dirName string) error {
	if fm.Name == "" {
		return fmt.Errorf("frontmatter is missing required field 'name'")
	}
	if len(fm.Name) > 64 || !nameRe.MatchString(fm.Name) {
		return fmt.Errorf("invalid name %q (1-64 chars, lowercase alphanumerics and single hyphens)", fm.Name)
	}
	if fm.Name != dirName {
		return fmt.Errorf("name %q must match its directory name %q", fm.Name, dirName)
	}
	if strings.TrimSpace(fm.Description) == "" {
		return fmt.Errorf("frontmatter is missing required field 'description'")
	}
	if len(fm.Description) > 1024 {
		return fmt.Errorf("description exceeds 1024 characters")
	}
	if len(fm.Compatibility) > 500 {
		return fmt.Errorf("compatibility exceeds 500 characters")
	}
	return nil
}

// RenderIndex builds the discovery section to append to an agent's instructions:
// each skill's name, description and path, plus guidance on using the
// read_skill_file tool to load full instructions on demand. Returns "" when
// there are no skills.
func RenderIndex(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Available skills\n\n")
	b.WriteString("A skill is a set of instructions stored in a SKILL.md file. ")
	b.WriteString("When the user names a skill or a task matches one's description below, ")
	b.WriteString("read its SKILL.md with the read_skill_file tool, then follow it.\n\n")
	for _, s := range skills {
		fmt.Fprintf(&b, "- **%s** (`%s`): %s\n", s.Name, s.Path, s.Description)
	}
	b.WriteString("\n## How to use skills\n")
	b.WriteString("- Activate a skill only when the task matches its description; otherwise ignore it.\n")
	b.WriteString("- Read its SKILL.md first, then load only the referenced files (references/, scripts/, assets/) you actually need.\n")
	b.WriteString("- Do not carry a skill across turns unless it is relevant again.\n")
	return b.String()
}

type readArgs struct {
	Path string `json:"path" jsonschema:"path to a file under the skills directory, e.g. pdf/SKILL.md or pdf/references/api.md"`
}

// ReadFileTool returns a function tool named "read_skill_file" that lets the
// model read a file under rootDir (a SKILL.md body, reference, asset, or script).
// Reads are confined to rootDir via os.Root, so "../" traversal and symlink
// escapes are rejected. Each read is capped at 256 KiB.
func ReadFileTool(rootDir string) agents.Tool {
	return agents.NewFunctionTool("read_skill_file",
		"Read a file from the skills directory (a SKILL.md body, reference, asset, or script) to follow a skill's instructions.",
		func(ctx context.Context, _ *agents.ToolContext, args readArgs) (string, error) {
			root, err := os.OpenRoot(rootDir)
			if err != nil {
				return "", fmt.Errorf("opening skills root: %w", err)
			}
			defer root.Close()
			f, err := root.Open(args.Path)
			if err != nil {
				return "", fmt.Errorf("reading %q: %w", args.Path, err)
			}
			defer f.Close()
			data, err := io.ReadAll(io.LimitReader(f, maxSkillFileBytes))
			if err != nil {
				return "", fmt.Errorf("reading %q: %w", args.Path, err)
			}
			return string(data), nil
		})
}
