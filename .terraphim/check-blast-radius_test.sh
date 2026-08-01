#!/usr/bin/env bash
# Copyright 2026 The Gitea Authors. All rights reserved.
# SPDX-License-Identifier: MIT
#
# Unit tests for check-blast-radius.sh and blast-radius-diff.sh. No network.
#
# check-blast-radius.sh deliberately has no environment-variable test seams:
# it is the script that decides whether a PR may land, so an env-controlled
# override of the issue lookup would be an env-controlled override of the
# verdict. The stubbing here is done from outside the script instead - each
# case runs it with a PATH pointing at a directory this suite builds, holding
# a stub `curl` plus symlinks to the handful of real tools the script needs.
# The jq and the no-jq parser paths are selected by whether that directory
# contains a `jq` symlink, which is exactly the `command -v jq` decision the
# script makes in production.
#
# The diff-range cases build a throwaway git repository under $TMPDIR.
#
# Run: .terraphim/check-blast-radius_test.sh
#   - locally via ./.adf-gates.sh (the fork's ADF gate contract)
#   - in CI via the "Guard self-test" step of
#     .github/workflows/check-blast-radius.yml, which runs the base-ref copy

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SCRIPT="${HERE}/check-blast-radius.sh"
DIFF_SCRIPT="${HERE}/blast-radius-diff.sh"
REAL_LIST="${HERE}/sync-blast-radius.txt"
# The repository root when the suite runs from a checkout. In CI the guard is
# copied to $RUNNER_TEMP first, so the repo-layout assertions below detect that
# and report a skip rather than a spurious failure.
REPO_ROOT=$(dirname "$HERE")
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

STUB_API_URL="https://blast-radius.invalid/api/v1"
STUB_REPO="terraphim/gitea"

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
skip=0

ok() {
  pass=$((pass + 1))
  printf 'ok   %-46s\n' "$1"
}

no() {
  fail=$((fail + 1))
  printf 'FAIL %-46s %s\n' "$1" "${2:-}"
}

skipped() {
  skip=$((skip + 1))
  printf 'skip %-46s %s\n' "$1" "${2:-}"
}

# --- PATH shims: the only stubbing mechanism ---------------------------------
# The script itself invokes basename, curl, awk, sed and (when present) jq.
# It is started as `"$BASH" "$SCRIPT"`, so bash does not have to be on the
# stub PATH; nothing else is looked up.
SHIM_JQ="${TMP}/bin-jq"
SHIM_NOJQ="${TMP}/bin-nojq"

make_shim_dir() {
  local dir="$1" want_jq="$2" tool path
  mkdir -p "$dir"
  for tool in basename awk sed cat; do
    path=$(command -v "$tool" 2> /dev/null) || continue
    ln -sf "$path" "${dir}/${tool}"
  done
  if [ "$want_jq" = "with-jq" ]; then
    if path=$(command -v jq 2> /dev/null); then
      ln -sf "$path" "${dir}/jq"
    fi
  fi
  # Stub curl. It ignores its arguments and replays the environment the case
  # set up, which is what the real curl's stdout/exit status boil down to as
  # far as the script is concerned.
  cat > "${dir}/curl" << 'STUB'
#!/bin/sh
printf '%s' "${STUB_ISSUE_JSON-}"
exit "${STUB_CURL_EXIT-0}"
STUB
  chmod +x "${dir}/curl"
}

make_shim_dir "$SHIM_JQ" with-jq
make_shim_dir "$SHIM_NOJQ" without-jq

if [ -e "${SHIM_JQ}/jq" ]; then
  ok 'jq is available: both parser paths covered'
else
  skipped 'jq path' '(jq not installed; only the awk parser is exercised)'
fi
if [ -e "${SHIM_NOJQ}/jq" ]; then
  no 'no-jq shim dir really has no jq'
else
  ok 'no-jq shim dir really has no jq'
fi

# run_guard <shim-dir> <issue-json> <curl-exit> [args...] - stdout+stderr on
# stdout, exit status in the caller's $?
run_guard() {
  local bindir="$1" issue="$2" curl_exit="$3"
  shift 3
  STUB_ISSUE_JSON="$issue" STUB_CURL_EXIT="$curl_exit" \
    PATH="$bindir" \
    GITEA_API_URL="$STUB_API_URL" GITEA_REPO="$STUB_REPO" \
    "$BASH" "$SCRIPT" "$@" 2>&1
}

# run_case <name> <expected-exit> <issue-json> <list-content> <changed-content>
run_case() {
  local name="$1" want="$2" issue="$3" list="$4" changed="$5"
  local list_file="${TMP}/list.txt" changed_file="${TMP}/changed.txt"
  printf '%s\n' "$list" > "$list_file"
  printf '%s' "$changed" > "$changed_file"

  local out got
  out=$(run_guard "$SHIM_JQ" "$issue" 0 --list "$list_file" --changed "$changed_file")
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

# --- stubbed API gate -------------------------------------------------------
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

# The issue lookup failing outright must fail closed (exit 2), never pass.
{
  list_file="${TMP}/apifail-list.txt"
  changed_file="${TMP}/apifail-changed.txt"
  printf '%s\n%s\n' "$HEADER" 'models/auth/oauth2.go' > "$list_file"
  printf '%s\n' 'README.md' > "$changed_file"

  out=$(run_guard "$SHIM_JQ" '' 22 --list "$list_file" --changed "$changed_file")
  got=$?
  if [ "$got" -eq 2 ]; then
    ok 'issue lookup failure fails closed (exit 2)'
  else
    no 'issue lookup failure fails closed (exit 2)' "(got ${got})
${out}"
  fi
}

# Missing API configuration must also fail closed rather than skip the gate.
{
  list_file="${TMP}/noapi-list.txt"
  changed_file="${TMP}/noapi-changed.txt"
  printf '%s\n%s\n' "$HEADER" 'models/auth/oauth2.go' > "$list_file"
  printf '%s\n' 'models/auth/oauth2.go' > "$changed_file"

  out=$(PATH="$SHIM_JQ" "$BASH" "$SCRIPT" \
    --list "$list_file" --changed "$changed_file" 2>&1)
  got=$?
  if [ "$got" -eq 2 ] && printf '%s' "$out" | grep -q 'GITEA_API_URL'; then
    ok 'unset GITEA_API_URL fails closed'
  else
    no 'unset GITEA_API_URL fails closed' "(exit ${got}: ${out})"
  fi
}

# --- nested payloads: the state must be the issue's, not a sub-object's -----
run_case 'nested payload, closed milestone, open issue' 1 "$NESTED_OPEN_JSON" \
  "${HEADER}"$'\nmodels/auth/oauth2.go' 'models/auth/oauth2.go'

run_case 'nested payload, open milestone, closed issue' 0 "$NESTED_CLOSED_JSON" \
  "${HEADER}"$'\nmodels/auth/oauth2.go' 'models/auth/oauth2.go'

run_case 'nested payload, braces in the issue body' 1 "$NESTED_BODY_JSON" \
  "${HEADER}"$'\nmodels/auth/oauth2.go' 'models/auth/oauth2.go'

# The jq-less path must reach the same verdicts. It is selected by running the
# script with a PATH that has no jq on it - the same `command -v jq` miss that
# happens on a runner without jq installed.
run_awk_case() {
  local name="$1" want="$2" issue="$3"
  local list_file="${TMP}/awk-list.txt" changed_file="${TMP}/awk-changed.txt"
  printf '%s\n%s\n' "$HEADER" 'models/auth/oauth2.go' > "$list_file"
  printf '%s\n' 'models/auth/oauth2.go' > "$changed_file"

  local out got
  out=$(run_guard "$SHIM_NOJQ" "$issue" 0 --list "$list_file" --changed "$changed_file")
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

# --- no test seams may creep back into the production path ------------------
# The suite stubs from outside the script on purpose. If someone reintroduces
# an env-controlled fetch or parser override, the guard becomes disable-able by
# anything that can write to the job environment on a self-hosted runner.
{
  if grep -nE '(^|[^_[:alnum:]])eval[[:space:]]' "$SCRIPT" > /dev/null 2>&1; then
    no 'check-blast-radius.sh contains no eval' "$(grep -nE '(^|[^_[:alnum:]])eval[[:space:]]' "$SCRIPT")"
  else
    ok 'check-blast-radius.sh contains no eval'
  fi

  if grep -n 'BLAST_RADIUS_ISSUE_FETCH\|BLAST_RADIUS_NO_JQ' "$SCRIPT" > /dev/null 2>&1; then
    no 'no env override of the fetch or the parser' \
      "$(grep -n 'BLAST_RADIUS_ISSUE_FETCH\|BLAST_RADIUS_NO_JQ' "$SCRIPT")"
  else
    ok 'no env override of the fetch or the parser'
  fi
}

# --- stdin mode -------------------------------------------------------------
{
  list_file="${TMP}/stdin-list.txt"
  printf '%s\n%s\n' "$HEADER" 'models/auth/*' > "$list_file"

  out=$(printf '%s\n' 'models/auth/oauth2.go' |
    run_guard "$SHIM_JQ" "$OPEN_JSON" 0 --list "$list_file" --changed -)
  got=$?
  if [ "$got" -eq 1 ]; then
    ok '--changed - reads stdin (violation)'
  else
    no '--changed - reads stdin (violation)' "(want exit 1, got ${got})
${out}"
  fi

  out=$(printf '%s\n' 'README.md' |
    run_guard "$SHIM_JQ" "$OPEN_JSON" 0 --list "$list_file" --changed -)
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

  # expect_real <name> <expected-exit> <path>
  expect_real() {
    local name="$1" want="$2" path="$3" out got
    printf '%s\n' "$path" > "$changed_file"
    out=$(run_guard "$SHIM_JQ" "$OPEN_JSON" 0 --list "$REAL_LIST" --changed "$changed_file")
    got=$?
    if [ "$got" -eq "$want" ]; then
      ok "$name"
    else
      no "$name" "(want exit ${want}, got ${got})
${out}"
    fi
  }

  expect_real 'shipped list blocks services/lfs/server.go' 1 'services/lfs/server.go'
  # the B1/B2 source files are covered by the B3 globs, not by exact entries
  expect_real 'shipped list blocks the B2 provider file' 1 'routers/web/auth/oauth2_provider.go'
  # the removed `services/lfs/server*.go` glob must not reserve nested paths
  expect_real 'shipped list does not over-reserve lfs' 0 'services/lfs/serverfoo/bar.go'
  expect_real 'shipped list allows a new robot file' 0 'services/robot/brand_new.go'

  # the guard's own surface is reserved, so narrowing it is at least reported
  expect_real 'shipped list reserves the path list' 1 '.terraphim/sync-blast-radius.txt'
  expect_real 'shipped list reserves the matcher' 1 '.terraphim/check-blast-radius.sh'
  expect_real 'shipped list reserves the diff collector' 1 '.terraphim/blast-radius-diff.sh'
  expect_real 'shipped list reserves this test suite' 1 '.terraphim/check-blast-radius_test.sh'
  expect_real 'shipped list reserves the workflow' 1 '.github/workflows/check-blast-radius.yml'
  # ... but not the rest of CI
  expect_real 'shipped list leaves other workflows alone' 0 '.github/workflows/pull-compliance.yml'

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

# --- workflow placement: .gitea/workflows must stay absent ------------------
# modules/actions/workflows.go ListWorkflows() breaks on the FIRST entry of
# setting.Actions.WorkflowDirs (default [".gitea/workflows", ".github/workflows"])
# that resolves; it does not union them. This fork ships its CI in
# .github/workflows/, so a .gitea/workflows/ directory would hide every one of
# those workflows. Pin that, since the mistake is silent.
{
  if [ ! -d "${REPO_ROOT}/.github/workflows" ]; then
    skipped 'workflow placement' '(not running from a checkout)'
  else
    if [ -f "${REPO_ROOT}/.github/workflows/check-blast-radius.yml" ]; then
      ok 'guard workflow lives in .github/workflows'
    else
      no 'guard workflow lives in .github/workflows'
    fi

    if [ -e "${REPO_ROOT}/.gitea/workflows" ]; then
      no '.gitea/workflows does not exist' \
        '(its presence hides every .github/workflows/* workflow from ListWorkflows)'
    else
      ok '.gitea/workflows does not exist'
    fi

    # the trigger must be pull_request_target: `pull_request` workflows are
    # detected from the PR head commit, so the PR could just delete this file
    if grep -q '^  pull_request_target:' "${REPO_ROOT}/.github/workflows/check-blast-radius.yml"; then
      ok 'guard triggers on pull_request_target'
    else
      no 'guard triggers on pull_request_target' \
        '(on: pull_request reads the workflow from the PR head, so it is removable by the PR)'
    fi

    # the env default and the hardcoded literals in the `if:` conditions
    # must agree; the workflow asserts this at run time too
    wf="${REPO_ROOT}/.github/workflows/check-blast-radius.yml"
    env_label=$(sed -n 's/^  GUARD_EXEMPT_LABEL: *//p' "$wf" | head -n 1)
    if_labels=$(grep -c "contains(github.event.pull_request.labels.\*.name, '${env_label}')" "$wf")
    if [ -n "$env_label" ] && [ "$if_labels" -eq 2 ]; then
      ok 'exempt label literal matches both if: conditions'
    else
      no 'exempt label literal matches both if: conditions' \
        "(env='${env_label}', matching if: conditions=${if_labels}, want 2)"
    fi
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

  out=$(cd "$repo" && BLAST_RADIUS_FETCH_REMOTE= bash "$DIFF_SCRIPT" --base "$base_sha" --head "$head_sha" 2> /dev/null)
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
  out=$(run_guard "$SHIM_JQ" "$OPEN_JSON" 0 --list "$REAL_LIST" --changed "$changed_file")
  got=$?
  if [ "$got" -eq 0 ]; then
    ok 'guard passes on the merge-base diff'
  else
    no 'guard passes on the merge-base diff' "(want exit 0, got ${got})
${out}"
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
  out=$(run_guard "$SHIM_JQ" "$OPEN_JSON" 0 --list "$REAL_LIST" --changed "$changed_file")
  got=$?
  if [ "$got" -eq 1 ]; then
    ok 'guard fails on a real reserved-path touch'
  else
    no 'guard fails on a real reserved-path touch' "(want exit 1, got ${got})
${out}"
  fi

  # an unresolvable base must fail loudly, not silently diff against nothing
  out=$(cd "$repo" && BLAST_RADIUS_FETCH_REMOTE= bash "$DIFF_SCRIPT" --base 0000000000000000000000000000000000000000 2>&1)
  got=$?
  if [ "$got" -eq 2 ] && printf '%s' "$out" | grep -q 'cannot resolve base revision'; then
    ok 'unresolvable base fails loudly'
  else
    no 'unresolvable base fails loudly' "(exit ${got}: ${out})"
  fi

  # and the diagnostic must carry git's own reason, not just a fetch-depth guess
  out=$(cd "$repo" && BLAST_RADIUS_FETCH_REMOTE=no-such-remote bash "$DIFF_SCRIPT" \
    --base 0000000000000000000000000000000000000000 2>&1)
  got=$?
  if [ "$got" -eq 2 ] && printf '%s' "$out" | grep -q 'git fetch said:.*no-such-remote'; then
    ok 'fetch failure reason is reported'
  else
    no 'fetch failure reason is reported' "(exit ${got}: ${out})"
  fi
}

echo
echo "passed: ${pass}  failed: ${fail}  skipped: ${skip}"
[ "$fail" -eq 0 ]
