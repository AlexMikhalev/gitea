#!/usr/bin/env bash
# Copyright 2026 The Gitea Authors. All rights reserved.
# SPDX-License-Identifier: MIT
#
# Changed-path collector for the upstream-sync blast-radius guard (issue #58).
#
# Emits the paths a pull request changed *relative to the merge base* with its
# base branch - i.e. the three-dot `git diff base...head` range.
#
# A two-dot `git diff base head` would be wrong here: `pull_request.base.sha`
# is the tip of the base branch at event time, not the merge base, so every
# commit landed on the base branch since the PR forked would be reported as
# "changed by this PR" (with the sign inverted). Since the whole point of #43
# is to land commits on the reserved paths, a two-dot range makes the guard
# fire on innocent PRs by construction.
#
# Rename detection is disabled (`--no-renames`), and that is load-bearing. With
# git's default `diff.renames=true`, `--name-only` prints only the DESTINATION
# of a detected rename, so `git mv services/lfs/server.go services/lfshandler/`
# emits the new path alone and the reserved source path never reaches
# check-blast-radius.sh - the guard reports PASSED on the single most
# conflict-inducing thing a PR can do to an in-flight cherry-pick. A plain
# deletion is caught (it is reported by path), so the hole was renames only.
# `--no-renames` decomposes the rename into a delete plus an add, which puts
# both paths on the list and makes the reserved source match.
#
# Output is NUL-terminated (`git diff -z`), and that is load-bearing for the
# same reason. Without -z, `core.quotePath` - which defaults to TRUE - makes git
# C-quote any path holding a non-ASCII or control byte, so `models/auth/héllo.go`
# is emitted as `"models/auth/h\303\251llo.go"`; check-blast-radius.sh compares
# the line verbatim against each list entry, and the leading `"` makes every
# pattern fail. A file added under a reserved glob subtree with a non-ASCII name
# was therefore reported PASSED. `-z` suppresses the quoting entirely rather
# than narrowing it: `core.quotePath=false` alone still quotes a path containing
# a newline (verified on git 2.25), which would leave the same hole one byte
# further out.
#
# Both flags are on the command line, where they outrank every config scope, and
# together they are what make the verdict independent of the runner's git
# configuration - without them, `diff.renames=false` or `core.quotePath=false`
# on one box and the defaults on another give two different answers for the same
# PR, with no signal that they disagree.
#
# Usage:
#   blast-radius-diff.sh --base <rev> [--head <rev>] [--output <file>]
#
#   --base    base revision (usually github.event.pull_request.base.sha)
#   --head    head revision (default: HEAD)
#   --output  write the paths here (default: stdout)
#
# Environment: none, deliberately - see check-blast-radius.sh's header. This
# script runs in the same job, on the same self-hosted runner, under the same
# threat model as the verdict script, so it reads no environment variable
# either; the fetch remote is hardcoded to `origin`. check-blast-radius_test.sh
# asserts that for both files.
#
# Exit codes: 0 = ok, 2 = usage/error. Failures are loud: an unresolvable base
# or merge base aborts with a diagnosable message instead of an opaque
# "unknown revision" from git diff.

set -euo pipefail

SCRIPT_NAME=$(basename "$0")
BASE_REV=""
HEAD_REV="HEAD"
OUTPUT=""
FETCH_REMOTE="origin"

die() {
  echo "${SCRIPT_NAME}: $*" >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --base)
      [ $# -ge 2 ] || die "--base needs a value"
      BASE_REV="$2"
      shift 2
      ;;
    --head)
      [ $# -ge 2 ] || die "--head needs a value"
      HEAD_REV="$2"
      shift 2
      ;;
    --output)
      [ $# -ge 2 ] || die "--output needs a value"
      OUTPUT="$2"
      shift 2
      ;;
    -h | --help)
      # print the header block, i.e. from line 4 to the first blank line - a
      # fixed line range silently truncates the help every time it is edited
      sed -n '4,/^$/p' "$0"
      exit 0
      ;;
    *)
      die "unknown argument: $1"
      ;;
  esac
done

[ -n "$BASE_REV" ] || die "--base is required"

have_commit() {
  git cat-file -e "${1}^{commit}" 2> /dev/null
}

if ! have_commit "$BASE_REV"; then
  echo "${SCRIPT_NAME}: base rev ${BASE_REV} is not local, fetching from ${FETCH_REMOTE}" >&2
  # Keep the reason. Fetching a bare SHA needs uploadpack.allowReachableSHA1InWant
  # (or allowAnySHA1InWant) on the server; where that is off, git says so
  # precisely, and swallowing it sends the operator to inspect a fetch-depth
  # that is already 0. A missing `origin` reports itself just as precisely.
  fetch_err=$(git fetch --no-tags --quiet "$FETCH_REMOTE" "$BASE_REV" 2>&1) || true
  have_commit "$BASE_REV" ||
    die "cannot resolve base revision '${BASE_REV}' - the checkout needs the base branch history (fetch-depth: 0), or the server must allow fetching a bare SHA. git fetch said: ${fetch_err:-<no git output>}"
fi

have_commit "$HEAD_REV" || die "cannot resolve head revision '${HEAD_REV}'"

merge_base=$(git merge-base "$BASE_REV" "$HEAD_REV") ||
  die "no merge base between '${BASE_REV}' and '${HEAD_REV}' - is the base branch history present?"

# --no-renames: report the source path of a rename too, not just the
# destination. -z: NUL-terminate the records and stop C-quoting non-ASCII and
# control bytes, which no list entry can match. See the header. Both override
# the corresponding config (diff.renames, core.quotePath) from any scope, so the
# output shape does not depend on the runner's git setup.
if [ -n "$OUTPUT" ]; then
  git diff -z --name-only --no-renames "$merge_base" "$HEAD_REV" > "$OUTPUT"
else
  git diff -z --name-only --no-renames "$merge_base" "$HEAD_REV"
fi
