#!/usr/bin/env bash
# Say whether a workbench database an older release created opens under a
# newer one — the line a release's notes lead with.
#
#   ./scripts/schema-compat.sh <old-ref> [<new-ref>=HEAD]
#
# The old ref creates a SQLite database, the new ref opens it: the same
# CreateSchema check the server makes at startup. Prints ONE line on stdout
# (unchanged / changed / not checked) and exits 0 for all three, so a release
# never fails here; the new ref's refusal goes to stderr.
set -euo pipefail

if [ $# -lt 1 ] || [ $# -gt 2 ] || [ -z "$1" ]; then
  echo "usage: $0 <old-ref> [<new-ref>=HEAD]" >&2
  exit 2
fi
old=$1
new=${2:-HEAD}

root=$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)
tmp=$(mktemp -d)
cleanup() {
  for side in old new; do
    git -C "$root" worktree remove --force "$tmp/$side" >/dev/null 2>&1 || true
  done
  rm -rf "$tmp"
}
trap cleanup EXIT

not_checked() {
  echo "schema-compat: $1" >&2
  echo "not checked — verify by hand."
  exit 0
}

# probe <side> <ref> builds $tmp/probe-<side>: open (or create) a database
# and run CreateSchema on it, at that ref. Lives only in the temp worktree.
probe() {
  local side=$1 ref=$2 dir
  git -C "$root" worktree add --detach "$tmp/$side" "$ref" >/dev/null 2>&1 || return 1
  dir="$tmp/$side/cmd/agents-server/internal/schemaprobe"
  mkdir -p "$dir"
  cat >"$dir/main.go" <<'GO'
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/zzir/agents-go/cmd/agents-server/internal/store"
)

func main() {
	db, err := store.OpenDB(os.Args[1])
	if err == nil {
		err = store.CreateSchema(context.Background(), db)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
GO
  (cd "$tmp/$side/cmd/agents-server" && GOWORK=off go build -o "$tmp/probe-$side" ./internal/schemaprobe) >&2
}

probe old "$old" || not_checked "cannot build the probe at $old"
probe new "$new" || not_checked "cannot build the probe at $new"
"$tmp/probe-old" "$tmp/old.db" >&2 || not_checked "$old could not create its own database"

if "$tmp/probe-new" "$tmp/old.db" >&2; then
  echo "unchanged — a $old database opens as is."
else
  echo "changed — this release refuses a $old database; back it up, delete it and restart (no migrations)."
fi
