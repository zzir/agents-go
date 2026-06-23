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

step "Verify formatting"
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
  echo "These files are not gofmt-ed:"; echo "$unformatted"; exit 1
fi

step "Vet"
go vet ./...

step "Build"
go build ./...

step "Test (race)"
go test -race ./...

step "Test sandbox backend modules"
(cd sandbox/docker && go vet ./... && go test ./...)
(cd sandbox/ssh && go vet ./... && go test ./...)

step "Test sessions module"
(cd sessions && go vet ./... && go test ./...)

step "Test skills module"
(cd skills && go vet ./... && go test ./...)

step "Test agents-server module"
(cd cmd/agents-server && go vet ./... && go test -race ./...)

step "golangci-lint"
if command -v golangci-lint >/dev/null; then
  golangci-lint run
  (cd cmd/agents-server && golangci-lint run)
else
  echo "golangci-lint not installed; skipping (CI runs it)." >&2
  echo "Install: brew install golangci-lint" >&2
fi

printf '\n\033[1;32mAll CI checks passed.\033[0m\n'
