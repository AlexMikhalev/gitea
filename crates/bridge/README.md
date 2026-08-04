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
| inbound (`src/inbound.rs`) | `GET /api/v1/robot/ready` | the approval gate (`state_file`), or `hermes kanban create --idempotency-key gitea:<owner>/<repo>#<index>` with the gate off |
| outbound (`src/outbound.rs`) | `hermes kanban watch --kinds completed,blocked,gave_up,crashed,timed_out` | `gitea-robot create-pull` / `edit-issue --add-labels` / `comment` |
| approval (`src/approval.rs`) | `GET /repos/{o}/{r}/issues/{index}/reactions` | `hermes kanban create` for a held issue; `promote` / `unblock` for a task kanban stopped |
| reconcile (`main.rs`) | `hermes kanban list --status <each of the nine>` | the outbound sinks, replayed |

The fourth leg is not decoration. `watch` is a live stream and a terminal kanban event fires
exactly once, so a single failed `gitea-robot` call — or a daemon that was simply down when
the event fired — would otherwise drop that issue's feedback permanently: no pull request,
no comment, no label, one `error!` line and nothing to re-drive it. The sweep looks for
terminal tasks carrying no report marker and replays their plan. It keys on the task's
*event trail*, not its status: a task can hold a status it never ran into — `blocked` with
only a `created` event on the trail — and labelling its issue would be a lie about work that
has not started.

It enumerates *every* documented status rather than the two a terminal event was observed to
leave behind (`done` after `complete`, `blocked` after `block`). Nothing pins where a
`crashed`, `gave_up` or `timed_out` task lands, and kanban's crash-reclaim exists to return a
dead worker's task to a claimable status — so a `done`/`blocked` scan covered two of the five
terminal kinds and silently dropped the other three, which are precisely the kinds that
coincide with the infrastructure trouble this leg exists for. Enumerating widely is safe
because the discrimination is `latest_terminal_kind`, not the status, and it is cheap because
a task whose report is *confirmed landed* in the status it was listed under is skipped without
paying for a `kanban show` subprocess — which is what keeps the sweep's cost off the board's
history, since `done` accumulates forever and every one of those tasks carries a durable
marker that cannot un-write itself.

A task with *nothing yet to report* is deliberately **not** remembered, and the asymmetry is
the point: it can acquire something to report and come back to the same status. Every task
passes through an idle status on its way in, so the ordinary path — idle, claimed, run, blocked
by the worker — can land back where it started, and a status-keyed "nothing to do" would blind
the sweep to it for the lifetime of the process. Same shape for a task idle in `ready` that is
claimed, crashes, and is returned to `ready` by crash-reclaim. The cost of not caching it is one
`kanban show` per idle bridge task per sweep, bounded by work in flight rather than by history.

**The inbound leg is capped, and that cap bounds the whole daemon.**
`GET /api/v1/robot/ready` returns *every* unblocked open issue — no limit, no paging
(`routers/api/v1/robot/ready_graph.go:192-276`). Uncapped, a repository with 300 open issues
means 300 entries at the approval gate per sweep — or, with the gate off, 300 `hermes kanban
create` subprocesses — and then a *permanent* cost behind it: 300 issues or tasks polled for a
🐝 every `approval_interval_secs` and one `kanban show` per idle task every
`reconcile_interval_secs`, forever, because an issue the bridge is holding (or whose task is
merely blocked) still counts as "ready" as far as `getInProgressIssues`
(`ready_graph.go:279-301`) is concerned, so nothing drains the set. With `require_approval` off
it is worse: it is unbounded *agent dispatch*.

`poll.max_tasks_per_sweep` (default 25) takes the top N by PageRank, ties broken by issue
index. The ordering is what makes it a bound on the *board* rather than on one sweep: the same
ready set selects the same issues every cycle, and re-creating them is deduplicated by
idempotency key, so steady state is at most N bridge-created tasks per repository. Work is not
lost — as those tasks finish, their issues stop being ready and the next best ones are admitted
— and what a sweep defers is counted and logged rather than silently truncated.

**`completed` assumes the head branch was pushed.** The bridge derives or reads
`branch_name` and hands it to `create-pull`; nothing in `src/` pushes anything. kanban
worktrees are local, so if the agent did not push its branch to the Gitea remote,
`create-pull` fails — loudly, and the reconcile sweep will keep retrying it, but no pull
request appears until the branch exists on the server. Pushing is the worker's job, not this
daemon's. After three failed attempts the bridge stops keeping that to itself: it comments on
the issue naming the missing head branch, so the failure appears where the humans are looking
instead of only in the daemon's log, where it is indistinguishable from an issue nobody picked
up. The retry continues — push the branch and the pull request opens — and the escalation
comment is posted exactly once, guarded by its own durable marker.

The same escalation covers *any* repeatedly failing action, not only the pull request, and it
is a comment and nothing else. Both parts are load-bearing: every non-`completed` plan leads
with a label, so gating escalation on `open_pr` left four of the five terminal kinds with no
escalation at all — and an escalation that itself led with a label would, in the case that
needs it most, be retrying the very call that is broken. A comment is the one verb with no
repository-side precondition. Each failing action escalates under its own marker, so a label
escalation cannot silence a later pull-request one. "The retry continues" is enforced, not just
promised: an escalation marker that will not write suppresses the *escalation* for this
process, never the task, because retiring the task would break the promise the comment it just
posted makes to whoever reads the issue.

**The watch leg and the sweep can reach the same task at once.** They are independent tokio
tasks over one shared state, and the window between planning a response and its marker landing
spans a `create-pull` subprocess, the comment, and the marker's retries. `create-pull` probes
for an existing pull request and adding a label twice is a no-op, but the reason comment is not
idempotent, so a plan is claimed per task for the length of its application and the second leg
steps aside rather than posting it again.

The claim alone covers them *overlapping*; it cannot cover them being sequential, because the
plan is computed from a `kanban show` read before the claim is taken. The other leg can finish
its whole apply, write the marker and drop its claim inside that gap, leaving a plan built from
a detail that is already stale. So the task is re-read under the claim and the marker that plan
would write is checked again — if it is already there, nothing is applied.

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
gitea-automations --config bridge.yaml approval-once  # one 🐝 sweep: creates the tasks the
                                                      # gate is holding for approved issues,
                                                      # and promotes the `todo` ones
gitea-automations --config bridge.yaml reconcile-once # replay unreported terminal tasks
gitea-automations --config bridge.yaml run            # all four legs
```

See `bridge.example.yaml` and `rules.example.yaml`.

**Neither one-shot can reach a `blocked` task**, and both say so before they run. No sweep
lists that status — on hermes v0.19.0 a `list` of it *promotes* what it returns — so blocked
tasks are reached by id, and the ids come from `kanban show` calls the process itself made.
A one-shot starts with none, so its `unlisted=0` means "this process knows of none", not
"there are none". Releasing or reconciling a blocked task is the running daemon's job (it
learned the id when it watched the task block); failing that, `hermes kanban unblock <id>` by
hand returns it to a status the sweeps do list — which is also what a restart costs.

## Eight things that bite

**The write leg needs a credential in the daemon's environment, and it decides its own
instance.** Every Gitea write is a `gitea-robot` subprocess, and neither of the two things it
needs is in the argv.

`main()` exits 1 with *"GITEA_TOKEN or GITEA_NOSTR_KEY environment variable required"* **before**
it reaches the dispatch table (`cmd/gitea-robot/main.go:165-168`). With neither set, every
terminal event fails at action 0, the task is left unmarked, the reconcile sweep replays it every
300s — and the escalation that exists to report a repeatedly failing action goes through the
*same* binary, so it fails too. Nothing reaches the issue, and the board is indistinguishable
from one nobody picked up. Set one in the unit file:

```ini
[Service]
Environment=GITEA_NOSTR_KEY=nsec1…   # the F1 agent identity: attributable, revocable
# or Environment=GITEA_TOKEN=…       # a bearer token, if no agent key is registered
```

The bridge forwards **only** the variable it resolved and removes the other from the child, so
the write leg's identity is the one `check` validated rather than whatever the box exported.

`GITEA_URL` is not left to the environment at all: the bridge sets it on every call, from
`robot.base_url` or, unset, from `gitea.base_url`. Inherited-and-unset, `gitea-robot` falls back
to its own `http://localhost:3000` (`cmd/gitea-robot/main.go:152-154`) — so on a box running a
dev Gitea, the daemon reads issues from the configured instance and comments, labels and opens
pull requests on the local one, with the agent's identity and no diagnostic anywhere, because the
request *succeeded*. Under `GITEA_NOSTR_KEY` the URL must also equal the instance's `ROOT_URL`
exactly (scheme, host, port, sub-path): the NIP-98 signature commits to the absolute URL, and a
mismatch is a deliberately opaque 401.

`check` probes all of it — by spawning `gitea-robot` once with a verb it deliberately does not
have, which reaches the credential check without making a request — and `run` logs the same
verdict at startup:

```console
$ gitea-automations --config bridge.yaml check
write leg OK: Writes go to https://git.example.org (GITEA_URL, set explicitly on every call),
signed with GITEA_NOSTR_KEY
```

```console
$ gitea-automations --config bridge.yaml check
warning: the write leg cannot write — gitea-robot refuses to start: no credential: none of
GITEA_NOSTR_KEY or GITEA_TOKEN is set in this process's environment — …
```

**`robot.blocked_label` must already exist in every repository.** Nothing here creates
repository labels — creating one is not in the action space — and a label name Gitea cannot
resolve is *dropped in silence* by `POST /issues/{index}/labels`: `GetLabelIDsInRepoByNames`
returns only what it found (`models/issues/label.go:333-341`) and the request still answers
200. `gitea-robot edit-issue` turns that into a hard error by re-reading the issue's label set,
which is right — but that error lands on action **0** of every `blocked`/`gave_up`/`crashed`/
`timed_out` plan, so the reason comment behind it is never posted either. Undetected, the issue
stays completely silent about work that ran and stopped, and the reconcile sweep replays the
same failing plan every 300s forever.

So `check` probes for it, `run` says so at startup, and after three failures the bridge
comments the failure onto the issue itself:

```console
$ gitea-automations --config bridge.yaml check
warning: robot.blocked_label "status/blocked" does not exist in terraphim/gitea — …
```

Create it once per repository (Issues → Labels, or `POST /repos/{o}/{r}/labels`). Mind the
case: the server resolves label names with a SQL `IN`, which is case-sensitive on sqlite and
Postgres, so `Status/Blocked` is not `status/blocked` — `check` calls that out separately.

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
approval gate ON (held by this bridge, not by kanban): a ready issue is recorded in
bridge-state.json and NO kanban task is created for it until a "honeybee" reaction from a
writer arrives; 3 issue(s) held right now
note: the gate cannot be a kanban status — on hermes v0.19.0 `hermes kanban list` promotes a
blocked task by reading it, …
```

**The approval gate is a file on this box, not a kanban status.** This is the part most worth
knowing, because it is not where you would look for it.

The obvious design — create the task with `--initial-status blocked` and let the 🐝 unblock it
— does not hold on the hermes this deploys against. On **v0.19.0**, `hermes kanban list`
*promotes a blocked task by reading it*: `show` reports `blocked`, one `list` runs, and the
next `show` reports `ready` with a `promoted` event appended to the trail. Both spellings do
it, plain and `--status blocked` (probed twice on a scratch board, 2026-08-04). Since the 🐝
sweep's very first call is a `kanban list`, the sweep looking for a task to release was itself
what released every gated task to a worker — no human involved, nothing in the log, and then an
empty list. The gate was not merely weak; the leg that implemented it was what defeated it.

Nothing in this crate can hold a task kanban will not hold. So the gate moved to the only side
that can: with `kanban.require_approval` on (the default), **inbound creates no kanban task at
all**. It records the ready issue in `state_file` (`bridge-state.json` by default), and the
approval leg runs `hermes kanban create` the moment an authorized 🐝 appears. An issue with no
task cannot be listed, promoted or claimed, so there is nothing for hermes to release.

Consequences worth stating:

* `state_file` is operational state. Back it up with the deployment, and give the daemon write
  access to it and its directory. It is rewritten (temp file + rename) on every hold and
  release, so a crash mid-write leaves the previous contents rather than a truncated file.
* Losing it is safe in both directions: nothing is released that was not approved, and the held
  set is rebuilt by the next inbound sweep, because an issue with no task is still ready. One
  ready interval of memory is the whole cost.
* `check` prints the path and how many issues are held; `run` logs the same at startup.
* A held issue is revalidated against Gitea on every 🐝 sweep, and one that is **closed or
  deleted** is dropped from the gate (`dropped=` in the sweep's log line). Nothing else would
  ever remove it: `/robot/ready` stops offering a closed issue, so it stops being re-held and
  stops being refreshed, and a 🐝 landing on it months later would otherwise dispatch an agent
  onto a wontfix with a title frozen at the last hold. The drop needs a *definite* answer — an
  unreachable Gitea, or a state the client cannot read, leaves the issue held and creates
  nothing.
* `hermes kanban create` is still `ready`-by-default and that is now correct — by the time the
  argv is built, a human has approved.

**A 🐝 is spent when it is used.** A reaction is durable on the issue and Gitea records nothing
about it having been acted on, so the bridge records that itself: releasing a task writes a
`gitea-bridge: approval-consumed <user>@<created_at>` comment on the kanban task. Without it, a
task approved once — which later blocks with `needs_input` or `transient` — is unblocked again
by the *same* 🐝 on every 60s sweep: it runs, blocks, and repeats indefinitely, invisibly,
because `BLOCK_MARKER` correctly suppresses the repeat Gitea comment.

And every authorized approval standing at that moment is marked, not just the one that did the
releasing. A 🐝 nobody has consumed is a *stored* release: two maintainers approving an issue
before it ran used to mean alex's released it and bo's sat pending, so when the worker later
blocked with `needs_input`, the next sweep spent bo's and re-dispatched the agent onto a
question nobody had answered. To release a task again, remove and re-add the 🐝 (which changes
its `created_at`, and so is a new approval), or have a maintainer who has not yet reacted add
theirs — a genuinely new fingerprint is never one of the marked ones.

Those markers live **on the task**, which is what makes archiving one a restart. The idempotency
key dedups against non-archived tasks only (`hermes kanban create`, probed live on v0.19.0 —
`an_archived_task_does_not_dedup_so_an_issue_can_be_restarted`), and the bridge's key never
rotates: it is a pure function of `owner/repo#index`. So an archived task frees its key, the next
sweep creates a fresh one for an issue that is still ready, and the 🐝 standing on that issue is
spent again on the new task. That is the point — a key that dedupped against archived tasks would
make the issue permanently un-restartable, with the gate resolving forever to a task nobody can
claim. Archiving means "run this one again"; un-approving means removing the 🐝, or closing the
issue, which drops it from the gate outright.

**No sweep ever lists `blocked`, and a restart forgets which tasks are.** This is the same
hermes bug as the gate above, reached by a different road, and it is worth stating on its own
because the blast radius is different: it undoes a human decision that was already made.

`hermes kanban list --status blocked` *promotes* what it returns. Both timed legs used to make
that call: the approval sweep every 60 seconds, the reconciliation sweep every 300. So a worker
that stopped a task with `--kind needs_input` — "waiting for spec", a question addressed to a
person — had it handed back to a worker within the minute, and the agent re-ran on the
unanswered question. No human, no log line, and no Gitea comment either, because `BLOCK_MARKER`
correctly suppresses the repeat. The gate redesign did not cover this: the gate is about tasks
that have not run, and these have.

Neither leg enumerates it now. `RECONCILE_STATUSES` is every documented status except that one,
and the approval sweep lists `todo` alone. Blocked tasks are reached **by id** through
`hermes kanban show`, which does not promote — so the 🐝 that unblocks a `needs_input` task
still works, and a block whose Gitea report never landed is still replayed.

The cost, stated plainly: the daemon knows a task is blocked because it *saw* it blocked — the
watch leg shows every task the instant it emits its terminal event — and that memory is
in-process.

* A **restart** forgets it, and nothing re-learns a task that is already blocked, because a
  blocked task emits no further event. Such a task needs `hermes kanban unblock <id>` by hand;
  a 🐝 on its issue will not move it.
* Everything else is unaffected: the issue already carries its `status/blocked` label and reason
  comment (that happened when the block fired), and nothing is released without an authorized
  approval. It is a liveness gap, not a safety one.
* `reconciliation sweep complete` logs `unlisted=` — how many blocked tasks this process is
  tracking. `unlisted=0` on a board that has blocked tasks means the daemon has been restarted
  since they blocked.

If a future hermes stops promoting on read, the honest fix is to put `blocked` back in both
lists and delete the by-id path. Until then, do not: `no_sweep_asks_kanban_for_the_status_that_listing_would_release`
fails if either list grows it back, and the live probe in `tests/live_kanban.rs` is the record
of why.

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
* **Every sweep is bounded.** The inbound leg admits at most `poll.max_tasks_per_sweep`
  issues per repository, deterministically ordered, which is the only bound on an endpoint
  that returns every open unblocked issue with no paging — and therefore on the per-task cost
  every other leg pays afterwards.
* **A mistyped config key is an error, not a default.** Every section rejects unknown fields,
  as the rules file already did. `kanban: {boad: f4}` would otherwise parse clean, leave
  `board` unset, drop `--board` from every argv and drive kanban's *default* board — creating,
  promoting and commenting on the wrong one, while `check` reported only `board None`.
* **Crashes orphan nothing.** Claims, heartbeats, reclaim and the circuit breaker are
  kanban's. Killing the bridge stops the bridge.
* **Nothing waits forever, and nothing dies quietly.** Every subprocess is run with a deadline
  and killed at it, and `gitea-robot` gives its HTTP client one too — without both, a half-open
  connection is not an error but a hang, and it happens *inside* the outbound leg's `watch`
  loop, which then stops reading events while still looking alive. The four legs are awaited
  alongside `ctrl-c` rather than left unjoined for the same reason: a leg that ends is a leg
  whose silence reads as an idle board, so the daemon exits non-zero and lets its supervisor
  rebuild it. The three tickers use `MissedTickBehavior::Delay`, so a sweep that overruns its
  interval does not come back to a burst of missed ticks stacking subprocesses onto a board
  that is already slow. `kanban watch` — the one long-lived subprocess — has its stderr piped
  and drained, and its exit code is read rather than discarded: a watcher that dies on startup
  (unsupported `--kinds`, missing board, version skew) writes nothing to stdout, so without
  those two the entire diagnosis was `watch exited; restarting` every five seconds while the
  outbound leg was dead.

## Multi-box

Option (a): one bridge per host, with **disjoint** `kanban.assignee` values. Two bridges
sharing an assignee would both act on the same terminal events. Cross-host kanban
(option (b)) is deferred.

## Tests

```sh
make lint-rust                        # cargo fmt --check + clippy -D warnings + rustdoc -D warnings
make test-rust                        # cargo test --all
make test-rust-robot-contract         # builds cmd/gitea-robot, runs the bridge's argv at it
go test ./cmd/gitea-robot/            # the other side of the write-leg contract

# opt-in: runs a real hermes kanban on a throwaway board it creates and deletes
cargo test --manifest-path crates/Cargo.toml --test live_kanban -- --ignored --test-threads=1
```

`tests/wiremock_gitea.rs` covers the read leg (ready shape, empty board, 404 flag-off,
5xx-then-retry, reaction paging, the approved / unapproved / non-collaborator / unknown-user
/ 403 / no-token reaction cases, and the token preflight). `tests/inbound_dedup.rs` drives
two full poll cycles against wiremock plus a stub kanban and asserts one task results — and,
for the cap, that a five-issue board produces exactly two `create` calls for the two highest
PageRanks and reports the other three as deferred. It also pins the approval gate, which is a
claim about a call that must *not* happen: with `require_approval` on, the stub kanban records
no invocation at all, and the held issue is in the state file with the `gitea-ref:` trailer a
later `create` needs. `tests/live_kanban.rs` is the opt-in live half, and is where the
promote-on-read behaviour that forced the gate out of kanban is documented against a real
hermes.

All three Rust gates run in CI: `.github/workflows/pull-compliance.yml` has a `rust` job, fired
by the `crates/**` and `cmd/gitea-robot/**` filters in `files-changed.yml` — or by the
`actions` filter, so that a PR which changes only the job's own wiring (the pinned toolchain,
the steps) still runs it.

The Gitea *write* leg is checked in two places, because the argv tests in `src/robot.rs` can
only assert the bridge agrees with itself. `TestBridgeWriteVerbsExist` in
`cmd/gitea-robot/write_test.go` asserts every verb and flag the bridge emits exists in the
CLI's dispatch table; `tests/robot_cli_contract.rs` runs a real `gitea-robot` binary, so it
also covers the case where the CLI builds but rejects the argv it is handed. Its tests are
`#[ignore]`d — they need that binary — which is why `make test-rust-robot-contract` builds it
and opts in, and why the `rust` job runs that target as well: unwired, the file's own claim to
run a real binary was never true in CI, and the `cmd/gitea-robot/**` filter is what makes a PR
touching only the Go side run it at all. What is still **not** covered anywhere is the HTTP
call itself — 🐝 on a real issue driving a real pull request end to end needs a reachable
instance and a NIP-98 agent key.

The write-leg *preflight* is checked from both sides for the same reason, because it classifies
the CLI's refusals by their stderr: `the_write_preflight_reads_the_binarys_own_answers` runs the
real binary with and without a credential, and `TestBridgePreflightProbeStaysDistinguishable`
(Go, always runs) pins the two messages and the fact that the probe's verb is *not* in the
dispatch table. Reword either message with only one side edited and the preflight downgrades a
missing credential from "will not write" to "cannot verify" — a check green enough to start a
daemon that reports nothing.
