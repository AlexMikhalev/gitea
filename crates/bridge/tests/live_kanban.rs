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

    let listed = board.run(&["list", "--json"]);
    let tasks: serde_json::Value = serde_json::from_str(&listed).expect("list --json");
    assert_eq!(
        tasks.as_array().map(Vec::len),
        Some(1),
        "exactly one task exists: {listed}"
    );
}

/// Pins the fact the whole approval gate rests on: what status `hermes kanban create` yields.
///
/// `create` has no `--status` flag, and its default is `ready` — immediately claimable. So
/// with `require_approval` off, an issue reaching the ready endpoint is dispatched to an
/// agent with no human in the loop, and the approval sweep (which covers `blocked` and
/// `todo`) never sees a bridge-created task: AC5's promote/unblock branch would be dead code.
/// Nothing in-repo could pin this down but this test.
#[tokio::test]
#[ignore = "runs a real hermes kanban on a throwaway board"]
async fn create_lands_in_a_status_the_approval_sweep_covers() {
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
        detail.task.status, "blocked",
        "a gated task must land in a status the approval sweep covers"
    );

    // …and what happens without the gate, which is why it defaults on.
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
        "kanban's own default is immediately claimable"
    );
}

/// The reconciliation sweep keys on the event trail, not the status — and this is why.
///
/// A task created blocked to await a 🐝 is `blocked` and has never run. Keying on status
/// would label its Gitea issue `status/blocked` and comment that it was blocked, about work
/// that has not started.
#[tokio::test]
#[ignore = "runs a real hermes kanban on a throwaway board"]
async fn an_approval_gated_task_has_nothing_to_reconcile() {
    let Some(board) = ScratchBoard::new("gated") else {
        return;
    };
    let kanban = board.kanban();
    let id = kanban
        .create(&create_request(62, "live gated probe"))
        .await
        .expect("create");

    let detail = kanban.show(&id).await.expect("show");
    assert_eq!(detail.task.status, "blocked");
    assert!(
        latest_terminal_kind(&detail).is_none(),
        "a task that never ran has no terminal event: {:?}",
        detail.events
    );

    // Once it really is blocked by a worker, there is something to report. `unblock` first
    // is the 🐝 leg's move, and it is not optional: kanban refuses to block an already
    // blocked task, so this is the only order in which the real lifecycle reaches `blocked`.
    board.run(&["unblock", &id]);
    board.run(&["block", "--kind", "transient", &id, "flaked"]);
    let detail = kanban.show(&id).await.expect("show");
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

    // …and the sweep really finds a task through one of them.
    let blocked = kanban.list(Some("blocked")).await.expect("list blocked");
    assert!(
        blocked.iter().any(|t| t.id == id),
        "a gated task must be listed by the status it holds: {blocked:?}"
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

    let robot = Robot::new(&bridge::config::RobotConfig::default());
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

    let robot = Robot::new(&bridge::config::RobotConfig::default());
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
