---
name: Research -- Upstream Feature Commit Triage (F1-F8)
description: Phase 1 research for issue #50. Pick/defer/reject decisions for the 8 upstream feature commits flagged in #43, with conflict-risk measurement, value assessment against this fork's Actions/ADF usage, and the mandated F4 robot-token audit.
type: project
---

# Research Document: Upstream Feature Commit Triage

**Issue:** terraphim/gitea#50
**Parent:** terraphim/gitea#43
**Date:** 2026-08-10
**Analyst:** adf-gitea-sec
**Baseline:** `origin/main` at `c40ac0b19b` (all five security picks #45-#49 merged)

---

## Executive Summary

Of the eight upstream feature commits flagged in #43, **three are accepted** (F7, F5, F6 -- all Actions operational capability, matching this fork's actual usage), **three are deferred** (F1, F8, and F4 pending a decision only Alex can make), and **two are rejected** (F2, F3).

Two structural findings dominate the triage and matter more than any individual verdict:

1. **F5 has a hard dependency on F7.** F7 creates the `services/actions/rerun.go` surface that F5 extends. F5's measured conflicts are an artefact of picking it in the wrong order.
2. **Any upstream feature carrying a schema migration collides with this fork's own migrations.** Fork migrations 327 and 328 are occupied by ADF features. Upstream F6 wants 327 and F4 wants 328.

A third point is methodological and carries forward from the security chain: **`git merge-tree` reporting "clean" is a lower bound on risk, not a verdict.** It said clean for two of the five security picks and both failed to compile. In this batch it does something subtler and worse in F4 -- see the audit below.

## Method

For each commit: inspect the upstream change, measure conflict surface against current main with an in-memory three-way merge (`git merge-tree --write-tree --merge-base=<sha>^ origin/main <sha>`, nothing mutated), then weigh value against how this fork is actually used -- it runs Gitea Actions with robot tokens for the ADF fleet, so Actions operability carries weight and cosmetic UI carries little.

## Decision Table

| ID | Upstream | PR | Feature | Conflicts | Files | Verdict |
|---|---|---|---|---|---|---|
| F7 | `054eb6d8a5` | #36768 | Actions API rerun endpoints | **none** | 9 | **PICK** (1st) |
| F5 | `b22123ef86` | #36924 | UI: re-run failed jobs button | 6 (inflated) | 12 | **PICK** (2nd, after F7) |
| F6 | `b3b2d111da` | #36776 | Per-runner Disable/Pause | 4 | 27 | **PICK** (3rd, migration renumber) |
| F1 | `e7af84df72` | #37275 | Actions post-run cleanup on cancel | 12 | 31 | **DEFER** |
| F8 | `a8505269ca` | #36248 | Workflow dependency visualisation | **none** | 11 | **DEFER** |
| F4 | `45809c8f54` | #36173 | Configurable Actions token permissions | 7 | 57 | **DEFER -- needs Alex's decision** |
| F2 | `eb93981d45` | #36514 | Branch protection bypass allowlist | 10 | 23 | **REJECT** |
| F3 | `34fd3c9f06` | #37410 | Default PR branch update style | 11 | 20 | **REJECT** |

### Correction to #43's risk estimates

`.docs/research-43.md` rated **F1 "Low; small touch"**. It is 31 files, 786 insertions, 12 conflicts, in core Actions internals (`models/actions/runner.go`, `services/actions/clear_tasks.go`, `services/actions/commit_status.go`). That estimate should not be relied on. F8 was rated "Low; frontend" and is accurate on risk but is 1,064 insertions including a 971-line Vue component.

---

## Cross-Cutting Finding 1: F5 depends on F7

Verified rather than inferred:

- `git merge-base --is-ancestor 054eb6d8a5 b22123ef86` → true. F7 precedes F5 upstream.
- F7 adds `services/actions/rerun.go` (+141). F5 then modifies that same file (+87/-x) and `routers/api/v1/repo/action.go`, which F7 also reworks (+178).

F5's six measured conflicts are against a main that does not yet have F7, and four of them (`routers/api/v1/api.go`, `routers/api/v1/repo/action.go`, `services/actions/rerun.go`, `routers/web/repo/actions/view.go`) sit precisely in F7's surface. **The 6-conflict figure overstates F5's real difficulty and must be re-measured after F7 lands.** Picking F5 first would mean hand-reconstructing the API layer F7 provides.

Encoded as a Gitea dependency so PageRank orders them correctly.

## Cross-Cutting Finding 2: migration version collision

This fork's migration registry ends at:

```
newMigration(327, "Add is_agent and agent identity tables", v1_26.AddAgentIdentity),
newMigration(328, "Add full-text indexes for the unified repository event stream", v1_26.AddRepoEventFullTextIndexes),
```

Both are load-bearing ADF features -- 327 creates the `AgentKey` table binding Nostr public keys to agent users, 328 backs the unified repository event stream.

Upstream **F6 adds `models/migrations/v1_26/v327.go`** and **F4 adds `v328.go`**. These collide on both filename and migration number. Of the eight features only F6 and F4 carry migrations; F1 and F2 conflict in `migrations.go` merely because the registry's trailing lines shifted.

**Rule for any future pick carrying a migration:** renumber to the next free slot (currently **329**) and never reuse a number that has already run in production. Fork migrations 327 and 328 have been applied to the live database at git.terraphim.cloud, so reusing either would silently skip the upstream migration on existing installs while running it on fresh ones -- a divergence that would not surface until something read the missing column.

---

## Accepted: research notes

### F7 -- Actions API rerun endpoints (`054eb6d8a5`, upstream #36768)

**Verdict: PICK, first of the three.**

Adds `POST` rerun endpoints for whole runs and individual jobs to the Actions REST API, factoring the rerun logic out of `routers/web/repo/actions/view.go` (-136) into `services/actions/rerun.go` (+141) so both web and API share it.

Highest value of the eight for this fork. An autonomous agent fleet whose jobs fail transiently needs to retrigger work *programmatically*; today that requires a human clicking in the web UI. This is the one feature in the set that converts a manual operation into an automatable one, which is the fork's whole reason for existing.

Merge-tree reports **no conflicts** across 9 files. Per the methodological caveat this means "no textual conflict", not "compiles" -- it touches `routers/api/v1/api.go`, which also carries the fork's robot route group, so the route-registration region deserves a look during the pick even though it merged. `models/repo/repo.go` (+49/-x) and `repo_unit.go` are also touched, which is broader than the feature's name suggests.

Verification on pick: `go build ./...`, `go vet` on `./routers/api/v1/... ./services/actions/...`, and the integration test the commit brings (`tests/integration/api_actions_run_test.go`, +123) via `make 'test-sqlite#<TestName>'`.

### F5 -- UI button to re-run failed jobs (`b22123ef86`, upstream #36924)

**Verdict: PICK, strictly second, after F7 has merged.**

Adds a "re-run failed jobs" control to the Actions run view, plus the supporting service and API changes on top of F7's rerun surface.

Moderate value, and honestly it is convenience rather than capability -- F7 already gives the fleet programmatic rerun. It earns its place because it is small once F7 lands, it closes the loop for humans supervising the fleet, and leaving the UI inconsistent with the API invites confusion later.

**Do not measure this against current main.** See cross-cutting finding 1: the six conflicts are inflated by F7's absence. Re-run the merge-tree measurement once F7 is on main and expect it to shrink substantially. If it does not, that is a signal to stop and re-scope rather than push through.

`templates/swagger/v1_json.tmpl` is touched by both F5 and F7 (+49 and +111 respectively); swagger regeneration should be checked rather than hand-merged.

### F6 -- Per-runner Disable/Pause (`b3b2d111da`, upstream #36776)

**Verdict: PICK, third, conditional on migration renumbering.**

Adds an `is_disabled` flag to runners with UI and API to pause a runner without deleting it, and makes the task-assignment path skip disabled runners.

Real operational value for a fleet running multiple runners: a misbehaving or noisy runner can be parked without tearing down its registration and re-enrolling it. That is a genuine incident-response capability, not cosmetics.

**Prerequisite, and it is mechanical but must not be skipped:** upstream's migration lands at `models/migrations/v1_26/v327.go`, which this fork already occupies with `AddAgentIdentity`. Renumber upstream's to **329**. The content is standalone and does not assume upstream's migration history -- it is a single column add:

```go
func AddDisabledToActionRunner(x *xorm.Engine) error {
	type ActionRunner struct {
		IsDisabled bool `xorm:"is_disabled NOT NULL DEFAULT false"`
	}
	_, err := x.SyncWithOptions(xorm.SyncOptions{IgnoreDropIndices: true}, new(ActionRunner))
	return err
}
```

so the renumber is safe. Four conflicts, the lowest of the conflicting set, three of them in the migration registry and its test. Sequence this last of the three picks so the renumber happens against a known state.

---

## F4 -- Configurable Actions automatic-token permissions: robot-token audit

**Verdict: DEFER -- needs Alex's decision. The audit resolves the mechanism precisely but not the exposure.**

Issue #50 requires an explicit audit of how this fork's robot tokens are issued and how F4 would change that surface. Here it is.

### How the fork's robot endpoints authenticate today

The robot API group in `routers/api/v1/api.go` (line ~2064) is registered as:

```go
m.Group("/robot", func() {
	m.Get("/triage", robot.Triage)
	m.Get("/ready", robot.Ready)
	m.Get("/graph", robot.Graph)
	m.Post("/room/hook", robot.RoomHook)
}, tokenRequiresScopes(auth_model.AccessTokenScopeCategoryIssue))
```

These authenticate with **standard Gitea access tokens carrying an issue scope** -- personal access tokens or OAuth tokens -- not with Actions automatic tokens. `/room/hook` is authenticated by an HMAC signature on the delivery instead, per `docs/ROBOT_SECURITY.md`. Permission resolution for these requests goes through `GetUserRepoPermission`, the individual-identity path.

**F4 does not touch that path.** The robot API surface as defined in this repository is unaffected.

### What the Actions automatic token can do today

`GetActionsUserRepoPermission` in `models/perm/access/repo_permission.go` currently resolves a task token to a single blanket access mode applied to every unit:

```go
} else if task.IsForkPullRequest {
	accessMode = perm_model.AccessModeRead
} else {
	accessMode = perm_model.AccessModeWrite
}
...
perm.SetUnitsWithDefaultAccessMode(repo.Units, accessMode)
```

So a workflow running against its own repository, not from a fork PR, receives **write to every unit** -- code, issues, packages, releases, wiki, the lot. There is no per-scope granularity.

### What F4 changes

F4 replaces that blanket grant with a configurable per-unit model: a `permissions:` block parsed from workflow YAML (`services/actions/permission_parser.go`, +141), repo and org level defaults (`models/repo/repo_unit_actions.go` +153, `routers/web/shared/actions/general.go` +160, plus settings UI), and a rewritten resolution in `repo_permission.go` (+115) that computes `effectivePerms.UnitAccessModes` and applies a `botPerm` ceiling.

**This is a security improvement, not merely a risk.** For an autonomous fleet, narrowing the automatic token from write-everything to declared-scopes-only is exactly the direction of travel you want; today a compromised or buggy workflow step can write issues and packages in a repository it was only meant to build.

### The subtle part, and why merge-tree is misleading here

F4 modifies `models/perm/access/repo_permission.go`, and that file **does not appear in F4's conflict list** -- despite this fork having added `GetDoerRepoPermission` to it in the #48 pick, merged one day ago. The merge auto-resolved in exactly the file where trouble was most likely.

Checked directly: **F4 does not change `GetActionsUserRepoPermission`'s signature.** It remains `(ctx, repo, actionsUser, taskID) (Permission, error)`, so the fork's `GetDoerRepoPermission` dispatcher keeps compiling and keeps calling it.

But F4 **rewrites that function's body substantially.** This is worse than a compile break, because nothing fails loudly: the fork's dispatcher is the LFS-token and wiki-write path from #48, and under F4 an Actions task reaching those paths would be resolved against declared per-unit permissions instead of blanket write. That may well be the correct outcome -- it is the point of the feature -- but it is a silent authorisation-behaviour change on a security path that landed yesterday, and it must be a deliberate decision rather than a side effect of a feature pick.

### The question only Alex can answer

This checkout cannot settle the exposure. There is **no `.gitea/workflows/` directory in this repository**; the `.github/workflows/` files are GitHub-side CI using `GITHUB_TOKEN` on github.com, which is unrelated to Gitea Actions on the instance. Whatever the ADF fleet runs lives in other repositories on git.terraphim.cloud.

> **Do any repositories on git.terraphim.cloud define `.gitea/workflows/` that use the Actions automatic token, and do any of them rely on write access to units beyond code -- issues, packages, releases, or wiki?**

- **If no** -- F4 is close to free, a pure hardening win, and should be promoted to a pick.
- **If yes** -- every such workflow needs an explicit `permissions:` block before F4 lands, or it will lose access it currently has implicitly. That is a coordinated change across repositories, not a cherry-pick.

Either way F4 is the largest item in the set at 57 files and 2,194 insertions, carries a colliding migration (v328, renumber to 329 or later), and warrants its own issue and its own verification pass rather than being folded in with the smaller Actions picks.

---

## Deferred

### F1 -- Actions post-run cleanup on cancel (`e7af84df72`, upstream #37275)

**Verdict: DEFER, but first in the queue to revisit.**

Runs post-run cleanup steps when a workflow is cancelled rather than leaving them unexecuted. The operational value is real for a fleet that cancels jobs: without it, cancelled runs can leave stale task state and skipped cleanup behind.

Deferred on cost, not merit. #43 rated it "Low; small touch"; it is 31 files and 786 insertions with 12 conflicts reaching into `models/actions/runner.go`, `services/actions/clear_tasks.go` and `services/actions/commit_status.go` -- core Actions internals, and the same task lifecycle F6 touches. It needs a scoped effort with real Actions integration testing, not a quick pick alongside others. Revisit once F7/F5/F6 have landed and the Actions surface has settled.

### F8 -- Workflow dependency visualisation (`a8505269ca`, upstream #36248)

**Verdict: DEFER.**

Merge-tree clean, so it is cheap to take whenever wanted -- which is precisely why it can wait. It is 1,064 insertions dominated by a 971-line `WorkflowGraph.vue`, delivering a visual `needs:` graph in the run view. No functional capability, nothing an agent can call, and the fork already has its own graph tooling in the PageRank/issue-graph features. Take it if a human supervising the fleet asks for it; do not spend a pick on it now.

## Rejected

### F2 -- Branch protection bypass allowlist (`eb93981d45`, upstream #36514)

**Verdict: REJECT.**

Lets named users or teams bypass branch protection rules. This is a governance feature for organisations with many contributors and a need for controlled exceptions. This fork is a single-org agent fleet; the useful direction for it is fewer bypass paths around protection, not a configurable list of them. It also adds a security-sensitive surface (`services/pull/check.go`, `routers/web/repo/setting/protected_branch.go`) that would need review effort disproportionate to any benefit here, for 10 conflicts across 23 files.

### F3 -- Default PR branch update style setting (`34fd3c9f06`, upstream #37410)

**Verdict: REJECT.**

Adds a repo setting for whether "update branch" defaults to merge or rebase. A preference toggle. Eleven conflicts across 20 files, concentrated in `services/pull/update.go`, `routers/web/repo/pull.go` and `routers/web/repo/issue_view.go` -- files with genuine merge cost -- in exchange for a default that can already be chosen per action at the point of use.

### The cost of rejecting

#43 states the goal as keeping the fork "on top of mainstream even if it means rework and retesting", and rejection sits in tension with that. Rejecting F2 and F3 means accepting permanent divergence in `services/pull/`, `routers/web/repo/pull.go` and `routers/web/repo/issue_view.go`, which raises the conflict cost of every future pick touching those files. That is a real, compounding price and it is being paid knowingly. If the fork's policy is strict upstream parity rather than selective adoption, these two should be reconsidered -- the argument against them is value-for-effort, not correctness.

---

## Sequencing

1. **F7** -- clean, highest value, unblocks F5.
2. **F5** -- only after F7 is on main; re-measure conflicts first.
3. **F6** -- last of the three, renumber migration to 329.
4. **F4** -- separate effort, gated on Alex's answer above.
5. **F1** -- revisit after the Actions surface settles.
6. **F8** -- on request only.

## Verification standard for each pick

Carried over from the security chain, where it caught divergence the merge did not:

- `go build ./...` and `go vet` with `-tags 'sqlite sqlite_unlock_notify'` on the touched packages. Compilation is the check that matters; three of the five security picks needed post-merge adaptation for API divergence the merge never saw.
- Integration tests through `make 'test-sqlite#<TestName>'`, and `./integrations.sqlite.test -test.run <Name> -test.v` when sub-cases need proving -- the make wrapper hides per-sub-test results.
- For F6, confirm the renumbered migration runs on a fresh database and is a no-op on one that already has 327/328.
