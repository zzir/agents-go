# Skills

The `github.com/zzir/agents-go/skills` module implements the `SKILL.md` document format of the open [Agent Skills](https://github.com/agentskills/agentskills) spec: a skill is one Markdown document with YAML frontmatter carrying its `name` and `description`. It is a **separate module** so the YAML dependency stays out of the core SDK.

Skills map onto plain SDK primitives — no sandbox required — following the format's progressive disclosure, with storage owned by the caller:

| Stage | Agent Skills | This module |
|---|---|---|
| Discovery | load name + description | `Parse` per document, `RenderIndex` → agent instructions |
| Activation / execution | read the full document | a `read_skill` function tool you provide (return content by name) |

## Usage

```go
import "github.com/zzir/agents-go/skills"

data, _ := os.ReadFile("skills/pdf/SKILL.md")
sk, err := skills.Parse(data) // validate frontmatter; sk.Name, sk.Description
content := map[string]string{sk.Name: string(data)}

readSkill := agents.NewTool("read_skill",
    "Read a skill's full SKILL.md instructions by name.",
    func(_ context.Context, _ *agents.ToolContext, args struct {
        Name string `json:"name"`
    }) (string, error) {
        doc, ok := content[args.Name]
        if !ok {
            return "", fmt.Errorf("no skill named %q", args.Name)
        }
        return doc, nil
    })

agent := &agents.Agent{
    Name: "assistant",
    Instructions: func(ctx context.Context, rc *agents.RunContext, a *agents.Agent) (string, error) {
        return base + "\n" + skills.RenderIndex([]skills.Skill{sk}), nil // discovery
    },
    Tools: []*agents.Tool{readSkill}, // activation
}
```

- **`Parse(data)`** validates one SKILL.md document (`name` 1–64 lowercase letters/digits/hyphens; `description` required, ≤1024 chars; other frontmatter keys such as `license`/`compatibility`/`metadata`/`allowed-tools` are ignored) and returns its metadata.
- **`RenderIndex(skills)`** builds the discovery section (each skill's name and description, plus how-to-use guidance) to add to an agent's instructions. The skill body is **not** inlined — progressive disclosure keeps the context footprint small. The rendered text names a `read_skill` tool, so pair it with a function tool of that name that takes a skill name and returns the document.

Where the documents live — files, a database, an embedded FS — is the caller's decision; the module never touches storage. The agents-server workbench, for example, stores skills as rows and serves `read_skill` from its database.

A runnable example is in `skills/example`.
