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
  lookup is an environment variable that can replace the verdict. The suite asserts the
  script contains no `eval` and no such override.
- Live: throwaway PR touching a listed file → CI red; second PR adding a new file → CI green
  (the verification step named in issue #58). This must be done after the guard merges,
  since `pull_request_target` workflows only run once they are on the base branch.

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
