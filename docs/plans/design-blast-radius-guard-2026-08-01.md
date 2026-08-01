# Design Gate — Upstream-sync blast-radius guard (issue #58, epic #53)

## Problem
Buzz uplift work (F1–F4) and the in-flight upstream sync cherry-picks (#43–#51: OAuth,
mermaid, wiki/LFS tokens, avatar fetch, link-account sync) touch overlapping files.
Concurrent edits produce cherry-pick conflicts and silent regressions. We need a
mechanical guard that blocks uplift PRs from touching sync-owned paths while #43 is open.

## Decision — exact touchpoints
1. `.terraphim/sync-blast-radius.txt` (new; dir does not yet exist). One path or glob per
   line, `#` comments. Header line: `# uplift PRs must not modify these files until #43 closes`.
   Content derived from the diffs of #43–#51 (B1–B5/F1–F8), not guessed.
2. `.gitea/workflows/check-blast-radius.yml` (new dir; repo currently ships CI only in
   `.github/workflows/`, e.g. `files-changed.yml`). Trigger `on: pull_request`. Job:
   a. query issue #43 state via Gitea API; exit 0 (skip) when `state != "open"`;
   b. compute PR diff paths against the base sha;
   c. fail if any changed path matches a blast-radius entry; added-only files pass.
3. No Go/TS source changes. No changes to existing workflows.

## Schema / enum ground truth (verify before coding — do not assume)
- Issue state enum: `GET /api/v1/repos/{owner}/{repo}/issues/{index}` → `.state` is
  `"open" | "closed"`. Confirmed against `.adf-issue.json` (`"state":"open"`). Verify #43's
  payload directly with the same endpoint before relying on the field.
- Gitea Actions workflow discovery: confirm the runner picks up `.gitea/workflows/` in this
  deployment (both dirs are supported upstream) before assuming the file runs at all.
- `actionlint` (`make lint-actions`, pinned `v1.7.10` at Makefile:24) defaults to
  `.github/workflows`; verify whether it scans `.gitea/workflows` or needs an explicit path.
- PR diff source: verify the event payload actually exposes `pull_request.base.sha` /
  `head.sha` on this Gitea version; otherwise use `git diff --name-only` after a fetch of base.
- `.yamllint.yaml` (`extends: default`) applies repo-wide — the new YAML must satisfy it.

## Acceptance criteria
- `.terraphim/sync-blast-radius.txt` exists, has the required header, and lists every path
  touched by #43–#51 cherry-picks.
- PR modifying a listed path → workflow job fails with the offending paths in the log.
- PR adding a new, unlisted file → job passes.
- With #43 closed, the job short-circuits to success regardless of touched paths.
- `make lint-yaml` and `make lint-actions` pass.

## Non-goals
- No enforcement of merge order or branch protection rules.
- No auto-generation of the path list from the API; it is a reviewed static file this phase.
- No blocking on issues other than #43; no coverage of direct pushes to `main`.
- No changes to sync cherry-pick content itself (#43–#51).

## Test plan
- Unit: matcher script exercised with a table of (changed path, list entry, expect) cases —
  exact match, glob match, non-match, added-only file, empty diff.
- Mocked API: stub the issue endpoint with `state:"open"` and `state:"closed"`; assert fail
  and skip paths respectively. No network in unit runs.
- Live: throwaway PR touching a listed file → CI red; second PR adding a new file → CI green
  (the verification step named in issue #58).

## Gates (repo toolchain)
- `make lint-yaml`, `make lint-actions` — required for this change.
- `make fmt`, `make lint-go`, `make test-backend` — only if any `.go` file ends up touched.
- `make lint-js` — only if any `.ts` file ends up touched.
- Trailing whitespace stripped; commit message `chore: add upstream-sync blast radius guard`.
