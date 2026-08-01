# Design Gate — Upstream-sync blast-radius guard (issue #58, epic #53)

## Problem
Buzz uplift work (F1–F4) and the in-flight upstream sync cherry-picks (#43–#51: OAuth,
mermaid, wiki/LFS tokens, avatar fetch, link-account sync) touch overlapping files.
Concurrent edits produce cherry-pick conflicts and silent regressions. We need a
mechanical guard that blocks uplift PRs from touching sync-owned paths while #43 is open.

## Decision — exact touchpoints
1. `.terraphim/sync-blast-radius.txt` (new; dir does not yet exist). One path or glob per
   line, `#` comments. Header line: `# uplift PRs must not modify these files until #43 closes`.
   Content derived from the diffs of #43–#51 (B1–B5/F1–F8), **with one recorded
   exception**: the B3 block reserves `models/auth/*` and `routers/web/auth/*` as a
   deliberate over-reservation, because research-43 never enumerated the auth files that
   pick touches. Because `*` spans `/`, those two entries reserve the whole subtrees and
   will block unrelated auth work for as long as #43 is open. That is the accepted trade —
   a missed conflict in the OAuth picks costs more than a `sync-owner` label on an auth PR —
   and it is annotated as such in the file itself. Narrow both entries once the B3 diff
   exists. Every other entry is diff-derived.
2. `.github/workflows/check-blast-radius.yml` (**not** `.gitea/workflows/`; see the
   resolved ground-truth item below). Trigger `on: pull_request_target`. Job:
   a. query issue #43 state via Gitea API; exit 0 (skip) when `state != "open"`;
   b. compute PR diff paths against the **merge base** of base and head (three-dot), not
      against the base tip — the base branch moves under the PR by construction here;
   c. fail if any changed path matches a blast-radius entry; added-only files pass.
3. No Go/TS source changes. No changes to existing workflows.
4. The guard's own surface (`.terraphim/*`, `.github/workflows/check-blast-radius.yml`) is
   itself on the reserved-path list. Base-ref pinning already stops an edit from taking
   effect in the PR that carries it; the list entries are what make the edit *visible*
   rather than silently inherited by the next PR.

## Schema / enum ground truth (verified — resolved)
- Issue state enum: `GET /api/v1/repos/{owner}/{repo}/issues/{index}` → `.state` is
  `"open" | "closed"`. It must be read from the **top level**: `modules/structs/issue.go`
  serialises `milestone` (which has a `state` of its own) *before* the issue's `state`, so
  a first-textual-match parse returns the milestone's state and disables the guard whenever
  #43 is attached to a closed milestone. Read via `jq -e`, with a depth-aware `awk`
  fallback when jq is absent.
- **Gitea Actions workflow discovery — RESOLVED, and the earlier assumption was wrong.**
  Both directories are *supported*, but they are not *combined*.
  `modules/actions/workflows.go:53` (`ListWorkflows`) iterates
  `setting.Actions.WorkflowDirs` — defaulted to `[".gitea/workflows", ".github/workflows"]`
  at `modules/setting/actions.go:34` — and `break`s on the first directory that resolves.
  This fork ships all fourteen of its workflows in `.github/workflows/`, so creating a
  `.gitea/workflows/` directory would make `ListWorkflows` return that directory only and
  every existing workflow (`pull-compliance`, `pull-db-tests`, `pull-e2e-tests`, the
  `release-*` and `cron-*` set) would stop being discovered — silently. The guard therefore
  lives in `.github/workflows/`. `check-blast-radius_test.sh` asserts both that the file is
  there and that `.gitea/workflows/` does not exist, because the failure mode reports nothing.
- **Trigger — RESOLVED.** `on: pull_request` detects workflows from the PR's own head
  commit (`services/actions/notifier_helper.go:203-212`); only `pull_request_target`
  workflows are read from the base branch (`notifier_helper.go:214-234`). Under
  `pull_request` a PR can delete the guard workflow in the same commit that touches a
  reserved path and no job is ever created — pinning `.terraphim/*` to the base ref does
  not help, because nothing invokes it. The trigger is `pull_request_target`. The usual
  `pull_request_target` escalation hazard does not apply: every step reads only paths
  (`git diff --name-only`, `git show BASE:…`) and the PR's tree is never checked out or
  executed. `WithPullRequest` is set on label changes
  (`services/actions/notifier.go:257`), so `labeled`/`unlabeled` still re-trigger the job
  and the `sync-owner` exemption takes effect without a push.
  Consequence: the guard does not run on the PR that introduces it — that PR is not on the
  base branch yet. The first real run is the PR after this one merges.
- **Runner label — RESOLVED.** The job runs on `[self-hosted, bigbox]`, the only label this
  fork has a demonstrated runner behind: `.github/workflows/sentrux-quality-gate.yml:20` has
  been dispatching to it since `05fe156a51`, and that job does `actions/checkout`, `curl`
  and `awk` in `run:` blocks, i.e. the toolchain this guard needs. Every other `runs-on` in
  `.github/workflows/` is `ubuntu-latest` or a `namespace-profile-gitea-release-*` label.
  This matters because an unserved label is *silent*: Gitea creates the run and leaves the
  job unassigned, and `timeout-minutes` bounds step execution, not queue time, so the check
  would sit "waiting" forever — on a PR status list, indistinguishable from "not started".
  That is the same failure class as the `.gitea/workflows` mistake and it is unobservable
  from the introducing PR, since `pull_request_target` means the guard cannot run on itself.
  For the same reason `.github/actionlint.yaml` lists only labels this fork actually uses:
  the allowlist silences the linter, it does not make a runner exist.
- `actionlint` (`make lint-actions`, pinned `v1.7.10` at Makefile:24) defaults to
  `.github/workflows`, so it reaches the guard with no change. The target still passes both
  workflow dirs explicitly via `$(wildcard …)` for future-proofing; non-existent patterns
  drop out, so actionlint never receives a literal glob.
- PR diff source: `pull_request.base.sha` / `head.sha` are present on this Gitea version.
  Under `pull_request_target` the checkout is the base branch, so the head commit is fetched
  explicitly from `refs/pull/<index>/head` (`modules/git/ref.go:17`) — and fetched only,
  never checked out.
- `.yamllint.yaml` (`extends: default`) applies repo-wide — the new YAML must satisfy it.

## Acceptance criteria
- `.terraphim/sync-blast-radius.txt` exists, has the required header, and lists every path
  touched by #43–#51 cherry-picks (plus the recorded B3 over-reservation above).
- PR modifying a listed path → workflow job fails with the offending paths in the log.
- PR adding a new, unlisted file → job passes.
- With #43 closed, the job short-circuits to success regardless of touched paths.
- Deleting or editing the guard in the PR under test does not weaken that PR's own run.
- `.terraphim/check-blast-radius_test.sh` passes, and is *run* by both `./.adf-gates.sh`
  and the workflow's own self-test step (an unrun suite pins nothing).
- `make lint-yaml` and `make lint-actions` pass **on this workflow**. Note both targets are
  red repo-wide for a pre-existing reason this branch did not introduce:
  `.github/workflows/sentrux-quality-gate.yml:141` is not valid YAML (prose at column 1
  where a mapping key is expected), on `main` since `05fe156a51`. Fixing it is a separate
  precursor change; until it lands, verify this file in isolation.

## Non-goals
- No enforcement of merge order or branch protection rules.
- No auto-generation of the path list from the API; it is a reviewed static file this phase.
- No blocking on issues other than #43; no coverage of direct pushes to `main`.
- No changes to sync cherry-pick content itself (#43–#51).

## Test plan
- Unit: matcher script exercised with a table of (changed path, list entry, expect) cases —
  exact match, glob match, non-match, added-only file, empty diff.
- Stubbed API: the issue endpoint is stubbed with `state:"open"`, `state:"closed"`, nested
  payloads and a failing lookup; assert fail, skip and fail-closed respectively. No network.
  The stubbing is done from *outside* the script — a PATH holding a stub `curl` and, for the
  fallback cases, no `jq` — because an environment variable that can replace the issue
  lookup is an environment variable that can replace the verdict. The suite asserts that
  *both* guard scripts contain no `eval` and read no `BLAST_RADIUS_*` variable;
  `blast-radius-diff.sh` hardcodes `origin` as its fetch remote for that reason, and the
  fetch-diagnostic case configures a dead `origin` in its throwaway repo instead of
  injecting one through the environment.
- Repo layout: the suite asserts the guard is in `.github/workflows`, that `.gitea/workflows`
  does not exist, that the trigger is `pull_request_target`, and that the exempt-label
  literal agrees with both `if:` conditions. These must *run in CI*, not only locally — the
  pinned suite executes from `$RUNNER_TEMP`, so it resolves the repo from the runner's
  `$GITHUB_WORKSPACE` (the base checkout under `pull_request_target`), falling back to the
  toplevel of the working directory, and **fails** rather than skips when CI markers are set
  but no checkout is reachable. A skipped invariant reports green. The
  `${{ github.workspace }}` expression is deliberately *not* used to pass this:
  `services/actions/context.go:84` leaves the server-side `workspace` value empty, so it
  would clobber the runner's own correct value.
- Live: throwaway PR touching a listed file → CI red; second PR adding a new file → CI green
  (the verification step named in issue #58). This must be done after the guard merges,
  since `pull_request_target` workflows only run once they are on the base branch.

## Setup — the `sync-owner` label (one-time, required before this lands on `main`)
Reserving `.terraphim/*` and the workflow file (Decision item 4) makes `sync-owner`
load-bearing: once #43 is open and the guard is on `main`, *every* change to the guard
itself fails the check until the PR carries that label. The label is not a repository
artefact — Gitea labels live in the database, not in the tree — so nothing in this PR can
create it, and the guard's failure message would otherwise instruct an author to apply a
label that does not exist.

- **Owner: the terraphim/gitea maintainer driving the #43–#51 sync** (the `terraphim` org
  owner). Creating and applying the label both require repository *write* access, which is
  deliberate: an outside contributor cannot self-exempt, so applying `sync-owner` is the
  review checkpoint. The same maintainer is expected to apply it to the #43–#51 sync PRs
  and to any guard-maintenance PR.
- **Provision it before merging this PR**, either in the repo's Issues → Labels UI or with:

  ```bash
  curl -sSf -X POST "https://git.terraphim.cloud/api/v1/repos/terraphim/gitea/labels" \
    -H "Authorization: token $GITEA_TOKEN" -H 'Content-Type: application/json' \
    -d '{"name":"sync-owner","color":"#b60205",
         "description":"Exempt from the upstream-sync blast-radius guard (#58); the PR is part of the #43-#51 sync or maintains the guard itself"}'
  ```

- This is not a deadlock if it is forgotten: the verdict is computed from the base ref, so a
  maintainer can create and apply the label on the failing PR without a new commit. It is
  an undocumented prerequisite on the only exit from a self-imposed block, which is why it
  is recorded here rather than left implicit.

## Gates (repo toolchain)
- `./.adf-gates.sh` — the fork-wide ADF gate contract, and the entry point that owns
  `.adf-gates.sh`. It runs the blast-radius guard's suite, then a sqlite-tagged
  `go build`, `go vet` and `make test-backend`. Two exclusions are argued in the file's
  header: `tests/` (live-server harnesses, exercised by CI instead) and, only when the
  detected git is older than 2.38, the `merge-tree --write-tree` dependent packages.
- `make lint-yaml`, `make lint-actions` — required for this change (see the caveat above).
- `make fmt`, `make lint-go`, `make test-backend` — only if any `.go` file ends up touched.
- `make lint-js` — only if any `.ts` file ends up touched.
- Trailing whitespace stripped; commit message `chore: add upstream-sync blast radius guard`.
