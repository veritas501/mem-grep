#!/usr/bin/env bash
# Cut a release as one self-contained transaction (called by `make bump`):
#   1. commit whatever is pending, so the release does not mix with unrelated edits;
#   2. create a release commit that carries only version metadata (git is the single source of
#      the version, so there is no version file — the commit is empty apart from its message);
#   3. create an annotated tag whose message summarizes the changes since the previous tag
#      (Features / Fixes / Maintenance) and ends with "Version: OLD -> NEW".
# Nothing is pushed.
#
# Usage: scripts/bump-version.sh major|minor|patch|vX.Y.Z
#   BUMP_MESSAGE  commit message for pending changes (default "chore: pending changes before vX.Y.Z")
set -euo pipefail

die() { echo "bump: $*" >&2; exit 1; }

part="${1:-}"
[ -n "$part" ] || die "usage: $0 major|minor|patch|vX.Y.Z"
git rev-parse --git-dir >/dev/null 2>&1 || die "not a git repository"

old=$(git describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null || true)
base=${old:-v0.0.0}
if [[ ! $base =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
  die "latest tag $base is not vMAJOR.MINOR.PATCH"
fi
major=${BASH_REMATCH[1]} minor=${BASH_REMATCH[2]} patch=${BASH_REMATCH[3]}
case "$part" in
  major) new="v$((major + 1)).0.0" ;;
  minor) new="v${major}.$((minor + 1)).0" ;;
  patch) new="v${major}.${minor}.$((patch + 1))" ;;
  v[0-9]*.[0-9]*.[0-9]*) [[ $part =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "bad version $part"; new=$part ;;
  *) die "expected major, minor, patch or vX.Y.Z, got $part" ;;
esac
git rev-parse -q --verify "refs/tags/$new" >/dev/null && die "tag $new already exists"

# 1. pending changes first
if [ -n "$(git status --porcelain)" ]; then
  git add -A
  git commit -q -m "${BUMP_MESSAGE:-chore: pending changes before $new}"
  echo "committed pending changes: $(git log -1 --format='%h %s')"
fi

# 2. change summary since the previous tag, classified by commit subject
range=${old:+$old..}HEAD
features=() fixes=() maintenance=()
while IFS= read -r subject; do
  [ -n "$subject" ] || continue
  shopt -s nocasematch
  if [[ $subject =~ ^(feat|feature)(\(.*\))?!?: ]] || [[ $subject =~ ^(add|implement|support)[[:space:]] ]]; then
    features+=("$subject")
  elif [[ $subject =~ ^(fix|bugfix|hotfix)(\(.*\))?!?: ]] || [[ $subject =~ ^fix[[:space:]] ]]; then
    fixes+=("$subject")
  else
    maintenance+=("$subject")
  fi
  shopt -u nocasematch
done < <(git log --no-merges --format=%s "$range")

msg=$(mktemp)
trap 'rm -f "$msg"' EXIT
{
  echo "$new"
  for section in Features Fixes Maintenance; do
    case $section in
      Features) items=("${features[@]+"${features[@]}"}") ;;
      Fixes) items=("${fixes[@]+"${fixes[@]}"}") ;;
      Maintenance) items=("${maintenance[@]+"${maintenance[@]}"}") ;;
    esac
    [ ${#items[@]} -gt 0 ] && [ -n "${items[0]}" ] || continue
    echo
    echo "$section:"
    printf -- '- %s\n' "${items[@]}"
  done
  echo
  echo "Version: ${old:-none} -> $new"
} >"$msg"

# 3. release commit (version metadata only) and annotated tag
git commit -q --allow-empty -m "chore(release): $new" -m "Version: ${old:-none} -> $new"
git tag -a "$new" -F "$msg"

echo "released $new at $(git rev-parse --short HEAD) (not pushed)"
echo "push with: git push origin HEAD && git push origin $new"
