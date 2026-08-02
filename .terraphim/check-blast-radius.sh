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
#   check-blast-radius.sh --changed <file|-> [--list <file>] [--issue <index>]
#                         [--repo <owner/name>]
#
#   --changed  changed paths, NUL-terminated or one per line ("-" reads stdin)
#   --list     blast-radius list (default: .terraphim/sync-blast-radius.txt)
#   --issue    tracker issue index that gates the check (default: 43)
#   --repo     owner/name the tracker issue lives in (default: terraphim/gitea)
#
# Environment:
#   GITEA_API_URL     API base, e.g. https://host/api/v1 (required)
#   GITEA_API_TOKEN   token for the issue lookup (optional for public repos)
#   GUARD_EXEMPT_LABEL
#                     name of the PR label that exempts a PR from the guard.
#                     Message text only: it is interpolated into the failure
#                     output and read nowhere else, so it cannot move the
#                     verdict. The exemption itself is applied by the workflow.
#
# No environment variable *that this script reads* can change the verdict, and
# each of the ways one could is closed separately. The API call is a literal
# curl with no command variable, so the lookup has no override. The parser
# branch is chosen only by `command -v jq`, so it has no override either. And
# the two inputs that decide *what* is looked up - which issue, and in which
# repository - are arguments (--issue, --repo), not env reads: the workflow
# passes both, so their values live in a file on the base branch that the PR
# under test cannot influence. Those two are the ones worth spelling out,
# because either one left in the environment is an env-settable verdict.
# Pointing the guard at any already-closed issue produces SKIPPED and exit 0 -
# and so does pointing it at any *repository* whose issue #43 is closed, which
# is every repository that does not have one. On a self-hosted runner anything
# able to write to the job environment would then be able to do exactly that.
# The answer is scope-checked as well as parsed: its top-level "number" must be
# the index that was requested, so an endpoint replying about some other issue
# is exit 2 rather than a verdict.
#
# The two remaining GITEA_* variables are runner-supplied connection details,
# and they are NOT immune - stated rather than claimed away. GITEA_API_TOKEN
# cannot flip the verdict by itself: a wrong token makes `curl -sSf` fail, and
# a failed lookup is exit 2. GITEA_API_URL can - point it at any reachable host
# that answers this repo path with a closed issue #43 and the result is
# SKIPPED, exit 0. No parse of the reply closes that, because the endpoint
# writes the reply. It falls under the same trusted-runner assumption as PATH
# below: the workflow sets it from a base-branch expression (github.api_url),
# and anything able to rewrite it in the job environment could rewrite PATH
# instead. What is closed is every seam a PR could reach, and both of the
# inputs that select the issue whose state is the verdict.
#
# The excluded assumption, stated rather than hidden: both `curl` and `jq` are
# still resolved through PATH, and PATH is an environment variable. Placing an
# executable on it substitutes either one - which is exactly how the test suite
# below does it. So the claim is scoped to the variables this script names; the
# guard assumes a trusted PATH on the runner, which is implied anyway by its
# ability to execute the pinned scripts at all, and under pull_request_target no
# PR-authored file is ever placed on the runner.
#
# check-blast-radius_test.sh therefore stubs `curl` and hides `jq` by running
# the script with a PATH of its own, and pins the property above by running the
# script with GUARD_ISSUE, GITEA_REPO, GUARD_REPO and GUARD_EXEMPT_LABEL set to
# hostile values and asserting the verdict does not move - a behavioural check,
# not a grep for a name pattern that any future seam could simply avoid
# matching. Its stub answers by repository *and* index, so "the argument
# redirects the lookup" and "the environment does not" are distinguishable
# rather than both passing for free.
#
# The rule is guard-wide, not file-wide: blast-radius-diff.sh runs in the same
# job on the same runner, so it reads no environment variable at all (its fetch
# remote is hardcoded to `origin`). The suite asserts "no eval" against both
# files.
#
# Exit codes: 0 = pass or skipped, 1 = blast-radius violation, 2 = usage/error.

set -euo pipefail

SCRIPT_NAME=$(basename "$0")
LIST_FILE=".terraphim/sync-blast-radius.txt"
CHANGED_FILE=""
# Deliberately NOT read from the environment - no `GUARD_ISSUE:-43` fallback
# and no GITEA_REPO one: see the header. (Spelled without the expansion syntax
# on purpose - the suite's env-read scanner reads this file as text, and a name
# written as an expansion in a comment is indistinguishable from a real seam.)
# Together these two select the issue whose state decides the verdict, so
# reading either from the environment would hand the verdict to anything that
# can write to the job environment. The defaults are what a manual run uses;
# the workflow passes both explicitly from base-branch expressions.
GUARD_ISSUE="43"
GUARD_REPO="terraphim/gitea"
# Message text only; it is never compared against anything.
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
    --issue)
      [ $# -ge 2 ] || die "--issue needs a value"
      # The value goes into a URL path. Constrain it to an issue index so a
      # typo cannot turn the lookup into a different endpoint.
      case "$2" in
        '' | *[!0-9]*) die "--issue needs a positive integer issue index, got '$2'" ;;
      esac
      [ "$2" -gt 0 ] || die "--issue needs a positive integer issue index, got '$2'"
      GUARD_ISSUE="$2"
      shift 2
      ;;
    --repo)
      [ $# -ge 2 ] || die "--repo needs a value"
      # The value goes into a URL path, and it selects *which* repository's
      # issue decides the verdict. Constrain it to exactly one owner/name pair
      # over the characters Gitea allows in either, so that neither a traversal
      # nor an extra path segment can turn the lookup into a different endpoint.
      case "$2" in
        */*/*) die "--repo needs one owner/name pair, got '$2'" ;;
        */*) ;;
        *) die "--repo needs one owner/name pair, got '$2'" ;;
      esac
      case "${2%%/*}" in
        '' | . | .. | *[!A-Za-z0-9._-]*) die "--repo has an invalid owner: '$2'" ;;
      esac
      case "${2#*/}" in
        '' | . | .. | *[!A-Za-z0-9._-]*) die "--repo has an invalid repository name: '$2'" ;;
      esac
      GUARD_REPO="$2"
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

[ -n "$CHANGED_FILE" ] || die "--changed is required"
[ -f "$LIST_FILE" ] || die "blast-radius list not found: $LIST_FILE"

# --- 1. gate on the tracker issue state ------------------------------------
fetch_issue() {
  [ -n "${GITEA_API_URL:-}" ] || die "GITEA_API_URL is not set"
  local url="${GITEA_API_URL%/}/repos/${GUARD_REPO}/issues/${GUARD_ISSUE}"
  if [ -n "${GITEA_API_TOKEN:-}" ]; then
    curl -sSf -m 30 -H "Authorization: token ${GITEA_API_TOKEN}" "$url"
  else
    curl -sSf -m 30 "$url"
  fi
}

# Reads one scalar from the TOP LEVEL of the payload:
#
#   extract_top_level state  string  -> the documented "open" | "closed" enum
#   extract_top_level number number  -> the issue index the answer is about
#
# on GET /api/v1/repos/{owner}/{repo}/issues/{index}.
#
# Top level is load-bearing for both. An issue carries nested objects that have
# a "state" of their own - `milestone` (serialised before `state` in
# modules/structs/issue.go), `pull_request`, `repository` - so a
# first-textual-match parse silently returns the milestone's state and the
# guard disables itself whenever #43 is attached to a closed milestone. The
# same holds for "number", which `pull_request` and `milestone` payloads also
# carry.
#
# jq is used when it is on PATH; the awk fallback keeps the script
# dependency-free by discarding everything nested inside sub-objects/arrays
# (string contents are tracked, so braces inside the issue body do not confuse
# it) and then requiring exactly one surviving top-level key of the wanted
# name. The branch is chosen only by whether jq exists - the tests exercise the
# fallback by running with a PATH that has no jq on it, not by an override
# variable. Both branches also require the value to have the wanted JSON type,
# so a `"number":"43"` string does not satisfy the index scope check below.
extract_top_level() {
  local key="$1"
  local kind="$2"
  if command -v jq > /dev/null 2>&1; then
    # One literal filter per supported pair, rather than passing the key in
    # with `--arg` and indexing by a jq variable: a jq program is just text to
    # this script, so a jq variable inside one reads as an unbound shell
    # expansion to the suite's env-read scanner - and the scanner is not the
    # thing to loosen. (Same reason this comment names none of them.) An
    # unrecognised pair is a programming error and fails closed like every
    # other unreadable lookup.
    case "${key} ${kind}" in
      'state string')
        jq -er 'if type == "object" and (.state | type) == "string" then .state else empty end'
        ;;
      'number number')
        jq -er 'if type == "object" and (.number | type) == "number" then (.number | tostring) else empty end'
        ;;
      *) die "internal error: no jq filter for top-level ${key} (${kind})" ;;
    esac
    return
  fi
  awk -v key="$key" -v kind="$kind" '
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

      # A string value cannot contain an unescaped quote, so "[^\"]*" is the
      # whole value; a number is matched as digits, which is also what makes a
      # quoted "43" fail the number lookup rather than pass it.
      head = "\"" key "\"[ \t\r\n]*:[ \t\r\n]*"
      if (kind == "number") { pat = head "-?[0-9]+" } else { pat = head "\"[^\"]*\"" }

      count = 0; value = ""
      while (match(top, pat)) {
        m = substr(top, RSTART, RLENGTH)
        sub("^" head, "", m)
        sub(/^"/, "", m)
        sub(/"$/, "", m)
        value = m; count++
        top = substr(top, RSTART + RLENGTH)
      }
      if (count == 1) { print value; exit 0 }
      exit 1
    }
  '
}

issue_json=$(fetch_issue) || die "failed to query issue ${GUARD_REPO}#${GUARD_ISSUE}"

# Scope check before the answer is trusted: it must be *about* the issue that
# was asked for. Only "closed" can turn into a pass, so an endpoint answering
# with some unrelated issue would otherwise be a silent SKIPPED; here it is
# exit 2. This does not make a hostile GITEA_API_URL safe - see the header -
# but it does mean a redirected or path-rewriting one has to forge the index
# rather than merely answer.
issue_number=$(printf '%s' "$issue_json" | extract_top_level number number) || issue_number=""
[ "$issue_number" = "$GUARD_ISSUE" ] ||
  die "the lookup for ${GUARD_REPO}#${GUARD_ISSUE} answered about issue '${issue_number:-<none>}'"

issue_state=$(printf '%s' "$issue_json" | extract_top_level state string) || issue_state=""

case "$issue_state" in
  open) ;;
  closed)
    echo "blast-radius guard: SKIPPED - tracker issue ${GUARD_REPO}#${GUARD_ISSUE} is closed"
    exit 0
    ;;
  *)
    die "could not read a single top-level state for issue ${GUARD_REPO}#${GUARD_ISSUE}: '${issue_state:-<empty>}'"
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
# Input format. blast-radius-diff.sh emits `git diff -z`, i.e. NUL-terminated
# records, so every byte of a path other than the terminator reaches the matcher
# verbatim. That is load-bearing: `git diff --name-only` without -z C-quotes any
# path holding a non-ASCII or control byte (core.quotePath defaults to true), and
# a leading `"` makes every pattern below fail - a file added under a reserved
# glob subtree with a non-ASCII name was silently reported PASSED.
#
# Newline-delimited input is still accepted, because it is what a human pipes
# into `--changed -` by hand and what every hand-written case in the suite uses:
# whatever follows the last NUL - the whole input, when there is none - is split
# on newlines instead. Those records are normalised leniently (trimmed, with a
# leading `git diff --name-status` status column dropped); NUL records are taken
# exactly as they are, since that format is unambiguous and a path may legally
# contain a tab or a leading space.
violations=()
violation_count=0

match_path() {
  local path="$1" pattern
  [ -n "$path" ] || return 0
  for pattern in "${patterns[@]}"; do
    # shellcheck disable=SC2053 # intentional glob match against the entry
    if [ "$path" = "$pattern" ] || [[ $path == $pattern ]]; then
      violations+=("$path -> $pattern")
      violation_count=$((violation_count + 1))
      return 0
    fi
  done
}

# Reads the changed paths from stdin. Not a subshell: a redirect on a function
# call is not one, so the arrays above are the ones being appended to.
read_changed() {
  local rec="" line
  while IFS= read -r -d '' rec; do
    match_path "$rec"
  done
  # `read` leaves what it consumed before EOF in $rec, so this is the tail after
  # the last NUL - i.e. all of a newline-delimited input.
  [ -n "$rec" ] || return 0
  while IFS= read -r line || [ -n "$line" ]; do
    # tolerate `git diff --name-status` input: drop a leading status column
    line="${line#*$'\t'}"
    line="${line#"${line%%[![:space:]]*}"}"
    line="${line%"${line##*[![:space:]]}"}"
    match_path "$line"
  done <<< "$rec"
}

if [ "$CHANGED_FILE" = "-" ]; then
  read_changed
else
  [ -f "$CHANGED_FILE" ] || die "changed-paths file not found: $CHANGED_FILE"
  read_changed < "$CHANGED_FILE"
fi

if [ "$violation_count" -gt 0 ]; then
  echo "blast-radius violation: issue #${GUARD_ISSUE} is open and this PR touches"
  echo "paths reserved for the upstream sync cherry-picks (#43-#51):"
  for violation in "${violations[@]+"${violations[@]}"}"; do
    echo "  ${violation}"
  done
  echo
  echo "Rebase the uplift work off these paths, or wait for #${GUARD_ISSUE} to close."
  if [ -n "$GUARD_EXEMPT_LABEL" ]; then
    echo "If this PR *is* part of the sync itself, or maintains the guard, ask a"
    echo "maintainer to apply the '${GUARD_EXEMPT_LABEL}' label - the exemption is then"
    echo "recorded on the PR and logged by the job. Applying it needs repository"
    echo "write access; if the label does not exist yet, the maintainer creates it"
    echo "(see docs/plans/design-blast-radius-guard-2026-08-01.md, \"Setup\")."
  fi
  echo "List: ${LIST_FILE}"
  exit 1
fi

echo "blast-radius guard: PASSED - no reserved paths touched (${LIST_FILE})"
exit 0
