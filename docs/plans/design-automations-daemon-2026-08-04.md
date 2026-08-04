# Design Gate — gitea-automations bridge daemon (issue #57, epic #53, phase F4)

## Problem
Gitea holds the shared board (PageRank triage, dependency graph, human review); Hermes kanban v0.12+ holds the execution fabric
(atomic claims, heartbeats, crash reclaim, circuit breaker, isolated workspaces, task_events audit). Nothing connects them: ready
issues never become runnable tasks, and finished/blocked tasks never return as PRs, comments or labels. F4 is that **bridge** —
not a second rules engine; the fabric is inherited, not rebuilt.

## Decision — exact touchpoints
Rust daemon in this fork at `crates/bridge` (workspace `crates/`), **plus** a fork-core Go change: the three write verbs
in `cmd/gitea-robot/write.go` did not exist and were added with it (see 2 below). Everything else on the Gitea side is
config + existing routes. The daemon was originally scoped as a separate repo, `terraphim/gitea-automations`; keeping it
in-tree is what makes the write-leg contract test (`TestBridgeWriteVerbsExist`) possible at all.
1. `crates/bridge/src/inbound.rs` — poll `GET /api/v1/robot/ready?owner=&repo=&skip_in_progress=1`
   (`routers/api/v1/robot/ready_graph.go:47`, gated by `[issue_graph] ENABLED`) → per `ready_issues[]` entry run `hermes kanban
   create <title> --assignee <p> --idempotency-key gitea:<owner>/<repo>#<index> --json`. Owner comes from daemon config.
2. `crates/bridge/src/outbound.rs` — `hermes kanban watch --kinds completed,blocked,gave_up,crashed,timed_out` is the event
   source, `hermes kanban show <id> --json` resolves detail. `completed` → `gitea-robot create-pull` with `Refs #N` +
   `gitea-robot comment` audit note; the other four → `edit-issue --add-labels status/blocked` + a reason comment.
   These three verbs did not exist on the CLI when this was written and were added with the bridge
   (`cmd/gitea-robot/write.go`); the flag is `--add-labels`, not `--labels`, because applying `status/blocked` must leave a
   human's labels in place.
3. `crates/bridge/src/rules.rs` — YAML rules: trigger (schedule | webhook | reaction) → **allowlisted action enum**
   (`create_task`, `comment`, `label`, `open_pr`, `unblock`). The enum has no shell variant.
4. `crates/bridge/src/approval.rs` — 🐝 = human approval, polled from `GET /repos/{o}/{r}/issues/{index}/reactions`
   (`routers/api/v1/api.go:1798` — `:1746` is the *comment* reactions combo, a different route); `content` is the emoji
   **alias**, not the codepoint. A reaction is durable and Gitea records nothing about it having been acted on, so the
   bridge writes a `gitea-bridge: approval-consumed <user>@<created_at>` marker on the kanban task; otherwise one 🐝
   unblocks the same task on every sweep, forever.
5. Auth: NIP-98 via the shipped `gitea-robot` (F1), shelled out. Multi-box: option (a) — one bridge/host, disjoint assignees.

## Ground truth — verified, not assumed
- `ReadyResponse` = `{repo_id, repo_name, total_count, ready_issues[]}`; `ReadyIssue` = `{id, index, title, page_rank, priority,
  is_blocked, blocker_count}` (`ready_graph.go:25-42`) — **no owner, body or labels**; `--extended` enriches client-side only.
- `hermes kanban create` does have `--idempotency-key` (returns the existing id, no duplicate), plus `--max-runtime`,
  `--max-retries`, `--workspace {scratch|worktree|worktree:<p>|dir:<p>}`, `--json`. Status enum (`list --status`): `triage, todo,
  ready, running, review, blocked, scheduled, done, archived`; `block --kind`: `capability, dependency, needs_input, transient`.
- **🐝 is not observable through the F2 event stream.** `repoevent.Kind` is exactly `action, agent_audit, comment, review,
  status` (`services/repoevent/event.go:29-37`) — no reaction kind, and F2 already landed (#55). Reaction polling is therefore
  the permanent path, not a stopgap.
- **🐝 requires an app.ini change.** Writes are rejected unless the type is in `setting.UI.ReactionsLookup`
  (`models/issues/reaction.go:223`) and reads filter on the same allowlist (`:165`), so an unconfigured 🐝 is invisible, not
  merely unwritable. Default omits it (`app.example.ini:1361`); set `[ui] REACTIONS = ..., honeybee` — the alias for U+1F41D
  (`modules/emoji/emoji_data.go:191`), which is also the `content` value returned.

## Acceptance criteria
1. Two poll cycles over one ready issue create exactly one kanban task, verified by returned task id equality.
2. `completed` opens one PR whose body contains `Refs #<index>` and posts one audit comment. **External dependency:** the
   head branch must already be on the Gitea remote. kanban worktrees are local and nothing in the daemon pushes, so if the
   worker did not push, `create-pull` fails; the reconcile sweep (6) keeps retrying, but no PR exists until the branch does.
3. `blocked`/`gave_up`/`crashed`/`timed_out` each apply `status/blocked` + a reason comment naming the block kind.
4. A rules YAML naming a non-allowlisted action fails to parse with a named error; no shell is ever spawned.
5. 🐝 from a user with write permission promotes the task; 🐝 from anyone else is ignored and logged. Reachable only because
   inbound passes `--initial-status blocked`: `hermes kanban create` has no `--status` flag and defaults to `ready`, which is
   immediately claimable, so without it every ready issue would reach an agent unreviewed and this branch would be dead code.
   An approval is consumed once — the same 🐝 does not release the same task twice.
6. Restart mid-flight duplicates nothing (dedup is the key, not in-memory state); a crash orphans nothing (kanban owns liveness).
   A terminal event fires exactly once on `watch`, so a failed `gitea-robot` call or a daemon that was down is recovered by a
   periodic reconcile sweep over terminal tasks lacking a report marker — keyed on the event trail, not on status.

## Non-goals
Rewriting kanban's claims/heartbeats/reclaim/workspaces/audit; shell actions; a web UI; cross-host kanban (option (b) deferred).

## Test plan
- Unit (`cargo test`): idempotency-key construction; rules YAML accept-allowlisted / reject-unknown-and-shell; event→action
  mapping for all five terminal kinds; reaction alias match (`honeybee`, not the raw codepoint).
- Wiremock: `/api/v1/robot/ready` in the verified shape incl. empty `ready_issues`; `/reactions` approved / unapproved /
  non-writer; 404 (flag off) and 5xx-then-retry.
- Live (opt-in, `--ignored`): real `hermes kanban init` on a temp `--board`; create→complete asserts PR body Refs, create→block
  asserts label. Dogfood: 🐝 on a terraphim/gitea issue, end to end.

## Gates
- Rust (`crates/`): `make lint-rust` (fmt --check + clippy -D warnings) and `make test-rust`. Both run in CI via the `rust` job
  in `.github/workflows/pull-compliance.yml`, fired by the `crates/**` filter in `files-changed.yml`.
- Go: `make fmt`, `make lint-go`, `make test-backend`. `make fmt` also rewrites unrelated quarto frontmatter — revert that hunk
  before committing.
