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
	"io/fs"
	"os"
	"path"
	"path/filepath"
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
//
// Only the fields something consumes are parsed: Name and Description feed the
// index the model reads, Dir/Path feed ReadFileTool. Other frontmatter keys
// (license, compatibility, metadata, allowed-tools) are ignored — parsing them
// into fields nothing reads would imply an enforcement that does not exist.
type Skill struct {
	Name        string
	Description string
	// Dir and Path are relative to the skills root passed to Load, so Path is
	// exactly what the model passes to ReadFileTool (e.g. "pdf/SKILL.md").
	Dir  string
	Path string
}

type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// buildSkill parses a SKILL.md's content into a Skill, validating that its
// frontmatter name matches relDir's own last path component. relDir must
// already be slash-separated (callers on Windows should normalize via
// filepath.ToSlash), since it becomes Path exactly as passed to ReadFileTool.
func buildSkill(relDir string, data []byte) (Skill, error) {
	fm, err := parseFrontmatter(data)
	if err != nil {
		return Skill{}, err
	}
	if err := validate(fm, path.Base(relDir)); err != nil {
		return Skill{}, err
	}
	return Skill{
		Name:        fm.Name,
		Description: fm.Description,
		Dir:         relDir,
		Path:        path.Join(relDir, "SKILL.md"),
	}, nil
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
		sk, err := buildSkill(e.Name(), data)
		if err != nil {
			return nil, fmt.Errorf("skills: %s: %w", mdPath, err)
		}
		out = append(out, sk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// LoadRecursive discovers skills under rootDir at any depth — a skill
// directly under rootDir ("name/SKILL.md"), or nested one or more levels
// deeper, as a cloned multi-skill repository typically is
// ("some-repo/name/SKILL.md"). Dir and Path are still relative to rootDir
// regardless of depth, so ReadFileTool resolves them the same way Load's
// results do. Results are sorted by Path rather than Name, since two
// differently-nested skills (e.g. from separate cloned repos) can share a name.
func LoadRecursive(rootDir string) ([]Skill, error) {
	var out []Skill
	err := filepath.WalkDir(rootDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable root is a caller error (typo'd path, missing
			// mount) and must not silently yield zero skills; unreadable
			// entries below it are skipped to keep walking.
			if p == rootDir {
				return fmt.Errorf("skills: reading root %s: %w", rootDir, err)
			}
			return nil // skip unreadable entries, keep walking
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "SKILL.md" {
			return nil
		}
		relDir, err := filepath.Rel(rootDir, filepath.Dir(p))
		if err != nil {
			return nil //nolint:nilerr // p came from walking rootDir, so this shouldn't happen
		}
		relDir = filepath.ToSlash(relDir)
		data, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("skills: reading %s: %w", p, err)
		}
		sk, err := buildSkill(relDir, data)
		if err != nil {
			return fmt.Errorf("skills: %s: %w", path.Join(relDir, "SKILL.md"), err)
		}
		out = append(out, sk)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
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
// escapes are rejected. Each read is capped at 256 KiB; a capped read ends with
// an explicit truncation marker so the model never mistakes a partial file for
// the whole thing.
func ReadFileTool(rootDir string) *agents.FunctionTool {
	return agents.NewFunctionTool("read_skill_file",
		"Read a file from the skills directory (a SKILL.md body, reference, asset, or script) to follow a skill's instructions.",
		func(_ context.Context, _ *agents.ToolContext, args readArgs) (string, error) {
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
			// Read one byte past the cap to detect truncation reliably.
			data, err := io.ReadAll(io.LimitReader(f, maxSkillFileBytes+1))
			if err != nil {
				return "", fmt.Errorf("reading %q: %w", args.Path, err)
			}
			if len(data) > maxSkillFileBytes {
				note := fmt.Sprintf("\n[... truncated: showing first %d bytes]", maxSkillFileBytes)
				if info, statErr := f.Stat(); statErr == nil {
					note = fmt.Sprintf("\n[... truncated: file is %d bytes, showing first %d bytes]", info.Size(), maxSkillFileBytes)
				}
				return string(data[:maxSkillFileBytes]) + note, nil
			}
			return string(data), nil
		})
}
