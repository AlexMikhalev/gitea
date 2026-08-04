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

use crate::config::RobotConfig;
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
