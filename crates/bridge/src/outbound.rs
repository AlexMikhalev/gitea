// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//! Outbound leg: finished and blocked kanban tasks return to Gitea.
//!
//! `hermes kanban watch --kinds completed,blocked,gave_up,crashed,timed_out` is the event
//! source and `hermes kanban show <id> --json` resolves the detail. `completed` opens a
//! pull request carrying `Refs #<index>` plus an audit comment; the other four apply
//! `status/blocked` and a comment naming the block kind.
//!
//! Liveness is kanban's: a crash here orphans nothing, because kanban owns claims,
//! heartbeats and reclaim. Idempotence here is a durable marker comment on the kanban task,
//! not in-memory state, so a restart mid-flight duplicates nothing.

use std::sync::LazyLock;

use regex::Regex;

use crate::hermes::{TERMINAL_KINDS, TaskDetail};
use crate::inbound::GiteaRef;
use crate::robot::PullRequest;
use crate::rules::Action;

/// Marker comment written on the kanban task once its pull request exists.
pub const PR_MARKER: &str = "gitea-bridge: pr-opened";

/// Marker comment written on the kanban task once its block was reported to Gitea.
pub const BLOCK_MARKER: &str = "gitea-bridge: blocked-reported";

/// Prefix of the marker written once a repeatedly-failing action was reported to Gitea.
///
/// Distinct from [`BLOCK_MARKER`] and [`PR_MARKER`] so that escalating does not suppress a
/// genuine later report, and so the escalation itself is posted exactly once however long the
/// underlying failure lasts.
///
/// The failing action's name is part of the marker: a task whose label cannot be applied and
/// a task whose pull request cannot be opened are two different things to tell a human, and
/// one must not silence the other.
pub const ESCALATION_MARKER_PREFIX: &str = "gitea-bridge: report-failed";

/// The marker recording that `action`'s repeated failure has been reported on the issue.
pub fn escalation_marker(action: Action) -> String {
    format!("{ESCALATION_MARKER_PREFIX} {}", action.name())
}

/// The five terminal kanban events the bridge acts on.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum TerminalKind {
    /// The task finished successfully.
    Completed,
    /// The task was blocked, typed by `capability | dependency | needs_input | transient`.
    Blocked,
    /// The circuit breaker tripped.
    GaveUp,
    /// The worker died.
    Crashed,
    /// The runtime cap was exceeded.
    TimedOut,
}

impl TerminalKind {
    /// Resolves a kanban event kind. Non-terminal kinds return `None`.
    pub fn from_event_kind(kind: &str) -> Option<Self> {
        match kind {
            "completed" => Some(Self::Completed),
            "blocked" => Some(Self::Blocked),
            "gave_up" => Some(Self::GaveUp),
            "crashed" => Some(Self::Crashed),
            "timed_out" => Some(Self::TimedOut),
            _ => None,
        }
    }

    /// The kanban event kind this variant came from.
    pub fn event_kind(self) -> &'static str {
        match self {
            Self::Completed => "completed",
            Self::Blocked => "blocked",
            Self::GaveUp => "gave_up",
            Self::Crashed => "crashed",
            Self::TimedOut => "timed_out",
        }
    }

    /// Whether this kind returns work as a pull request rather than as a block.
    pub fn is_success(self) -> bool {
        matches!(self, Self::Completed)
    }

    /// Human-readable phrase used in the Gitea comment.
    pub fn phrase(self) -> &'static str {
        match self {
            Self::Completed => "completed",
            Self::Blocked => "was blocked",
            Self::GaveUp => "gave up after repeated failures",
            Self::Crashed => "crashed",
            Self::TimedOut => "exceeded its runtime cap",
        }
    }
}

/// One thing the bridge will do to Gitea, drawn from the [`Action`] allowlist.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum PlannedAction {
    /// Open a pull request.
    OpenPull(PullRequest),
    /// Post an issue comment.
    Comment(String),
    /// Add labels to an issue.
    Label(Vec<String>),
}

impl PlannedAction {
    /// The allowlisted action this corresponds to.
    ///
    /// Outbound cannot express anything outside the allowlist: the mapping is total and
    /// lands in `open_pr`, `comment` or `label`.
    pub fn action(&self) -> Action {
        match self {
            Self::OpenPull(_) => Action::OpenPr,
            Self::Comment(_) => Action::Comment,
            Self::Label(_) => Action::Label,
        }
    }
}

/// The full response to one terminal event.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct OutboundPlan {
    /// The Gitea issue this task came from.
    pub gitea_ref: GiteaRef,
    /// The terminal kind that produced the plan.
    pub kind: TerminalKind,
    /// Actions to perform, in order.
    pub actions: Vec<PlannedAction>,
}

/// Why a terminal event produced no plan.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum PlanError {
    /// The task body has no `gitea-ref:` trailer, so it did not come from this bridge.
    #[error("task {task} has no {prefix} trailer — not a bridge-created task", prefix = crate::inbound::GITEA_REF_PREFIX)]
    NoGiteaRef {
        /// Kanban task id.
        task: String,
    },
    /// The event was already reported to Gitea.
    #[error("task {task} already reported {kind} to gitea")]
    AlreadyReported {
        /// Kanban task id.
        task: String,
        /// The kind that was already reported.
        kind: &'static str,
    },
}

/// Builds the pull-request head branch for an issue, matching this repo's convention.
///
/// `task/<index>-<slug>`, which is what `taskBranchRe` in `ready_graph.go` recognises as
/// in-progress work for that issue.
pub fn head_branch(index: i64, title: &str) -> String {
    let slug: String = title
        .chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() {
                c.to_ascii_lowercase()
            } else {
                '-'
            }
        })
        .collect();
    let slug = slug
        .split('-')
        .filter(|s| !s.is_empty())
        .take(6)
        .collect::<Vec<_>>()
        .join("-");
    if slug.is_empty() {
        format!("task/{index}")
    } else {
        format!("task/{index}-{slug}")
    }
}

/// Builds the pull-request body. Acceptance criterion 2 rests on `Refs #<index>`.
pub fn pull_body(gref: &GiteaRef, task_id: &str, summary: Option<&str>) -> String {
    let mut body = format!("Refs #{}\n\n", gref.index);
    if let Some(summary) = summary.map(str::trim).filter(|s| !s.is_empty()) {
        body.push_str(summary);
        body.push_str("\n\n");
    }
    body.push_str(&format!(
        "Opened by the gitea-automations bridge from kanban task `{task_id}`.\n"
    ));
    body
}

/// Builds the audit comment posted alongside a pull request.
pub fn completed_comment(task_id: &str, summary: Option<&str>) -> String {
    let mut body = format!("{PR_MARKER}\n\nKanban task `{task_id}` completed; a pull request was opened.");
    if let Some(summary) = summary.map(str::trim).filter(|s| !s.is_empty()) {
        body.push_str(&format!("\n\n> {summary}"));
    }
    body.push('\n');
    body
}

/// Builds the comment posted when a task reaches a non-`completed` terminal state.
///
/// Acceptance criterion 3 requires the block kind to be named, so it is stated explicitly
/// even when kanban left it generic.
pub fn blocked_comment(
    task_id: &str,
    kind: TerminalKind,
    block_kind: Option<&str>,
    reason: Option<&str>,
) -> String {
    let block_kind = block_kind
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .unwrap_or("unspecified");
    let mut body = format!(
        "{BLOCK_MARKER}\n\nKanban task `{task}` {phrase} (event `{event}`, block kind `{block_kind}`).",
        task = task_id,
        phrase = kind.phrase(),
        event = kind.event_kind(),
    );
    if let Some(reason) = reason.map(str::trim).filter(|s| !s.is_empty()) {
        body.push_str(&format!("\n\n> {reason}"));
    }
    body.push('\n');
    body
}

/// Builds the comment posted when one action of a plan keeps failing.
///
/// `context` is the one fact that tells a human *which* failure this is: the head branch for
/// a pull request, the label name for a label.
pub fn escalation_comment(
    task_id: &str,
    action: Action,
    context: Option<&str>,
    attempts: u32,
    error: &str,
) -> String {
    let mut body = format!(
        "{marker}\n\nKanban task `{task_id}` reached a terminal state, but the `{action}` step \
         of reporting it here has failed {attempts} times. The bridge will keep retrying.\n\n",
        marker = escalation_marker(action),
        action = action.name(),
    );
    match (action, context) {
        (Action::OpenPr, Some(head)) => body.push_str(&format!(
            "Head branch `{head}` — if it was never pushed to this instance, no pull request \
             can be opened until it is. Pushing is the worker's job, not the bridge's.\n\n"
        )),
        (Action::Label, Some(label)) => body.push_str(&format!(
            "Label `{label}` — a label that does not exist in this repository is dropped by the \
             API in silence, so it most likely needs creating. Until it does, this comment is \
             the only report this task gets.\n\n"
        )),
        _ => {}
    }
    body.push_str(&format!("> {error}\n"));
    body
}

/// The actions that surface a repeatedly-failing report on the Gitea issue.
///
/// Two failures are reachable here and both used to be silent. A `completed` task returns work
/// as a pull request against a head branch the bridge does not push, so `create-pull` fails
/// until someone pushes it; and every other terminal kind leads with a label, which
/// `gitea-robot edit-issue` refuses outright when the repository does not have it
/// (`cmd/gitea-robot/write.go`). Either way the daemon's `apply_actions` (`main.rs`) stops at the
/// failing action, the task is left unmarked, and the reconcile sweep replays the same plan
/// every 300s indefinitely — with the only signal a recurring `error!` line in the daemon's
/// log, while the Gitea issue stays completely silent about work that ran and stopped. A human
/// watching the board cannot tell that from an issue nobody picked up.
///
/// So after [`crate::state::ESCALATE_AFTER`] failures the failure is said where the humans are
/// looking, and [`escalation_marker`] makes each one land exactly once. The retry is *not*
/// abandoned: push the branch, or create the label, and the original plan applies.
///
/// **This is a comment and nothing else, deliberately.** The escalation must not depend on the
/// action that is failing — leading with a label, as this once did, means the missing-label
/// case escalates by trying the very call that is broken, gets nowhere, and stays silent.
/// A comment is the one verb with no repository-side precondition.
pub fn escalation_actions(
    detail: &TaskDetail,
    action: Action,
    context: Option<&str>,
    attempts: u32,
    error: &str,
) -> Result<Vec<PlannedAction>, PlanError> {
    if detail.has_marker(&escalation_marker(action)) {
        return Err(PlanError::AlreadyReported {
            task: detail.task.id.clone(),
            kind: "escalation",
        });
    }
    Ok(vec![PlannedAction::Comment(escalation_comment(
        &detail.task.id,
        action,
        context,
        attempts,
        error,
    ))])
}

/// Maps one terminal event onto Gitea actions.
///
/// `detail` is the `hermes kanban show --json` payload; `marker_guard` makes the mapping
/// idempotent across a restart by consulting the durable marker comments on the task.
pub fn plan(
    detail: &TaskDetail,
    kind: TerminalKind,
    blocked_label: &str,
    marker_guard: bool,
) -> Result<OutboundPlan, PlanError> {
    let task_id = detail.task.id.clone();
    let body = detail.task.body.clone().unwrap_or_default();
    let gitea_ref = GiteaRef::parse_from_body(&body).ok_or_else(|| PlanError::NoGiteaRef {
        task: task_id.clone(),
    })?;

    if kind.is_success() {
        if marker_guard && detail.has_marker(PR_MARKER) {
            return Err(PlanError::AlreadyReported {
                task: task_id,
                kind: kind.event_kind(),
            });
        }
        let summary = detail
            .last_event("completed")
            .and_then(|e| e.payload_str("summary"))
            .or_else(|| detail.latest_summary.clone());
        // Always a name, never a question of whether one exists: `head_branch` falls back to
        // `task/<index>` for a title that slugifies to nothing, so this cannot be empty.
        //
        // "Completed but the worker never pushed a branch" is a real operational state and it
        // is deliberately *not* handled here. Planning is offline — it sees a kanban payload,
        // not a git remote — so a head that does not exist is indistinguishable at this point
        // from one that does. It is answered where the answer is knowable: `create-pull` fails,
        // the failure is counted, and `escalate_plan_failure` reports it on the issue. A guard
        // here could only ever have caught a shape that cannot occur, while reading like it
        // covered the one that does.
        let head = detail
            .task
            .branch_name
            .clone()
            .filter(|b| !b.trim().is_empty())
            .unwrap_or_else(|| head_branch(gitea_ref.index, &detail.task.title));
        let pr = PullRequest {
            title: format!("issue #{}: {}", gitea_ref.index, detail.task.title),
            head,
            body: pull_body(&gitea_ref, &task_id, summary.as_deref()),
        };
        return Ok(OutboundPlan {
            gitea_ref,
            kind,
            actions: vec![
                PlannedAction::OpenPull(pr),
                PlannedAction::Comment(completed_comment(&task_id, summary.as_deref())),
            ],
        });
    }

    if marker_guard && detail.has_marker(BLOCK_MARKER) {
        return Err(PlanError::AlreadyReported {
            task: task_id,
            kind: kind.event_kind(),
        });
    }
    let event = detail.last_event(kind.event_kind());
    let block_kind = event.and_then(|e| e.payload_str("kind"));
    let reason = event
        .and_then(|e| e.payload_str("reason"))
        .or_else(|| event.and_then(|e| e.payload_str("error")))
        .or_else(|| event.and_then(|e| e.payload_str("summary")));
    Ok(OutboundPlan {
        gitea_ref,
        kind,
        actions: vec![
            PlannedAction::Label(vec![blocked_label.to_string()]),
            PlannedAction::Comment(blocked_comment(
                &task_id,
                kind,
                block_kind.as_deref(),
                reason.as_deref(),
            )),
        ],
    })
}

/// The most recent terminal kind recorded in a task's event trail.
///
/// The reconciliation sweep uses this instead of the task's *status*, which cannot tell a
/// task blocked by a worker from one created blocked to await a 🐝 — the latter has no
/// terminal event at all and nothing to report.
pub fn latest_terminal_kind(detail: &TaskDetail) -> Option<TerminalKind> {
    detail
        .last_terminal_event()
        .and_then(|e| TerminalKind::from_event_kind(&e.kind))
}

/// One line of `hermes kanban watch` output.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct WatchEvent {
    /// Kanban task id.
    pub task_id: String,
    /// Terminal kind.
    pub kind: TerminalKind,
}

/// What one line of `watch` output turned out to be.
///
/// The three cases are worth keeping apart because they deserve different log levels. The
/// line format below is not a documented interface — it is a terminal display — so a change
/// to it would otherwise strand the outbound leg in silence.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum WatchLine {
    /// A terminal event to act on.
    Event(WatchEvent),
    /// An event line whose kind is not one of the five terminal kinds.
    ///
    /// `watch` is invoked with `--kinds`, so this should not happen: seeing it means either
    /// the filter is not honoured or the kind names moved.
    UnexpectedKind {
        /// The kind that was read off the line.
        kind: String,
    },
    /// Not an event line at all — a banner, a blank, or a format that no longer matches.
    Unrecognised,
}

/// `[2026-08-04 16:23] t_ea5c4ee9 completed          (@-) {'summary': 'done'}`
///
/// Only the id and the kind are taken from the line: the trailing payload is a Python dict
/// repr, not JSON, so detail is resolved with `show --json` instead of parsed from here.
static WATCH_LINE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"^\[[^\]]*\]\s+(?<id>\S+)\s+(?<kind>[a-z_]+)\b").expect("static regex compiles")
});

/// Classifies one `watch` line.
pub fn classify_watch_line(line: &str) -> WatchLine {
    let Some(caps) = WATCH_LINE.captures(line.trim_end()) else {
        return WatchLine::Unrecognised;
    };
    match TerminalKind::from_event_kind(&caps["kind"]) {
        Some(kind) => WatchLine::Event(WatchEvent {
            task_id: caps["id"].to_string(),
            kind,
        }),
        None => WatchLine::UnexpectedKind {
            kind: caps["kind"].to_string(),
        },
    }
}

/// Parses one `watch` line, ignoring banners and non-terminal kinds.
pub fn parse_watch_line(line: &str) -> Option<WatchEvent> {
    match classify_watch_line(line) {
        WatchLine::Event(event) => Some(event),
        _ => None,
    }
}

/// Sanity check that the watcher subscription and the mapping cover the same set.
pub fn subscribed_kinds_match_mapping() -> bool {
    TERMINAL_KINDS
        .iter()
        .all(|k| TerminalKind::from_event_kind(k).is_some())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::hermes::{Task, TaskComment, TaskEvent};

    fn detail(kind: &str, payload: &[(&str, serde_json::Value)]) -> TaskDetail {
        TaskDetail {
            task: Task {
                id: "t_abc123".into(),
                title: "automations daemon".into(),
                body: Some("gitea-ref: terraphim/gitea#57".into()),
                status: "done".into(),
                assignee: None,
                branch_name: None,
            },
            latest_summary: None,
            comments: vec![],
            events: vec![TaskEvent {
                kind: kind.into(),
                payload: payload
                    .iter()
                    .map(|(k, v)| ((*k).to_string(), v.clone()))
                    .collect(),
                created_at: 1,
            }],
        }
    }

    #[test]
    fn all_five_terminal_kinds_map() {
        // Acceptance criterion 3 covers four of these; criterion 2 the fifth. The watcher
        // subscribes to exactly this set, so a gap here would be a silently dropped event.
        assert!(subscribed_kinds_match_mapping());
        let expected = [
            ("completed", TerminalKind::Completed, Action::OpenPr),
            ("blocked", TerminalKind::Blocked, Action::Label),
            ("gave_up", TerminalKind::GaveUp, Action::Label),
            ("crashed", TerminalKind::Crashed, Action::Label),
            ("timed_out", TerminalKind::TimedOut, Action::Label),
        ];
        for (name, kind, first_action) in expected {
            assert_eq!(TerminalKind::from_event_kind(name), Some(kind));
            let plan = plan(&detail(name, &[]), kind, "status/blocked", true).expect("plans");
            assert_eq!(plan.actions[0].action(), first_action, "{name}");
            assert_eq!(plan.actions[1].action(), Action::Comment, "{name}");
            assert_eq!(plan.actions.len(), 2, "{name}");
            assert_eq!(plan.gitea_ref.index, 57);
        }
    }

    #[test]
    fn non_terminal_kinds_do_not_map() {
        for k in ["created", "commented", "claimed", "heartbeat", "archived", ""] {
            assert_eq!(TerminalKind::from_event_kind(k), None, "{k}");
        }
    }

    #[test]
    fn completed_opens_one_pr_referencing_the_issue_and_one_audit_comment() {
        let d = detail("completed", &[("summary", serde_json::json!("shipped"))]);
        let plan = plan(&d, TerminalKind::Completed, "status/blocked", true).expect("plans");
        let PlannedAction::OpenPull(pr) = &plan.actions[0] else {
            panic!("first action must be a PR")
        };
        assert!(pr.body.contains("Refs #57"), "{}", pr.body);
        assert!(pr.body.contains("shipped"), "{}", pr.body);
        assert_eq!(pr.head, "task/57-automations-daemon");
        assert!(pr.title.contains("#57"));
        let PlannedAction::Comment(comment) = &plan.actions[1] else {
            panic!("second action must comment")
        };
        assert!(comment.contains(PR_MARKER), "{comment}");
        assert_eq!(
            plan.actions
                .iter()
                .filter(|a| matches!(a, PlannedAction::OpenPull(_)))
                .count(),
            1,
            "exactly one PR"
        );
    }

    #[test]
    fn a_worktree_branch_wins_over_the_derived_one() {
        let mut d = detail("completed", &[]);
        d.task.branch_name = Some("wt/t6-wire".into());
        let plan = plan(&d, TerminalKind::Completed, "status/blocked", true).expect("plans");
        let PlannedAction::OpenPull(pr) = &plan.actions[0] else {
            panic!("PR")
        };
        assert_eq!(pr.head, "wt/t6-wire");
    }

    #[test]
    fn each_block_kind_names_itself_in_the_comment() {
        for block_kind in ["capability", "dependency", "needs_input", "transient"] {
            let d = detail(
                "blocked",
                &[
                    ("kind", serde_json::json!(block_kind)),
                    ("reason", serde_json::json!("waiting for spec")),
                ],
            );
            let plan = plan(&d, TerminalKind::Blocked, "status/blocked", true).expect("plans");
            assert_eq!(
                plan.actions[0],
                PlannedAction::Label(vec!["status/blocked".into()])
            );
            let PlannedAction::Comment(comment) = &plan.actions[1] else {
                panic!("comment")
            };
            assert!(comment.contains(block_kind), "{comment}");
            assert!(comment.contains("waiting for spec"), "{comment}");
        }
    }

    #[test]
    fn an_untyped_block_still_names_a_kind() {
        let d = detail("crashed", &[("error", serde_json::json!("worker died"))]);
        let plan = plan(&d, TerminalKind::Crashed, "status/blocked", true).expect("plans");
        let PlannedAction::Comment(comment) = &plan.actions[1] else {
            panic!("comment")
        };
        assert!(comment.contains("unspecified"), "{comment}");
        assert!(comment.contains("worker died"), "{comment}");
        assert!(comment.contains("crashed"), "{comment}");
    }

    #[test]
    fn a_task_without_a_trailer_is_not_ours() {
        let mut d = detail("completed", &[]);
        d.task.body = Some("hand-written task".into());
        assert!(matches!(
            plan(&d, TerminalKind::Completed, "status/blocked", true),
            Err(PlanError::NoGiteaRef { .. })
        ));
    }

    #[test]
    fn the_marker_makes_a_replay_a_no_op() {
        // Restart mid-flight duplicates nothing: the guard lives in kanban, not in memory.
        let mut d = detail("completed", &[]);
        d.comments.push(TaskComment {
            author: Some("bridge".into()),
            body: completed_comment("t_abc123", None),
        });
        assert!(matches!(
            plan(&d, TerminalKind::Completed, "status/blocked", true),
            Err(PlanError::AlreadyReported { .. })
        ));
        // …and the guard is what does it, not the shape of the event.
        assert!(plan(&d, TerminalKind::Completed, "status/blocked", false).is_ok());
    }

    #[test]
    fn a_block_replay_is_also_a_no_op() {
        let mut d = detail("blocked", &[]);
        d.comments.push(TaskComment {
            author: Some("bridge".into()),
            body: blocked_comment("t_abc123", TerminalKind::Blocked, None, None),
        });
        assert!(matches!(
            plan(&d, TerminalKind::Blocked, "status/blocked", true),
            Err(PlanError::AlreadyReported { .. })
        ));
    }

    #[test]
    fn a_pr_marker_does_not_suppress_a_later_block() {
        let mut d = detail("blocked", &[]);
        d.comments.push(TaskComment {
            author: Some("bridge".into()),
            body: completed_comment("t_abc123", None),
        });
        assert!(plan(&d, TerminalKind::Blocked, "status/blocked", true).is_ok());
    }

    /// A completed task whose branch was never pushed used to retry forever in silence.
    #[test]
    fn a_pull_request_that_cannot_be_opened_is_escalated_to_the_issue_once() {
        let mut d = detail("completed", &[]);
        let actions =
            escalation_actions(&d, Action::OpenPr, Some("task/57-x"), 3, "404 not found").expect("escalates");
        // Comment-only: an escalation that led with a label would be trying the one verb that
        // is broken whenever the *label* is what failed.
        assert_eq!(actions.len(), 1);
        let PlannedAction::Comment(comment) = &actions[0] else {
            panic!("comment")
        };
        // Naming the head branch is the whole point: it is the one fact that tells a human
        // this is "the worker never pushed" rather than "nobody picked it up".
        assert!(comment.contains("task/57-x"), "{comment}");
        assert!(comment.contains("404 not found"), "{comment}");
        assert_eq!(
            comment.lines().next(),
            Some(escalation_marker(Action::OpenPr).as_str())
        );

        // Once reported, it is not reported again — the retry continues, the noise does not.
        d.comments.push(TaskComment {
            author: Some("bridge".into()),
            body: escalation_marker(Action::OpenPr),
        });
        assert!(matches!(
            escalation_actions(&d, Action::OpenPr, Some("task/57-x"), 9, "404 not found"),
            Err(PlanError::AlreadyReported { .. })
        ));
    }

    /// The P1 this closes: `blocked`/`gave_up`/`crashed`/`timed_out` plans lead with a label,
    /// `edit-issue` fails hard on a label the repository does not have, and `apply_actions`
    /// stops there — so the reason comment never posted and the issue stayed silent while the
    /// reconcile sweep replayed the same failing plan every 300s forever.
    #[test]
    fn a_label_that_cannot_be_applied_is_escalated_by_comment_alone() {
        let d = detail("blocked", &[("kind", serde_json::json!("needs_input"))]);
        let actions = escalation_actions(
            &d,
            Action::Label,
            Some("status/blocked"),
            3,
            "labels status/blocked are not present after the request",
        )
        .expect("escalates");
        assert_eq!(actions.len(), 1, "a label escalation must not use the label verb");
        let PlannedAction::Comment(comment) = &actions[0] else {
            panic!("comment")
        };
        assert!(comment.contains("status/blocked"), "{comment}");
        assert!(comment.contains("does not exist in this repository"), "{comment}");
        assert_eq!(
            comment.lines().next(),
            Some(escalation_marker(Action::Label).as_str())
        );
    }

    /// One failing action's escalation must not silence another's: a task can complete, have
    /// its label fail, and later block for real.
    #[test]
    fn each_failing_action_escalates_under_its_own_marker() {
        let mut d = detail("completed", &[]);
        d.comments.push(TaskComment {
            author: Some("bridge".into()),
            body: escalation_marker(Action::Label),
        });
        assert_ne!(
            escalation_marker(Action::Label),
            escalation_marker(Action::OpenPr)
        );
        assert!(escalation_actions(&d, Action::OpenPr, Some("task/57-x"), 3, "404").is_ok());
        assert!(matches!(
            escalation_actions(&d, Action::Label, Some("status/blocked"), 3, "nope"),
            Err(PlanError::AlreadyReported { .. })
        ));
    }

    #[test]
    fn escalating_a_completed_task_does_not_suppress_a_later_block_or_its_own_pull_request() {
        let mut d = detail("completed", &[]);
        d.comments.push(TaskComment {
            author: Some("bridge".into()),
            body: escalation_marker(Action::OpenPr),
        });
        // The pull request is still attempted: if the branch is pushed later, it opens.
        assert!(plan(&d, TerminalKind::Completed, "status/blocked", true).is_ok());
        // And a real block afterwards is still reported, because the markers are distinct.
        assert!(plan(&d, TerminalKind::Blocked, "status/blocked", true).is_ok());
    }

    #[test]
    fn watch_lines_parse() {
        let line =
            "[2026-08-04 16:23] t_ea5c4ee9 completed          (@-) {'result_len': 0, 'summary': 'done'}";
        assert_eq!(
            parse_watch_line(line),
            Some(WatchEvent {
                task_id: "t_ea5c4ee9".into(),
                kind: TerminalKind::Completed
            })
        );
        let line = "[2026-08-04 16:23] t_3b49a6e0 timed_out (@codex) {'limit': '30m'}";
        assert_eq!(
            parse_watch_line(line),
            Some(WatchEvent {
                task_id: "t_3b49a6e0".into(),
                kind: TerminalKind::TimedOut
            })
        );
    }

    #[test]
    fn watch_banners_and_noise_are_ignored() {
        assert_eq!(parse_watch_line("Watching kanban events. Ctrl-C to stop."), None);
        assert_eq!(parse_watch_line(""), None);
        assert_eq!(parse_watch_line("[2026-08-04 16:23] t_1 created (@-) {}"), None);
    }

    #[test]
    fn an_unrecognised_line_is_told_apart_from_an_unexpected_kind() {
        // Both are dropped, but only one of them means the line format moved under us, and
        // the caller logs them at different levels for exactly that reason: a banner is
        // routine, a `watch --kinds` filter that stopped being honoured is not.
        assert_eq!(
            classify_watch_line("Watching kanban events. Ctrl-C to stop."),
            WatchLine::Unrecognised
        );
        assert_eq!(classify_watch_line(""), WatchLine::Unrecognised);
        assert_eq!(
            classify_watch_line("[2026-08-04 16:23] t_1 created (@-) {}"),
            WatchLine::UnexpectedKind {
                kind: "created".into()
            }
        );
        assert!(matches!(
            classify_watch_line("[2026-08-04 16:23] t_1 completed (@-) {}"),
            WatchLine::Event(_)
        ));
    }

    #[test]
    fn the_latest_terminal_event_is_what_reconciliation_replays() {
        let mut d = detail("blocked", &[("kind", serde_json::json!("transient"))]);
        assert_eq!(latest_terminal_kind(&d), Some(TerminalKind::Blocked));

        // A later terminal event wins over an earlier one.
        d.events.push(TaskEvent {
            kind: "completed".into(),
            payload: Default::default(),
            created_at: 9,
        });
        assert_eq!(latest_terminal_kind(&d), Some(TerminalKind::Completed));

        // A task that never ran has nothing to report, whatever its status says.
        let gated = TaskDetail {
            task: Task {
                id: "t_1".into(),
                status: "blocked".into(),
                body: Some("gitea-ref: o/r#1".into()),
                ..Task::default()
            },
            events: vec![TaskEvent {
                kind: "created".into(),
                payload: Default::default(),
                created_at: 1,
            }],
            ..TaskDetail::default()
        };
        assert_eq!(latest_terminal_kind(&gated), None);
    }

    #[test]
    fn head_branch_matches_the_task_branch_convention() {
        // ready_graph.go's taskBranchRe is ^task/(\d+)- , so the index must lead.
        assert_eq!(
            head_branch(57, "automations daemon"),
            "task/57-automations-daemon"
        );
        assert_eq!(
            head_branch(1, "Fix: the thing!! (again)"),
            "task/1-fix-the-thing-again"
        );
        assert_eq!(head_branch(9, ""), "task/9");
        assert_eq!(head_branch(9, "🐝🐝"), "task/9");
        assert!(head_branch(12, "a b c d e f g h").starts_with("task/12-"));
        assert_eq!(head_branch(12, "a b c d e f g h").split('-').count(), 7);
    }
}
