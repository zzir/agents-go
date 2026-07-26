#!/usr/bin/env bash
# Run the same checks as .github/workflows/ci.yml locally.
#
#   ./scripts/ci.sh
#
# Two things make local runs differ from CI unless handled:
#   1. go.work (gitignored) — CI builds each module standalone, so we disable it.
#   2. CI runs on Linux — for a faithful OS match, run this script inside the
#      golang image:  docker run --rm -v "$PWD":/src -w /src golang:1.26 ./scripts/ci.sh
set -euo pipefail
cd "$(dirname "$0")/.."

export GOWORK=off

step() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }

step "Frontend build"
(cd cmd/agents-server/internal/web/frontend && npm install --ignore-scripts && npm run build)

step "Vet"
go vet ./...

step "Build"
go build ./...

step "Test (race)"
go test -race ./...

step "Test sandbox backend modules"
(cd sandbox/docker && go vet ./... && go test ./...)
(cd sandbox/ssh && go vet ./... && go test ./...)

step "Test tracing/otel module"
(cd tracing/otel && go vet ./... && go test ./...)
# The otel example has its own go.mod (it pulls the OTel stdout exporter), so
# `go build ./...` at the root does not reach it. Discard the output: a bare
# `go build` in a main package writes the executable into the source tree, and
# an 18MB binary once made it into a commit that way.
(cd examples/otel && go vet ./... && go build -o /dev/null ./...)

step "Test sessions module"
(cd sessions && go vet ./... && go test ./...)

step "Test skills module"
(cd skills && go vet ./... && go test ./...)

step "Test agents-server module"
(cd cmd/agents-server && go vet ./... && go test -race ./...)

step "Examples run (fake Responses API)"
# go build proves they compile; this proves they still *work*. Every API change
# rewrites all of them, and a runtime break (bad struct tag, nil provider,
# infinite loop) is invisible to the compiler.
go run ./cmd/verifyexamples

step "Docs name things that exist"
# The docs are the API's other surface and nothing compiled them: sessions.md
# kept calling GetItems and PopItem after both were renamed. Snippets are
# fragments, so this checks names rather than compiling.
go run ./cmd/verifydocs

step "govulncheck"
if command -v govulncheck >/dev/null; then
  govulncheck ./...
else
  echo "govulncheck not installed; skipping (CI runs it)." >&2
  echo "Install: go install golang.org/x/vuln/cmd/govulncheck@latest" >&2
fi

step "OpenAPI spec up to date"
(cd cmd/agents-server \
  && go tool swag init --v3.1 -g main.go --parseDependency --parseInternal -o internal/docs --outputTypes yaml --quiet \
  && git diff --exit-code internal/docs) || {
  echo "internal/docs is stale — run 'make openapi' in cmd/agents-server and commit the result." >&2
  exit 1
}

step "golangci-lint (lint + gofmt/goimports)"
if command -v golangci-lint >/dev/null; then
  golangci-lint run                            # root: lint + formatters
  (cd cmd/agents-server && golangci-lint run)  # agents-server: lint + formatters
  # The support submodules don't run full golangci; check their formatting only.
  for m in sandbox/docker sandbox/ssh sessions skills tracing/otel; do
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
