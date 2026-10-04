#!/usr/bin/env bash
# A release tags the root module and every library module at one version on
# one commit (decisions §5.7). Four modes:
#
#   ./scripts/release-prep.sh vX.Y.Z                         # write the version into every go.mod; print what comes next
#   ./scripts/release-prep.sh --check                        # every in-repo require names one version (CI)
#   ./scripts/release-prep.sh --tag-modules vX.Y.Z [--push]  # tag every library module at HEAD (release.yml, from the root tag)
#   ./scripts/release-prep.sh --verify vX.Y.Z                # HEAD is ready for the root tag
#
# The first mode leaves go.mod edits to commit; the `replace` lines stay, so CI
# and local builds keep using HEAD while a consumer gets the tagged root. A
# person pushes the root tag alone; release.yml runs --tag-modules on its
# commit, so the library tags never need typing. --tag-modules refuses a HEAD
# whose go.mod does not require the version and a library tag that already
# points elsewhere; a tag already on HEAD is kept. --verify is the gate after
# it, and what a release made before --tag-modules still passes by hand.
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

# requires_version <version> prints the in-repo requires that do not name
# <version>, one per line; nothing when they all do.
requires_version() {
  local version="$1" bad=""
  for mod in "${LIB_MODULES[@]}" "${APP_MODULES[@]}"; do
    while read -r path ver; do
      [ "$ver" = "$version" ] || bad+="  $mod/go.mod requires $path $ver"$'\n'
    done < <(own_requires "$mod")
  done
  printf '%s' "$bad"
}

tag_modules() {
  local version="$1" push="$2" bad tag at head tags=()
  check_lists
  bad=$(requires_version "$version")
  if [ -n "$bad" ]; then
    echo "HEAD does not require $version, so it is not tagged as it:" >&2
    printf '%s' "$bad" >&2
    exit 1
  fi
  head=$(git rev-parse HEAD)
  for mod in "${LIB_MODULES[@]}"; do
    tag="$mod/$version"
    if at=$(git rev-parse -q --verify "refs/tags/$tag^{commit}"); then
      if [ "$at" != "$head" ]; then
        echo "tag $tag already points at ${at:0:12}, not at HEAD; a version is tagged once" >&2
        exit 1
      fi
      echo "kept $tag"
    else
      git tag "$tag"
      echo "created $tag"
    fi
    tags+=("$tag")
  done
  if [ "$push" = "1" ]; then
    git push origin "${tags[@]}"
  fi
}

verify() {
  local version="$1" bad="" here tag
  check_lists
  bad=$(requires_version "$version")
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
  git tag $version
  # The root tag starts the release; release.yml tags the library modules
  # (${tags[*]}) on the same commit and pushes those tags itself.
  git push origin main $version
EOF
}

case "${1:-}" in
  --check)
    check
    ;;
  --tag-modules)
    [[ "${2:-}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-.+)?$ ]] || { echo "usage: $0 --tag-modules vX.Y.Z [--push]" >&2; exit 2; }
    push=0
    if [ "${3:-}" = "--push" ]; then push=1; elif [ -n "${3:-}" ]; then echo "usage: $0 --tag-modules vX.Y.Z [--push]" >&2; exit 2; fi
    tag_modules "$2" "$push"
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
    echo "usage: $0 vX.Y.Z | --check | --tag-modules vX.Y.Z [--push] | --verify vX.Y.Z" >&2
    exit 2
    ;;
esac
