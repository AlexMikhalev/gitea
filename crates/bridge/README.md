# gitea-automations — the F4 bridge daemon

Issue #57, epic #53, phase F4.

Gitea holds the shared board (PageRank triage, dependency graph, human review). Hermes
kanban holds the execution fabric (atomic claims, heartbeats, crash reclaim, circuit
breaker, isolated workspaces, `task_events` audit). Nothing connected them: ready issues
never became runnable tasks, and finished or blocked tasks never came back as PRs, comments
or labels.

This is that bridge — **not** a second rules engine. The fabric is inherited, not rebuilt.

## Shape

| Leg | Source | Sink |
| --- | --- | --- |
| inbound (`src/inbound.rs`) | `GET /api/v1/robot/ready` | `hermes kanban create --idempotency-key gitea:<owner>/<repo>#<index> --initial-status blocked` |
| outbound (`src/outbound.rs`) | `hermes kanban watch --kinds completed,blocked,gave_up,crashed,timed_out` | `gitea-robot create-pull` / `edit-issue --add-labels` / `comment` |
| approval (`src/approval.rs`) | `GET /repos/{o}/{r}/issues/{index}/reactions` | `hermes kanban promote` / `unblock` |
| reconcile (`main.rs`) | `hermes kanban list --status <each of the nine>` | the outbound sinks, replayed |

The fourth leg is not decoration. `watch` is a live stream and a terminal kanban event fires
exactly once, so a single failed `gitea-robot` call — or a daemon that was simply down when
the event fired — would otherwise drop that issue's feedback permanently: no pull request,
no comment, no label, one `error!` line and nothing to re-drive it. The sweep looks for
terminal tasks carrying no report marker and replays their plan. It keys on the task's
*event trail*, not its status: a task created blocked to await a 🐝 is `blocked` and has
never run, and labelling its issue would be a lie about work that has not started.

It enumerates *every* documented status rather than the two a terminal event was observed to
leave behind (`done` after `complete`, `blocked` after `block`). Nothing pins where a
`crashed`, `gave_up` or `timed_out` task lands, and kanban's crash-reclaim exists to return a
dead worker's task to a claimable status — so a `done`/`blocked` scan covered two of the five
terminal kinds and silently dropped the other three, which are precisely the kinds that
coincide with the infrastructure trouble this leg exists for. Enumerating widely is safe
because the discrimination is `latest_terminal_kind`, not the status, and it is cheap because
a task already settled in the status it was listed under is skipped without paying for a
`kanban show` subprocess.

**`completed` assumes the head branch was pushed.** The bridge derives or reads
`branch_name` and hands it to `create-pull`; nothing in `src/` pushes anything. kanban
worktrees are local, so if the agent did not push its branch to the Gitea remote,
`create-pull` fails — loudly, and the reconcile sweep will keep retrying it, but no pull
request appears until the branch exists on the server. Pushing is the worker's job, not this
daemon's. After three failed attempts the bridge stops keeping that to itself: it labels the
issue `status/blocked` and comments naming the missing head branch, so the failure appears
where the humans are looking instead of only in the daemon's log, where it is
indistinguishable from an issue nobody picked up. The retry continues — push the branch and
the pull request opens — and the escalation comment is posted exactly once, guarded by its
own durable marker.

`src/rules.rs` parses the declarative rules file; `src/config.rs` is the daemon config;
`src/gitea.rs` is the read client and `src/robot.rs` the write side. The three write verbs
themselves live in `cmd/gitea-robot/write.go`, so that a write carries the agent's NIP-98
identity rather than a bearer token held by this daemon.

## Running

```sh
cargo build --manifest-path crates/Cargo.toml --release
gitea-automations --config bridge.yaml check          # validate config (+ rules file)
gitea-automations --config bridge.yaml check-rules rules.yaml
gitea-automations --config bridge.yaml poll-once      # one inbound sweep; safe to repeat
gitea-automations --config bridge.yaml approval-once  # one 🐝 sweep
gitea-automations --config bridge.yaml reconcile-once # replay unreported terminal tasks
gitea-automations --config bridge.yaml run            # all four legs
```

See `bridge.example.yaml` and `rules.example.yaml`.

## Four things that bite

**🐝 needs an `app.ini` change.** Reactions are rejected on write unless the type is in
`setting.UI.ReactionsLookup` (`models/issues/reaction.go:223`), and reads filter on the same
allowlist (`:165`). An unconfigured 🐝 is therefore *invisible*, not merely unwritable — the
API returns an empty list and the bridge cannot tell that from "nobody approved yet". The
shipped default omits it (`custom/conf/app.example.ini:1361`), so the operator must set:

```ini
[ui]
REACTIONS = +1, -1, laugh, hooray, confused, heart, rocket, eyes, honeybee
```

`honeybee` is the alias for U+1F41D (`modules/emoji/emoji_data.go:191`) and is also the
`content` value the reactions API returns. Configuring the raw codepoint would never match.

**Approval needs an *admin* token, not just a token.**
`GET /repos/{o}/{r}/collaborators/{u}/permission` sits behind `reqToken()`
(`routers/api/v1/api.go:1466`), and it answers for a user other than the caller only when
the caller is a **site admin or an admin of that repository**
(`routers/api/v1/repo/collaborators.go:279`; anyone else gets 403). Since the bridge is
always asking about somebody else — whoever left the 🐝 — a plain write-scoped
`gitea.token` makes every approval resolve to `Undetermined`. It fails closed and logs
rather than guessing, but the leg is then permanently dead.

`check` probes this rather than merely noticing whether a token is set: it resolves
`GET /api/v1/user` and, for a non-site-admin, its own permission on each configured repo —
the one query `collaborators.go:279` lets a non-admin make. `run` logs the same verdict at
startup.

```console
$ gitea-automations --config bridge.yaml check
approval OK: gitea.token user "bridge-bot" is an admin of every configured repository
approval gate ON: tasks are created with --initial-status blocked, so a "honeybee" reaction
from a writer is what releases each one to a worker
```

**A 🐝 is spent when it is used.** `hermes kanban create` has no `--status` flag and defaults
to **`ready`**, which is immediately claimable — so `kanban.require_approval` (on by default)
is the only thing standing between a ready issue and an agent run. It passes
`--initial-status blocked`, and the approval leg is what returns the task to `ready`.

Because a reaction is durable on the issue and Gitea records nothing about it having been
acted on, the bridge records that itself: promoting a task writes a
`gitea-bridge: approval-consumed <user>@<created_at>` comment on the kanban task. Without it,
a task approved once — which later blocks with `needs_input` or `transient` — is unblocked
again by the *same* 🐝 on every 60s sweep: it runs, blocks, and repeats indefinitely,
invisibly, because `BLOCK_MARKER` correctly suppresses the repeat Gitea comment. To release
such a task again, remove and re-add the 🐝 (which changes its `created_at`, and so is a new
approval), or have a second maintainer add theirs.

**A base branch may not contain a slash.** The existence probe is
`GET /repos/{o}/{r}/pulls/{base}/{head}` and only `{head}` is a catch-all segment
(`routers/api/v1/api.go:1642`). A `robot.base_branch` of `release/1.0` would be read by the
server as `base=release`, `head=1.0/task/…`, so the probe would 404 for a branch that is not
the one asked about, the POST would follow, and Gitea's own duplicate check would reject it —
wedging the plan at its first action. `check` and `create-pull` both refuse one outright.

## Why polling, and why it stays

Reactions are not observable through the F2 unified event stream. `repoevent.Kind` is
exactly `action, agent_audit, comment, review, status`
(`services/repoevent/event.go:29-37`) — there is no reaction kind, and F2 already landed
(#55). Reaction polling is the permanent path.

## The rules file is validated, not executed

`src/rules.rs`, `rules.example.yaml` and `check-rules` are **scaffolding**. Nothing
consults a rule at runtime: `RuleSet::load` is called twice, by `check-rules` and by config
validation, and both parse and discard. In particular `poll-ready-issues / every_secs: 60`
in the example does *not* drive the poll cadence — `poll.ready_interval_secs` does — and
editing a rule changes nothing about a running daemon. `check-rules` says so in its own
output. Treat the file as a validated declaration of intent for a future release.

## Safety properties

* **The action space is closed.** `create_task, comment, label, open_pr, unblock`. This is
  a property of the outbound mapping, which is where the daemon actually chooses what to
  do: `PlannedAction::action()` is total onto that enum, so outbound cannot express
  anything outside it. In the rules file the same list is enforced at parse time — an
  unknown action name is a named parse error, never a fallthrough — but see the section
  above: that file is not consulted at runtime.
* **No shell is ever spawned.** Every external call is an argv vector handed to
  `Command::new`. `sh -c` appears nowhere, so no config value can become shell syntax.
* **Restarts duplicate nothing.** Inbound dedup is the idempotency key, which is a pure
  function of `owner/repo#index`. Outbound dedup is a marker comment on the kanban task.
  Neither lives in this process.
* **A half-applied plan is safe to replay, and something replays it.** The marker is written
  only once every action in a plan landed, so a failure mid-plan leaves the task unmarked —
  and the reconcile sweep is what comes back for it. (The next terminal event would not: a
  kanban terminal event fires exactly once.) `create-pull` probes
  `GET /pulls/{base}/{head}` and reports an existing **open, unmerged** pull request as
  success, and adding a label an issue already carries is a server-side no-op
  (`models/issues/issue_label.go:125-130`) — without the probe, a `completed` plan whose
  audit comment failed would wedge forever on Gitea's refusal to open a second pull
  request for the same base and head. The open-and-unmerged qualifier matters:
  `GetPullRequestByBaseHeadInfo` (`models/issues/pull.go:596-601`) has no state predicate,
  so a merged pull request on the same deterministic head branch would otherwise be reported
  as "already exists" forever, and nothing would ever be opened.
* **A spent approval cannot be spent twice.** Promoting a task records the reaction it acted
  on, so the same durable 🐝 cannot unblock the same task on every sweep.
* **A marker that will not land is a dead letter, not a loop.** Every marker is written after
  the move it records — a marker without a move would strand the task — which is only safe
  while the marker eventually lands. So the write is retried (three attempts, exponential
  backoff, a local subprocess), and a failure that survives the retries suppresses that
  fingerprint or that task for the lifetime of the process, at `error!`. Without it an
  unwritten `approval-consumed` marker releases the same task every 60s forever, and an
  unwritten report marker reposts the same comment on a user-visible Gitea issue every 300s
  forever. The suppression is in-process only: kanban stays the durable side, and a restart
  costs at most one extra release or one duplicate comment — the bound the marker already
  gives — rather than one per sweep.
* **Crashes orphan nothing.** Claims, heartbeats, reclaim and the circuit breaker are
  kanban's. Killing the bridge stops the bridge.

## Multi-box

Option (a): one bridge per host, with **disjoint** `kanban.assignee` values. Two bridges
sharing an assignee would both act on the same terminal events. Cross-host kanban
(option (b)) is deferred.

## Tests

```sh
make lint-rust                        # cargo fmt --check + cargo clippy -D warnings
make test-rust                        # cargo test --all
go test ./cmd/gitea-robot/            # the other side of the write-leg contract

# opt-in: runs a real hermes kanban on a throwaway board it creates and deletes
cargo test --manifest-path crates/Cargo.toml --test live_kanban -- --ignored --test-threads=1

# opt-in: runs a real gitea-robot binary and checks it accepts the argv the bridge emits
go build -o /tmp/gitea-robot ./cmd/gitea-robot
GITEA_ROBOT_BIN=/tmp/gitea-robot cargo test --manifest-path crates/Cargo.toml \
    --test robot_cli_contract -- --ignored
```

`tests/wiremock_gitea.rs` covers the read leg (ready shape, empty board, 404 flag-off,
5xx-then-retry, reaction paging, the approved / unapproved / non-collaborator / unknown-user
/ 403 / no-token reaction cases, and the token preflight). `tests/inbound_dedup.rs` drives
two full poll cycles against wiremock plus a stub kanban and asserts one task results.
`tests/live_kanban.rs` is the opt-in live half.

Both Rust gates run in CI: `.github/workflows/pull-compliance.yml` has a `rust` job, fired by
the `crates/**` filter in `files-changed.yml`.

The Gitea *write* leg is checked in two places, because the argv tests in `src/robot.rs` can
only assert the bridge agrees with itself. The two halves are **not** symmetric:
`TestBridgeWriteVerbsExist` in `cmd/gitea-robot/write_test.go` asserts every verb and flag
the bridge emits exists in the CLI, and runs on every Go CI build; `tests/robot_cli_contract.rs`
runs a real `gitea-robot` binary but is `--ignored`, so it runs only when someone asks for it.
What is still **not** covered anywhere is the HTTP call itself — 🐝 on a real issue driving a
real pull request end to end needs a reachable instance and a NIP-98 agent key.
