# Skills

The `github.com/zzir/agents-go/skills` module implements the open [Agent Skills](https://github.com/agentskills/agentskills) format: a skill is a directory with a `SKILL.md` (YAML frontmatter + Markdown instructions), optionally bundling `scripts/`, `references/` and `assets/`. It is a **separate module** so the YAML dependency stays out of the core SDK.

Skills map onto plain SDK primitives — no sandbox required — following the format's progressive disclosure:

| Stage | Agent Skills | This module |
|---|---|---|
| Discovery | load name + description | `RenderIndex` → agent instructions |
| Activation | read `SKILL.md` | `ReadFileTool` (model reads on demand) |
| Execution | run scripts / read refs | `ReadFileTool`; scripts via `sandbox.CodeTool` (optional) |

## Usage

```go
import "github.com/zzir/agents-go/skills"

loaded, _ := skills.Load("./skills") // scan dirs, parse each SKILL.md frontmatter

agent := &agents.Agent{
    Name: "assistant",
    Instructions: agents.InstructionsFunc(func(ctx context.Context, rc *agents.RunContext, a *agents.Agent) (string, error) {
        return base + "\n" + skills.RenderIndex(loaded), nil // discovery
    }),
    Tools: []*agents.Tool{skills.ReadFileTool("./skills")},   // activation / execution
}
```

- **`Load(dir)`** scans immediate subdirectories for a `SKILL.md`, validates the frontmatter (`name` 1–64 lowercase letters/digits/hyphens matching the directory name; `description` ≤1024 chars; optional `license`/`compatibility`/`metadata`/`allowed-tools`), and returns skills sorted by name. A directory without a `SKILL.md` is skipped; a present-but-invalid one is an error.
- **`LoadRecursive(dir)`** is `Load`, but scans to any depth — useful when `dir` bundles multiple cloned skill repositories, each contributing its own subdirectory of skills (e.g. `dir/some-repo/pdf/SKILL.md`). Each skill's `Dir`/`Path` stay relative to `dir` regardless of nesting depth, and results are sorted by `Path` (not `Name`, since differently-nested skills can share a name).
- **`RenderIndex(skills)`** builds the discovery section (each skill's name, description and path, plus how-to-use guidance) to add to an agent's instructions. The skill body is **not** loaded here — progressive disclosure keeps the context footprint small.
- **`ReadFileTool(dir)`** is a `read_skill_file` function tool that lets the model read a file under `dir` on demand (a `SKILL.md` body, reference, asset, or script). Reads are confined to `dir` via Go's `os.Root`, so `../` traversal and symlink escapes are rejected, and each read is capped at 256 KiB.

## No sandbox dependency

Skills are implemented directly on `Instructions` plus a function tool, against the provider-agnostic Agent Skills standard. Binding them to a sandbox instead would make loading a `SKILL.md` require a container.

A runnable example is in `skills/example`.
