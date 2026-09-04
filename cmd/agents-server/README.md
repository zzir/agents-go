# agents-server — the agents-go workbench

**Go agents. Local first.** The Go-native agent workbench you run yourself,
built on the [agents-go](../../README.md) SDK: one binary, your data (SQLite
or PostgreSQL), an embedded UI. Its documentation lives with the rest of the
project in [`docs/`](../../docs/README.md), sorted by what you came for —
start at [Running the workbench](../../docs/tutorial/workbench.md).

![screenshot](screenshot.png)

## In this directory

- [`PROTOCOL.md`](PROTOCOL.md) — the two WebSocket changes still open (entry
  ids on deltas, one `run.entry` event) and the decisions the code cites by
  number (F3, F4). It stays beside the code because those comments name it
  by path; the protocol that ships today is in
  [the wire surface](../../docs/reference/protocol.md).
- **Generated API surface.** A handler annotation change is three commands:
  `make openapi` here (writes `internal/docs/swagger.yaml`), `npm run gen:api`
  in `internal/web/frontend` (writes `src/lib/apiTypes.gen.ts`), then commit
  both. CI fails when either is stale, and lints the frontend.
