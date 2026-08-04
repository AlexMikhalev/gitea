// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//! Inbound leg: ready Gitea issues become runnable kanban tasks.
//!
//! Poll `GET /api/v1/robot/ready?owner=&repo=&skip_in_progress=1` and, per `ready_issues[]`
//! entry, run `hermes kanban create <title> --idempotency-key gitea:<owner>/<repo>#<index>`.
//!
//! Deduplication is the key, not in-memory state. Two poll cycles over one ready issue
//! therefore create exactly one task, and a restart mid-flight duplicates nothing, because
//! nothing about the decision lives in this process.
//!
//! The sweep is *capped*, and the cap is what bounds the whole daemon. See
//! [`select_ready`].

use crate::config::RepoRef;
use crate::gitea::{GiteaClient, GiteaError, ReadyIssue};
use crate::hermes::{CreateTask, Kanban, KanbanError};
use crate::state::{PendingApprovals, PendingTask};

/// Trailer prefix that carries the Gitea coordinates on the kanban task body.
///
/// `hermes kanban show --json` reports the task's body but not its idempotency key, so the
/// outbound leg would otherwise have no way back from a task id to `owner/repo#index`.
/// Putting it in the body keeps the mapping in kanban — durable, and readable by a human
/// looking at the task.
pub const GITEA_REF_PREFIX: &str = "gitea-ref:";

/// The Gitea coordinates of an issue.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct GiteaRef {
    /// Repository owner.
    pub owner: String,
    /// Repository name.
    pub repo: String,
    /// Issue index.
    pub index: i64,
}

impl GiteaRef {
    /// Builds a reference.
    pub fn new(owner: impl Into<String>, repo: impl Into<String>, index: i64) -> Self {
        Self {
            owner: owner.into(),
            repo: repo.into(),
            index,
        }
    }

    /// The trailer line written onto the kanban task body.
    pub fn trailer(&self) -> String {
        format!("{GITEA_REF_PREFIX} {}/{}#{}", self.owner, self.repo, self.index)
    }

    /// Recovers a reference from a kanban task body, if the trailer is present.
    pub fn parse_from_body(body: &str) -> Option<Self> {
        for line in body.lines() {
            let line = line.trim();
            let Some(rest) = line.strip_prefix(GITEA_REF_PREFIX) else {
                continue;
            };
            let rest = rest.trim();
            let (slug, index) = rest.rsplit_once('#')?;
            let (owner, repo) = slug.split_once('/')?;
            let index: i64 = index.trim().parse().ok()?;
            if owner.is_empty() || repo.is_empty() {
                return None;
            }
            return Some(Self::new(owner.trim(), repo.trim(), index));
        }
        None
    }
}

/// Builds the kanban idempotency key for a Gitea issue.
///
/// `gitea:<owner>/<repo>#<index>`. Stable across restarts and across hosts, which is what
/// makes acceptance criteria 1 and 6 hold.
pub fn idempotency_key(owner: &str, repo: &str, index: i64) -> String {
    format!("gitea:{owner}/{repo}#{index}")
}

/// Builds the opening post for a task created from a ready issue.
pub fn task_body(repo: &RepoRef, issue: &ReadyIssue, base_url: &str) -> String {
    let gref = GiteaRef::new(&repo.owner, &repo.repo, issue.index);
    let url = format!(
        "{}/{}/{}/issues/{}",
        base_url.trim_end_matches('/'),
        repo.owner,
        repo.repo,
        issue.index
    );
    format!(
        "Created by the gitea-automations bridge from a ready issue.\n\n\
         Issue: {url}\n\
         PageRank: {:.4} | priority: {} | blockers: {}\n\n\
         {}\n",
        issue.page_rank,
        issue.priority,
        issue.blocker_count,
        gref.trailer()
    )
}

/// Builds the create request for one ready issue.
pub fn create_request(repo: &RepoRef, issue: &ReadyIssue, base_url: &str) -> CreateTask {
    CreateTask {
        title: issue.title.clone(),
        body: task_body(repo, issue, base_url),
        idempotency_key: idempotency_key(&repo.owner, &repo.repo, issue.index),
    }
}

/// Selects the issues one sweep will act on: unblocked, best-first, at most `max_tasks`.
///
/// The endpoint returns *every* unblocked open issue with no limit and no paging
/// (`routers/api/v1/robot/ready_graph.go:192-276`), so on a busy board an uncapped sweep is
/// one `hermes kanban create` subprocess per open issue — and, worse, a permanent cost:
/// every task it creates is then swept for a 🐝 every `approval_interval_secs` and shown by
/// the reconcile sweep every `reconcile_interval_secs`, forever. Nothing drains the set on
/// its own, because a task the bridge blocked leaves its issue "ready" as far as
/// `getInProgressIssues` (`ready_graph.go:279-301`) is concerned.
///
/// Two properties make the cap bound the *board* and not merely one sweep's work:
///
/// * the order is **total and deterministic** — PageRank descending, issue index ascending
///   as the tie-break — so consecutive sweeps over an unchanged ready set select the *same*
///   issues rather than a rotating window;
/// * creation is keyed by [`idempotency_key`], so re-selecting them costs one deduplicated
///   `create` each and produces no new task.
///
/// Steady state is therefore at most `max_tasks` bridge-created tasks in flight per
/// repository. Work is not lost: as capped tasks complete, their issues stop being ready
/// (a `task/<idx>-` branch or an open pull request), and the next sweep admits the next
/// best ones. The deferred count is reported, never silently dropped —
/// [`InboundReport::deferred`].
///
/// The endpoint already sorts by PageRank descending, but sorting here is not redundant: it
/// is what makes the *selection* well-defined rather than a property of the transport.
pub fn select_ready(issues: &[ReadyIssue], max_tasks: usize) -> (Vec<&ReadyIssue>, usize) {
    let mut ready: Vec<&ReadyIssue> = issues
        .iter()
        .filter(|issue| {
            // `is_blocked` should already be false here, but the endpoint is the source of
            // truth for readiness and we do not re-derive it — we just refuse to run
            // backwards.
            if issue.is_blocked {
                tracing::debug!(
                    index = issue.index,
                    "skipping issue the ready endpoint marked blocked"
                );
            }
            !issue.is_blocked
        })
        .collect();
    // `total_cmp` rather than `partial_cmp().unwrap()`: a NaN score would otherwise panic
    // inside the comparator, and a panic in a sweep is a dead leg.
    ready.sort_by(|a, b| {
        b.page_rank
            .total_cmp(&a.page_rank)
            .then_with(|| a.index.cmp(&b.index))
    });
    let deferred = ready.len().saturating_sub(max_tasks);
    ready.truncate(max_tasks);
    (ready, deferred)
}

/// What one inbound sweep did.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct InboundReport {
    /// `(issue index, kanban task id)` for every ready issue resolved this cycle.
    ///
    /// A repeat cycle yields the *same* task id, which is how acceptance criterion 1 is
    /// checked: id equality, not an absence of a second call.
    pub resolved: Vec<(i64, String)>,
    /// Issues that could not be turned into a task this cycle.
    pub failed: Vec<(i64, String)>,
    /// Ready issues left for a later sweep because `poll.max_tasks_per_sweep` was reached.
    ///
    /// A cap that truncates in silence reads exactly like a board with nothing left to do,
    /// so this is carried out of the sweep and logged rather than dropped.
    pub deferred: usize,
    /// Issue indices held at the approval gate rather than created.
    ///
    /// Every admitted issue lands here instead of in `resolved` when the gate is on: no
    /// kanban task exists for it yet, and the 🐝 sweep is what creates one.
    pub held: Vec<i64>,
}

impl InboundReport {
    /// The task id resolved for an issue index, if any.
    pub fn task_for(&self, index: i64) -> Option<&str> {
        self.resolved
            .iter()
            .find(|(i, _)| *i == index)
            .map(|(_, id)| id.as_str())
    }
}

/// Inbound failures that abort a whole sweep (per-issue failures are collected instead).
#[derive(Debug, thiserror::Error)]
pub enum InboundError {
    /// The ready endpoint could not be read.
    #[error(transparent)]
    Gitea(#[from] GiteaError),
}

/// Runs one inbound sweep over a single repository.
///
/// At most `max_tasks` issues are admitted, chosen by [`select_ready`].
///
/// `gate` is the approval gate. When it is `Some`, an admitted issue is **held** rather than
/// created: no `hermes kanban create` runs at all, so no task exists for a worker to claim, and
/// [`crate::state::PendingApprovals`] is what a 🐝 releases. That is the only place the gate can
/// live — a task created `--initial-status blocked` is promoted out of `blocked` by the next
/// `hermes kanban list`, which is the approval sweep's own first call. When it is `None` the
/// task is created immediately, in kanban's default `ready`, with no human in the loop.
pub async fn poll_once(
    gitea: &GiteaClient,
    kanban: &Kanban,
    repo: &RepoRef,
    base_url: &str,
    skip_in_progress: bool,
    max_tasks: usize,
    gate: Option<&PendingApprovals>,
) -> Result<InboundReport, InboundError> {
    let ready = gitea.ready(&repo.owner, &repo.repo, skip_in_progress).await?;
    let (selected, deferred) = select_ready(&ready.ready_issues, max_tasks);
    let mut report = InboundReport {
        deferred,
        ..InboundReport::default()
    };
    if deferred > 0 {
        tracing::warn!(
            repo = %repo.slug(),
            deferred,
            max_tasks,
            "more ready issues than poll.max_tasks_per_sweep; the rest wait for a later sweep \
             (raise the cap to admit more work per repository)"
        );
    }
    for issue in selected {
        let req = create_request(repo, issue, base_url);
        if let Some(gate) = gate {
            // Held, not created. Re-holding an issue is the steady state rather than an
            // anomaly: an issue with no task is still ready, so it is offered every sweep —
            // which is also what makes losing the gate file recoverable.
            let entry = PendingTask {
                owner: repo.owner.clone(),
                repo: repo.repo.clone(),
                index: issue.index,
                title: req.title.clone(),
                body: req.body.clone(),
                idempotency_key: req.idempotency_key.clone(),
            };
            match gate.hold(entry) {
                Ok(fresh) => {
                    if fresh {
                        tracing::info!(
                            repo = %repo.slug(),
                            index = issue.index,
                            key = %req.idempotency_key,
                            "ready issue held at the approval gate; no kanban task exists until a 🐝 arrives"
                        );
                    }
                    report.held.push(issue.index);
                }
                Err(err) => {
                    // Not merely a failed write: the gate is the only record that this issue
                    // is waiting, and the alternative to holding it is creating it unapproved.
                    tracing::error!(
                        repo = %repo.slug(), index = issue.index, error = %err,
                        "cannot hold a ready issue at the approval gate; it stays uncreated"
                    );
                    report.failed.push((issue.index, err.to_string()));
                }
            }
            continue;
        }
        match kanban.create(&req).await {
            Ok(id) => {
                tracing::info!(
                    repo = %repo.slug(),
                    index = issue.index,
                    task = %id,
                    key = %req.idempotency_key,
                    "ready issue resolved to kanban task"
                );
                report.resolved.push((issue.index, id));
            }
            Err(err) => {
                tracing::error!(repo = %repo.slug(), index = issue.index, error = %err, "kanban create failed");
                report.failed.push((issue.index, describe(&err)));
            }
        }
    }
    Ok(report)
}

fn describe(err: &KanbanError) -> String {
    err.to_string()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn issue(index: i64) -> ReadyIssue {
        ReadyIssue {
            id: 900 + index,
            index,
            title: "automations daemon".into(),
            page_rank: 0.4237,
            priority: 2,
            is_blocked: false,
            blocker_count: 0,
        }
    }

    fn ranked(index: i64, page_rank: f64) -> ReadyIssue {
        ReadyIssue {
            page_rank,
            ..issue(index)
        }
    }

    #[test]
    fn idempotency_key_is_the_documented_shape() {
        assert_eq!(
            idempotency_key("terraphim", "gitea", 57),
            "gitea:terraphim/gitea#57"
        );
    }

    #[test]
    fn idempotency_key_is_stable_across_calls() {
        // Acceptance criterion 1 rests on this being a pure function of the coordinates:
        // nothing about the poll cycle, the clock or the host may enter it.
        let a = idempotency_key("terraphim", "gitea", 57);
        let b = idempotency_key("terraphim", "gitea", 57);
        assert_eq!(a, b);
        assert_ne!(a, idempotency_key("terraphim", "gitea", 58));
        assert_ne!(a, idempotency_key("other", "gitea", 57));
    }

    #[test]
    fn task_body_carries_a_parsable_trailer() {
        let repo = RepoRef::new("terraphim", "gitea");
        let body = task_body(&repo, &issue(57), "https://git.example.org/");
        let parsed = GiteaRef::parse_from_body(&body).expect("trailer round-trips");
        assert_eq!(parsed, GiteaRef::new("terraphim", "gitea", 57));
        assert!(
            body.contains("https://git.example.org/terraphim/gitea/issues/57"),
            "{body}"
        );
    }

    #[test]
    fn trailer_parsing_rejects_malformed_lines() {
        assert!(GiteaRef::parse_from_body("no trailer here").is_none());
        assert!(GiteaRef::parse_from_body("gitea-ref: terraphim/gitea").is_none());
        assert!(GiteaRef::parse_from_body("gitea-ref: terraphim#57").is_none());
        assert!(GiteaRef::parse_from_body("gitea-ref: /gitea#57").is_none());
        assert!(GiteaRef::parse_from_body("gitea-ref: terraphim/gitea#not-a-number").is_none());
    }

    #[test]
    fn trailer_survives_a_repo_name_containing_a_hash() {
        // rsplit on '#' so a '#' earlier in the slug cannot steal the index.
        let parsed = GiteaRef::parse_from_body("gitea-ref: own/re#po#57").expect("parses");
        assert_eq!(parsed.index, 57);
        assert_eq!(parsed.repo, "re#po");
    }

    #[test]
    fn create_request_pairs_title_and_key() {
        let repo = RepoRef::new("terraphim", "gitea");
        let req = create_request(&repo, &issue(57), "https://git.example.org");
        assert_eq!(req.title, "automations daemon");
        assert_eq!(req.idempotency_key, "gitea:terraphim/gitea#57");
        assert!(req.body.contains("gitea-ref: terraphim/gitea#57"));
    }

    #[test]
    fn report_looks_up_by_index() {
        let report = InboundReport {
            resolved: vec![(57, "t_abc".into()), (58, "t_def".into())],
            ..InboundReport::default()
        };
        assert_eq!(report.task_for(57), Some("t_abc"));
        assert_eq!(report.task_for(99), None);
    }

    #[test]
    fn selection_caps_the_sweep_and_reports_what_it_deferred() {
        // The ready endpoint has no limit and no paging, so without this the per-sweep cost —
        // and every downstream leg's permanent cost — is whatever the board happens to hold.
        let issues: Vec<ReadyIssue> = (1..=300).map(|i| ranked(i, 0.001 * i as f64)).collect();
        let (selected, deferred) = select_ready(&issues, 25);
        assert_eq!(selected.len(), 25);
        assert_eq!(deferred, 275, "the remainder is reported, never silently dropped");
    }

    #[test]
    fn selection_takes_the_highest_page_rank_first() {
        // The endpoint sorts by PageRank descending already; the cap must not depend on that
        // staying true, or "the top N" would be a property of the transport.
        let issues = vec![ranked(1, 0.10), ranked(2, 0.90), ranked(3, 0.50)];
        let (selected, deferred) = select_ready(&issues, 2);
        assert_eq!(selected.iter().map(|i| i.index).collect::<Vec<_>>(), vec![2, 3]);
        assert_eq!(deferred, 1);
    }

    #[test]
    fn selection_is_stable_across_sweeps() {
        // This is what makes the cap bound the *board* rather than one sweep: a rotating
        // window would create a new task every cycle and the set would grow without bound
        // anyway. Equal scores must therefore break the same way every time.
        let issues = vec![ranked(9, 0.5), ranked(3, 0.5), ranked(7, 0.5), ranked(5, 0.5)];
        let first: Vec<i64> = select_ready(&issues, 2).0.iter().map(|i| i.index).collect();
        let shuffled = vec![ranked(5, 0.5), ranked(7, 0.5), ranked(3, 0.5), ranked(9, 0.5)];
        let second: Vec<i64> = select_ready(&shuffled, 2).0.iter().map(|i| i.index).collect();
        assert_eq!(first, vec![3, 5], "index ascending is the documented tie-break");
        assert_eq!(first, second, "the same ready set must select the same issues");
    }

    #[test]
    fn selection_drops_blocked_issues_before_it_counts_them() {
        // A blocked issue must not consume a slot: it can never become a task, so counting it
        // against the cap would starve real work.
        let issues = vec![
            ReadyIssue {
                is_blocked: true,
                ..ranked(1, 0.99)
            },
            ranked(2, 0.50),
            ranked(3, 0.40),
        ];
        let (selected, deferred) = select_ready(&issues, 2);
        assert_eq!(selected.iter().map(|i| i.index).collect::<Vec<_>>(), vec![2, 3]);
        assert_eq!(deferred, 0);
    }

    #[test]
    fn a_nan_page_rank_does_not_panic_the_sweep() {
        // JSON cannot carry NaN today, but `partial_cmp().unwrap()` in a comparator is a
        // panic waiting for the day it can, and a panic here is a dead inbound leg.
        let issues = vec![ranked(1, f64::NAN), ranked(2, 0.5)];
        let (selected, _) = select_ready(&issues, 2);
        assert_eq!(selected.len(), 2);
    }
}
