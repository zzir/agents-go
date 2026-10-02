# Upstream Watch

`agents-go` does **not** track [openai-agents-python](https://github.com/openai/openai-agents-python).
It began as a port of that project and shares its core concepts, but behavior is
now specified in [spec.md](../reference/spec.md) and the two evolve independently.

We still read upstream releases, because good ideas are worth taking. This file
records what we looked at and what we decided. **There is no obligation to match.**

## Process

After each upstream **minor** release:

1. Read the changelog and the diff of `src/agents/run.py`, `run_state.py`,
   `run_internal/`, `tool.py`, `memory/`, `sandbox/` and `retry.py`.
2. For each notable change, add a row below with one of:
   - **ported** — implemented here; link the PR
   - **adapted** — implemented differently; say how and why
   - **declined** — not doing it; say why (if it is a permanent non-goal, also
     add it to [scope §1.2 or §3](scope.md))
   - **deferred** — worth doing, nobody has needed it yet
3. Anything that becomes a design invariant goes into `spec.md` in the same change.

Other sources worth the same treatment when something notable lands — a row
when it bears on this project, not a review of every release:

- [microsoft/agent-framework-go](https://github.com/microsoft/agent-framework-go)
- [earendil-works/pi](https://github.com/earendil-works/pi)
- [openai/codex](https://github.com/openai/codex)
- the [Claude Code changelog](https://github.com/anthropics/claude-code/blob/main/CHANGELOG.md)
- the [OpenAI API changelog](https://developers.openai.com/api/docs/changelog)

## Log

| Date | Source | Version | Change | Decision | Notes |
|---|---|---|---|---|---|
| — | openai-agents-python | v0.18.2 | *(baseline)* | — | Last version this project tracked. Concept mapping and the differences as of that point are in [migration_from_python.md](migration_from_python.md). |
| 2026-04-15 | openai-agents-python | v0.14.0 | Sandbox agents (`SandboxAgent`, manifests, unix-local, Docker and hosted providers; predates the baseline) | declined | No agent subtype: a `Sandbox` behind tools any agent can hold ([migration](migration_from_python.md#api-mapping)) |
| 2026-04-15 | openai-agents-python | v0.14.0 | Workspace tar snapshot and rehydrate | deferred | Candidate: an import beside `ExportTar`, for a fork that copies files |
| 2026-04-15 | openai-agents-python | v0.14.0 | Manifest seeding, `view_image` | deferred | No consumer |
| 2026-06-03 | OpenAI API | deprecations | Reusable prompt objects and `v1/prompts` shut down 2026-11-30 | adapted | `Agent.Prompt` deprecated (86b11158); it leaves in the next breaking minor (decisions §5.3) |
| 2026-07-17 | openai-agents-python | v0.18.3 | Configurable task and turn spans | deferred | When the UI groups traces by turn |
| 2026-07-17 | openai-agents-python | v0.18.3 | Strict schema conversion bounds `$ref` expansion | deferred | Candidate: `EnsureStrictJSONSchema` unravels a `$ref` with sibling keys without a bound — a chain twenty definitions deep takes about a second, and each level doubles it |
| 2026-07-27 | openai-agents-python | v0.19.0 | Programmatic Tool Calling | declined | A hosted executor (scope §1.2, provider-hosted tools; decisions §5.4) |
| 2026-07-27 | openai-agents-python | v0.19.0 | Decorators, configuration coercion | declined | Python-specific |
| 2026-07-27 | openai-agents-python | v0.19.0 | Diagnostics redacted in logs | deferred | Candidate: one pass over error strings in `mcp`, `sandbox/docker` (SSH URLs) and `sandbox/e2b` |
| 2026-08-05 | openai-agents-python | v0.19.4 | Final turn saved after output guardrails | ported | Already present: spec §2.5 |
| 2026-08-05 | openai-agents-python | v0.19.4 | A failed concurrent call cancels its siblings | ported | Already present: spec §2.2 |
| 2026-08-11 | openai-agents-python | v0.20.0 | Default model `gpt-5.6-luna` | declined | scope §3: no built-in default model |
| 2026-08-11 | openai-agents-python | v0.20.0 | Mount-credential acknowledgement | declined | The mount mechanism it guards is not here |
| 2026-08-11 | openai-agents-python | v0.20.0 | A retry of a stateful request fails closed unless the application approves the replay | adapted | A stateful request is retried only when the server answered or the dial failed; no approval hook (decisions §5.83). The stream-position half is §5.16 |
| 2026-08-11 | openai-agents-python | v0.20.0 | `RunState.add_input` runs input guardrails | adapted | Injected input passes the input guardrails before it is recorded (a97d9fd4; decisions §5.78) |
| 2026-08-11 | openai-agents-python | v0.20.0, v0.21.1 | Tool approvals bound to concrete invocations; exact call decisions honored | adapted | An exact per-call decision outranks a standing one (5c8015ff; decisions §5.74). A call digest: deferred |
| 2026-08-11 | openai-agents-python | v0.20.0 | Raw usage payloads | deferred | No consumer |
| 2026-08-15 | openai-agents-python | v0.21.0 | Public testing kit | deferred | Candidate: publish the `Storage` conformance suite; the fake model stays internal (decisions §5.23) |
| 2026-08-16 | openai-agents-python | v0.21.1 | Model call timeouts | adapted | `RetryPolicy.AttemptTimeout` and `IdleTimeout`, in the retry layer rather than the client (decisions §5.83) |
| 2026-08-16 | openai-agents-python | v0.21.1 | Run-scoped sandbox working directory | deferred | When parallel tasks share a container |
| 2026-08-16 | openai-agents-python | v0.21.1 | Docker sandbox without a network | ported | Already present: spec §2.7o |
| 2026-08-19 | openai-agents-python | v0.22.0 | A terminal tool output an output guardrail refuses is redacted from stored state | adapted | A turn `ToolResult.Terminate` ended is saved with its outputs withheld (4f891891; decisions §5.79); a `ShouldStopAfterTurn` stop is not covered |
| 2026-08-19 | openai-agents-python | v0.22.0 | A `failed` or `incomplete` response is an error on the blocking path | ported | Already present: spec §2.7e |
| 2026-09-08 | openai-agents-python | v0.22.1 | MCP server-wide guardrails | deferred | A guardrail's tool stages already cover MCP calls; binding one to a server has no caller |
| 2026-09-08 | openai-agents-python | v0.22.1 | Unix-local environment isolation | ported | Already present: `LocalSandbox` passes `PATH`, `HOME` and `TMPDIR` unless `InheritHostEnv` |
| 2026-09-08 | openai-agents-python | v0.22.1 | The runner closes the providers it created | declined | scope §3: a `Model` has no lifecycle the runner manages |
| 2026-09-17 | openai-agents-python | v0.22.3 | Conditional approvals see validated tool arguments | deferred | `NeedsApprovalFunc` receives the arguments as the model sent them, before the tool validates |
| 2026-10-02 | openai-agents-python | v0.23.0 | Default tool failure details redacted | deferred | The default error text is what the model corrects itself from; a redacted default waits for a caller |
| 2026-10-02 | openai-agents-python | v0.23.0 | Function tool approvals scoped to their owning agent | deferred | Candidate: a standing decision is keyed by tool name alone |
| 2026-10-02 | openai-agents-python | v0.23.0 | Completed tool results kept when an approval resumes | ported | Already present: spec §2.2 |
| 2026-10-02 | openai-agents-python | v0.23.0 | Tools resolved after the agent's start hook | ported | Already present: `OnStart` runs before the turn's tools are resolved |
| 2026-10-02 | openai-agents-python | v0.23.0 | Configurable MCP listing page limit | ported | Already present as a fixed cap: `tools/list` stops at a page limit and on a repeated cursor |
| — | openai-agents-python | v0.19–v0.23 | Realtime, voice, Chat Completions, LiteLLM, extension sessions, hosted sandbox providers | declined | scope §1.2, §3 |
| 2026-08-31 | openai/codex | a9519cbcd | `update_plan` made opt-in | adapted | The workbench checklist is opt-in per agent (a8c4c127; decisions §5.82) |
| 2026-09-05 | openai/codex | 531f3836a | `codex mcp-server` removed | — | Matches decisions §5.23 |
| 2026-09-11 | openai/codex | 68bc5369b | Worktrees on by default | — | — |
| 2026-09-24 | openai/codex | 12cb14f7b | Paginated history by default | — | — |
| 2026-09-10 | claude-code | v2.1.268 | Task-tracking tools offered only on older models, opt-in elsewhere | — | First step v2.1.233 (2026-08-14) |
| 2026-09-10 | OpenAI API | changelog | Agents API in public beta: a hosted harness where OpenAI runs session orchestration, context compaction and recovery | — | Watch: general availability |

<!--
Row template:

| YYYY-MM-DD | openai-agents-python | vX.Y.Z | what changed upstream | ported / adapted / declined / deferred | why, and where it landed |
-->
