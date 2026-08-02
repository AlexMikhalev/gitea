#!/usr/bin/env bash
# Copyright 2026 The Gitea Authors. All rights reserved.
# SPDX-License-Identifier: MIT
#
# Unit tests for check-blast-radius.sh and blast-radius-diff.sh. No network.
#
# check-blast-radius.sh deliberately has no environment-variable test seams:
# it is the script that decides whether a PR may land, so an env-controlled
# override of the issue lookup - or of *which* issue is looked up - would be an
# env-controlled override of the verdict. The stubbing here is done from
# outside the script instead - each case runs it with a PATH pointing at a
# directory this suite builds, holding a stub `curl` plus symlinks to the
# handful of real tools the script needs. The jq and the no-jq parser paths are
# selected by whether that directory contains a `jq` symlink, which is exactly
# the `command -v jq` decision the script makes in production.
#
# The same rule covers blast-radius-diff.sh, which runs in the same job: it
# reads no environment variable at all. That property is asserted twice for
# both files - statically, by enumerating the env vars each script reads and
# checking them against a documented allowlist, and behaviourally, by running
# the guard with hostile values set and asserting the verdict does not move.
#
# The diff-range, rename and quoting cases build throwaway git repositories
# under $TMPDIR. The rename ones exist because `git diff --name-only` reports
# only the destination of a detected rename, which let a PR move a reserved file
# out of its reserved path and still be told PASSED. The quoting ones exist
# because the same command C-quotes any path holding a non-ASCII or control
# byte, and a quoted record matches no list entry - the same silent pass, for a
# newly added file. Both are configuration-dependent by default
# (diff.renames, core.quotePath), so both throwaway repos turn the hazardous
# setting ON explicitly and both sets of cases first assert that the unfixed
# command really does lose the path.
#
# Run: .terraphim/check-blast-radius_test.sh
#   - locally via ./.adf-gates.sh (the fork's ADF gate contract)
#   - in CI via the "Guard self-test" step of
#     .github/workflows/check-blast-radius.yml, which runs the base-ref copy
#     out of $RUNNER_TEMP - the repo-layout assertions still run there, against
#     the base checkout, rather than skipping

set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SCRIPT="${HERE}/check-blast-radius.sh"
DIFF_SCRIPT="${HERE}/blast-radius-diff.sh"
REAL_LIST="${HERE}/sync-blast-radius.txt"

# The repository root the repo-layout assertions run against.
#
# Locally (./.adf-gates.sh) that is the parent of this script's directory. In
# CI it is NOT: the workflow pins the guard into $RUNNER_TEMP/blast-radius-guard
# and runs the copy from there, so $(dirname "$HERE") is $RUNNER_TEMP and the
# layout assertions used to skip on every CI run - i.e. the `.gitea/workflows`
# invariant, the single highest-blast-radius mistake this change exists to
# prevent, was pinned only by whoever remembered to run the gates locally.
# $GITHUB_WORKSPACE - exported by the runner, and under pull_request_target the
# base-branch checkout - is on disk during that same job, and is exactly the
# tree these assertions are about. The toplevel of the working directory is the
# backstop: the `${{ github.workspace }}` *expression* is not usable here,
# because services/actions/context.go:84 leaves the server-side value empty.
#
# Resolution never falls through to a silent skip: if a checkout is *expected*
# (any CI marker in the environment) but none is found, that is a failure, not
# a skip - see the "repo layout" block near the end of this file.
resolve_repo_root() {
  local candidate
  for candidate in "${GITHUB_WORKSPACE:-}" "$(dirname "$HERE")" \
    "$(git rev-parse --show-toplevel 2> /dev/null || true)"; do
    [ -n "$candidate" ] || continue
    [ -d "${candidate}/.github/workflows" ] || continue
    printf '%s' "$candidate"
    return 0
  done
  return 1
}
REPO_ROOT=$(resolve_repo_root) || REPO_ROOT=""

# Is a checkout expected to be reachable? True in any CI run; act_runner and
# GitHub Actions both export GITHUB_WORKSPACE and CI.
if [ -n "${GITHUB_WORKSPACE:-}" ] || [ -n "${GITHUB_ACTIONS:-}" ] || [ -n "${CI:-}" ]; then
  REPO_LAYOUT_REQUIRED=1
else
  REPO_LAYOUT_REQUIRED=0
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

STUB_API_URL="https://blast-radius.invalid/api/v1"
STUB_REPO="terraphim/gitea"
# A well-formed owner/name that is NOT the guarded repository: the shape a
# misdirection attempt has, since anything malformed is rejected outright.
HOSTILE_REPO="attacker/decoy"

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
  # GITEA_REPO is deliberately hostile in EVERY case in this file: the script
  # stopped reading it when the repository became a `--repo` argument, so the
  # verdicts asserted below are also an assertion that it is inert. That is a
  # weak proof on its own - the stub curl here ignores the URL entirely - so
  # the load-bearing version lives in the run_idx block, whose stub answers by
  # repository.
  STUB_ISSUE_JSON="$issue" STUB_CURL_EXIT="$curl_exit" \
    PATH="$bindir" \
    GITEA_API_URL="$STUB_API_URL" GITEA_REPO="$HOSTILE_REPO" \
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

run_case 'unknown state -> error' 2 '{"number":43,"state":"draft"}' \
  "${HEADER}"$'\nmodels/auth/oauth2.go' 'models/auth/oauth2.go'

run_case 'missing state -> error' 2 '{"number":43}' \
  "${HEADER}"$'\nmodels/auth/oauth2.go' 'models/auth/oauth2.go'

run_case 'malformed json -> error' 2 'not json at all' \
  "${HEADER}"$'\nmodels/auth/oauth2.go' 'models/auth/oauth2.go'

run_case 'empty list -> error' 2 "$OPEN_JSON" \
  '# only a comment' 'models/auth/oauth2.go'

# --- the answer must be about the issue that was asked for ------------------
# Only "closed" turns into a pass, so an endpoint that replies about some other
# issue would otherwise be a silent SKIPPED. Each payload below therefore says
# "closed": if the scope check were dropped, these cases would go to exit 0,
# not to a different error.
run_case 'answer about another issue -> error' 2 '{"number":44,"title":"other","state":"closed"}' \
  "${HEADER}"$'\nmodels/auth/oauth2.go' 'models/auth/oauth2.go'

run_case 'answer with no issue number -> error' 2 '{"title":"tracker","state":"closed"}' \
  "${HEADER}"$'\nmodels/auth/oauth2.go' 'models/auth/oauth2.go'

# a JSON string is not a JSON number: the index must be typed, or "43" from an
# endpoint that stringifies everything would satisfy the check by accident
run_case 'answer with a stringified number -> error' 2 '{"number":"43","state":"closed"}' \
  "${HEADER}"$'\nmodels/auth/oauth2.go' 'models/auth/oauth2.go'

# the number must be the issue's own, not a nested object's - `pull_request`
# and `milestone` payloads carry one too
run_case 'nested number does not satisfy the scope check' 2 \
  '{"pull_request":{"number":43},"title":"other","state":"closed"}' \
  "${HEADER}"$'\nmodels/auth/oauth2.go' 'models/auth/oauth2.go'

# ... and it must accept the answer when the index asked for is not the default,
# so the check tracks --issue rather than the literal 43. run_case always runs
# with the default index, so this pair is spelled out: the SAME payload is a
# pass under `--issue 51` and exit 2 without it, which is what makes the pass
# evidence that the check followed the argument rather than evidence that it
# stopped running.
{
  scope_list="${TMP}/scope-list.txt"
  scope_changed="${TMP}/scope-changed.txt"
  scope_json='{"number":51,"title":"other tracker","state":"closed"}'
  printf '%s\n%s\n' "$HEADER" 'models/auth/oauth2.go' > "$scope_list"
  printf '%s\n' 'models/auth/oauth2.go' > "$scope_changed"

  out=$(run_guard "$SHIM_JQ" "$scope_json" 0 \
    --list "$scope_list" --changed "$scope_changed" --issue 51)
  got=$?
  if [ "$got" -eq 0 ]; then
    ok 'the scope check follows --issue'
  else
    no 'the scope check follows --issue' "(want exit 0, got ${got})
${out}"
  fi

  out=$(run_guard "$SHIM_JQ" "$scope_json" 0 \
    --list "$scope_list" --changed "$scope_changed")
  got=$?
  if [ "$got" -eq 2 ]; then
    ok 'the same answer is out of scope for the default index'
  else
    no 'the same answer is out of scope for the default index' \
      "(want exit 2, got ${got})
${out}"
  fi
}

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
# the index scope check has to reject on this branch too - it is the branch a
# runner without jq takes, and "closed" is the only answer that becomes a pass
run_awk_case 'awk fallback: answer about another issue errors' 2 \
  '{"number":44,"title":"other","state":"closed"}'
run_awk_case 'awk fallback: stringified number errors' 2 \
  '{"number":"43","state":"closed"}'
run_awk_case 'awk fallback: nested number does not satisfy it' 2 \
  '{"milestone":{"number":43},"state":"closed"}'

# --- no test seams may creep back into the production path ------------------
# The suite stubs from outside the scripts on purpose. If someone reintroduces
# an env-controlled fetch, parser, issue index or remote, the guard becomes
# disable-able by anything that can write to the job environment on a
# self-hosted runner. Both scripts are covered: they run in the same job, on
# the same runner, under the same threat model, so a seam in either one is a
# seam in the guard.
#
# This used to be a grep for `$BLAST_RADIUS_*`, which is a name-shaped proxy
# for the property rather than the property: it passed happily while
# `GUARD_ISSUE="${GUARD_ISSUE:-43}"` sat in the script, and any future seam
# that avoided the prefix would have passed too. It is replaced by (a) an
# enumeration of every environment variable each script actually reads,
# checked against the documented allowlist, and (b) behavioural cases that run
# the guard under a hostile environment and assert the verdict does not move.
{
  for guard_script in "$SCRIPT" "$DIFF_SCRIPT"; do
    guard_name=$(basename "$guard_script")

    if grep -nE '(^|[^_[:alnum:]])eval[[:space:]]' "$guard_script" > /dev/null 2>&1; then
      no "${guard_name} contains no eval" "$(grep -nE '(^|[^_[:alnum:]])eval[[:space:]]' "$guard_script")"
    else
      ok "${guard_name} contains no eval"
    fi
  done

  # env_reads <file> - every name the file expands but never binds itself.
  #
  # Scope, stated rather than implied: this sees `$VAR` / `${VAR}` expansions in
  # the script text, plus the three read forms that carry NO `$` sigil and were
  # therefore invisible to the sigil pass alone - arithmetic contexts
  # (`if (( GUARD_SKIP ))`), the `-v` existence test (`[[ -v GUARD_SKIP ]]`) and
  # indirect expansion (`${!name}`, which reads `name`). Each of those is a
  # complete seam on its own: `if (( GUARD_SKIP )); then exit 0; fi` is four
  # words and no dollar sign. Variables the *shell* consults without the script
  # naming them - PATH above all, plus IFS and BASHOPTS - are structurally
  # invisible to it, and PATH is exactly how this suite substitutes `curl` and
  # `jq`. That is
  # not a gap in the allowlist but its boundary: the guard assumes a trusted
  # PATH on the runner, which its ability to execute the pinned scripts at all
  # already assumes. check-blast-radius.sh's header says so in the same terms.
  #
  # A name counts as bound only when it is assigned WITHOUT reading itself:
  # `X="${X:-default}"` both assigns and reads the environment, and treating it
  # as a binding is precisely how the GUARD_ISSUE seam stayed invisible. `for`
  # variables, `read` targets and bare `local` declarations are bindings too.
  env_reads() {
    local f="$1" refs bound
    refs=$( {
      grep -oE '\$\{?[A-Za-z_][A-Za-z0-9_]*' "$f" | sed 's/^\$[{]\?//'
      # arithmetic context: `(( VAR ))` and `$(( VAR + 1 ))` both read VAR
      grep -oE '\(\([[:space:]]*[A-Za-z_][A-Za-z0-9_]*' "$f" |
        sed -e 's/^((//' -e 's/^[[:space:]]*//'
      # `[[ -v VAR ]]` / `[ -v VAR ]`, optionally negated. Anchored to the test
      # bracket on purpose: a bare `-v` also introduces `command -v jq` and
      # `awk -v key=...`, neither of which reads an environment variable.
      grep -oE '\[\[?[[:space:]]+(![[:space:]]*)?-v[[:space:]]+[A-Za-z_][A-Za-z0-9_]*' "$f" |
        awk '{print $NF}'
      # indirect expansion: `${!name}` reads `name` (and, through it, whatever
      # name holds - so the name itself has to be accounted for)
      grep -oE '\$\{![A-Za-z_][A-Za-z0-9_]*' "$f" | sed 's/^\$[{]!//'
    } | grep -v '^$' | sort -u)
    bound=$( {
      grep -oE '(^|[[:space:];(])(local[[:space:]]+|export[[:space:]]+)?[A-Za-z_][A-Za-z0-9_]*=.*' "$f" |
        while IFS= read -r line; do
          local name=${line#"${line%%[![:space:]]*}"}
          name=${name#local }
          name=${name#export }
          name=${name%%=*}
          case "$line" in
            *"\$${name}"* | *"\${${name}"*) ;;
            *) printf '%s\n' "$name" ;;
          esac
        done
      grep -oE '\bfor[[:space:]]+[A-Za-z_][A-Za-z0-9_]*' "$f" | awk '{print $2}'
      grep -oE '\bread[[:space:]]+(-r[[:space:]]+)?[A-Za-z_][A-Za-z0-9_]*' "$f" | awk '{print $NF}'
      grep -oE '\blocal[[:space:]]+[A-Za-z_ ][A-Za-z0-9_ ]*' "$f" | sed 's/^local//' | tr ' ' '\n'
    } | grep -v '^$' | sort -u)
    comm -23 <(printf '%s\n' "$refs") <(printf '%s\n' "$bound")
  }

  # The scanner is load-bearing, so prove it detects a seam before trusting it
  # to report their absence. The synthetic file below is the exact shape that
  # slipped through the old grep.
  printf '%s\n' '#!/usr/bin/env bash' 'GUARD_ISSUE="${GUARD_ISSUE:-43}"' \
    'if [ -n "${SNEAKY_OVERRIDE:-}" ]; then echo "$GUARD_ISSUE"; fi' > "${TMP}/seamy.sh"
  seamy=$(env_reads "${TMP}/seamy.sh" | tr '\n' ' ')
  case "$seamy" in
    *GUARD_ISSUE*SNEAKY_OVERRIDE* | *SNEAKY_OVERRIDE*GUARD_ISSUE*)
      ok 'env-read scanner detects a reintroduced seam'
      ;;
    *) no 'env-read scanner detects a reintroduced seam' "(reported: ${seamy})" ;;
  esac

  # ... and the same for a seam with no `$` anywhere in it. An arithmetic test
  # is the cheapest complete kill switch there is - one line, no sigil - so the
  # scanner has to be shown catching it rather than assumed to.
  printf '%s\n' '#!/usr/bin/env bash' 'if (( GUARD_SKIP )); then exit 0; fi' \
    > "${TMP}/seamy-arith.sh"
  seamy_arith=$(env_reads "${TMP}/seamy-arith.sh" | tr '\n' ' ')
  case "$seamy_arith" in
    *GUARD_SKIP*) ok 'env-read scanner detects a sigil-free arithmetic seam' ;;
    *)
      no 'env-read scanner detects a sigil-free arithmetic seam' \
        "(reported: ${seamy_arith})"
      ;;
  esac

  # The allowlist, and what each entry can and cannot do:
  #   GITEA_API_TOKEN - a wrong token makes `curl -sSf` fail, and a failed
  #     lookup is exit 2, so it cannot flip the verdict on its own.
  #   GITEA_API_URL - runner-supplied connection detail, and NOT immune: an
  #     endpoint that answers this repo path with a closed #43 yields SKIPPED.
  #     No parse of the reply closes that, because the endpoint writes the
  #     reply; it is the same trusted-runner assumption as PATH, stated in the
  #     matcher's header. What the script does check is that the answer is
  #     about the index it asked for.
  #   GUARD_EXEMPT_LABEL - interpolated into the failure message and compared
  #     against nothing.
  # Note what is NOT on this list any more: GITEA_REPO. The repository holding
  # the tracker issue selects the verdict exactly as the issue index does -
  # point the guard at a repository whose #43 is closed (i.e. any repository
  # without one) and it reports SKIPPED, exit 0 - so it became a `--repo`
  # argument and the script no longer names it at all. The run_idx cases below
  # pin that behaviourally against a repo-sensitive stub.
  # Anything else the script *names* is a seam. The collector's allowlist is
  # empty. PATH is out of this scanner's reach by construction - see above.
  check_env_allowlist() {
    local f="$1" want="$2" got
    got=$(env_reads "$f" | tr '\n' ' ')
    got=${got%% }
    if [ "$got" = "$want" ]; then
      ok "$(basename "$f") reads only the allowlisted env vars"
    else
      no "$(basename "$f") reads only the allowlisted env vars" \
        "(want '${want}', got '${got}')"
    fi
  }
  check_env_allowlist "$SCRIPT" 'GITEA_API_TOKEN GITEA_API_URL GUARD_EXEMPT_LABEL'
  check_env_allowlist "$DIFF_SCRIPT" ''
}

# --- the lookup target is an argument, and the environment cannot move it ----
# WHICH issue gates the check IS the verdict, and that target has two halves:
# the index, and the repository the issue lives in. Point the guard at any
# already-closed issue - or at any repository that holds a closed #43, which
# anyone can create in a repository of their own - and it reports SKIPPED,
# exit 0. A repository with no #43 is not the seam: that lookup 404s, `curl
# -sSf` fails and the script exits 2, fail-closed. The header claims the
# environment cannot move either half; these cases are what makes the claim
# testable instead of a name-pattern grep.
#
# The stub curl here answers by repository AND index - open only for
# terraphim/gitea#43, closed for anything else - so "the environment did not
# redirect the lookup" and "the argument does redirect it" are distinguishable
# rather than both trivially passing, on both halves. Its "closed" branch
# models the reachable case, a repository that *has* a closed #43, not a 404.
{
  SHIM_IDX="${TMP}/bin-idx"
  make_shim_dir "$SHIM_IDX" with-jq
  cat > "${SHIM_IDX}/curl" << 'STUB'
#!/bin/sh
url=""
for a in "$@"; do
  case "$a" in http*) url="$a" ;; esac
done
idx=${url##*/}
rest=${url%/issues/*}
repo=${rest##*/repos/}
if [ "$repo" = "terraphim/gitea" ] && [ "$idx" = "43" ]; then
  printf '{"number":%s,"title":"tracker","state":"open"}' "$idx"
else
  # Every other repo/index here is a repository that HAS a closed issue at
  # that index - the reachable redirect. A repository missing the issue would
  # 404 instead, `curl -sSf` would fail and the script would exit 2, so that
  # case cannot produce a verdict and is not what this stub models.
  printf '{"number":%s,"title":"other","state":"closed"}' "$idx"
fi
exit 0
STUB
  chmod +x "${SHIM_IDX}/curl"

  idx_list="${TMP}/idx-list.txt"
  idx_changed="${TMP}/idx-changed.txt"
  printf '%s\n%s\n' "$HEADER" 'models/auth/oauth2.go' > "$idx_list"
  printf '%s\n' 'models/auth/oauth2.go' > "$idx_changed"

  # run_idx <expected-exit> <name> [env assignments...] -- [args...]
  run_idx() {
    local want="$1" name="$2" out got
    shift 2
    local envs=()
    while [ $# -gt 0 ] && [ "$1" != "--" ]; do
      envs+=("$1")
      shift
    done
    [ $# -eq 0 ] || shift
    # The case's own assignments come LAST, so a case may override the honest
    # defaults with a hostile value - `env` applies assignments left to right.
    out=$(env PATH="$SHIM_IDX" \
      GITEA_API_URL="$STUB_API_URL" GITEA_REPO="$STUB_REPO" \
      "${envs[@]+"${envs[@]}"}" \
      "$BASH" "$SCRIPT" --list "$idx_list" --changed "$idx_changed" "$@" 2>&1)
    got=$?
    if [ "$got" -eq "$want" ]; then
      ok "$name"
    else
      no "$name" "(want exit ${want}, got ${got})
${out}"
    fi
  }

  # baseline: the defaults are terraphim/gitea#43, the stub says open, the path
  # is reserved
  run_idx 1 'default lookup target is terraphim/gitea#43' --
  # the stub really is index- and repo-sensitive, so the env cases below cannot
  # pass vacuously: the argument DOES move the verdict on both halves
  run_idx 0 '--issue selects the gating issue (closed -> skipped)' -- --issue 99
  run_idx 0 '--repo selects the gating repository (closed -> skipped)' \
    -- --repo attacker/decoy

  # A case's own assignments really do reach the script and really do outrank
  # the honest defaults set alongside them - otherwise every hostile-env case
  # below would pass by never having been applied. GITEA_API_URL is a variable
  # the script does read, so emptying it must turn into exit 2.
  run_idx 2 'a case env assignment outranks the default' GITEA_API_URL= --

  # the R5 finding: GUARD_ISSUE in the environment must be inert
  run_idx 1 'GUARD_ISSUE in the env cannot redirect the lookup' \
    GUARD_ISSUE=99 --
  run_idx 1 'GUARD_ISSUE in the env cannot override --issue' \
    GUARD_ISSUE=99 -- --issue 43

  # the R6 finding: the same for the repository half of the target. A hostile
  # GITEA_REPO used to produce SKIPPED, exit 0, on this very changed path.
  run_idx 1 'GITEA_REPO in the env cannot redirect the lookup' \
    GITEA_REPO="$HOSTILE_REPO" --
  run_idx 1 'GITEA_REPO in the env cannot override --repo' \
    GITEA_REPO="$HOSTILE_REPO" -- --repo terraphim/gitea
  # and the script's internal name for it is not a seam either
  run_idx 1 'GUARD_REPO in the env cannot redirect the lookup' \
    GUARD_REPO="$HOSTILE_REPO" --
  # both halves hostile at once, which is what an attacker would actually set
  run_idx 1 'a wholly hostile environment cannot redirect the lookup' \
    GUARD_ISSUE=99 GUARD_REPO="$HOSTILE_REPO" GITEA_REPO="$HOSTILE_REPO" --

  # GUARD_EXEMPT_LABEL is message text; it must not be able to exempt anything
  run_idx 1 'GUARD_EXEMPT_LABEL cannot exempt a violation' \
    GUARD_EXEMPT_LABEL=sync-owner --

  # a bad --issue is a usage error, not a silently different lookup
  run_idx 2 '--issue rejects a non-numeric value' -- --issue not-a-number
  run_idx 2 '--issue rejects a path traversal' -- --issue '43/../1'
  run_idx 2 '--issue rejects zero' -- --issue 0
  run_idx 2 '--issue needs a value' -- --issue

  # nor is a bad --repo: the value goes into a URL path, so anything that is
  # not exactly one owner/name pair must fail closed rather than resolve to
  # some other endpoint
  run_idx 2 '--repo rejects a bare owner' -- --repo terraphim
  run_idx 2 '--repo rejects an extra path segment' -- --repo terraphim/gitea/issues
  run_idx 2 '--repo rejects a traversal' -- --repo 'terraphim/..'
  run_idx 2 '--repo rejects a traversal in the owner' -- --repo '../gitea'
  run_idx 2 '--repo rejects an empty owner' -- --repo /gitea
  run_idx 2 '--repo rejects an empty name' -- --repo terraphim/
  run_idx 2 '--repo rejects a URL' -- --repo 'https://evil.invalid/a/b'
  run_idx 2 '--repo rejects a shell metacharacter' -- --repo 'terraphim/gitea;id'
  run_idx 2 '--repo needs a value' -- --repo
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
  # the ADF gate is the only thing that runs this suite locally, so editing it
  # out is a way to disable the guard's own tests without touching .terraphim/*
  expect_real 'shipped list reserves the ADF gate' 1 '.adf-gates.sh'
  # ... but not the rest of CI
  expect_real 'shipped list leaves other workflows alone' 0 '.github/workflows/pull-compliance.yml'

  # A PR adding *any* file under .gitea/workflows/ makes ListWorkflows resolve
  # .gitea/workflows first and stop, hiding .github/workflows/* - this guard
  # included - from the merge onwards. The repo-layout block below pins that the
  # directory is absent from the tree; this pins that the guard would fail the PR
  # introducing it, which is the only check that runs on the PR itself.
  expect_real 'shipped list blocks a new .gitea/workflows file' 1 '.gitea/workflows/anything.yml'
  # ... and only that directory - the reservation must not spread over .gitea/
  expect_real 'shipped list does not over-reserve .gitea' 0 '.gitea/issue_template.md'

  if head -n 1 "$REAL_LIST" | grep -qx "$HEADER"; then
    ok 'shipped list carries the required header'
  else
    no 'shipped list carries the required header'
  fi

  # every entry must be reachable: no entry may be subsumed by another
  #
  # Pairs are excluded by INDEX, not by string equality. Skipping equal strings
  # made the one case the check is cheapest at catching - a verbatim duplicate -
  # structurally invisible: the two copies subsume each other, `[ "$a" = "$b" ]`
  # dropped the pair, and the dead second line stayed in the list unreported.
  entries=$(sed 's/#.*//' "$REAL_LIST" | sed 's/[[:space:]]*$//' | grep -v '^[[:space:]]*$')
  entry_list=()
  while IFS= read -r line; do
    entry_list+=("$line")
  done <<< "$entries"

  dead=""
  for i in "${!entry_list[@]}"; do
    for j in "${!entry_list[@]}"; do
      [ "$i" = "$j" ] && continue
      a=${entry_list[i]}
      b=${entry_list[j]}
      # shellcheck disable=SC2053 # intentional glob match against the entry
      if [[ $a == $b ]]; then
        dead="${dead}${a} subsumed by ${b}; "
      fi
    done
  done
  if [ -z "$dead" ]; then
    ok 'shipped list has no subsumed entries'
  else
    no 'shipped list has no subsumed entries' "$dead"
  fi

  # ... and duplicates named as duplicates rather than as self-subsumption, so
  # the report says what the fix is: delete the second copy.
  dupes=$(printf '%s\n' "$entries" | sort | uniq -d | tr '\n' ' ')
  dupes=${dupes%% }
  if [ -z "$dupes" ]; then
    ok 'shipped list has no duplicate entries'
  else
    no 'shipped list has no duplicate entries' "(repeated: ${dupes})"
  fi
}

# --- workflow placement: .gitea/workflows must stay absent ------------------
# modules/actions/workflows.go ListWorkflows() breaks on the FIRST entry of
# setting.Actions.WorkflowDirs (default [".gitea/workflows", ".github/workflows"])
# that resolves; it does not union them. This fork ships its CI in
# .github/workflows/, so a .gitea/workflows/ directory would hide every one of
# those workflows. Pin that, since the mistake is silent.
{
  if [ -z "$REPO_ROOT" ] && [ "$REPO_LAYOUT_REQUIRED" = "1" ]; then
    # A skip here would be indistinguishable from a pass, which is the exact
    # failure class the guard exists to eliminate. In CI the base checkout is
    # on disk, so not finding it is a broken harness, not "nothing to check".
    no 'repo-layout assertions ran' \
      "(CI markers are set but no checkout containing .github/workflows was found; tried GITHUB_WORKSPACE='${GITHUB_WORKSPACE:-<unset>}', $(dirname "$HERE") and git rev-parse --show-toplevel)"
  elif [ -z "$REPO_ROOT" ]; then
    skipped 'workflow placement' '(not running from a checkout)'
  else
    printf '     repo layout asserted against %s\n' "$REPO_ROOT"

    if [ -f "${REPO_ROOT}/.github/workflows/check-blast-radius.yml" ]; then
      ok 'guard workflow lives in .github/workflows'
    else
      no 'guard workflow lives in .github/workflows'
    fi

    # reserving .adf-gates.sh (above) only buys anything while the gate still
    # runs this suite: drop the invocation and every assertion here goes quiet
    # locally, with only the workflow's self-test step left to catch it.
    #
    # Anchored on the INVOCATION, not on the filename - same reasoning as the
    # --issue/--repo assertions below. The gate names this suite twice, once in
    # an `[ -f ... ]` existence test and once in the command that runs it, so a
    # grep for the bare string was satisfied by the existence test and would
    # have stayed green with the `bash ...` line deleted: the assertion could
    # not fail for the reason it exists. Only lines that put the script in
    # COMMAND position count. Continuations are joined and whitespace squeezed
    # first, so the gate may spell the invocation across lines.
    gate_runs=$(
      sed -e ':a' -e '/\\$/{N;s/\\\n/ /;ba' -e '}' "${REPO_ROOT}/.adf-gates.sh" |
        sed -e 's/[[:space:]][[:space:]]*/ /g' -e 's/^ //' |
        grep -cE '(^|[;&|]|(^|[[:space:]])(then|do|else)[[:space:]])[[:space:]]*((bash|sh)[[:space:]]+[^[:space:]]*|\.?/[^[:space:]]*)check-blast-radius_test\.sh("|[[:space:]]|$)'
    )
    if [ "$gate_runs" -gt 0 ]; then
      ok 'the ADF gate still invokes this suite'
    else
      no 'the ADF gate still invokes this suite' \
        '(no line RUNS it - an `[ -f ... ]` test that merely names the file is not an invocation; nothing else runs it locally, and the shell guards are outside make test-backend)'
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

    # The tracker issue and the repository holding it must both be pinned as
    # ARGUMENTS. As `GUARD_ISSUE:`/`GITEA_REPO:` env keys they would be two more
    # things on a self-hosted runner's job environment, and the script no longer
    # reads either - so a workflow that only set the env vars would silently
    # fall back to the built-in defaults instead of failing, which is the wrong
    # direction for a guard to drift.
    #
    # Both assertions below match the *invocation*, not the file: the enforcing
    # step's own header comment spells out "`--issue 43` and `--repo`" in prose,
    # so a grep for the bare flag was satisfied by the comment that documents it
    # and stayed green with the flag deleted from the command line - the
    # assertion could not fail for the reason it exists. Every
    # `bash .../check-blast-radius.sh` line is collected and each one must carry
    # both flags, so a second invocation cannot hide behind the first. Lines are
    # joined across `\` continuations and squeezed to a single space first,
    # which is what makes the flags matchable at all: the workflow writes each
    # one on its own continuation line.
    guard_cmds=$(
      sed -e ':a' -e '/\\$/{N;s/\\\n/ /;ba' -e '}' "$wf" |
        grep -E '^[[:space:]]*bash [^[:space:]]*check-blast-radius\.sh"?([[:space:]]|$)' |
        sed -e 's/[[:space:]][[:space:]]*/ /g' -e 's/^ //' -e 's/$/ /'
    )
    guard_cmds_total=$(printf '%s\n' "$guard_cmds" | grep -c '[^[:space:]]')

    if [ "$guard_cmds_total" -gt 0 ] &&
      [ "$(printf '%s\n' "$guard_cmds" | grep -cF -- ' --issue 43 ')" -eq "$guard_cmds_total" ]; then
      ok 'workflow pins the tracker issue via --issue'
    else
      no 'workflow pins the tracker issue via --issue' \
        "(check-blast-radius.sh takes the index as an argument, not from GUARD_ISSUE; ${guard_cmds_total} invocation(s) found)
${guard_cmds}"
    fi

    # Same reasoning for the repository: an unpinned lookup resolves against
    # whatever GITEA_REPO the job environment holds, and any repository with a
    # closed #43 yields SKIPPED, exit 0.
    if [ "$guard_cmds_total" -gt 0 ] &&
      [ "$(printf '%s\n' "$guard_cmds" | grep -cF -- ' --repo ')" -eq "$guard_cmds_total" ]; then
      ok 'workflow pins the gating repository via --repo'
    else
      no 'workflow pins the gating repository via --repo' \
        "(check-blast-radius.sh takes owner/name as an argument, not from GITEA_REPO; ${guard_cmds_total} invocation(s) found)
${guard_cmds}"
    fi

    if grep -qE '^[[:space:]]*GUARD_ISSUE:' "$wf"; then
      no 'workflow sets no GUARD_ISSUE env key' \
        "$(grep -nE '^[[:space:]]*GUARD_ISSUE:' "$wf")"
    else
      ok 'workflow sets no GUARD_ISSUE env key'
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

  # No remote is configured in this throwaway repo and none is needed: both
  # revisions are local, so blast-radius-diff.sh never reaches its fetch.
  # The collector emits NUL-terminated records, so translate before comparing -
  # a command substitution would drop the NULs (with a warning) and run the
  # paths together.
  out=$(cd "$repo" && bash "$DIFF_SCRIPT" --base "$base_sha" --head "$head_sha" 2> /dev/null | tr '\0' '\n')
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
  (cd "$repo" && bash "$DIFF_SCRIPT" --base "$base_sha" --head "$head_sha" --output "$changed_file") > /dev/null 2>&1
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
  (cd "$repo" && bash "$DIFF_SCRIPT" --base "$base_sha" --head "$head_sha" --output "$changed_file") > /dev/null 2>&1
  out=$(run_guard "$SHIM_JQ" "$OPEN_JSON" 0 --list "$REAL_LIST" --changed "$changed_file")
  got=$?
  if [ "$got" -eq 1 ]; then
    ok 'guard fails on a real reserved-path touch'
  else
    no 'guard fails on a real reserved-path touch' "(want exit 1, got ${got})
${out}"
  fi

  # An unresolvable base must fail loudly, not silently diff against nothing.
  # There is still no `origin` here, so the hardcoded fetch fails immediately
  # and offline - git resolves the remote name before it touches the network.
  out=$(cd "$repo" && bash "$DIFF_SCRIPT" --base 0000000000000000000000000000000000000000 2>&1)
  got=$?
  if [ "$got" -eq 2 ] && printf '%s' "$out" | grep -q 'cannot resolve base revision'; then
    ok 'unresolvable base fails loudly'
  else
    no 'unresolvable base fails loudly' "(exit ${got}: ${out})"
  fi

  # And the diagnostic must carry git's own reason, not just a fetch-depth
  # guess. `origin` now exists but points nowhere, which is the shape of the
  # real failure (a server that refuses to serve a bare SHA); the remote is
  # configured in the repo rather than injected through the environment,
  # because blast-radius-diff.sh deliberately reads no environment at all.
  git -C "$repo" remote add origin "${TMP}/no-such-remote.git"
  out=$(cd "$repo" && bash "$DIFF_SCRIPT" \
    --base 0000000000000000000000000000000000000000 2>&1)
  got=$?
  if [ "$got" -eq 2 ] && printf '%s' "$out" | grep -q 'git fetch said:.*no-such-remote'; then
    ok 'fetch failure reason is reported'
  else
    no 'fetch failure reason is reported' "(exit ${got}: ${out})"
  fi
}

# --- renames: moving a reserved file away must still trip the guard ---------
# `git diff --name-only` under git's default diff.renames=true prints only the
# DESTINATION of a detected rename, so `git mv services/lfs/server.go ...`
# reported the new path alone and the guard said PASSED - a silent
# non-enforcement on the single most conflict-inducing thing a PR can do to an
# in-flight cherry-pick. blast-radius-diff.sh passes --no-renames; these cases
# pin that, because nothing else here pins the *shape* of the diff output.
#
# The throwaway repo sets diff.renames=true explicitly rather than relying on
# the default: without it the case would pass for free on a box configured with
# diff.renames=false and prove nothing.
{
  rrepo="${TMP}/renames"
  mkdir -p "$rrepo"
  (
    cd "$rrepo" || exit 1
    git init --quiet .
    git symbolic-ref HEAD refs/heads/main
    git config user.email tester@example.com
    git config user.name tester
    git config commit.gpgsign false
    # the hazard is rename detection being ON; make the case independent of
    # whatever the box's global git config says
    git config diff.renames true
    mkdir -p services/lfs models/auth .terraphim
    # distinct, non-trivial content: a rename is only *detected* when the blob
    # is similar enough, so an empty file would not exercise the flag at all
    printf 'package lfs\nfunc A() {}\nfunc B() {}\nfunc C() {}\nfunc D() {}\n' > services/lfs/server.go
    printf 'package auth\nfunc E() {}\nfunc F() {}\nfunc G() {}\nfunc H() {}\n' > models/auth/oauth2.go
    printf '#!/bin/sh\n# guard matcher\nexit 0\n' > .terraphim/check-blast-radius.sh
    # present at the base so the deletion case below has something to remove
    printf 'lockfileVersion: 9\npackages: {}\n' > pnpm-lock.yaml
    printf 'root\n' > root.md
    git add -A
    git commit --quiet -m root
    git checkout --quiet -b feature
  ) > /dev/null 2>&1

  rbase=$(git -C "$rrepo" rev-parse main)

  # expect_rename <name> <from> <to>
  # commits the move on `feature`, collects the diff and asserts both that the
  # source path is reported and that the guard fails on it.
  expect_rename() {
    local name="$1" from="$2" to="$3" out got changed
    changed="${TMP}/rename-changed.txt"
    (
      cd "$rrepo" || exit 1
      mkdir -p "$(dirname "$to")"
      git mv "$from" "$to"
      git commit --quiet -m "rename ${from}"
    ) > /dev/null 2>&1
    local rhead
    rhead=$(git -C "$rrepo" rev-parse feature)

    # git really did detect this as a rename and really did drop the source -
    # otherwise --no-renames is being credited for something git never did and
    # the case proves nothing
    local plain
    plain=$(cd "$rrepo" && git diff --name-only "$rbase" "$rhead")
    if printf '%s\n' "$plain" | grep -qx "$to" &&
      ! printf '%s\n' "$plain" | grep -qx "$from"; then
      ok "${name}: plain --name-only drops the source (case is not vacuous)"
    else
      no "${name}: plain --name-only drops the source (case is not vacuous)" \
        "(reported: $(printf '%s' "$plain" | tr '\n' ' '))"
    fi

    (cd "$rrepo" && bash "$DIFF_SCRIPT" --base "$rbase" --head "$rhead" --output "$changed") > /dev/null 2>&1
    # the collector's records are NUL-terminated (see the quoting block below),
    # so a line-oriented grep has to be given lines first
    if tr '\0' '\n' < "$changed" | grep -qxF "$from"; then
      ok "${name}: source path survives the diff"
    else
      no "${name}: source path survives the diff" \
        "(collected: $(tr '\0' ' ' < "$changed"))"
    fi

    out=$(run_guard "$SHIM_JQ" "$OPEN_JSON" 0 --list "$REAL_LIST" --changed "$changed")
    got=$?
    if [ "$got" -eq 1 ] && printf '%s' "$out" | grep -q "$from"; then
      ok "${name}: guard fails and names the reserved source"
    else
      no "${name}: guard fails and names the reserved source" "(want exit 1, got ${got})
${out}"
    fi
  }

  # an exact entry (B4, the block the list itself calls the highest conflict
  # risk), a glob entry (B3), and the guard's own surface - narrowing the guard
  # by moving it must cost the same `sync-owner` label as editing it
  expect_rename 'renamed exact reserved entry' \
    'services/lfs/server.go' 'services/lfshandler/server.go'
  expect_rename 'renamed glob-covered reserved entry' \
    'models/auth/oauth2.go' 'models/authn/oauth2.go'
  expect_rename 'renamed guard matcher' \
    '.terraphim/check-blast-radius.sh' 'ci/check-blast-radius.sh'

  # plain deletion was never affected by this - keep it pinned so a future
  # change to the collector cannot trade one hole for the other.
  #
  # On its OWN branch off the same base, for the reason the quoting block below
  # spells out: `feature` now carries three renamed reserved paths, so a
  # deletion committed on top of it would fail the guard whether or not the
  # deletion itself was ever reported - the case could not fail for the reason
  # it exists. The pre-assertion pins the same thing from the other side: the
  # collected records must actually name the deleted path.
  (
    cd "$rrepo" || exit 1
    git checkout --quiet main
    git checkout --quiet -b deletion
    git rm --quiet pnpm-lock.yaml
    git commit --quiet -m 'delete a reserved path'
  ) > /dev/null 2>&1
  rhead=$(git -C "$rrepo" rev-parse deletion)
  changed_file="${TMP}/delete-changed.txt"
  (cd "$rrepo" && bash "$DIFF_SCRIPT" --base "$rbase" --head "$rhead" --output "$changed_file") > /dev/null 2>&1
  if tr '\0' '\n' < "$changed_file" | grep -qxF pnpm-lock.yaml; then
    ok 'deleted path is reported by the collector (case is not vacuous)'
  else
    no 'deleted path is reported by the collector (case is not vacuous)' \
      "(collected: $(tr '\0' ' ' < "$changed_file"))"
  fi
  out=$(run_guard "$SHIM_JQ" "$OPEN_JSON" 0 --list "$REAL_LIST" --changed "$changed_file")
  got=$?
  if [ "$got" -eq 1 ] && printf '%s' "$out" | grep -q 'pnpm-lock.yaml'; then
    ok 'deleting a reserved path still fails'
  else
    no 'deleting a reserved path still fails' "(want exit 1, got ${got})
${out}"
  fi

  # Both flag assertions below match the *invocation*, not the file: the
  # collector's header explains `--no-renames` and `-z` in prose, so a grep for
  # the bare flag was satisfied by the comment that documents it and stayed
  # green with the flag deleted from the command line - the assertion could not
  # fail for the reason it exists. Every `git diff` line is collected and each
  # one must carry both flags, so a second invocation cannot hide behind the
  # first. Lines are joined across `\` continuations and squeezed to a single
  # space first, so moving the flags onto a continuation line does not break
  # the match either.
  diff_cmds=$(
    sed -e ':a' -e '/\\$/{N;s/\\\n/ /;ba' -e '}' "$DIFF_SCRIPT" |
      grep -E '^[[:space:]]*git diff([[:space:]]|$)' |
      sed -e 's/[[:space:]][[:space:]]*/ /g' -e 's/^ //' -e 's/$/ /'
  )
  diff_cmds_total=$(printf '%s\n' "$diff_cmds" | grep -c '[^[:space:]]')

  # --no-renames must be on the command line, where it outranks every config
  # scope: a runner with diff.renames left at the default would otherwise get a
  # different verdict than one with it turned off, for the same PR.
  if [ "$diff_cmds_total" -gt 0 ] &&
    [ "$(printf '%s\n' "$diff_cmds" | grep -c -- ' --no-renames ')" -eq "$diff_cmds_total" ]; then
    ok 'collector passes --no-renames explicitly'
  else
    no 'collector passes --no-renames explicitly' \
      "(the verdict would then depend on the runner git config; ${diff_cmds_total} invocation(s) found)
${diff_cmds}"
  fi

  # -z is on the command line for exactly the same reason, against
  # core.quotePath rather than diff.renames - see the quoting block below.
  if [ "$diff_cmds_total" -gt 0 ] &&
    [ "$(printf '%s\n' "$diff_cmds" | grep -c -- ' -z ')" -eq "$diff_cmds_total" ]; then
    ok 'collector passes -z explicitly'
  else
    no 'collector passes -z explicitly' \
      "(without it core.quotePath C-quotes non-ASCII paths, which match no entry; ${diff_cmds_total} invocation(s) found)
${diff_cmds}"
  fi
}

# --- quoted paths: a non-ASCII name must not launder a reserved path --------
# `git diff --name-only` C-quotes any path holding a non-ASCII or control byte -
# core.quotePath defaults to TRUE - and check-blast-radius.sh compares the record
# verbatim against each entry, so the leading `"` makes every pattern fail. A
# file added under a reserved glob subtree with a non-ASCII name was therefore
# reported PASSED, and the verdict depended on the runner's core.quotePath the
# same way it used to depend on its diff.renames. blast-radius-diff.sh passes
# `-z`, which suppresses the quoting outright and terminates records with NUL.
#
# Both hazards get a case, on separate branches off the same base so neither can
# inherit the other's violation and pass for free. Each first asserts that plain
# --name-only DOES quote the path AND that the guard fed that quoted output
# passes - i.e. the hole is reproduced here before -z is credited with closing
# it. The newline case is why -z rather than `-c core.quotePath=false`: that
# setting unquotes non-ASCII bytes but still quotes control characters.
{
  qrepo="${TMP}/quoted"
  mkdir -p "$qrepo"
  # inside models/auth/*, a reserved glob on the shipped list
  nonascii_path='models/auth/héllo.go'
  newline_path=$'models/auth/two\nlines.go'
  (
    cd "$qrepo" || exit 1
    git init --quiet .
    git symbolic-ref HEAD refs/heads/main
    git config user.email tester@example.com
    git config user.name tester
    git config commit.gpgsign false
    # the hazard is quoting being ON; do not depend on the box's global config
    git config core.quotePath true
    printf 'root\n' > root.md
    git add -A
    git commit --quiet -m root

    git checkout --quiet -b nonascii
    mkdir -p models/auth
    printf 'package auth\n' > "$nonascii_path"
    git add -A
    git commit --quiet -m 'add a non-ASCII path under a reserved glob'

    git checkout --quiet main
    git checkout --quiet -b newline
    mkdir -p models/auth
    printf 'package auth\n' > "$newline_path"
    git add -A
    git commit --quiet -m 'add a control-character path under a reserved glob'
  ) > /dev/null 2>&1

  qbase=$(git -C "$qrepo" rev-parse main)

  # expect_quoted <name> <branch>
  #
  # Asserted on the guard's *verdict* and on the presence of a quote character,
  # never on the exact bytes of the path: a case-folding or normalising
  # filesystem (macOS) stores a different byte sequence for the same name, which
  # would make a byte-exact assertion fail for a reason that has nothing to do
  # with the property under test.
  expect_quoted() {
    local name="$1" branch="$2" qhead plain out got changed total stripped
    changed="${TMP}/quoted-changed.txt"
    qhead=$(git -C "$qrepo" rev-parse "$branch")

    plain=$(cd "$qrepo" && git diff --name-only "$qbase" "$qhead")
    if printf '%s\n' "$plain" | grep -q '^"models/auth/'; then
      ok "${name}: plain --name-only quotes it (case is not vacuous)"
    else
      no "${name}: plain --name-only quotes it (case is not vacuous)" \
        "(reported: ${plain})"
    fi

    printf '%s\n' "$plain" > "$changed"
    out=$(run_guard "$SHIM_JQ" "$OPEN_JSON" 0 --list "$REAL_LIST" --changed "$changed")
    got=$?
    if [ "$got" -eq 0 ]; then
      ok "${name}: the quoted form is what used to slip through"
    else
      no "${name}: the quoted form is what used to slip through" \
        "(want exit 0 for the quoted record, got ${got})
${out}"
    fi

    (cd "$qrepo" && bash "$DIFF_SCRIPT" --base "$qbase" --head "$qhead" --output "$changed") > /dev/null 2>&1
    if tr '\0' '\n' < "$changed" | grep -q '"'; then
      no "${name}: collector emits it unquoted" \
        "(collected: $(tr '\0' ' ' < "$changed"))"
    else
      ok "${name}: collector emits it unquoted"
    fi

    # and the records really are NUL-terminated, so that a future collector
    # change cannot drop -z and still pass the case above on some other box
    total=$(wc -c < "$changed")
    stripped=$(tr -d '\0' < "$changed" | wc -c)
    if [ "$total" -gt "$stripped" ]; then
      ok "${name}: collector output is NUL-terminated"
    else
      no "${name}: collector output is NUL-terminated" \
        "(no NUL in ${total} bytes)"
    fi

    out=$(run_guard "$SHIM_JQ" "$OPEN_JSON" 0 --list "$REAL_LIST" --changed "$changed")
    got=$?
    if [ "$got" -eq 1 ] && printf '%s' "$out" | grep -q -- '-> models/auth/\*'; then
      ok "${name}: guard fails on the reserved glob"
    else
      no "${name}: guard fails on the reserved glob" "(want exit 1, got ${got})
${out}"
    fi
  }

  expect_quoted 'non-ASCII path' nonascii
  expect_quoted 'newline in path' newline

  # why -z and not `-c core.quotePath=false`: the latter leaves control
  # characters quoted, so it would close the non-ASCII hole and keep the newline
  # one. If a future git stops quoting these, this case is the notice.
  qnl=$(git -C "$qrepo" rev-parse newline)
  unquoted=$(cd "$qrepo" && git -c core.quotePath=false diff --name-only "$qbase" "$qnl")
  if printf '%s\n' "$unquoted" | grep -q '^"models/auth/'; then
    ok 'core.quotePath=false alone would not have closed this'
  else
    no 'core.quotePath=false alone would not have closed this' \
      "(it no longer quotes a newline in a path: ${unquoted})"
  fi

  # the NUL-delimited reader must not have cost the newline-delimited input
  # path, which is what `--changed -` is fed by hand and what every case above
  # uses; a mixed input exercises both halves of the reader in one run
  {
    mixed="${TMP}/mixed-changed.txt"
    printf 'README.md\0models/auth/oauth2.go\0' > "$mixed"
    out=$(run_guard "$SHIM_JQ" "$OPEN_JSON" 0 --list "$REAL_LIST" --changed "$mixed")
    got=$?
    if [ "$got" -eq 1 ]; then
      ok 'NUL-delimited input is matched'
    else
      no 'NUL-delimited input is matched' "(want exit 1, got ${got})
${out}"
    fi

    # NUL records, then a tail with no terminator: the tail is split on newlines
    printf 'README.md\0docs/a.md\nmodels/auth/oauth2.go' > "$mixed"
    out=$(run_guard "$SHIM_JQ" "$OPEN_JSON" 0 --list "$REAL_LIST" --changed "$mixed")
    got=$?
    if [ "$got" -eq 1 ] && printf '%s' "$out" | grep -q 'models/auth/oauth2.go'; then
      ok 'a newline-delimited tail after the last NUL is still read'
    else
      no 'a newline-delimited tail after the last NUL is still read' "(want exit 1, got ${got})
${out}"
    fi

    printf 'README.md\0docs/a.md\0' > "$mixed"
    out=$(run_guard "$SHIM_JQ" "$OPEN_JSON" 0 --list "$REAL_LIST" --changed "$mixed")
    got=$?
    if [ "$got" -eq 0 ]; then
      ok 'NUL-delimited input with no reserved path passes'
    else
      no 'NUL-delimited input with no reserved path passes' "(want exit 0, got ${got})
${out}"
    fi
  }
}

echo
echo "passed: ${pass}  failed: ${fail}  skipped: ${skip}"
[ "$fail" -eq 0 ]
