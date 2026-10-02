#!/usr/bin/env bash
# Refuses a patch release that breaks the SDK's exported API (decisions §5.8).
#
#   ./scripts/release-check.sh vX.Y.Z      # on the commit to tag, BEFORE tagging
#
# A minor (vX.Y.0) may break and passes unchecked. For a patch, the exported
# API of the root module and of every library module at HEAD is compared with
# the previous root tag's, read from a local worktree of that tag — nothing is
# fetched but the apidiff tool. PREV_TAG overrides the tag compared against.
# A constant whose VALUE changed (a prompt's wording, a schema version) still
# compiles for every caller: it is listed and does not refuse the release.
#
# release.yml runs it again after the tag is pushed. A refusal there comes too
# late to unpublish: the module proxy keeps a pushed version for good. Leave
# the tag, tag the next minor on the same commit, and retract the patch (a
# `retract` line in go.mod) in the release after.
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck source=scripts/modules.sh
source scripts/modules.sh

APIDIFF='golang.org/x/exp/cmd/apidiff@v0.0.0-20260908205506-85c1c2202aba'

version="${1:-}"
if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.([0-9]+)(-.+)?$ ]]; then
  echo "usage: $0 vX.Y.Z" >&2
  exit 2
fi
if [ "${BASH_REMATCH[1]}" = "0" ]; then
  echo "$version is a minor release: it may break exported API (decisions §5.8)."
  exit 0
fi

# The previous root tag: --match keeps a library module's prefixed tag out,
# and a tag already sitting on HEAD is the one being released, not its base.
prev="${PREV_TAG:-}"
if [ -z "$prev" ]; then
  from=HEAD
  if [ "$(git rev-parse -q --verify "refs/tags/$version^{commit}" || true)" = "$(git rev-parse HEAD)" ]; then
    from="HEAD^"
  fi
  prev=$(git describe --tags --abbrev=0 --match 'v[0-9]*' "$from")
fi

export GOWORK=off
base=$(mktemp -d)
work=$(mktemp -d)
trap 'git worktree remove --force "$base" >/dev/null 2>&1 || true; rm -rf "${work:?}"' EXIT
git worktree add --detach "$base" "$prev" >"$work/log" 2>&1 || { cat "$work/log" >&2; exit 2; }

# apidiff exits 0 whatever it finds, so a failure is the tool's own: it stops
# the check instead of passing for "no difference".
broken=""
values=""
for mod in . "${LIB_MODULES[@]}"; do
  # A module the previous release did not have has no API to keep.
  [ -f "$base/$mod/go.mod" ] && [ -f "$mod/go.mod" ] || continue
  if ! (cd "$base/$mod" && go run "$APIDIFF" -m -w "$work/export" .) >"$work/log" 2>&1; then
    echo "cannot read the exported API of module '$mod' at $prev:" >&2
    cat "$work/log" >&2
    exit 2
  fi
  if ! (cd "$mod" && go run "$APIDIFF" -m -incompatible "$work/export" .) >"$work/diff" 2>"$work/log"; then
    echo "cannot compare the exported API of module '$mod' with $prev:" >&2
    cat "$work/log" >&2
    exit 2
  fi
  breaks=$(grep '^- ' "$work/diff" | grep -v ': value changed from ' || true)
  consts=$(grep '^- .*: value changed from ' "$work/diff" || true)
  [ -z "$breaks" ] || broken+="$mod:"$'\n'"$breaks"$'\n'
  [ -z "$consts" ] || values+="$mod:"$'\n'"$consts"$'\n'
done

if [ -n "$values" ]; then
  echo "Constants whose value changed since $prev (not an API break; say so in the release notes):"
  printf '%s' "$values"
fi
if [ -n "$broken" ]; then
  echo "$version is a patch release, and its exported API breaks $prev:" >&2
  printf '%s' "$broken" >&2
  echo "Tag the next minor instead, or take the break out (decisions §5.8)." >&2
  exit 1
fi
echo "$version keeps the exported API of $prev."
