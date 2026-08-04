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

use crate::config::RepoRef;
use crate::gitea::{GiteaClient, GiteaError, ReadyIssue};
use crate::hermes::{CreateTask, Kanban, KanbanError};

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
pub async fn poll_once(
    gitea: &GiteaClient,
    kanban: &Kanban,
    repo: &RepoRef,
    base_url: &str,
    skip_in_progress: bool,
) -> Result<InboundReport, InboundError> {
    let ready = gitea.ready(&repo.owner, &repo.repo, skip_in_progress).await?;
    let mut report = InboundReport::default();
    for issue in &ready.ready_issues {
        // `is_blocked` should already be false here, but the endpoint is the source of
        // truth for readiness and we do not re-derive it — we just refuse to run backwards.
        if issue.is_blocked {
            tracing::debug!(
                index = issue.index,
                "skipping issue the ready endpoint marked blocked"
            );
            continue;
        }
        let req = create_request(repo, issue, base_url);
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
            failed: vec![],
        };
        assert_eq!(report.task_for(57), Some("t_abc"));
        assert_eq!(report.task_for(99), None);
    }
}
