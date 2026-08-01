#!/usr/bin/env bash
# Copyright 2026 The Gitea Authors. All rights reserved.
# SPDX-License-Identifier: MIT
#
# Upstream-sync blast-radius guard (issue #58, epic #53).
#
# Fails when a pull request touches a path reserved by the in-flight upstream
# sync cherry-picks (#43-#51) while the tracker issue #43 is still open.
#
# Usage:
#   check-blast-radius.sh --changed <file|-> [--list <file>]
#
#   --changed  file with one changed path per line ("-" reads stdin)
#   --list     blast-radius list (default: .terraphim/sync-blast-radius.txt)
#
# Environment:
#   GUARD_ISSUE       issue index that gates the check (default: 43)
#   GITEA_API_URL     API base, e.g. https://host/api/v1 (required unless
#                     BLAST_RADIUS_ISSUE_FETCH is set)
#   GITEA_REPO        owner/repo (required unless BLAST_RADIUS_ISSUE_FETCH is set)
#   GITEA_API_TOKEN   token for the issue lookup (optional for public repos)
#   BLAST_RADIUS_ISSUE_FETCH
#                     command printing the issue JSON; overrides the HTTP call
#                     (used by the unit tests to stub the API)
#
# Exit codes: 0 = pass or skipped, 1 = blast-radius violation, 2 = usage/error.

set -euo pipefail

SCRIPT_NAME=$(basename "$0")
LIST_FILE=".terraphim/sync-blast-radius.txt"
CHANGED_FILE=""
GUARD_ISSUE="${GUARD_ISSUE:-43}"

die() {
  echo "${SCRIPT_NAME}: $*" >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --list)
      [ $# -ge 2 ] || die "--list needs a value"
      LIST_FILE="$2"
      shift 2
      ;;
    --changed)
      [ $# -ge 2 ] || die "--changed needs a value"
      CHANGED_FILE="$2"
      shift 2
      ;;
    -h | --help)
      sed -n '4,26p' "$0"
      exit 0
      ;;
    *)
      die "unknown argument: $1"
      ;;
  esac
done

[ -n "$CHANGED_FILE" ] || die "--changed is required"
[ -f "$LIST_FILE" ] || die "blast-radius list not found: $LIST_FILE"

# --- 1. gate on the tracker issue state ------------------------------------
fetch_issue() {
  if [ -n "${BLAST_RADIUS_ISSUE_FETCH:-}" ]; then
    eval "$BLAST_RADIUS_ISSUE_FETCH"
    return
  fi
  [ -n "${GITEA_API_URL:-}" ] || die "GITEA_API_URL is not set"
  [ -n "${GITEA_REPO:-}" ] || die "GITEA_REPO is not set"
  local url="${GITEA_API_URL%/}/repos/${GITEA_REPO}/issues/${GUARD_ISSUE}"
  if [ -n "${GITEA_API_TOKEN:-}" ]; then
    curl -sSf -m 30 -H "Authorization: token ${GITEA_API_TOKEN}" "$url"
  else
    curl -sSf -m 30 "$url"
  fi
}

# `.state` is the documented "open" | "closed" enum on
# GET /api/v1/repos/{owner}/{repo}/issues/{index}.
issue_json=$(fetch_issue) || die "failed to query issue #${GUARD_ISSUE}"
issue_state=$(printf '%s' "$issue_json" |
  tr ',{}' '\n\n\n' |
  sed -n 's/.*"state"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' |
  head -n 1)

case "$issue_state" in
  open) ;;
  closed)
    echo "issue #${GUARD_ISSUE} is closed - blast-radius guard skipped"
    exit 0
    ;;
  *)
    die "unexpected state for issue #${GUARD_ISSUE}: '${issue_state:-<empty>}'"
    ;;
esac

# --- 2. load the list -------------------------------------------------------
patterns=()
while IFS= read -r raw || [ -n "$raw" ]; do
  entry="${raw%%#*}"
  # strip surrounding whitespace
  entry="${entry#"${entry%%[![:space:]]*}"}"
  entry="${entry%"${entry##*[![:space:]]}"}"
  [ -n "$entry" ] || continue
  patterns+=("$entry")
done < "$LIST_FILE"

[ "${#patterns[@]}" -gt 0 ] || die "no entries in $LIST_FILE"

# --- 3. match the changed paths --------------------------------------------
if [ "$CHANGED_FILE" = "-" ]; then
  changed_input=$(cat)
else
  [ -f "$CHANGED_FILE" ] || die "changed-paths file not found: $CHANGED_FILE"
  changed_input=$(cat "$CHANGED_FILE")
fi

violations=()
while IFS= read -r path || [ -n "$path" ]; do
  # tolerate `git diff --name-status` input: drop a leading status column
  path="${path#*$'\t'}"
  path="${path#"${path%%[![:space:]]*}"}"
  path="${path%"${path##*[![:space:]]}"}"
  [ -n "$path" ] || continue
  for pattern in "${patterns[@]}"; do
    # shellcheck disable=SC2053 # intentional glob match against the entry
    if [ "$path" = "$pattern" ] || [[ $path == $pattern ]]; then
      violations+=("$path -> $pattern")
      break
    fi
  done
done <<< "$changed_input"

if [ "${#violations[@]}" -gt 0 ]; then
  echo "blast-radius violation: issue #${GUARD_ISSUE} is open and this PR touches"
  echo "paths reserved for the upstream sync cherry-picks (#43-#51):"
  for violation in "${violations[@]}"; do
    echo "  ${violation}"
  done
  echo
  echo "Rebase the uplift work off these paths, or wait for #${GUARD_ISSUE} to close."
  echo "List: ${LIST_FILE}"
  exit 1
fi

echo "blast-radius guard: no reserved paths touched (${LIST_FILE})"
exit 0
