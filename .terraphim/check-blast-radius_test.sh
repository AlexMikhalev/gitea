#!/usr/bin/env bash
# Copyright 2026 The Gitea Authors. All rights reserved.
# SPDX-License-Identifier: MIT
#
# Unit tests for check-blast-radius.sh and blast-radius-diff.sh. No network:
# the issue endpoint is stubbed through BLAST_RADIUS_ISSUE_FETCH, and the
# diff-range cases build a throwaway git repository under $TMPDIR.
#
# Run: .terraphim/check-blast-radius_test.sh

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SCRIPT="${HERE}/check-blast-radius.sh"
DIFF_SCRIPT="${HERE}/blast-radius-diff.sh"
REAL_LIST="${HERE}/sync-blast-radius.txt"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

OPEN_JSON='{"number":43,"title":"tracker","state":"open","comments":1}'
CLOSED_JSON='{"number":43,"title":"tracker","state":"closed","closed_at":"2026-08-01T00:00:00Z"}'

# Realistic payload shape: modules/structs/issue.go serialises `milestone`
# (which carries a "state" of its own) BEFORE the issue's own `state`, and
# `user`, `labels`, `repository` and `pull_request` are nested objects too. A
# parser that takes the first textual "state" match reads the milestone here.
NESTED_OPEN_JSON='{"id":9,"number":43,"user":{"id":1,"login":"sync-owner","active":true},"labels":[{"id":3,"name":"sync","description":"{not json}"}],"milestone":{"id":1,"title":"upstream sync","state":"closed","open_issues":0,"closed_issues":9},"state":"open","pull_request":null,"repository":{"id":2,"name":"gitea","full_name":"terraphim/gitea"},"comments":1}'
NESTED_CLOSED_JSON='{"id":9,"number":43,"milestone":{"id":1,"title":"upstream sync","state":"open","open_issues":3},"repository":{"id":2,"name":"gitea"},"state":"closed","comments":1}'
# an issue body may contain braces, quotes and even a "state" mention
NESTED_BODY_JSON='{"id":9,"number":43,"body":"see {\"state\": \"closed\"} in the payload example","milestone":{"id":1,"state":"closed"},"state":"open"}'

pass=0
fail=0

ok() {
  pass=$((pass + 1))
  printf 'ok   %-46s\n' "$1"
}

no() {
  fail=$((fail + 1))
  printf 'FAIL %-46s %s\n' "$1" "${2:-}"
}

# run_case <name> <expected-exit> <issue-json> <list-content> <changed-content>
run_case() {
  local name="$1" want="$2" issue="$3" list="$4" changed="$5"
  local list_file="${TMP}/list.txt" changed_file="${TMP}/changed.txt"
  printf '%s\n' "$list" > "$list_file"
  printf '%s' "$changed" > "$changed_file"

  local out got
  out=$(BLAST_RADIUS_ISSUE_FETCH="printf '%s' '${issue}'" \
    "$SCRIPT" --list "$list_file" --changed "$changed_file" 2>&1)
  got=$?

  if [ "$got" -eq "$want" ]; then
    pass=$((pass + 1))
    printf 'ok   %-46s (exit %s)\n' "$name" "$got"
  else
    fail=$((fail + 1))
    printf 'FAIL %-46s (want exit %s, got %s)\n%s\n' "$name" "$want" "$got" "$out"
  fi
}

HEADER='# uplift PRs must not modify these files until #43 closes'

# --- matcher table: exact / glob / non-match / added-only / empty -----------
run_case 'exact match fails' 1 "$OPEN_JSON" \
  "${HEADER}"$'\nmodels/auth/oauth2.go' 'models/auth/oauth2.go'

run_case 'glob match fails' 1 "$OPEN_JSON" \
  "${HEADER}"$'\nmodels/auth/*' 'models/auth/oauth2_test.go'

run_case 'glob spans subdirectories' 1 "$OPEN_JSON" \
  "${HEADER}"$'\nmodels/auth/*' 'models/auth/nested/thing.go'

run_case 'non-match passes' 0 "$OPEN_JSON" \
  "${HEADER}"$'\nmodels/auth/oauth2.go' 'services/robot/pagerank.go'

run_case 'prefix lookalike does not match' 0 "$OPEN_JSON" \
  "${HEADER}"$'\nmodels/auth/oauth2.go' 'models/auth/oauth2.go.bak'

run_case 'added unlisted file passes' 0 "$OPEN_JSON" \
  "${HEADER}"$'\nmodels/auth/oauth2.go' $'services/robot/new_file.go\ndocs/new.md'

run_case 'added listed file fails' 1 "$OPEN_JSON" \
  "${HEADER}"$'\ntests/e2e/mermaid.test.ts' 'tests/e2e/mermaid.test.ts'

run_case 'name-status input, added unlisted' 0 "$OPEN_JSON" \
  "${HEADER}"$'\nmodels/auth/oauth2.go' $'A\tservices/robot/new_file.go'

run_case 'name-status input, modified listed' 1 "$OPEN_JSON" \
  "${HEADER}"$'\nmodels/auth/oauth2.go' $'M\tmodels/auth/oauth2.go'

run_case 'empty diff passes' 0 "$OPEN_JSON" \
  "${HEADER}"$'\nmodels/auth/oauth2.go' ''

run_case 'comments and blanks ignored' 0 "$OPEN_JSON" \
  "${HEADER}"$'\n\n# comment only\n   \nmodels/auth/oauth2.go' 'README.md'

run_case 'multiple paths, one offender' 1 "$OPEN_JSON" \
  "${HEADER}"$'\npnpm-lock.yaml' $'README.md\npnpm-lock.yaml\nmain.go'

# --- mocked API gate --------------------------------------------------------
run_case 'issue open  -> enforced' 1 "$OPEN_JSON" \
  "${HEADER}"$'\nmodels/auth/oauth2.go' 'models/auth/oauth2.go'

run_case 'issue closed -> skipped' 0 "$CLOSED_JSON" \
  "${HEADER}"$'\nmodels/auth/oauth2.go' 'models/auth/oauth2.go'

run_case 'unknown state -> error' 2 '{"state":"draft"}' \
  "${HEADER}"$'\nmodels/auth/oauth2.go' 'models/auth/oauth2.go'

run_case 'missing state -> error' 2 '{"number":43}' \
  "${HEADER}"$'\nmodels/auth/oauth2.go' 'models/auth/oauth2.go'

run_case 'malformed json -> error' 2 'not json at all' \
  "${HEADER}"$'\nmodels/auth/oauth2.go' 'models/auth/oauth2.go'

run_case 'empty list -> error' 2 "$OPEN_JSON" \
  '# only a comment' 'models/auth/oauth2.go'

# --- nested payloads: the state must be the issue's, not a sub-object's -----
run_case 'nested payload, closed milestone, open issue' 1 "$NESTED_OPEN_JSON" \
  "${HEADER}"$'\nmodels/auth/oauth2.go' 'models/auth/oauth2.go'

run_case 'nested payload, open milestone, closed issue' 0 "$NESTED_CLOSED_JSON" \
  "${HEADER}"$'\nmodels/auth/oauth2.go' 'models/auth/oauth2.go'

run_case 'nested payload, braces in the issue body' 1 "$NESTED_BODY_JSON" \
  "${HEADER}"$'\nmodels/auth/oauth2.go' 'models/auth/oauth2.go'

# the jq-less path must reach the same verdicts: BLAST_RADIUS_NO_JQ forces the
# awk parser, so both branches are covered wherever the suite runs.
run_awk_case() {
  local name="$1" want="$2" issue="$3"
  local list_file="${TMP}/awk-list.txt" changed_file="${TMP}/awk-changed.txt"
  printf '%s\n%s\n' "$HEADER" 'models/auth/oauth2.go' > "$list_file"
  printf '%s\n' 'models/auth/oauth2.go' > "$changed_file"

  local out got
  out=$(BLAST_RADIUS_NO_JQ=1 \
    BLAST_RADIUS_ISSUE_FETCH="printf '%s' '${issue}'" \
    "$SCRIPT" --list "$list_file" --changed "$changed_file" 2>&1)
  got=$?
  if [ "$got" -eq "$want" ]; then
    ok "$name (exit ${got})"
  else
    no "$name" "(want exit ${want}, got ${got})
${out}"
  fi
}

run_awk_case 'awk fallback: nested open issue enforced' 1 "$NESTED_OPEN_JSON"
run_awk_case 'awk fallback: nested closed issue skipped' 0 "$NESTED_CLOSED_JSON"
run_awk_case 'awk fallback: braces in the body' 1 "$NESTED_BODY_JSON"
run_awk_case 'awk fallback: flat open payload' 1 "$OPEN_JSON"
run_awk_case 'awk fallback: flat closed payload' 0 "$CLOSED_JSON"
run_awk_case 'awk fallback: missing state errors' 2 '{"number":43}'
run_awk_case 'awk fallback: malformed json errors' 2 'not json at all'

# --- stdin mode -------------------------------------------------------------
{
  list_file="${TMP}/stdin-list.txt"
  printf '%s\n%s\n' "$HEADER" 'models/auth/*' > "$list_file"

  out=$(printf '%s\n' 'models/auth/oauth2.go' |
    BLAST_RADIUS_ISSUE_FETCH="printf '%s' '${OPEN_JSON}'" \
      "$SCRIPT" --list "$list_file" --changed - 2>&1)
  got=$?
  if [ "$got" -eq 1 ]; then
    ok '--changed - reads stdin (violation)'
  else
    no '--changed - reads stdin (violation)' "(want exit 1, got ${got})
${out}"
  fi

  out=$(printf '%s\n' 'README.md' |
    BLAST_RADIUS_ISSUE_FETCH="printf '%s' '${OPEN_JSON}'" \
      "$SCRIPT" --list "$list_file" --changed - 2>&1)
  got=$?
  if [ "$got" -eq 0 ]; then
    ok '--changed - reads stdin (pass)'
  else
    no '--changed - reads stdin (pass)' "(want exit 0, got ${got})
${out}"
  fi
}

# --- the shipped list is loadable and enforces the real reserved paths ------
{
  changed_file="${TMP}/real-changed.txt"
  printf '%s\n' 'services/lfs/server.go' > "$changed_file"
  if BLAST_RADIUS_ISSUE_FETCH="printf '%s' '${OPEN_JSON}'" \
    "$SCRIPT" --list "$REAL_LIST" --changed "$changed_file" > /dev/null 2>&1; then
    no 'shipped list blocks services/lfs/server.go' '(want exit 1, got 0)'
  else
    ok 'shipped list blocks services/lfs/server.go'
  fi

  # the B1/B2 source files are covered by the B3 globs, not by exact entries
  printf '%s\n' 'routers/web/auth/oauth2_provider.go' > "$changed_file"
  if BLAST_RADIUS_ISSUE_FETCH="printf '%s' '${OPEN_JSON}'" \
    "$SCRIPT" --list "$REAL_LIST" --changed "$changed_file" > /dev/null 2>&1; then
    no 'shipped list blocks the B2 provider file' '(want exit 1, got 0)'
  else
    ok 'shipped list blocks the B2 provider file'
  fi

  # the removed `services/lfs/server*.go` glob must not reserve nested paths
  printf '%s\n' 'services/lfs/serverfoo/bar.go' > "$changed_file"
  if BLAST_RADIUS_ISSUE_FETCH="printf '%s' '${OPEN_JSON}'" \
    "$SCRIPT" --list "$REAL_LIST" --changed "$changed_file" > /dev/null 2>&1; then
    ok 'shipped list does not over-reserve services/lfs/'
  else
    no 'shipped list does not over-reserve services/lfs/' '(want exit 0, got 1)'
  fi

  printf '%s\n' 'services/robot/brand_new.go' > "$changed_file"
  if BLAST_RADIUS_ISSUE_FETCH="printf '%s' '${OPEN_JSON}'" \
    "$SCRIPT" --list "$REAL_LIST" --changed "$changed_file" > /dev/null 2>&1; then
    ok 'shipped list allows a new robot file'
  else
    no 'shipped list allows a new robot file' '(want exit 0, got 1)'
  fi

  if head -n 1 "$REAL_LIST" | grep -qx "$HEADER"; then
    ok 'shipped list carries the required header'
  else
    no 'shipped list carries the required header'
  fi

  # every entry must be reachable: no entry may be subsumed by another
  dead=""
  entries=$(sed 's/#.*//' "$REAL_LIST" | sed 's/[[:space:]]*$//' | grep -v '^[[:space:]]*$')
  while IFS= read -r a; do
    while IFS= read -r b; do
      [ "$a" = "$b" ] && continue
      # shellcheck disable=SC2053 # intentional glob match against the entry
      if [[ $a == $b ]]; then
        dead="${dead}${a} subsumed by ${b}; "
      fi
    done <<< "$entries"
  done <<< "$entries"
  if [ -z "$dead" ]; then
    ok 'shipped list has no subsumed entries'
  else
    no 'shipped list has no subsumed entries' "$dead"
  fi
}

# --- diff range: three-dot semantics against a real repository --------------
{
  repo="${TMP}/repo"
  mkdir -p "$repo"
  (
    cd "$repo" || exit 1
    # `git init -b` needs git >= 2.28; name the branch the portable way
    git init --quiet .
    git symbolic-ref HEAD refs/heads/main
    git config user.email tester@example.com
    git config user.name tester
    git config commit.gpgsign false
    # distinct content per file: identical (e.g. empty) blobs would let git's
    # rename detection collapse the two paths and hide the two-dot difference
    printf 'root\n' > root.md
    git add root.md
    git commit --quiet -m root

    # the PR forks here
    git checkout --quiet -b feature
    printf 'unrelated change\n' > unrelated.md
    git add unrelated.md
    git commit --quiet -m feature

    # meanwhile a sync pick lands on the base branch
    git checkout --quiet main
    mkdir -p models/auth
    printf 'package auth // upstream pick\n' > models/auth/oauth2.go
    git add models/auth/oauth2.go
    git commit --quiet -m 'B1 pick'
    git checkout --quiet feature
  ) > /dev/null 2>&1

  base_sha=$(git -C "$repo" rev-parse main)
  head_sha=$(git -C "$repo" rev-parse feature)

  out=$(cd "$repo" && BLAST_RADIUS_FETCH_REMOTE= bash "$DIFF_SCRIPT" --base "$base_sha" --head "$head_sha" 2>/dev/null)
  got=$?
  if [ "$got" -eq 0 ] && [ "$out" = "unrelated.md" ]; then
    ok 'diff range excludes base-branch drift'
  else
    no 'diff range excludes base-branch drift' "(exit ${got}, paths: $(printf '%s' "$out" | tr '\n' ' '))"
  fi

  # a two-dot range would report models/auth/oauth2.go as a deletion here,
  # which is exactly the false violation the three-dot form avoids
  two_dot=$(cd "$repo" && git diff --name-only "$base_sha" "$head_sha")
  case "$two_dot" in
    *models/auth/oauth2.go*) ok 'two-dot range would have false-flagged' ;;
    *) no 'two-dot range would have false-flagged' "(got: ${two_dot})" ;;
  esac

  # the guard must pass on this diff, and the range is what makes it pass
  changed_file="${TMP}/range-changed.txt"
  (cd "$repo" && BLAST_RADIUS_FETCH_REMOTE= bash "$DIFF_SCRIPT" --base "$base_sha" --head "$head_sha" --output "$changed_file") > /dev/null 2>&1
  if BLAST_RADIUS_ISSUE_FETCH="printf '%s' '${OPEN_JSON}'" \
    "$SCRIPT" --list "$REAL_LIST" --changed "$changed_file" > /dev/null 2>&1; then
    ok 'guard passes on the merge-base diff'
  else
    no 'guard passes on the merge-base diff' '(want exit 0, got 1)'
  fi

  # a genuine touch of a reserved path on the PR branch still fails
  (
    cd "$repo" || exit 1
    mkdir -p models/auth
    printf 'x\n' > models/auth/oauth2.go
    git add models/auth/oauth2.go
    git commit --quiet -m 'pr touches a reserved path'
  ) > /dev/null 2>&1
  head_sha=$(git -C "$repo" rev-parse feature)
  (cd "$repo" && BLAST_RADIUS_FETCH_REMOTE= bash "$DIFF_SCRIPT" --base "$base_sha" --head "$head_sha" --output "$changed_file") > /dev/null 2>&1
  if BLAST_RADIUS_ISSUE_FETCH="printf '%s' '${OPEN_JSON}'" \
    "$SCRIPT" --list "$REAL_LIST" --changed "$changed_file" > /dev/null 2>&1; then
    no 'guard fails on a real reserved-path touch' '(want exit 1, got 0)'
  else
    ok 'guard fails on a real reserved-path touch'
  fi

  # an unresolvable base must fail loudly, not silently diff against nothing
  out=$(cd "$repo" && BLAST_RADIUS_FETCH_REMOTE= bash "$DIFF_SCRIPT" --base 0000000000000000000000000000000000000000 2>&1)
  got=$?
  if [ "$got" -eq 2 ] && printf '%s' "$out" | grep -q 'cannot resolve base revision'; then
    ok 'unresolvable base fails loudly'
  else
    no 'unresolvable base fails loudly' "(exit ${got}: ${out})"
  fi
}

echo
echo "passed: ${pass}  failed: ${fail}"
[ "$fail" -eq 0 ]
