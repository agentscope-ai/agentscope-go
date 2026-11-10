#!/usr/bin/env bash
# Checks the Stable-tier package APIs (STABILITY.md) for incompatible changes
# since the latest v2 tag, using the apidiff on PATH. The api-compat workflow
# runs this script; from a checkout you can run the same logic:
#
#   bash .github/scripts/api-compat.sh              # report against the tag
#   bash .github/scripts/api-compat.sh origin/main  # check as a PR onto main
#
# Pull-request mode (GITHUB_EVENT_NAME=pull_request, or a BASE argument) also
# checks BASE and fails only on incompatible changes that the checked-out tree
# has and BASE does not, unless BREAKING_CHANGE_LABEL=true. In Actions, BASE is
# the first parent of the pull request's test merge commit, which
# actions/checkout checks out. Report mode (any other event) lists every
# incompatible change since the tag and does not fail on them.
#
# apidiff exits 0 when it reports incompatible changes, so findings are read
# from its output. Any apidiff failure, such as a package that does not load,
# fails the run in both modes after every package has been checked.
set -eu

# Keep this list in sync with the Stable tier in STABILITY.md.
STABLE="message model agent tool permission formatter errors"
MODULE=github.com/agentscope-ai/agentscope-go/v2

if [ $# -gt 1 ]; then
  echo "usage: $0 [BASE]" >&2
  exit 2
fi

cd "$(git rev-parse --show-toplevel)"

mode=report
base=
if [ $# -eq 1 ]; then
  mode=pr
  base=$(git rev-parse --verify "$1^{commit}")
elif [ "${GITHUB_EVENT_NAME:-}" = pull_request ]; then
  mode=pr
  if ! git rev-parse -q --verify 'HEAD^2' >/dev/null; then
    echo "ERROR: HEAD is not a merge commit; expected the pull request's test merge from actions/checkout" >&2
    exit 1
  fi
  base=$(git rev-parse HEAD^1)
fi

TAG=$(git tag --sort=-v:refname | grep -E '^v2\.' | head -1)
if [ -z "$TAG" ]; then
  echo "No v2 tag found; skipping baseline comparison"
  exit 0
fi
echo "Baseline tag: $TAG"
if [ "$mode" = pr ]; then
  echo "Pull request base: $base"
fi

tmp=$(mktemp -d)
cleanup() {
  for wt in "$tmp/tag" "$tmp/base"; do
    if [ -d "$wt" ]; then
      git worktree remove --force "$wt" || true
    fi
  done
  rm -rf "$tmp" || true
}
trap cleanup EXIT
git worktree add --quiet --detach "$tmp/tag" "$TAG"
if [ "$mode" = pr ]; then
  git worktree add --quiet --detach "$tmp/base" "$base"
fi

found=0
failed=
for name in $STABLE; do
  pkg="$MODULE/pkg/agentscope/$name"
  exp="$tmp/$name.exp"
  echo "=== $pkg vs $TAG ==="
  if ! (cd "$tmp/tag" && apidiff -w "$exp" "$pkg"); then
    echo "ERROR: apidiff could not load $pkg at $TAG"
    failed="$failed $name"
    continue
  fi
  if ! apidiff -incompatible "$exp" "$pkg" >"$tmp/$name.head"; then
    cat "$tmp/$name.head"
    echo "ERROR: apidiff could not compare $pkg"
    failed="$failed $name"
    continue
  fi
  cat "$tmp/$name.head"
  if [ "$mode" = report ]; then
    if [ -s "$tmp/$name.head" ]; then
      found=1
    fi
    continue
  fi
  if ! (cd "$tmp/base" && apidiff -incompatible "$exp" "$pkg") >"$tmp/$name.base"; then
    cat "$tmp/$name.base"
    echo "ERROR: apidiff could not compare $pkg at the pull request base"
    failed="$failed $name"
    continue
  fi
  # Keep the lines the base does not report. apidiff prints one "- ..." line
  # per change and nothing when there is none, so an empty base file excludes
  # nothing.
  status=0
  grep -vxF -f "$tmp/$name.base" "$tmp/$name.head" >"$tmp/$name.new" || status=$?
  case $status in
    0)
      found=1
      echo "Not in the pull request base:"
      cat "$tmp/$name.new"
      ;;
    1) ;;
    *)
      echo "ERROR: could not compare the $name findings with the pull request base"
      failed="$failed $name"
      ;;
  esac
done

if [ -n "$failed" ]; then
  echo "ERROR: could not check:$failed. Fix the errors above; the result is incomplete until every Stable-tier package is checked."
  exit 1
fi
if [ "$found" = 0 ]; then
  if [ "$mode" = pr ]; then
    echo "No incompatible changes to Stable-tier packages beyond those already in the pull request base."
  else
    echo "No incompatible changes to Stable-tier packages since $TAG."
  fi
  exit 0
fi
if [ "$mode" = report ]; then
  echo "::warning::Incompatible changes to Stable-tier packages since $TAG (report only; see the log)."
  exit 0
fi
if [ "${BREAKING_CHANGE_LABEL:-false}" = true ]; then
  echo "::warning::Incompatible changes to Stable-tier packages accepted by the breaking-change label."
  exit 0
fi
echo "ERROR: incompatible changes to Stable-tier packages. Add the breaking-change label and declare the break per STABILITY.md."
exit 1
