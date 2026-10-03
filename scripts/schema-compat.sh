#!/usr/bin/env bash
# Say whether a workbench database an older release created opens under a
# newer one — the line a release's notes lead with.
#
#   ./scripts/schema-compat.sh <old-ref> [<new-ref>=HEAD] [--pg <DSN>] [--wipe]
#
# The old ref creates a database, the new ref opens it: the same CreateSchema
# check the server makes at startup. Always on SQLite; with --pg the same two
# probes run against that PostgreSQL database too, which is EMPTIED first.
# The wipe runs only when the DSN's database name ends in _test or --wipe is
# given; the DSN comes from the command line alone, never from an environment
# variable. Prints ONE line on stdout (unchanged / changed / not checked) and
# exits 0 for all three, so a release never fails here; the new ref's refusal
# goes to stderr.
set -euo pipefail

usage() {
  echo "usage: $0 <old-ref> [<new-ref>=HEAD] [--pg <DSN>] [--wipe]" >&2
  exit 2
}

old="" new="" pg="" wipe=0
while [ $# -gt 0 ]; do
  case "$1" in
    --pg)
      [ $# -ge 2 ] || usage
      pg=$2
      shift 2
      ;;
    --wipe)
      wipe=1
      shift
      ;;
    -*)
      usage
      ;;
    *)
      if [ -z "$old" ]; then
        old=$1
      elif [ -z "$new" ]; then
        new=$1
      else
        usage
      fi
      shift
      ;;
  esac
done
[ -n "$old" ] || usage
new=${new:-HEAD}

# The wipe guard: a database that is not named for testing is wiped only on
# an explicit --wipe.
if [ -n "$pg" ]; then
  case "$pg" in
    postgres://*|postgresql://*) ;;
    *) echo "schema-compat: --pg takes a postgres:// DSN" >&2; exit 2 ;;
  esac
  dbname=${pg%%\?*}
  dbname=${dbname##*/}
  if [[ "$dbname" != *_test ]] && [ "$wipe" -ne 1 ]; then
    echo "schema-compat: refusing to wipe database '$dbname': name it *_test or pass --wipe" >&2
    exit 2
  fi
fi

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
# and run CreateSchema on it, at that ref; "wipe" as the second argument
# empties a PostgreSQL schema first. Lives only in the temp worktree.
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
	ctx := context.Background()
	db, err := store.OpenDB(os.Args[1])
	if err == nil && len(os.Args) > 2 && os.Args[2] == "wipe" {
		for _, stmt := range []string{"DROP SCHEMA public CASCADE", "CREATE SCHEMA public"} {
			if _, err = db.ExecContext(ctx, stmt); err != nil {
				break
			}
		}
	}
	if err == nil {
		err = store.CreateSchema(ctx, db)
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

opens=1
"$tmp/probe-new" "$tmp/old.db" >&2 || opens=0

backends="SQLite"
if [ -n "$pg" ]; then
  "$tmp/probe-old" "$pg" wipe >&2 || not_checked "$old could not create its own PostgreSQL database"
  if "$tmp/probe-new" "$pg" >&2; then
    [ "$opens" -eq 1 ] || echo "schema-compat: PostgreSQL opens, SQLite does not" >&2
  else
    [ "$opens" -eq 0 ] || echo "schema-compat: SQLite opens, PostgreSQL does not" >&2
    opens=0
  fi
  backends="SQLite and PostgreSQL"
fi

if [ "$opens" -eq 1 ]; then
  echo "unchanged — a $old database opens as is ($backends)."
else
  echo "changed — this release refuses a $old database; back it up, delete it and restart (no migrations)."
fi
