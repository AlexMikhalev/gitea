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
#   GUARD_EXEMPT_LABEL
#                     name of the PR label that exempts a PR from the guard.
#                     Only used to make the failure message actionable; the
#                     exemption itself is applied by the workflow.
#   GITEA_API_URL     API base, e.g. https://host/api/v1 (required unless
#                     BLAST_RADIUS_ISSUE_FETCH is set)
#   GITEA_REPO        owner/repo (required unless BLAST_RADIUS_ISSUE_FETCH is set)
#   GITEA_API_TOKEN   token for the issue lookup (optional for public repos)
#   BLAST_RADIUS_ISSUE_FETCH
#                     command printing the issue JSON; overrides the HTTP call
#                     (used by the unit tests to stub the API)
#   BLAST_RADIUS_NO_JQ
#                     when set, skip jq and use the awk state parser (used by
#                     the unit tests to exercise the dependency-free path)
#
# Exit codes: 0 = pass or skipped, 1 = blast-radius violation, 2 = usage/error.

set -euo pipefail

SCRIPT_NAME=$(basename "$0")
LIST_FILE=".terraphim/sync-blast-radius.txt"
CHANGED_FILE=""
GUARD_ISSUE="${GUARD_ISSUE:-43}"
GUARD_EXEMPT_LABEL="${GUARD_EXEMPT_LABEL:-}"

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
      sed -n '4,31p' "$0"
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
#
# It MUST be read from the top level of the payload. An issue carries nested
# objects that have a "state" of their own - `milestone` (serialised before
# `state` in modules/structs/issue.go), `pull_request`, `repository` - so a
# first-textual-match parse silently returns the milestone's state and the
# guard disables itself whenever #43 is attached to a closed milestone.
#
# jq is used when present; the awk fallback keeps the script dependency-free by
# discarding everything nested inside sub-objects/arrays (string contents are
# tracked, so braces inside the issue body do not confuse it) and then
# requiring exactly one surviving top-level "state" key.
extract_top_level_state() {
  if [ -z "${BLAST_RADIUS_NO_JQ:-}" ] && command -v jq > /dev/null 2>&1; then
    jq -er 'if type == "object" and has("state") then .state else empty end'
    return
  fi
  awk '
    { buf = buf $0 "\n" }
    END {
      n = length(buf)
      depth = 0; instr = 0; esc = 0
      top = ""; nparts = 0; chunk = ""
      for (i = 1; i <= n; i++) {
        c = substr(buf, i, 1)
        keep = 0
        if (instr) {
          keep = (depth <= 1)
          if (esc) { esc = 0 }
          else if (c == "\\") { esc = 1 }
          else if (c == "\"") { instr = 0 }
        } else if (c == "\"") {
          instr = 1; keep = (depth <= 1)
        } else if (c == "{" || c == "[") {
          depth++; keep = (depth <= 1)
        } else if (c == "}" || c == "]") {
          keep = (depth <= 1); depth--
        } else {
          keep = (depth <= 1)
        }
        if (keep) {
          chunk = chunk c
          if (length(chunk) >= 2048) { parts[++nparts] = chunk; chunk = "" }
        }
      }
      for (p = 1; p <= nparts; p++) top = top parts[p]
      top = top chunk

      count = 0; value = ""
      while (match(top, /"state"[ \t\r\n]*:[ \t\r\n]*"[^"]*"/)) {
        m = substr(top, RSTART, RLENGTH)
        sub(/^"state"[ \t\r\n]*:[ \t\r\n]*"/, "", m)
        sub(/"$/, "", m)
        value = m; count++
        top = substr(top, RSTART + RLENGTH)
      }
      if (count == 1) { print value; exit 0 }
      exit 1
    }
  '
}

issue_json=$(fetch_issue) || die "failed to query issue #${GUARD_ISSUE}"
issue_state=$(printf '%s' "$issue_json" | extract_top_level_state) || issue_state=""

case "$issue_state" in
  open) ;;
  closed)
    echo "blast-radius guard: SKIPPED - tracker issue #${GUARD_ISSUE} is closed"
    exit 0
    ;;
  *)
    die "could not read a single top-level state for issue #${GUARD_ISSUE}: '${issue_state:-<empty>}'"
    ;;
esac

# --- 2. load the list -------------------------------------------------------
# Counters are tracked explicitly: `${#array[@]}` on an empty array trips
# `set -u` on bash < 4.4 (including the bash 3.2 shipped on macOS), which would
# turn a clean run into exit 2.
patterns=()
pattern_count=0
while IFS= read -r raw || [ -n "$raw" ]; do
  entry="${raw%%#*}"
  # strip surrounding whitespace
  entry="${entry#"${entry%%[![:space:]]*}"}"
  entry="${entry%"${entry##*[![:space:]]}"}"
  [ -n "$entry" ] || continue
  patterns+=("$entry")
  pattern_count=$((pattern_count + 1))
done < "$LIST_FILE"

[ "$pattern_count" -gt 0 ] || die "no entries in $LIST_FILE"

# --- 3. match the changed paths --------------------------------------------
if [ "$CHANGED_FILE" = "-" ]; then
  changed_input=$(cat)
else
  [ -f "$CHANGED_FILE" ] || die "changed-paths file not found: $CHANGED_FILE"
  changed_input=$(cat "$CHANGED_FILE")
fi

violations=()
violation_count=0
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
      violation_count=$((violation_count + 1))
      break
    fi
  done
done <<< "$changed_input"

if [ "$violation_count" -gt 0 ]; then
  echo "blast-radius violation: issue #${GUARD_ISSUE} is open and this PR touches"
  echo "paths reserved for the upstream sync cherry-picks (#43-#51):"
  for violation in "${violations[@]+"${violations[@]}"}"; do
    echo "  ${violation}"
  done
  echo
  echo "Rebase the uplift work off these paths, or wait for #${GUARD_ISSUE} to close."
  if [ -n "$GUARD_EXEMPT_LABEL" ]; then
    echo "If this PR *is* part of the sync itself, add the '${GUARD_EXEMPT_LABEL}'"
    echo "label - the exemption is then recorded on the PR and logged by the job."
  fi
  echo "List: ${LIST_FILE}"
  exit 1
fi

echo "blast-radius guard: PASSED - no reserved paths touched (${LIST_FILE})"
exit 0
