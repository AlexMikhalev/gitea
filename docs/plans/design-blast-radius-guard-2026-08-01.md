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
   c. collect those paths with **`--no-renames`**. `git diff --name-only` under git's
      default `diff.renames=true` prints only the *destination* of a detected rename, so
      `git mv services/lfs/server.go …` reported the new path alone and the guard said
      PASSED — silent non-enforcement on the operation that conflicts hardest with an
      in-flight cherry-pick. Deletions were never affected (they are reported by path).
      `--no-renames` decomposes the rename into a delete plus an add so the reserved
      source is collected too, and putting it on the command line rather than in config
      keeps the verdict independent of the runner's git setup;
   d. collect them **NUL-terminated (`git diff -z`)**. Without `-z`, `core.quotePath` —
      which defaults to *true* — C-quotes any path holding a non-ASCII or control byte,
      so `models/auth/héllo.go` arrives as `"models/auth/h\303\251llo.go"`; the matcher
      compares the record verbatim against each entry and the leading `"` makes every
      pattern fail, so a file added under a reserved glob subtree with a non-ASCII name
      was reported PASSED. This is the identical failure shape as the rename hole,
      including the runner-config dependency (exit 0 on a default box, exit 1 on one with
      `core.quotePath=false`). `-z` rather than `-c core.quotePath=false`: the latter
      unquotes non-ASCII bytes but still quotes control characters, which would close the
      hole for `é` and leave it open for a newline. `check-blast-radius.sh` reads NUL
      records verbatim and still accepts newline-delimited input (what `--changed -` is
      fed by hand) for anything after the last NUL;
   e. fail if any changed path matches a blast-radius entry; added-only files pass.
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

## Threat model — what can still suppress the guard
The guard is a CI check, so the question that matters is not "does the matcher work" but
"what stops the job from ever being created". Two suppression vectors exist. One is closed
in-repo; the other cannot be, and is closed at the instance level.

- **Workflow-file removal and guard-source edits — closed in-repo.** `pull_request_target`
  reads the workflow from the base branch and the `Pin guard sources` step reads
  `.terraphim/*` out of the base ref, so neither the workflow nor the scripts nor the path
  list can be weakened by the PR under test. See Decision item 4 and the trigger item above.
- **`[skip ci]` in a PR title or head commit message — NOT closable in-repo.**
  `services/actions/notifier_helper.go:180` calls `skipWorkflows()` and returns on true
  **before** the `pull_request_target` detection block at `:214`. `skipWorkflows()`
  (`notifier_helper.go:245-264`) fires on `HookEventPush`, `HookEventPullRequest` and
  `HookEventPullRequestSync` — i.e. on `opened` and `synchronize` — and substring-matches
  every entry of `setting.Actions.SkipWorkflowStrings` against `PullRequest.Issue.Title`
  and against the head `commit.CommitMessage`. Both are controlled by the PR author, and
  the default list (`modules/setting/actions.go:33`) is `[skip ci]`, `[ci skip]`, `[no ci]`,
  `[skip actions]`, `[actions skip]`. On a default instance, a PR titled
  `fix auth [skip ci]` that also touches `models/auth/*` creates **no run at all**: not a
  failure, not a skip — nothing. On the PR's status list that is indistinguishable from
  "not started", which is the same silent-non-enforcement failure class as the
  `.gitea/workflows` shadowing and the unserved-runner-label hazard recorded above.
  Base-ref pinning does not help, because the base-ref copy is never consulted; the
  detection code never runs.
  This vector is **out of reach of any file in this repository** — the setting is
  instance-scoped, and the check happens before workflow discovery. It is therefore closed
  in the fork instance's `app.ini` (see "Setup — `SKIP_WORKFLOW_STRINGS`"), and the workflow
  header states the dependency so that an operator who copies the guard into another
  instance inherits the requirement rather than the hole.
  Partial recovery, recorded because it is what makes the exposure survivable rather than
  fatal: label changes emit `HookEventPullRequestLabel`
  (`services/actions/notifier.go:199-204`), which is *not* in `skipWorkflowEvents`, so
  adding or removing any label on a suppressed PR re-triggers the guard. That requires a
  human to notice the missing check first, so it is a recovery path, not a defence.

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
  lookup is an environment variable that can replace the verdict.
- No env seams, asserted as a *property* rather than as a name pattern. The suite used to
  grep for `$BLAST_RADIUS_*`, which is a proxy: it passed while `GUARD_ISSUE="${GUARD_ISSUE:-43}"`
  sat in the matcher, and *which* issue gates the check is the verdict — point it at any
  already-closed issue and the guard reports SKIPPED, exit 0. Two things replace it. The
  index is now an argument (`--issue 43`, passed by the workflow, which
  `pull_request_target` reads from the base branch), not an env read. So is the repository
  (`--repo`, validated as exactly one `owner/name` pair): *which* issue gates the check is
  two inputs, not one, and pointing the guard at any repository that *has* a closed #43 —
  which anyone can create in a repository of their own — yields SKIPPED and exit 0 just as
  surely as pointing it at a closed index. (A repository with no #43 is not the seam: that
  lookup 404s, `curl -sSf` fails and the script exits 2, fail-closed.) `GITEA_REPO` is **not read by any script**; the workflow
  passes `github.repository` through a step `env:` block into `--repo`. And the suite (a)
  enumerates every environment variable each script actually reads and checks it against a
  documented allowlist — exactly `GITEA_API_TOKEN`, `GITEA_API_URL` and `GUARD_EXEMPT_LABEL`
  for the matcher (the first two are connection details: misdirecting them fails the lookup,
  which fails closed; the third is message text, compared against nothing), and nothing at
  all for the collector — with a self-test proving the scanner detects a reintroduced seam,
  and (b) runs the guard with `GUARD_ISSUE`, `GITEA_REPO` and `GUARD_EXEMPT_LABEL` set to
  hostile values against a stub that is sensitive to **both** the index and the repository —
  it answers "open" only for `terraphim/gitea#43` and "closed" for anything else — and
  asserts the verdict does not move. A stub keyed on the index alone would have passed while
  `GITEA_REPO` still steered the lookup. `eval` is still barred in both scripts;
  `blast-radius-diff.sh` hardcodes `origin` as its fetch remote for the same reason, and the
  fetch-diagnostic case configures a dead `origin` in its throwaway repo instead of
  injecting one through the environment. The boundary of that claim is stated where it is
  made, in both the matcher's header and the scanner's: `curl` and `jq` are still resolved
  through `PATH`, which is an environment variable and is precisely how the suite
  substitutes them. The guard assumes a trusted `PATH` on the runner — implied already by
  its ability to execute the pinned scripts — and under `pull_request_target` no PR-authored
  file is ever placed there. The scanner reads `$VAR` expansions in the script text, so
  `PATH`/`IFS` are outside it by construction, not overlooked.
- Renames: a throwaway repo with `diff.renames=true` set explicitly (so the case does not
  pass for free on a box that has it off) moves a reserved file out of its reserved path —
  an exact entry (`services/lfs/server.go`), a glob-covered one (`models/auth/*`) and the
  guard's own matcher — and asserts the source path survives the collector and the guard
  exits 1 naming it. Each case first asserts that plain `--name-only` *does* drop the
  source, so it cannot pass vacuously. Deletion is pinned alongside, since it was never
  affected and a future change to the collector must not trade one hole for the other.
- Quoted paths: a second throwaway repo with `core.quotePath=true` set explicitly adds a
  non-ASCII path and a newline-bearing path under `models/auth/*`, on two branches off the
  same base so neither inherits the other's violation. Each case asserts that plain
  `--name-only` quotes the path **and that the guard, fed that quoted record, exits 0** —
  the hole is reproduced before `-z` is credited with closing it — then that the collector
  emits it unquoted and NUL-terminated and the guard exits 1 naming the reserved glob. One
  further case pins that `-c core.quotePath=false` alone would *not* have closed the
  newline variant, i.e. records why `-z` is the fix. Verdicts and quote-presence are
  asserted, never the exact bytes of the path, so a normalising filesystem (macOS) cannot
  fail the case for an unrelated reason. The NUL-delimited reader keeps the
  newline-delimited input path, which is what `--changed -` is fed by hand, and mixed input
  exercises both halves in one run.
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

## Setup — instance prerequisites (one-time, required before this lands on `main`)
Two things must be provisioned on the fork instance before the guard is meaningful. Neither
can be created by this PR: one is a database object, the other is instance configuration.

### Setup — `SKIP_WORKFLOW_STRINGS` (required — without it the guard is bypassable by PR title)
Per the threat model above, `skipWorkflows()` runs before `pull_request_target` detection
and matches PR-author-controlled text, so the default `[skip ci]` family silently suppresses
this check. Neutralise it in the fork instance's `app.ini`, alongside the `sync-owner` label
provisioning below:

```ini
[actions]
; Neutralised for the blast-radius guard (#58): skipWorkflows() runs before
; pull_request_target detection, so any author-settable skip string is a bypass.
; This value is a sentinel that no PR title or commit message will contain.
SKIP_WORKFLOW_STRINGS = [never-skip-ci-a9f1c2e4]
```

Two spellings that look correct and are not — both verified against
`gopkg.in/ini.v1@v1.67.1`, the version this fork pins, by loading each through
`Section("actions").MapTo(&struct{ ... }{defaults})`:

- **`SKIP_WORKFLOW_STRINGS =` (empty) is a silent no-op.** `setSliceWithProperType`
  (`struct.go:89-92`) returns early when the value parses to zero elements, leaving the
  target field — i.e. the compiled-in default list — untouched. The config reads as if the
  hole were closed while `[skip ci]` still works. This is worth stating explicitly because
  an empty assignment is the obvious thing to reach for.
- **`SKIP_WORKFLOW_STRINGS = ,` is actively dangerous.** It parses to `[]string{""}`, and
  `strings.Contains(s, "")` is true for every `s`, so *every* push and pull request on the
  instance skips *all* workflows. Do not use it.

A sentinel string is the working form: it is a non-empty one-element list, so it replaces
the default, and no real title or commit message contains it. Verify after restart with
`grep -A2 '^\[actions\]' app.ini` and by opening a throwaway PR titled `test [skip ci]` and
confirming a run is created.

### Setup — the `sync-owner` label (one-time, required before this lands on `main`)
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
  `go build`, `go vet` and `make test-backend`. Three exclusions are argued in the file's
  header: `tests/` (live-server harnesses, exercised by CI instead), `models/migrations/...`
  (upstream's own `GO_TEST_PACKAGES` filters them out — they belong to the
  `make migrations.sqlite.test` harness), and, only when the detected git is older than
  2.38, the `merge-tree --write-tree` dependent packages.
- `make lint-yaml`, `make lint-actions` — required for this change (see the caveat above).
- `make fmt`, `make lint-go`, `make test-backend` — only if any `.go` file ends up touched.
- `make lint-js` — only if any `.ts` file ends up touched.
- Trailing whitespace stripped; commit message `chore: add upstream-sync blast radius guard`.
