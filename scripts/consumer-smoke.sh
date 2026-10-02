#!/usr/bin/env bash
# Proves a release go-gettable the way a consumer gets it: from the module
# proxy, with no workspace and no replace — what CI, which tests HEAD, never
# sees.
#
#   ./scripts/consumer-smoke.sh vX.Y.Z      # once the release's tags are pushed
#
# Three passes, each in a fresh module: every module alone, all of them in one
# go.mod (a version conflict between them shows here), and each docs page's
# `go get` commands in the order printed. A tag pushed moments ago may not be
# on the proxy yet, so a fetch is retried a minute apart; SMOKE_ATTEMPTS=1
# turns that off.
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck source=scripts/modules.sh
source scripts/modules.sh

OWN='github.com/zzir/agents-go'
version="${1:-}"
if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-.+)?$ ]]; then
  echo "usage: $0 vX.Y.Z" >&2
  exit 2
fi

export GOWORK=off GOFLAGS=-mod=mod
attempts="${SMOKE_ATTEMPTS:-5}"
repo=$PWD
tmp=$(mktemp -d)
trap 'rm -rf "${tmp:?}"' EXIT

# get <module[@version]>...: `go get`, retried while the proxy catches up.
get() {
  local n
  for ((n = 1; n <= attempts; n++)); do
    if go get "$@"; then return 0; fi
    if [ "$n" -lt "$attempts" ]; then sleep 60; fi
  done
  return 1
}

# build <module path>...: a program importing a package of each builds. The
# root module's own directory holds no package, so it lends agents.
build() {
  {
    echo 'package main'
    for m in "$@"; do
      if [ "$m" = "$OWN" ]; then m="$OWN/agents"; fi
      echo "import _ \"$m\""
    done
    echo 'func main() {}'
  } >main.go
  go build -o /dev/null .
}

alone() { get "$1@$version" && build "$1"; }

together() {
  local m at=()
  for m in "$@"; do at+=("$m@$version"); done
  get "${at[@]}" && build "$@"
}

# page <file>: its `go get` commands as printed, in order, then one build over
# every module they named.
page() {
  local cmd target named=()
  while read -r cmd; do
    target="${cmd#go get }"
    get "$target" || return 1
    named+=("${target%@*}")
  done < <(grep -oE "go get ${OWN//./\\.}[a-z/]*(@[A-Za-z0-9._-]+)?" "$repo/$1")
  # shellcheck disable=SC2046
  build $(printf '%s\n' "${named[@]}" | sort -u)
}

# attempt <label> <command>...: runs it in a fresh module and keeps a failure
# for the report, so one run shows everything that is broken.
count=0
failed=""
attempt() {
  local label="$1" dir
  shift
  count=$((count + 1))
  dir="$tmp/$count"
  mkdir "$dir"
  if (cd "$dir" && go mod init smoke >/dev/null 2>&1 && "$@") >"$dir.log" 2>&1; then
    echo "ok    $label"
  else
    echo "FAIL  $label"
    failed+="--- $label"$'\n'"$(tail -n 4 "$dir.log")"$'\n'
  fi
}

modules=("$OWN")
for m in "${LIB_MODULES[@]}"; do modules+=("$OWN/$m"); done

for m in "${modules[@]}"; do attempt "$m@$version" alone "$m"; done
attempt "every module at $version in one go.mod" together "${modules[@]}"
while read -r file; do
  attempt "the go get commands of $file" page "$file"
done < <(grep -rlE "go get ${OWN//./\\.}" README.md docs | sort)

if [ -n "$failed" ]; then
  echo "$version is not go-gettable as released and documented:" >&2
  printf '%s' "$failed" >&2
  exit 1
fi
echo "$version: every module fetched and built as a consumer would."
