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
# Usage:
#   blast-radius-diff.sh --base <rev> [--head <rev>] [--output <file>]
#
#   --base    base revision (usually github.event.pull_request.base.sha)
#   --head    head revision (default: HEAD)
#   --output  write the paths here (default: stdout)
#
# Environment:
#   BLAST_RADIUS_FETCH_REMOTE  remote to fetch a missing base rev from
#                              (default: origin; empty disables the fetch)
#
# Exit codes: 0 = ok, 2 = usage/error. Failures are loud: an unresolvable base
# or merge base aborts with a diagnosable message instead of an opaque
# "unknown revision" from git diff.

set -euo pipefail

SCRIPT_NAME=$(basename "$0")
BASE_REV=""
HEAD_REV="HEAD"
OUTPUT=""
FETCH_REMOTE="${BLAST_RADIUS_FETCH_REMOTE-origin}"

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
  fetch_err=""
  if [ -n "$FETCH_REMOTE" ]; then
    echo "${SCRIPT_NAME}: base rev ${BASE_REV} is not local, fetching from ${FETCH_REMOTE}" >&2
    # Keep the reason. Fetching a bare SHA needs uploadpack.allowReachableSHA1InWant
    # (or allowAnySHA1InWant) on the server; where that is off, git says so
    # precisely, and swallowing it sends the operator to inspect a fetch-depth
    # that is already 0.
    fetch_err=$(git fetch --no-tags --quiet "$FETCH_REMOTE" "$BASE_REV" 2>&1) || true
  fi
  have_commit "$BASE_REV" ||
    die "cannot resolve base revision '${BASE_REV}' - the checkout needs the base branch history (fetch-depth: 0), or the server must allow fetching a bare SHA. git fetch said: ${fetch_err:-<not attempted>}"
fi

have_commit "$HEAD_REV" || die "cannot resolve head revision '${HEAD_REV}'"

merge_base=$(git merge-base "$BASE_REV" "$HEAD_REV") ||
  die "no merge base between '${BASE_REV}' and '${HEAD_REV}' - is the base branch history present?"

if [ -n "$OUTPUT" ]; then
  git diff --name-only "$merge_base" "$HEAD_REV" > "$OUTPUT"
else
  git diff --name-only "$merge_base" "$HEAD_REV"
fi
