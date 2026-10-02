#!/usr/bin/env bash
# A release tags the root module and every library module at one version on
# one commit (decisions §5.7). Three modes:
#
#   ./scripts/release-prep.sh vX.Y.Z            # write the version into every go.mod; print the tag commands
#   ./scripts/release-prep.sh --check           # every in-repo require names one version (CI)
#   ./scripts/release-prep.sh --verify vX.Y.Z   # HEAD is ready for the root tag
#
# The first mode leaves go.mod edits to commit; the `replace` lines stay, so CI
# and local builds keep using HEAD while a consumer gets the tagged root.
# --verify runs before the root tag is pushed and again in release.yml. A
# refusal there for a missing prefixed tag is mended by pushing that tag and
# re-running the workflow; one for a go.mod that does not require the version
# needs the next patch release.
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck source=scripts/modules.sh
source scripts/modules.sh

OWN='github.com/zzir/agents-go'

# own_requires <dir>: "<module path> <version>" for each of this repository's
# modules that <dir>/go.mod requires.
own_requires() {
  awk -v own="$OWN" '
    function ours(p) { return p == own || index(p, own "/") == 1 }
    $1 == "require" && ours($2) { print $2, $3 }
    ours($1) && $2 ~ /^v/ { print $1, $2 }
  ' "$1/go.mod"
}

# Every go.mod beside the root's is in exactly one of the two lists. The tree
# is walked rather than asked of git, so the check also runs in a container
# that mounts the repository (where git refuses another user's checkout).
check_lists() {
  local listed found
  listed=$(printf '%s\n' "${LIB_MODULES[@]}" "${APP_MODULES[@]}" | sort)
  found=$(find . \( -name node_modules -o -name '.?*' \) -prune -o -name go.mod ! -path ./go.mod -print |
    sed -e 's|^\./||' -e 's|/go.mod$||' | sort)
  if [ "$listed" != "$found" ]; then
    echo "scripts/modules.sh does not list the repository's modules:" >&2
    diff <(echo "$listed") <(echo "$found") >&2 || true
    exit 1
  fi
}

check() {
  check_lists
  local versions
  versions=$(for mod in "${LIB_MODULES[@]}" "${APP_MODULES[@]}"; do own_requires "$mod"; done | awk '{print $2}' | sort -u)
  if [ "$(echo "$versions" | wc -l | tr -d ' ')" != "1" ]; then
    echo "in-repo requires name more than one version:" >&2
    for mod in "${LIB_MODULES[@]}" "${APP_MODULES[@]}"; do
      own_requires "$mod" | sed "s|^|  $mod/go.mod: |" >&2
    done
    exit 1
  fi
  echo "every in-repo require is at $versions."
}

verify() {
  local version="$1" bad="" here tag
  check_lists
  for mod in "${LIB_MODULES[@]}" "${APP_MODULES[@]}"; do
    while read -r path ver; do
      [ "$ver" = "$version" ] || bad+="  $mod/go.mod requires $path $ver"$'\n'
    done < <(own_requires "$mod")
  done
  here=$(git tag --points-at HEAD)
  for mod in "${LIB_MODULES[@]}"; do
    tag="$mod/$version"
    grep -qxF "$tag" <<<"$here" || bad+="  tag $tag is not on HEAD"$'\n'
  done
  if [ -n "$bad" ]; then
    echo "HEAD is not ready to be released as $version:" >&2
    printf '%s' "$bad" >&2
    exit 1
  fi
  echo "HEAD is ready for $version: every go.mod requires it and every library tag is here."
}

prepare() {
  local version="$1" tags=()
  ./scripts/release-check.sh "$version"
  check_lists
  for mod in "${LIB_MODULES[@]}" "${APP_MODULES[@]}"; do
    while read -r path _; do
      (cd "$mod" && GOWORK=off go mod edit -require="$path@$version")
    done < <(own_requires "$mod")
  done
  for mod in "${LIB_MODULES[@]}"; do tags+=("$mod/$version"); done
  cat <<EOF

Every go.mod now requires $version. Next:

  ./scripts/ci.sh
  git commit -am "chore(release): $version"
  for t in ${tags[*]} $version; do git tag "\$t"; done
  ./scripts/release-prep.sh --verify $version

  # The library tags first: no workflow listens to them.
  git push origin ${tags[*]}
  git ls-remote --tags origin '*/$version'
  # The root tag alone, once they are there: it starts the release.
  git push origin $version
EOF
}

case "${1:-}" in
  --check)
    check
    ;;
  --verify)
    [[ "${2:-}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-.+)?$ ]] || { echo "usage: $0 --verify vX.Y.Z" >&2; exit 2; }
    verify "$2"
    ;;
  v[0-9]*)
    [[ "$1" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-.+)?$ ]] || { echo "usage: $0 vX.Y.Z" >&2; exit 2; }
    prepare "$1"
    ;;
  *)
    echo "usage: $0 vX.Y.Z | --check | --verify vX.Y.Z" >&2
    exit 2
    ;;
esac
