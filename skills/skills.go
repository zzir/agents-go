// Package skills implements the SKILL.md document format of the open Agent
// Skills spec (https://github.com/agentskills/agentskills) for the agents SDK:
// a skill is one Markdown document with YAML frontmatter carrying its name and
// description.
//
// The module maps the format's progressive disclosure onto plain SDK
// primitives and leaves storage to the caller:
//
//   - Parse validates a SKILL.md and returns its metadata.
//   - RenderIndex builds a discovery section (name + description per skill) to
//     add to an agent's instructions; it tells the model to fetch a skill's
//     full document through a tool named read_skill, which the caller provides
//     (a function tool that returns the skill's content by name).
//
// It is a separate module so the YAML dependency stays out of the core SDK.
package skills

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// nameRe enforces the spec's name rules: lowercase alphanumerics in
// hyphen-separated groups, so it cannot start/end with a hyphen or contain "--".
var nameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// closeRe matches the closing frontmatter delimiter: "---" alone on its line,
// so a line that merely starts with "---" (inside a quoted value) does not end it.
var closeRe = regexp.MustCompile(`(?m)^---$`)

// Skill is a parsed SKILL.md's frontmatter metadata: what the index shows and
// what a read_skill tool is keyed by. The body stays with the caller — the
// model reads it on demand during activation (progressive disclosure).
//
// Only the fields something consumes are parsed. Other frontmatter keys
// (license, compatibility, metadata, allowed-tools) are ignored — parsing them
// into fields nothing reads would imply an enforcement that does not exist.
type Skill struct {
	Name        string
	Description string
}

type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// Parse validates data as a SKILL.md document (YAML frontmatter + Markdown
// instructions) and returns its metadata. The name must be 1-64 chars of
// lowercase alphanumerics and single hyphens; the description is required and
// capped at 1024 chars.
func Parse(data []byte) (Skill, error) {
	fm, err := parseFrontmatter(data)
	if err != nil {
		return Skill{}, err
	}
	if err := validate(fm); err != nil {
		return Skill{}, err
	}
	return Skill{Name: fm.Name, Description: fm.Description}, nil
}

// parseFrontmatter splits a SKILL.md into its YAML frontmatter and decodes it.
func parseFrontmatter(content []byte) (frontmatter, error) {
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return frontmatter{}, fmt.Errorf("must start with YAML frontmatter (---)")
	}
	rest := text[len("---\n"):]
	loc := closeRe.FindStringIndex(rest)
	if loc == nil {
		return frontmatter{}, fmt.Errorf("unterminated frontmatter (missing closing ---)")
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(rest[:loc[0]]), &fm); err != nil {
		return frontmatter{}, fmt.Errorf("parsing frontmatter: %w", err)
	}
	return fm, nil
}

func validate(fm frontmatter) error {
	if fm.Name == "" {
		return fmt.Errorf("frontmatter is missing required field 'name'")
	}
	if len(fm.Name) > 64 || !nameRe.MatchString(fm.Name) {
		return fmt.Errorf("invalid name %q (1-64 chars, lowercase alphanumerics and single hyphens)", fm.Name)
	}
	if strings.TrimSpace(fm.Description) == "" {
		return fmt.Errorf("frontmatter is missing required field 'description'")
	}
	if utf8.RuneCountInString(fm.Description) > 1024 {
		return fmt.Errorf("description exceeds 1024 characters")
	}
	return nil
}

// RenderIndex builds the discovery section to append to an agent's
// instructions: each skill's name and description, plus guidance on loading a
// skill's full document on demand. The wording names a read_skill tool — the
// caller must give the agent a function tool with that name that takes a
// skill name and returns its SKILL.md content. Returns "" when there are no
// skills.
func RenderIndex(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Available skills\n\n")
	b.WriteString("A skill is a set of instructions for a specific kind of task. ")
	b.WriteString("When the user names a skill or a task matches one's description below, ")
	b.WriteString("read its full instructions with the read_skill tool, then follow them.\n\n")
	for _, s := range skills {
		fmt.Fprintf(&b, "- **%s**: %s\n", s.Name, s.Description)
	}
	b.WriteString("\n## How to use skills\n")
	b.WriteString("- Activate a skill only when the task matches its description; otherwise ignore it.\n")
	b.WriteString("- Read its instructions first (read_skill), then follow them for the task at hand.\n")
	b.WriteString("- Do not carry a skill across turns unless it is relevant again.\n")
	return b.String()
}
