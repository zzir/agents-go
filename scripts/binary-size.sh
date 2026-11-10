#!/usr/bin/env bash
# agents-server, built as it ships, stays under its size budget, and both build
# definitions carry the same tags — decisions §5.102.
#
#   ./scripts/binary-size.sh      # the frontend must be built first (go:embed)
set -euo pipefail
cd "$(dirname "$0")/.."

budget_mb=48

tags=$(sed -n 's/^TAGS *:= *//p' cmd/agents-server/Makefile)
if [ -z "$tags" ]; then
  echo "cmd/agents-server/Makefile defines no TAGS" >&2
  exit 1
fi
for tag in ${tags//,/ }; do
  if ! grep -q -- "- $tag\$" .goreleaser.yaml; then
    echo ".goreleaser.yaml does not build with the tag $tag" >&2
    exit 1
  fi
done

out=$(mktemp)
trap 'rm -f "$out"' EXIT
(cd cmd/agents-server && CGO_ENABLED=0 GOWORK=off go build -trimpath -tags "$tags" -ldflags '-s -w' -o "$out" .)
size_mb=$(( $(wc -c < "$out") / 1048576 ))
echo "agents-server: ${size_mb} MB (budget ${budget_mb} MB)"
if [ "$size_mb" -gt "$budget_mb" ]; then
  echo "over budget: a dependency that looks methods up by name switches off the linker's method pruning" >&2
  exit 1
fi
