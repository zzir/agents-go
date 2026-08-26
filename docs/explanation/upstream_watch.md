# Upstream Watch

`agents-go` does **not** track [openai-agents-python](https://github.com/openai/openai-agents-python).
It began as a port of that project and shares its core concepts, but behavior is
now specified in [spec.md](../reference/spec.md) and the two evolve independently.

We still read upstream releases, because good ideas are worth taking. This file
records what we looked at and what we decided. **There is no obligation to match.**

## Process

After each upstream **minor** release:

1. Read the changelog and the diff of `src/agents/run.py`, `src/agents/tool.py`,
   `src/agents/memory/`.
2. For each notable change, add a row below with one of:
   - **ported** — implemented here; link the PR
   - **adapted** — implemented differently; say how and why
   - **declined** — not doing it; say why (if it is a permanent non-goal, also
     add it to [scope §1.2 or §3](scope.md))
   - **deferred** — worth doing, nobody has needed it yet
3. Anything that becomes a design invariant goes into `spec.md` in the same change.

Other sources worth the same treatment when something notable lands:

- [microsoft/agent-framework-go](https://github.com/microsoft/agent-framework-go)
- [earendil-works/pi](https://github.com/earendil-works/pi)

## Log

| Date | Source | Version | Change | Decision | Notes |
|---|---|---|---|---|---|
| — | openai-agents-python | v0.18.2 | *(baseline)* | — | Last version this project tracked. Concept mapping and the differences as of that point are in [migration_from_python.md](migration_from_python.md). |

<!--
Row template:

| YYYY-MM-DD | openai-agents-python | vX.Y.Z | what changed upstream | ported / adapted / declined / deferred | why, and where it landed |
-->
