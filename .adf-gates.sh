#!/usr/bin/env bash
# ADF gate override for the terraphim/gitea fork.
#
# This is the gate contract for ALL uplift work on this fork, not for any one
# issue: every ADF task runs `./.adf-gates.sh` and it must exit 0 before the
# task's PR is opened. It is documented as such in
# docs/plans/design-blast-radius-guard-2026-08-01.md ("Gates (repo toolchain)"),
# which is where its exclusions are argued.
#
# Gates = the shell guards' own suites + tagged build + vet + the repo's own
# unit-test target (make test-backend). A plain `go test ./...` cannot work
# here: the fork needs the sqlite build tags, and without the Makefile's test
# env the migration tests refuse to run.
#
# INTENTIONAL EXCLUSION - tests/
# `tests/` holds the live-server harnesses (tests/integration, tests/e2e), not
# unit tests. They require a built Gitea binary, a provisioned data dir and a
# running server; the ADF gate stands up none of that, so including them would
# report infrastructure failures, not code failures. They are exercised by fork
# CI (the pull-e2e-tests / db-tests workflows) against a real server, which is
# the only place they are meaningful. Excluding them here is deliberate and is
# not a coverage gap - if you change anything under tests/, rely on CI.
#
# CONDITIONAL EXCLUSION - git < 2.38
# modules/git, modules/gitrepo, services/gitdiff and services/pull need
# `git merge-tree --write-tree`, which landed in git 2.38; on an older git they
# fail for the environment, not for the code. The exclusion below is applied
# only when the *detected* git is older than 2.38, so a contributor on a modern
# git gets that coverage locally. It is self-clearing: upgrade the box's git and
# the packages come back with no edit to this file.
set -euo pipefail

# The upstream-sync blast-radius guard is shell, so `make test-backend` never
# reaches it and neither does any lint target. Its suite is the only thing that
# pins the issue-state parser (jq and awk paths), the three-dot diff range, the
# list-subsumption invariant and the workflow's placement in .github/workflows;
# unrun, all of them regress silently. It needs no network and takes under a
# second, so it runs first - a broken guard should not cost a full build.
if [ -x .terraphim/check-blast-radius_test.sh ] || [ -f .terraphim/check-blast-radius_test.sh ]; then
  bash .terraphim/check-blast-radius_test.sh
fi

go build -tags 'sqlite sqlite_unlock_notify' ./...
go vet ./...

# Unit tests need the generated test config, else RepoRootPath defaults into
# the dev data dir and PrepareTestEnv's SyncDirs guard refuses to run.
make generate-ini-sqlite
export GITEA_TEST_CONF=tests/sqlite.ini

GIT_VERSION=$(git version | awk '{print $3}')
GIT_MAJOR=${GIT_VERSION%%.*}
GIT_REST=${GIT_VERSION#*.}
GIT_MINOR=${GIT_REST%%.*}
[ "$GIT_MAJOR" -eq "$GIT_MAJOR" ] 2> /dev/null || GIT_MAJOR=0
[ "$GIT_MINOR" -eq "$GIT_MINOR" ] 2> /dev/null || GIT_MINOR=0

EXCLUDE_RE='/tests/'
if [ "$((GIT_MAJOR * 1000 + GIT_MINOR))" -lt 2038 ]; then
  echo ".adf-gates: git ${GIT_VERSION} < 2.38, excluding the merge-tree-dependent packages" >&2
  EXCLUDE_RE="${EXCLUDE_RE}|/(modules/git|modules/gitrepo|services/gitdiff|services/pull)\$"
fi

PKGS="$(go list ./... | grep -v -E "$EXCLUDE_RE" | tr '\n' ' ')"
make test-backend GO_TEST_PACKAGES="$PKGS"
