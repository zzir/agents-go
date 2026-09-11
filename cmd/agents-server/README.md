# agents-server — the agents-go workbench

**Go agents. Local first.** The Go-native agent workbench you run yourself,
built on the [agents-go](../../README.md) SDK: one binary, your data (SQLite
or PostgreSQL), an embedded UI. Its documentation lives with the rest of the
project in [`docs/`](../../docs/README.md), sorted by what you came for —
start at [Running the workbench](../../docs/tutorial/workbench.md).

![screenshot](screenshot.png)

## First mile

`make build` here — Node 22 and npm for the SPA, then Go — leaves
`./agents-server` beside this file; run it and open `http://127.0.0.1:9527`
with the token it printed. Add a provider and an agent in Settings, and say
something. Nothing on that path needs Docker; a sandbox is
[the tutorial's second chapter](../../docs/tutorial/workbench.md#give-it-a-sandbox).

## Development loop

`make dev` starts the Vite dev server (`npm run dev` in
`internal/web/frontend`), which proxies `/api` and `/ws` to a backend on
`127.0.0.1:9527` — start one with `go run . --token X` (add `--db` for a
scratch database) and edit under `src/` with hot reload. `go run .` embeds
`internal/web/frontend/dist`, so it needs one build of the SPA first
(`make frontend`, or `go generate ./internal/web`). `./scripts/ci.sh` at the
repo root is CI locally; its frontend steps are `npm install`, `npm run lint`,
`npm run build` (`tsc --noEmit`, `vitest run`, `vite build`, then gzip) and
`npm audit --omit=dev --audit-level=high`. The lockfile is deliberately not
committed (`package-lock.json` is gitignored), so the tree `npm install`
resolves today is what gets built and audited.

## In this directory

- **The wire surface.** What the API and the WebSocket mean is
  [`docs/reference/protocol.md`](../../docs/reference/protocol.md), the two
  WebSocket changes still open its last section; the live definition is
  [`internal/protocol/messages.go`](internal/protocol/messages.go), mirrored
  in [`src/lib/protocol.ts`](internal/web/frontend/src/lib/protocol.ts).
- **Generated API surface.** A handler annotation change is three commands:
  `make openapi` here (writes `internal/docs/swagger.yaml`), `npm run gen:api`
  in `internal/web/frontend` (writes `src/lib/apiTypes.gen.ts`), then commit
  both. CI fails when either is stale, and lints the frontend. A settings
  registry change is one more: `make settings-doc` rewrites the runtime
  settings table of [`docs/reference/configuration.md`](../../docs/reference/configuration.md),
  and the settings tests fail while it is stale.
