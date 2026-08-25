#!/usr/bin/env bash
# Run the same checks as .github/workflows/ci.yml locally.
#
#   ./scripts/ci.sh
#
# Two things make local runs differ from CI unless handled:
#   1. go.work (gitignored) — CI builds each module standalone, so we disable it.
#   2. CI runs on Linux — for a faithful OS match, run this script inside the
#      golang image:  docker run --rm -v "$PWD":/src -w /src golang:1.27 ./scripts/ci.sh
set -euo pipefail
cd "$(dirname "$0")/.."

export GOWORK=off

step() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }

step "Frontend build"
(cd cmd/agents-server/internal/web/frontend && npm install --ignore-scripts && npm run lint && npm run build)

step "Frontend audit"
# The lockfile is deliberately not committed, so the installed tree is what
# gets audited — high and above fails, here as in CI.
(cd cmd/agents-server/internal/web/frontend && npm audit --omit=dev --audit-level=high)

step "Vet"
go vet ./...

step "Build"
go build ./...

step "Test (race)"
go test -race ./...

step "Test sandbox backend modules"
(cd sandbox/docker && go vet ./... && go test ./...)

step "Test sessions module"
(cd sessions && go vet ./... && go test ./...)

step "Test skills module"
(cd skills && go vet ./... && go test ./...)

step "Test mcp module"
# -race, unlike the other submodules: these tests used to live in the root
# module and so ran under the root's race step. mcp_locking_test.go exists
# because that coverage caught something; plain `go test` would drop it.
(cd mcp && go vet ./... && go test -race ./...)

step "Test models/anthropic module"
(cd models/anthropic && go vet ./... && go test ./...)
# examples/anthropic has its own go.mod (anthropic-sdk-go), so root `go build
# ./...` does not reach it. Discard the output: a bare `go build` in a main
# package writes the executable into the source tree, and an 18MB binary once
# made it into a commit that way.
(cd examples/anthropic && go vet ./... && go build -o /dev/null ./...)

step "Test agents-server module"
(cd cmd/agents-server && go vet ./... && go test -race ./...)

# CI runs the store suite on PostgreSQL too (a service container). Locally
# it takes a server: set AGENTS_PG_TEST_DSN to run it, else the step is skipped.
#   docker run -d -e POSTGRES_PASSWORD=test -e POSTGRES_DB=agents_test -p 54329:5432 postgres:16-alpine
#   AGENTS_PG_TEST_DSN='postgres://postgres:test@localhost:54329/agents_test?sslmode=disable' ./scripts/ci.sh
if [ -n "${AGENTS_PG_TEST_DSN:-}" ]; then
  step "Test agents-server store on PostgreSQL"
  (cd cmd/agents-server && go test ./internal/store/)
else
  step "Test agents-server store on PostgreSQL (skipped: AGENTS_PG_TEST_DSN unset)"
fi

step "govulncheck"
if command -v govulncheck >/dev/null; then
  govulncheck ./...
  # mcp is scanned explicitly: it used to be part of the root module, and it is
  # the module carrying the go-sdk's OAuth and jsonrpc2 code — the split must
  # not quietly drop it out of the scan.
  (cd mcp && govulncheck ./...)
else
  echo "govulncheck not installed; skipping (CI runs it)." >&2
  echo "Install: go install golang.org/x/vuln/cmd/govulncheck@latest" >&2
fi

step "OpenAPI spec up to date"
# Stale means "regenerating changes the file" — compared by content (not by
# `git diff`, which would also fail on a fresh regeneration that simply is not
# committed yet), so this passes on a dirty tree whose spec is current.
(cd cmd/agents-server \
  && before=$(git hash-object internal/docs/swagger.yaml) \
  && go tool swag init --v3.1 -g main.go --parseDependency --parseInternal -o internal/docs --outputTypes yaml --quiet \
  && after=$(git hash-object internal/docs/swagger.yaml) \
  && [ "$before" = "$after" ]) || {
  echo "internal/docs is stale — run 'make openapi' in cmd/agents-server and commit the result." >&2
  exit 1
}

# The frontend's generated API types must match that spec, by the same
# hash-compare rule.
(cd cmd/agents-server/internal/web/frontend \
  && before=$(git hash-object src/lib/apiTypes.gen.ts) \
  && npm run --silent gen:api \
  && after=$(git hash-object src/lib/apiTypes.gen.ts) \
  && [ "$before" = "$after" ]) || {
  echo "src/lib/apiTypes.gen.ts is stale — run 'npm run gen:api' in the frontend and commit the result." >&2
  exit 1
}

step "golangci-lint (lint + gofmt/goimports)"
if command -v golangci-lint >/dev/null; then
  golangci-lint run                            # root: lint + formatters
  (cd cmd/agents-server && golangci-lint run)  # agents-server: lint + formatters
  # mcp gets the full run too: it is a client with real logic, not a thin
  # backend adapter, and it was fully linted for free while it lived in the
  # root module. The support submodules below run formatters only.
  (cd mcp && golangci-lint run)                # mcp: lint + formatters
  for m in models/anthropic examples/anthropic sandbox/docker sessions skills; do
    out=$(cd "$m" && golangci-lint fmt --diff)
    if [ -n "$out" ]; then
      echo "$m is not gofmt/goimports-clean:"; echo "$out"; exit 1
    fi
  done
else
  echo "golangci-lint not installed; skipping lint AND format checks (CI runs them)." >&2
  echo "Install: brew install golangci-lint" >&2
fi

printf '\n\033[1;32mAll CI checks passed.\033[0m\n'
