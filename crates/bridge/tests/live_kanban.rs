// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//! Live kanban tests — opt-in, `--ignored`.
//!
//! ```text
//! cargo test --manifest-path crates/Cargo.toml --test live_kanban -- --ignored --test-threads=1
//! ```
//!
//! These run a **real** `hermes kanban` against a throwaway `--board`, created and deleted
//! by the test. They exercise the half of the loop that has no credential requirement: the
//! kanban side, and the Gitea actions the bridge *plans* from real kanban state.
//!
//! The Gitea write leg (`gitea-robot create-pull` / `edit-issue` / `comment`) is not
//! exercised here. It needs a reachable Gitea instance and a NIP-98 agent key, and no such
//! credentials are documented as available for this work — see the report accompanying the
//! change. Everything up to the argv handed to `gitea-robot` is asserted; the HTTP call
//! itself is not.

use std::process::Command;

use bridge::config::KanbanConfig;
use bridge::hermes::{CreateTask, Kanban, RECONCILE_STATUSES};
use bridge::inbound::{GiteaRef, idempotency_key};
use bridge::outbound::{PlannedAction, TerminalKind, latest_terminal_kind, plan};
use bridge::robot::Robot;
use bridge::rules::Action;

/// The instance the `Robot` handles below claim to write to.
///
/// Only their argv is asserted — no request is made — so this exists to be named rather than
/// reached, and it is a closed port so that a test which started making one would fail.
const WRITE_URL: &str = "http://127.0.0.1:1";

/// A throwaway board, deleted on drop.
struct ScratchBoard {
    slug: String,
}

impl ScratchBoard {
    fn new(tag: &str) -> Option<Self> {
        if Command::new("hermes").arg("--version").output().is_err() {
            eprintln!("skipping: `hermes` is not on PATH");
            return None;
        }
        let slug = format!("f4-live-{tag}-{}", std::process::id());
        let init = Command::new("hermes")
            .args(["kanban", "init"])
            .output()
            .expect("hermes kanban init");
        assert!(
            init.status.success(),
            "kanban init failed: {}",
            String::from_utf8_lossy(&init.stderr)
        );
        let out = Command::new("hermes")
            .args(["kanban", "boards", "create", &slug])
            .output()
            .expect("boards create");
        assert!(
            out.status.success(),
            "boards create failed: {}",
            String::from_utf8_lossy(&out.stderr)
        );
        Some(Self { slug })
    }

    fn kanban(&self) -> Kanban {
        Kanban::new(&KanbanConfig {
            board: Some(self.slug.clone()),
            created_by: "gitea-automations-test".into(),
            ..KanbanConfig::default()
        })
    }

    fn run(&self, args: &[&str]) -> String {
        let mut argv = vec!["kanban", "--board", &self.slug];
        argv.extend_from_slice(args);
        let out = Command::new("hermes").args(&argv).output().expect("hermes");
        assert!(
            out.status.success(),
            "hermes {argv:?} failed: {}",
            String::from_utf8_lossy(&out.stderr)
        );
        String::from_utf8_lossy(&out.stdout).into_owned()
    }
}

impl Drop for ScratchBoard {
    fn drop(&mut self) {
        let _ = Command::new("hermes")
            .args(["kanban", "boards", "rm", &self.slug, "--delete"])
            .output();
    }
}

fn create_request(index: i64, title: &str) -> CreateTask {
    let gref = GiteaRef::new("terraphim", "gitea", index);
    CreateTask {
        title: title.to_string(),
        body: format!("live bridge test\n\n{}", gref.trailer()),
        idempotency_key: idempotency_key("terraphim", "gitea", index),
    }
}

/// The dedup the whole inbound leg rests on — and, in its second half, the fact the **approval
/// gate** rests on.
///
/// `gate_sweep` calls `create` for every approved held issue, and a re-hold is the steady state
/// rather than a corner: inbound holds every ready issue unconditionally and never asks kanban
/// whether a task exists, and an issue whose task has produced no branch and no pull request is
/// still ready. So the `create` in the gate routinely lands on a task that already exists — and
/// that task may be one a worker has since **blocked**, waiting on the very human whose 🐝 is
/// being processed.
///
/// Three things had to be true of that call for `gate_sweep`'s hand-off to `release_sweep` to
/// be the right shape, and only a live probe can say so — `hermes.rs` documents the
/// return-existing contract but only for a non-archived task in the abstract. **Observed on
/// hermes v0.19.0, probed twice on 2026-08-04**: a duplicate-key `create` against a blocked
/// task (1) returns the *same* task id, (2) does not error, and (3) does not promote it — the
/// task is still `blocked` afterwards.
///
/// `hermes kanban block` is what puts it there, deliberately: `create --initial-status blocked`
/// does **not** hold on v0.19.0 — the task auto-promotes — which is the whole reason the
/// approval gate is bridge-side rather than a kanban status.
#[tokio::test]
#[ignore = "runs a real hermes kanban on a throwaway board"]
async fn the_idempotency_key_dedups_against_real_kanban() {
    let Some(board) = ScratchBoard::new("dedup") else {
        return;
    };
    let kanban = board.kanban();
    let req = create_request(57, "live dedup probe");

    let first = kanban.create(&req).await.expect("first create");
    let second = kanban.create(&req).await.expect("second create");
    assert_eq!(
        first, second,
        "kanban must return the existing task id, not a duplicate"
    );

    // Before the block, because `list` promotes a blocked task by reading it.
    let listed = board.run(&["list", "--json"]);
    let tasks: serde_json::Value = serde_json::from_str(&listed).expect("list --json");
    assert_eq!(
        tasks.as_array().map(Vec::len),
        Some(1),
        "exactly one task exists: {listed}"
    );

    // …and now the case `gate_sweep` actually meets. Nothing below may run a `list`.
    board.run(&["block", "--kind", "needs_input", &first, "waiting for spec"]);
    assert_eq!(
        kanban.show(&first).await.expect("show").task.status,
        "blocked",
        "`kanban block` is what sticks; `create --initial-status blocked` does not on v0.19.0"
    );

    let third = kanban
        .create(&req)
        .await
        .expect("a duplicate-key create against a blocked task must not error");
    assert_eq!(
        third, first,
        "the same key must resolve to the same task even when kanban has blocked it, or \
         `gate_sweep` would create a second task for one issue behind a human's back"
    );
    assert_eq!(
        kanban.show(&first).await.expect("show").task.status,
        "blocked",
        "and `create` must not promote it: a 🐝 arriving while a worker waits on an answer \
         would otherwise dispatch the agent back onto the unanswered question"
    );
}

/// The **other** branch of the dedup contract, and the R9 P2: an *archived* task does not
/// dedup.
///
/// `CreateTask::idempotency_key` documents the contract as "if a **non-archived** task with
/// this key exists, its id is returned". The blocked half of that sentence is pinned above; the
/// archived half was asserted only by the doc comment, and the bridge's behaviour after an
/// operator archives a task follows entirely from which way it goes.
///
/// **Observed on hermes v0.19.0, 2026-08-04**: it creates a **new** task. That is the desired
/// behaviour and the reason this is a test rather than a fix. The bridge's key is a pure
/// function of `owner/repo#index` (`inbound::idempotency_key`) and never rotates, so a key that
/// dedupped against archived tasks would make an issue permanently un-restartable: the gate
/// would keep resolving to a task no worker can claim, and no `create` could ever replace it.
/// Archiving means "retire this one"; a still-ready, still-approved issue getting a fresh task
/// is what that has to mean.
///
/// What it costs is stated plainly rather than papered over: the new task's comment list is
/// empty, so the approvals standing on the issue are unconsumed against *it* and the gate
/// spends them again (`releasing_a_task_consumes_every_approval_standing_at_the_time` is the
/// per-task property, and per-task is the whole of it). One archive is one re-dispatch of an
/// issue a human already approved and nobody has since un-approved — which is what an operator
/// archiving a bridge task is asking for. Un-approving is removing the 🐝.
#[tokio::test]
#[ignore = "runs a real hermes kanban on a throwaway board"]
async fn an_archived_task_does_not_dedup_so_an_issue_can_be_restarted() {
    let Some(board) = ScratchBoard::new("archived") else {
        return;
    };
    let kanban = board.kanban();
    let req = create_request(64, "live archived dedup probe");

    let first = kanban.create(&req).await.expect("first create");
    assert_eq!(
        kanban.create(&req).await.expect("second create"),
        first,
        "the non-archived branch, restated here so a failure names which half moved"
    );

    board.run(&["archive", &first]);
    let second = kanban
        .create(&req)
        .await
        .expect("a duplicate-key create against an archived task must not error");
    assert_ne!(
        second, first,
        "an archived task must not keep its key: the bridge's key never rotates, so an issue \
         whose task was archived could otherwise never be restarted"
    );

    // The new task is a real, claimable one — not a resurrection of the archived id.
    let detail = kanban.show(&second).await.expect("show");
    assert_eq!(detail.task.status, "ready");
    assert!(
        detail.comments.is_empty(),
        "the restart starts with no consumed-approval markers, which is why the gate spends the \
         issue's standing 🐝 again: {:?}",
        detail.comments
    );
}

/// Pins the fact the whole approval gate now rests on: `hermes kanban create` yields a
/// **claimable** task, whatever the bridge's `require_approval` setting says.
///
/// That used to be the bug the `--initial-status blocked` flag existed to fix. It is now the
/// premise: the gate is bridge-side, so by the time this argv is built a human has already
/// approved, and `ready` is exactly right. What must hold is that `require_approval` no longer
/// changes what kanban is asked for — because if it did, an *approved* task would land in a
/// status a second human decision has to move it out of.
#[tokio::test]
#[ignore = "runs a real hermes kanban on a throwaway board"]
async fn create_lands_in_a_claimable_status_whatever_the_gate_setting_is() {
    let Some(board) = ScratchBoard::new("gate") else {
        return;
    };

    let gated = board.kanban();
    assert!(
        gated.require_approval(),
        "the shipped default must gate on a human"
    );
    let id = gated
        .create(&create_request(60, "live gate probe"))
        .await
        .expect("create");
    let detail = gated.show(&id).await.expect("show");
    assert_eq!(
        detail.task.status, "ready",
        "a task created after approval must be immediately claimable, not gated a second time"
    );

    let ungated = Kanban::new(&KanbanConfig {
        board: Some(board.slug.clone()),
        created_by: "gitea-automations-test".into(),
        require_approval: false,
        ..KanbanConfig::default()
    });
    let open_id = ungated
        .create(&create_request(61, "live ungated probe"))
        .await
        .expect("create");
    let detail = ungated.show(&open_id).await.expect("show");
    assert_eq!(
        detail.task.status, "ready",
        "kanban's own default, and the setting must not change it"
    );
}

/// The reconciliation sweep keys on the event trail, not the status — and this is why.
///
/// A freshly created task has never run, whatever status it holds. Keying on status would
/// label its Gitea issue `status/blocked` and comment that it was blocked, about work that has
/// not started — and `blocked` is a status a task can hold for reasons that are not a report.
#[tokio::test]
#[ignore = "runs a real hermes kanban on a throwaway board"]
async fn a_freshly_created_task_has_nothing_to_reconcile() {
    let Some(board) = ScratchBoard::new("gated") else {
        return;
    };
    let kanban = board.kanban();
    let id = kanban
        .create(&create_request(62, "live gated probe"))
        .await
        .expect("create");

    let detail = kanban.show(&id).await.expect("show");
    assert_eq!(detail.task.status, "ready");
    assert!(
        latest_terminal_kind(&detail).is_none(),
        "a task that never ran has no terminal event: {:?}",
        detail.events
    );

    // Once it really is blocked by a worker, there is something to report.
    board.run(&["block", "--kind", "transient", &id, "flaked"]);
    let detail = kanban.show(&id).await.expect("show");
    assert_eq!(detail.task.status, "blocked");
    assert_eq!(latest_terminal_kind(&detail), Some(TerminalKind::Blocked));
}

/// The reconcile sweep enumerates every documented status, so every one of them must be a
/// value `kanban list --status` actually accepts.
///
/// If one is not, the sweep lists nothing for it and the widening is decorative — which is
/// exactly the failure the widening was for: `crashed`, `gave_up` and `timed_out` land in a
/// status nothing in-repo pins, so the sweep has to be able to ask about all of them.
#[tokio::test]
#[ignore = "runs a real hermes kanban on a throwaway board"]
async fn every_reconcile_status_is_one_kanban_will_list() {
    let Some(board) = ScratchBoard::new("statuses") else {
        return;
    };
    let kanban = board.kanban();
    let id = kanban
        .create(&create_request(63, "live status probe"))
        .await
        .expect("create");

    for status in RECONCILE_STATUSES {
        if let Err(err) = kanban.list(Some(status)).await {
            panic!("kanban list --status {status} must be accepted: {err}");
        }
    }

    // …and the sweep really finds the task through one of them. Which one is deliberately not
    // asserted here — hermes moves a task between statuses on its own (see
    // `the_approval_gate_does_not_rely_on_kanban_holding_a_task`). The reconcile sweep
    // enumerates all nine and dedups by id, so being listed by *some* status is what it needs.
    let mut listed = None;
    for status in RECONCILE_STATUSES {
        let tasks = kanban.list(Some(status)).await.expect("list");
        if let Some(task) = tasks.into_iter().find(|t| t.id == id) {
            listed = Some(task);
            break;
        }
    }
    let listed = listed.expect("a bridge task must be listed by one of the enumerated statuses");

    // The id is not enough. Both list-driven legs — the approval sweep and the reconcile
    // sweep — open with `GiteaRef::parse_from_body(task.body)` and `continue` past every task
    // that has none, so a `list --json` that projects rows *without* the body would leave them
    // evaluating nothing, silently and with both preflights green: every inbound task would sit
    // blocked awaiting a 🐝 nobody looks for, and the reconcile summary would read idle.
    // `Task::body` is `#[serde(default)] Option<String>`, so nothing but this asserts it.
    // `Kanban::list` rejects a payload whose rows have no `body` key at all; this pins the
    // stronger fact — that the trailer the bridge wrote survives the round trip through `list`,
    // not merely through `show`.
    assert_eq!(
        listed.body.as_deref().and_then(GiteaRef::parse_from_body),
        Some(GiteaRef::new("terraphim", "gitea", 63)),
        "`kanban list --json` must return the body carrying the gitea-ref trailer, or the \
         approval and reconcile legs are both dead: {listed:?}"
    );
}

/// The approval gate does not depend on kanban holding a task still — and this is the probe
/// that says why it cannot.
///
/// **Observed on hermes v0.19.0** (probed twice on 2026-08-04, on a scratch board): `hermes
/// kanban list` *promotes* a blocked task by reading it. `show` reports `blocked` across
/// repeated calls; one `list` runs; the next `show` reports `ready` with a `promoted` event
/// appended to the trail. Both spellings do it — plain `list` and `list --status blocked`.
///
/// The old design put the gate exactly there: inbound created the task `--initial-status
/// blocked`, and the 🐝 sweep unblocked it. `approval_sweep`'s first act was
/// `kanban.list(Some("blocked"))`, so the sweep looking for a task to release was itself what
/// released every gated task to a worker — no 🐝, no log line — and then found an empty list.
///
/// So the gate moved out of kanban entirely: with `require_approval` on the bridge creates no
/// task at all until an authorized 🐝 arrives, and holds the issue in
/// `bridge::state::PendingApprovals` meanwhile
/// (`a_gated_sweep_creates_no_kanban_task_at_all`, which runs in CI, pins that half). What is
/// left to check against a real hermes is that the promote-on-read behaviour can no longer
/// touch the gate: whatever `list` does to a `blocked` task, the bridge is not relying on it.
///
/// This test therefore *documents* the behaviour rather than asserting a hold. It is not a
/// contradiction that it passes either way — that is the point of the redesign. It fails only
/// if `create` starts gating tasks again, which would put an approved task behind a second
/// human decision that nothing would ever make.
#[tokio::test]
#[ignore = "runs a real hermes kanban on a throwaway board"]
async fn the_approval_gate_does_not_rely_on_kanban_holding_a_task() {
    let Some(board) = ScratchBoard::new("gatehold") else {
        return;
    };
    let kanban = board.kanban();
    assert!(kanban.require_approval(), "the shipped default gates");
    let id = kanban
        .create(&create_request(63, "live gate hold probe"))
        .await
        .expect("create");
    assert_eq!(
        kanban.show(&id).await.expect("show").task.status,
        "ready",
        "an approved task is claimable; the gate was upstream of this call ever being made"
    );

    // The probe that killed the old design, kept because it is the only in-repo record of it
    // and because a future hermes fixing it must not silently move the gate back into kanban.
    board.run(&["block", "--kind", "needs_input", &id, "waiting for spec"]);
    assert_eq!(kanban.show(&id).await.expect("show").task.status, "blocked");
    let listed = kanban.list(Some("blocked")).await.expect("list blocked");
    let after = kanban.show(&id).await.expect("show").task.status;
    if after != "blocked" {
        eprintln!(
            "hermes still promotes on read: `kanban list --status blocked` moved {id} to \
             {after:?} (listed {} task(s)). This is why the approval gate is bridge-side.",
            listed.len()
        );
    }
    // Whatever hermes did to it, the gate is unaffected: it is a file in this process's state
    // directory, and nothing on a board can open it.
    assert!(
        after == "blocked" || after == "ready",
        "unexpected status after a plain list: {after:?}"
    );
}

#[tokio::test]
#[ignore = "runs a real hermes kanban on a throwaway board"]
async fn create_then_complete_plans_a_pull_request_referencing_the_issue() {
    let Some(board) = ScratchBoard::new("complete") else {
        return;
    };
    let kanban = board.kanban();
    let id = kanban
        .create(&create_request(57, "live complete probe"))
        .await
        .expect("create");
    board.run(&["complete", &id, "--summary", "live probe done"]);

    let detail = kanban.show(&id).await.expect("show");
    assert_eq!(detail.task.status, "done");
    assert!(
        detail.last_event("completed").is_some(),
        "kanban recorded the terminal event"
    );

    let robot = Robot::new(&bridge::config::RobotConfig::default(), WRITE_URL);
    let plan = plan(&detail, TerminalKind::Completed, robot.blocked_label(), true).expect("plans");
    let PlannedAction::OpenPull(pr) = &plan.actions[0] else {
        panic!("first action must open a PR")
    };
    assert!(
        pr.body.contains("Refs #57"),
        "PR body must reference the issue: {}",
        pr.body
    );
    assert!(pr.body.contains("live probe done"), "{}", pr.body);

    // The argv that would reach gitea-robot, asserted without making the call.
    let argv = robot.create_pull_args("terraphim", "gitea", pr);
    assert_eq!(argv[0], "create-pull");
    assert!(argv.iter().any(|a| a.contains("Refs #57")), "{argv:?}");
    assert_eq!(plan.actions[1].action(), Action::Comment);
}

#[tokio::test]
#[ignore = "runs a real hermes kanban on a throwaway board"]
async fn create_then_block_plans_the_blocked_label_and_a_reason_comment() {
    let Some(board) = ScratchBoard::new("block") else {
        return;
    };
    let kanban = board.kanban();
    let id = kanban
        .create(&create_request(58, "live block probe"))
        .await
        .expect("create");
    // The task is created blocked so a human's 🐝 releases it; `unblock` stands in for the
    // approval leg here. kanban refuses to block a task that is already blocked, so without
    // this the `blocked` event below would never be recorded.
    board.run(&["unblock", &id]);
    board.run(&["block", "--kind", "needs_input", &id, "waiting for spec"]);

    let detail = kanban.show(&id).await.expect("show");
    assert_eq!(detail.task.status, "blocked");

    let robot = Robot::new(&bridge::config::RobotConfig::default(), WRITE_URL);
    let plan = plan(&detail, TerminalKind::Blocked, robot.blocked_label(), true).expect("plans");
    assert_eq!(
        plan.actions[0],
        PlannedAction::Label(vec!["status/blocked".into()])
    );
    let PlannedAction::Comment(comment) = &plan.actions[1] else {
        panic!("second action must comment")
    };
    assert!(
        comment.contains("needs_input"),
        "the block kind must be named: {comment}"
    );
    assert!(comment.contains("waiting for spec"), "{comment}");

    let argv = robot.add_labels_args(
        "terraphim",
        "gitea",
        plan.gitea_ref.index,
        &["status/blocked".into()],
    );
    assert!(argv.contains(&"--add-labels".to_string()), "{argv:?}");
    assert!(argv.contains(&"58".to_string()), "{argv:?}");
}

#[tokio::test]
#[ignore = "runs a real hermes kanban on a throwaway board"]
async fn a_restart_does_not_reopen_a_pull_request() {
    // Acceptance criterion 6 against real kanban: the marker is a kanban comment, so a
    // process that forgot everything still refuses to act twice.
    let Some(board) = ScratchBoard::new("restart") else {
        return;
    };
    let kanban = board.kanban();
    let id = kanban
        .create(&create_request(59, "live restart probe"))
        .await
        .expect("create");
    board.run(&["complete", &id, "--summary", "done"]);

    let detail = kanban.show(&id).await.expect("show");
    let first = plan(&detail, TerminalKind::Completed, "status/blocked", true).expect("first pass plans");
    let PlannedAction::Comment(marker) = &first.actions[1] else {
        panic!("comment")
    };
    kanban
        .comment(&id, marker.lines().next().expect("marker line"))
        .await
        .expect("write the marker");

    // A brand-new handle — the moral equivalent of a restart.
    let fresh = board.kanban();
    let detail = fresh.show(&id).await.expect("show");
    assert!(
        plan(&detail, TerminalKind::Completed, "status/blocked", true).is_err(),
        "the durable marker must suppress the replay"
    );
}
