#!/usr/bin/env bash
# Copyright 2026 The Gitea Authors. All rights reserved.
# SPDX-License-Identifier: MIT
#
# Unit tests for check-blast-radius.sh. No network: the issue endpoint is
# stubbed through BLAST_RADIUS_ISSUE_FETCH.
#
# Run: .terraphim/check-blast-radius_test.sh

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SCRIPT="${HERE}/check-blast-radius.sh"
REAL_LIST="${HERE}/sync-blast-radius.txt"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

OPEN_JSON='{"number":43,"title":"tracker","state":"open","comments":1}'
CLOSED_JSON='{"number":43,"title":"tracker","state":"closed","closed_at":"2026-08-01T00:00:00Z"}'

pass=0
fail=0

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

run_case 'empty list -> error' 2 "$OPEN_JSON" \
  '# only a comment' 'models/auth/oauth2.go'

# --- the shipped list is loadable and enforces the real reserved paths ------
{
  changed_file="${TMP}/real-changed.txt"
  printf '%s\n' 'services/lfs/server.go' > "$changed_file"
  if BLAST_RADIUS_ISSUE_FETCH="printf '%s' '${OPEN_JSON}'" \
    "$SCRIPT" --list "$REAL_LIST" --changed "$changed_file" > /dev/null 2>&1; then
    fail=$((fail + 1))
    printf 'FAIL %-46s (want exit 1, got 0)\n' 'shipped list blocks services/lfs/server.go'
  else
    pass=$((pass + 1))
    printf 'ok   %-46s (exit 1)\n' 'shipped list blocks services/lfs/server.go'
  fi

  printf '%s\n' 'services/robot/brand_new.go' > "$changed_file"
  if BLAST_RADIUS_ISSUE_FETCH="printf '%s' '${OPEN_JSON}'" \
    "$SCRIPT" --list "$REAL_LIST" --changed "$changed_file" > /dev/null 2>&1; then
    pass=$((pass + 1))
    printf 'ok   %-46s (exit 0)\n' 'shipped list allows a new robot file'
  else
    fail=$((fail + 1))
    printf 'FAIL %-46s (want exit 0, got 1)\n' 'shipped list allows a new robot file'
  fi

  head -n 1 "$REAL_LIST" | grep -qx "$HEADER"
  if [ $? -eq 0 ]; then
    pass=$((pass + 1))
    printf 'ok   %-46s\n' 'shipped list carries the required header'
  else
    fail=$((fail + 1))
    printf 'FAIL %-46s\n' 'shipped list carries the required header'
  fi
}

echo
echo "passed: ${pass}  failed: ${fail}"
[ "$fail" -eq 0 ]
