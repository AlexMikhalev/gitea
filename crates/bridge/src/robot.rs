// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//! Gitea write side, via the shipped `gitea-robot` CLI (F1).
//!
//! Writes are not made with a bare token: `gitea-robot` holds the NIP-98 agent identity, so
//! every comment, label and pull request the bridge produces is attributable to the agent
//! key and revocable with it. The bridge shells out rather than reimplementing NIP-98.
//!
//! The three verbs used here — `comment`, `edit-issue --add-labels`, `create-pull` — are
//! implemented in `cmd/gitea-robot/write.go` and dispatched from the `commands` table in
//! `cmd/gitea-robot/main.go`. The argv tests below can only check that this module agrees
//! with itself; `TestBridgeWriteVerbsExist` in `cmd/gitea-robot/write_test.go` is the half
//! that checks the verbs and flags actually exist on the other side.
//!
//! As everywhere in this crate, invocation is by argv — never `sh -c`.

use crate::config::{RepoRef, RobotConfig};
use crate::gitea::GiteaClient;
use crate::hermes::{KanbanError, run_argv};

/// Handle on the `gitea-robot` CLI.
#[derive(Debug, Clone)]
pub struct Robot {
    binary: String,
    base_branch: String,
    blocked_label: String,
    draft_pulls: bool,
}

/// A pull request the bridge wants opened.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PullRequest {
    /// PR title.
    pub title: String,
    /// Source branch.
    pub head: String,
    /// PR body. Must contain `Refs #<index>` so the PR links back to its issue.
    pub body: String,
}

impl Robot {
    /// Builds a handle from config.
    pub fn new(cfg: &RobotConfig) -> Self {
        Self {
            binary: cfg.binary.clone(),
            base_branch: cfg.base_branch.clone(),
            blocked_label: cfg.blocked_label.clone(),
            draft_pulls: cfg.draft_pulls,
        }
    }

    /// The executable this handle runs.
    pub fn binary(&self) -> &str {
        &self.binary
    }

    /// The label applied on a non-`completed` terminal event.
    pub fn blocked_label(&self) -> &str {
        &self.blocked_label
    }

    /// Builds the argv for `gitea-robot comment`.
    pub fn comment_args(&self, owner: &str, repo: &str, index: i64, body: &str) -> Vec<String> {
        vec![
            "comment".into(),
            "--owner".into(),
            owner.into(),
            "--repo".into(),
            repo.into(),
            "--issue".into(),
            index.to_string(),
            "--body".into(),
            body.into(),
        ]
    }

    /// Builds the argv for `gitea-robot edit-issue --add-labels`.
    ///
    /// The design writes this as `--labels`; the flag is `--add-labels`
    /// (`cmd/gitea-robot/write.go`, `editIssueFlagSet`) because the operation must be
    /// additive — applying `status/blocked` must leave a human's labels alone. It posts to
    /// `POST /repos/{o}/{r}/issues/{index}/labels`, which skips labels the issue already
    /// carries (`models/issues/issue_label.go:125-130`), so a replay adds nothing twice.
    pub fn add_labels_args(&self, owner: &str, repo: &str, index: i64, labels: &[String]) -> Vec<String> {
        vec![
            "edit-issue".into(),
            "--owner".into(),
            owner.into(),
            "--repo".into(),
            repo.into(),
            "--issue".into(),
            index.to_string(),
            "--add-labels".into(),
            labels.join(","),
        ]
    }

    /// Builds the argv for `gitea-robot create-pull`.
    ///
    /// The verb is idempotent: it probes `GET /repos/{o}/{r}/pulls/{base}/{head}` first and
    /// reports an existing pull request as success (`cmd/gitea-robot/write.go`,
    /// `runCreatePull`). That is what makes retrying a partly-applied `completed` plan
    /// safe — see `apply_event` in `main.rs`.
    pub fn create_pull_args(&self, owner: &str, repo: &str, pr: &PullRequest) -> Vec<String> {
        let mut argv = vec![
            "create-pull".into(),
            "--owner".into(),
            owner.into(),
            "--repo".into(),
            repo.into(),
            "--title".into(),
            pr.title.clone(),
            "--head".into(),
            pr.head.clone(),
            "--base".into(),
            self.base_branch.clone(),
            "--body".into(),
            pr.body.clone(),
        ];
        if self.draft_pulls {
            argv.push("--draft".into());
        }
        argv
    }

    /// Posts an issue comment.
    pub async fn comment(
        &self,
        owner: &str,
        repo: &str,
        index: i64,
        body: &str,
    ) -> Result<String, KanbanError> {
        run_argv(&self.binary, &self.comment_args(owner, repo, index, body)).await
    }

    /// Adds labels to an issue, leaving existing labels in place.
    pub async fn add_labels(
        &self,
        owner: &str,
        repo: &str,
        index: i64,
        labels: &[String],
    ) -> Result<String, KanbanError> {
        run_argv(&self.binary, &self.add_labels_args(owner, repo, index, labels)).await
    }

    /// Opens a pull request.
    pub async fn create_pull(
        &self,
        owner: &str,
        repo: &str,
        pr: &PullRequest,
    ) -> Result<String, KanbanError> {
        run_argv(&self.binary, &self.create_pull_args(owner, repo, pr)).await
    }
}

/// What the blocked-label preflight concluded.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum LabelCheck {
    /// The label exists in every configured repository.
    Present,
    /// It is missing from at least one, where four of the five terminal kinds will fail.
    Missing {
        /// Human-readable explanation, naming the repositories and how to fix it.
        reason: String,
    },
    /// The check could not be made — no answer either way.
    Unknown {
        /// Why it could not be made.
        reason: String,
    },
}

impl LabelCheck {
    /// Whether the label is known to exist everywhere it is needed.
    pub fn is_present(&self) -> bool {
        matches!(self, Self::Present)
    }

    /// The explanation, whichever variant this is.
    pub fn reason(&self) -> &str {
        match self {
            Self::Present => "the blocked label exists in every configured repository",
            Self::Missing { reason } | Self::Unknown { reason } => reason,
        }
    }
}

/// Probes whether [`RobotConfig::blocked_label`] actually exists in each configured repo.
///
/// A label name Gitea cannot resolve is not applied and not reported: `POST
/// /issues/{index}/labels` answers 200 having silently dropped it, because
/// `GetLabelIDsInRepoByNames` returns only what it found (`models/issues/label.go:333-341`).
/// `gitea-robot edit-issue` turns that into a hard error by re-reading the issue's label set
/// (`cmd/gitea-robot/write.go`) — which is right, but it means a missing label fails the
/// *first* action of every `blocked`/`gave_up`/`crashed`/`timed_out` plan, so the reason
/// comment never posts and the issue stays completely silent about work that ran and stopped.
///
/// Nothing in the bridge creates the label — creating repository labels is not one of the
/// five allowlisted actions — so the only defence is to say so before the fact, here and in
/// `check`, rather than once per sweep in a log nobody is reading.
pub async fn check_blocked_label(gitea: &GiteaClient, label: &str, repos: &[RepoRef]) -> LabelCheck {
    let want = label.trim();
    if want.is_empty() {
        return LabelCheck::Unknown {
            reason: "robot.blocked_label is empty".into(),
        };
    }
    let (mut missing, mut case_variants) = (Vec::new(), Vec::new());
    for repo in repos {
        let labels = match gitea.repo_labels(&repo.owner, &repo.repo).await {
            Ok(labels) => labels,
            Err(err) => {
                return LabelCheck::Unknown {
                    reason: format!("cannot list the labels of {}: {err}", repo.slug()),
                };
            }
        };
        if labels.iter().any(|l| l.name.trim() == want) {
            continue;
        }
        // Reported apart from "absent" because it is a different fix and an easy one to
        // stare past: `IN (name)` is case-sensitive on sqlite and Postgres, so `Status/Blocked`
        // is simply not `status/blocked` there, however much it looks like it.
        if let Some(found) = labels.iter().find(|l| l.name.trim().eq_ignore_ascii_case(want)) {
            case_variants.push(format!("{} has {:?}", repo.slug(), found.name.trim()));
        }
        missing.push(repo.slug());
    }
    if missing.is_empty() {
        return LabelCheck::Present;
    }
    let mut reason = format!(
        "robot.blocked_label {want:?} does not exist in {} — a label Gitea cannot resolve is \
         dropped from POST /issues/{{index}}/labels in silence (models/issues/label.go:333-341), \
         so every blocked/gave_up/crashed/timed_out task will fail at its first action and its \
         reason comment will never be posted. Create the label in each repository",
        missing.join(", ")
    );
    if !case_variants.is_empty() {
        reason.push_str(&format!(
            "; note the case: {} (label lookup is a SQL IN, which is case-sensitive on sqlite \
             and Postgres)",
            case_variants.join(", ")
        ));
    }
    LabelCheck::Missing { reason }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn robot() -> Robot {
        Robot::new(&RobotConfig::default())
    }

    #[test]
    fn comment_args_target_the_issue_index() {
        let argv = robot().comment_args("terraphim", "gitea", 57, "audit note");
        assert_eq!(
            argv,
            vec![
                "comment",
                "--owner",
                "terraphim",
                "--repo",
                "gitea",
                "--issue",
                "57",
                "--body",
                "audit note"
            ]
        );
    }

    #[test]
    fn add_labels_args_are_additive() {
        let argv = robot().add_labels_args("terraphim", "gitea", 57, &["status/blocked".to_string()]);
        assert!(argv.contains(&"--add-labels".to_string()));
        assert!(argv.contains(&"status/blocked".to_string()));
        assert!(
            !argv.contains(&"--labels".to_string()),
            "must not clobber the label set"
        );
    }

    #[test]
    fn create_pull_args_include_base_and_body() {
        let argv = robot().create_pull_args(
            "terraphim",
            "gitea",
            &PullRequest {
                title: "issue #57: automations daemon".into(),
                head: "task/57-automations-daemon".into(),
                body: "Refs #57".into(),
            },
        );
        let joined = argv.join(" ");
        assert!(joined.contains("--head task/57-automations-daemon"), "{joined}");
        assert!(joined.contains("--base main"), "{joined}");
        assert!(joined.contains("Refs #57"), "{joined}");
        assert!(!argv.contains(&"--draft".to_string()));
    }

    #[test]
    fn draft_flag_is_opt_in() {
        let r = Robot::new(&RobotConfig {
            draft_pulls: true,
            ..RobotConfig::default()
        });
        let argv = r.create_pull_args(
            "o",
            "r",
            &PullRequest {
                title: "t".into(),
                head: "h".into(),
                body: "Refs #1".into(),
            },
        );
        assert!(argv.contains(&"--draft".to_string()));
    }

    #[test]
    fn shell_metacharacters_stay_data() {
        let argv = robot().comment_args("o", "r", 1, "; rm -rf / #$(id)");
        assert_eq!(argv[0], "comment");
        assert_eq!(argv.last().expect("body"), "; rm -rf / #$(id)");
        for a in &argv {
            assert_ne!(a, "-c", "no shell invocation is ever constructed");
        }
    }
}
