// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//! Daemon configuration.
//!
//! Everything the bridge needs that it cannot learn from an API. In particular the
//! repository *owner*: `ReadyResponse` carries `repo_id`/`repo_name` but no owner, so the
//! owner is config, never inferred (`ready_graph.go:25-42`).

use std::path::{Path, PathBuf};
use std::time::Duration;

use serde::{Deserialize, Serialize};

/// Default reaction alias used as the human approval signal.
///
/// This is the *alias* for U+1F41D (`modules/emoji/emoji_data.go:191`), which is also the
/// `content` value the reactions API returns — never the raw codepoint. It must also be
/// present in `[ui] REACTIONS` in `app.ini`, otherwise the reaction is not merely
/// unwritable but invisible: writes are rejected by `ReactionsLookup`
/// (`models/issues/reaction.go:223`) and reads filter on the same allowlist (`:165`).
pub const DEFAULT_APPROVAL_REACTION: &str = "honeybee";

/// Label applied to a Gitea issue when its kanban task reaches a non-`completed`
/// terminal state.
pub const DEFAULT_BLOCKED_LABEL: &str = "status/blocked";

/// A single `<owner>/<repo>` the bridge is responsible for.
///
/// Unknown keys are rejected here and in every other config struct — see [`Config`].
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RepoRef {
    /// Repository owner. Config-supplied: the ready endpoint never returns it.
    pub owner: String,
    /// Repository name.
    pub repo: String,
}

impl RepoRef {
    /// Builds a `RepoRef` from its parts.
    pub fn new(owner: impl Into<String>, repo: impl Into<String>) -> Self {
        Self {
            owner: owner.into(),
            repo: repo.into(),
        }
    }

    /// `<owner>/<repo>`, the form used in idempotency keys and body trailers.
    pub fn slug(&self) -> String {
        format!("{}/{}", self.owner, self.repo)
    }
}

/// How to reach the Gitea read API.
///
/// Writes never go through here — they shell out to `gitea-robot`, which holds the NIP-98
/// agent identity (F1). See [`crate::robot`].
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct GiteaConfig {
    /// Base URL, e.g. `https://git.example.org`.
    pub base_url: String,
    /// Optional API token for reads.
    ///
    /// The ready and reactions endpoints allow anonymous reads on public repositories, but
    /// `GET /repos/{o}/{r}/collaborators/{u}/permission` sits behind `reqToken()`
    /// (`routers/api/v1/api.go:1464,1466`). Without a token the write-permission check
    /// cannot run and approval fails closed.
    #[serde(default)]
    pub token: Option<String>,
    /// Per-request timeout in seconds.
    #[serde(default = "default_request_timeout_secs")]
    pub request_timeout_secs: u64,
    /// How many times a 5xx or transport failure is retried before giving up.
    #[serde(default = "default_max_retries")]
    pub max_retries: u32,
    /// Base backoff between retries, in milliseconds. Doubles each attempt.
    #[serde(default = "default_retry_backoff_ms")]
    pub retry_backoff_ms: u64,
}

/// Kanban side: which binary, which board, and the task-shaping defaults.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct KanbanConfig {
    /// `hermes` executable, resolved on `PATH` when not absolute.
    #[serde(default = "default_hermes_binary")]
    pub binary: String,
    /// Board slug. Boards are the isolation unit — each has its own DB and dispatcher.
    #[serde(default)]
    pub board: Option<String>,
    /// Profile tasks are assigned to.
    ///
    /// Multi-box deployment is option (a) from the design: one bridge per host, with
    /// disjoint assignees. Cross-host kanban (option (b)) is a non-goal.
    #[serde(default)]
    pub assignee: Option<String>,
    /// `scratch | worktree | worktree:<path> | dir:<path>`.
    #[serde(default)]
    pub workspace: Option<String>,
    /// Per-task runtime cap, e.g. `90s`, `30m`, `2h`.
    #[serde(default)]
    pub max_runtime: Option<String>,
    /// Per-task override for the consecutive-failure circuit breaker.
    #[serde(default)]
    pub max_retries: Option<u32>,
    /// Author recorded on created tasks.
    #[serde(default = "default_created_by")]
    pub created_by: String,
    /// Tenant namespace, when the board is shared.
    #[serde(default)]
    pub tenant: Option<String>,
    /// Create tasks blocked, so a 🐝 is what releases them to a worker.
    ///
    /// This is what makes the approval leg reachable at all. `hermes kanban create` has no
    /// `--status` flag and defaults to **`ready`** — a created task is immediately
    /// dispatchable — so with this off, an issue reaching the ready endpoint runs an agent
    /// with no human in the loop, and the approval sweep (which covers `blocked` and
    /// `todo`) never sees a bridge-created task. With it on, `--initial-status blocked` is
    /// passed, and [`crate::approval`] is what returns the task to `ready`.
    ///
    /// Turn it off deliberately, for a board where the Gitea triage *is* the approval.
    #[serde(default = "default_true")]
    pub require_approval: bool,
}

/// Write side: the `gitea-robot` CLI that carries the NIP-98 agent identity.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RobotConfig {
    /// `gitea-robot` executable, resolved on `PATH` when not absolute.
    #[serde(default = "default_robot_binary")]
    pub binary: String,
    /// Base branch for opened pull requests.
    #[serde(default = "default_base_branch")]
    pub base_branch: String,
    /// Label applied on a non-`completed` terminal event.
    #[serde(default = "default_blocked_label")]
    pub blocked_label: String,
    /// Open pull requests as drafts.
    #[serde(default)]
    pub draft_pulls: bool,
}

/// Poll cadences. The bridge is a poller by construction on both approval and ready.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PollConfig {
    /// Seconds between `/api/v1/robot/ready` sweeps.
    #[serde(default = "default_ready_interval_secs")]
    pub ready_interval_secs: u64,
    /// Seconds between 🐝 reaction sweeps.
    ///
    /// Reaction polling is the permanent path, not a stopgap: `repoevent.Kind` is exactly
    /// `action, agent_audit, comment, review, status`
    /// (`services/repoevent/event.go:29-37`) — there is no reaction kind, and F2 already
    /// landed (#55).
    #[serde(default = "default_approval_interval_secs")]
    pub approval_interval_secs: u64,
    /// Ask the ready endpoint to drop issues that already have a branch, PR or
    /// `status/in-progress` label.
    #[serde(default = "default_true")]
    pub skip_in_progress: bool,
    /// Seconds between reconciliation sweeps over terminal tasks that were never reported.
    ///
    /// `watch` is a live stream and a terminal event fires once, so a `gitea-robot` failure
    /// — or a daemon that was simply down when the event fired — would otherwise drop that
    /// issue's feedback permanently. The sweep is what makes "a half-applied plan is safe to
    /// replay" reachable rather than merely true.
    #[serde(default = "default_reconcile_interval_secs")]
    pub reconcile_interval_secs: u64,
    /// Most kanban tasks one inbound sweep may create, per repository.
    ///
    /// `GET /api/v1/robot/ready` returns *every* unblocked open issue — no limit, no paging
    /// (`ready_graph.go:192-276`) — so this is the only bound on the daemon's steady-state
    /// cost. Uncapped, a board with 300 open issues means 300 `hermes kanban create`
    /// subprocesses per sweep, 300 tasks swept for a 🐝 every
    /// [`approval_interval_secs`](Self::approval_interval_secs), and 300 `kanban show`
    /// subprocesses per reconcile sweep — permanently, because a task the bridge blocked
    /// leaves its issue "ready" as far as `getInProgressIssues` is concerned.
    ///
    /// Selection is PageRank-descending and deterministic, so the cap bounds the *board* and
    /// not merely one sweep: see [`crate::inbound::select_ready`].
    #[serde(default = "default_max_tasks_per_sweep")]
    pub max_tasks_per_sweep: usize,
}

/// Human-approval configuration.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ApprovalConfig {
    /// Reaction alias that means "a human approved this".
    #[serde(default = "default_approval_reaction")]
    pub reaction: String,
    /// Require the reacting user to hold write permission.
    ///
    /// Leave on. With it off, any reader's 🐝 promotes a task.
    #[serde(default = "default_true")]
    pub require_write_permission: bool,
}

/// Complete daemon configuration.
///
/// Every struct here rejects unknown keys, for the same reason the rules file does
/// (`rules.rs:242`): a typo cannot silently become a default. `kanban:\n  boad: f4` would
/// otherwise parse clean, leave `board` at `None`, drop `--board` from every argv
/// (`hermes.rs:331`) and drive kanban's *default* board — creating, promoting and commenting
/// on the wrong one — while `check` printed `board None`, which reads as absence by choice.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Config {
    /// Gitea read endpoint.
    pub gitea: GiteaConfig,
    /// Repositories this bridge owns.
    pub repos: Vec<RepoRef>,
    /// Kanban side.
    #[serde(default)]
    pub kanban: KanbanConfig,
    /// Gitea write side.
    #[serde(default)]
    pub robot: RobotConfig,
    /// Poll cadences.
    #[serde(default)]
    pub poll: PollConfig,
    /// Approval signal.
    #[serde(default)]
    pub approval: ApprovalConfig,
    /// Optional declarative rules file. See [`crate::rules`].
    #[serde(default)]
    pub rules_file: Option<PathBuf>,
}

/// Configuration load / validation failures.
#[derive(Debug, thiserror::Error)]
pub enum ConfigError {
    /// The file could not be read.
    #[error("cannot read config {path}: {source}")]
    Read {
        /// Path that failed to open.
        path: PathBuf,
        /// Underlying I/O error.
        #[source]
        source: std::io::Error,
    },
    /// The file is not valid YAML, or does not match the schema.
    #[error("cannot parse config {path}: {source}")]
    Parse {
        /// Path that failed to parse.
        path: PathBuf,
        /// Underlying YAML error.
        #[source]
        source: serde_norway::Error,
    },
    /// The config parsed but is not usable.
    #[error("invalid config: {0}")]
    Invalid(String),
}

impl Config {
    /// Loads and validates a YAML config file.
    pub fn load(path: &Path) -> Result<Self, ConfigError> {
        let raw = std::fs::read_to_string(path).map_err(|source| ConfigError::Read {
            path: path.to_path_buf(),
            source,
        })?;
        let cfg: Self = serde_norway::from_str(&raw).map_err(|source| ConfigError::Parse {
            path: path.to_path_buf(),
            source,
        })?;
        cfg.validate()?;
        Ok(cfg)
    }

    /// Rejects configurations that would fail later, at a less obvious place.
    pub fn validate(&self) -> Result<(), ConfigError> {
        if self.gitea.base_url.trim().is_empty() {
            return Err(ConfigError::Invalid("gitea.base_url is required".into()));
        }
        if !self.gitea.base_url.starts_with("http://") && !self.gitea.base_url.starts_with("https://") {
            return Err(ConfigError::Invalid(format!(
                "gitea.base_url must be an http(s) URL, got {:?}",
                self.gitea.base_url
            )));
        }
        if self.repos.is_empty() {
            return Err(ConfigError::Invalid(
                "at least one entry in repos is required".into(),
            ));
        }
        for r in &self.repos {
            if r.owner.trim().is_empty() || r.repo.trim().is_empty() {
                return Err(ConfigError::Invalid(format!(
                    "repos entry {:?} must have a non-empty owner and repo",
                    r.slug()
                )));
            }
        }
        // A slashed base branch cannot be probed for an existing pull request: the route is
        // GET /repos/{o}/{r}/pulls/{base}/{head} and only {head} is a catch-all segment
        // (`routers/api/v1/api.go:1642`). `gitea-robot create-pull` refuses one, so catching
        // it here turns a per-event wedge into a startup error.
        if self.robot.base_branch.trim().is_empty() {
            return Err(ConfigError::Invalid("robot.base_branch is required".into()));
        }
        if self.robot.base_branch.contains('/') {
            return Err(ConfigError::Invalid(format!(
                "robot.base_branch {:?} contains a slash; the pull-request existence probe is \
                 GET /repos/{{o}}/{{r}}/pulls/{{base}}/{{head}}, where only {{head}} is a \
                 catch-all segment (routers/api/v1/api.go:1642), so a slashed base cannot be \
                 probed and every pull request would wedge on Gitea's duplicate check",
                self.robot.base_branch
            )));
        }
        // The label is the first action of every non-`completed` plan, and `gitea-robot
        // edit-issue` fails hard on one the repository does not have — so a label that cannot
        // be applied silences the reason comment behind it. Two spellings never can be, and
        // both are caught here rather than once per terminal event:
        //
        // * empty — `--add-labels` rejects it as a usage error;
        // * containing a comma — `add_labels_args` joins on commas and `splitLabels`
        //   (cmd/gitea-robot/write.go) splits on them, so `needs,triage` is sent as two label
        //   names, neither of which is the configured one.
        //
        // Whether the label *exists* is a question for the server, not for a YAML file:
        // `crate::robot::check_blocked_label` asks it in `check` and at startup.
        if self.robot.blocked_label.trim().is_empty() {
            return Err(ConfigError::Invalid(
                "robot.blocked_label is required: it is the first action of every \
                 blocked/gave_up/crashed/timed_out plan"
                    .into(),
            ));
        }
        if self.robot.blocked_label.contains(',') {
            return Err(ConfigError::Invalid(format!(
                "robot.blocked_label {:?} contains a comma; `gitea-robot edit-issue --add-labels` \
                 takes a comma-separated list and splits on it, so this would be sent as two \
                 label names rather than one",
                self.robot.blocked_label
            )));
        }
        // 0 is not "unlimited" here, deliberately: the ready endpoint has no limit and no
        // paging, so an unlimited sweep's cost is whatever the board happens to hold, forever.
        // An operator who wants more work in flight raises the number.
        if self.poll.max_tasks_per_sweep == 0 {
            return Err(ConfigError::Invalid(
                "poll.max_tasks_per_sweep must be at least 1: it is the only bound on how many \
                 tasks one sweep creates, and /api/v1/robot/ready returns every unblocked open \
                 issue with no limit and no paging"
                    .into(),
            ));
        }
        if self.approval.reaction.trim().is_empty() {
            return Err(ConfigError::Invalid("approval.reaction is required".into()));
        }
        if self
            .approval
            .reaction
            .chars()
            .any(|c| !c.is_ascii_alphanumeric() && c != '_' && c != '-' && c != '+')
        {
            return Err(ConfigError::Invalid(format!(
                "approval.reaction must be an emoji alias such as {DEFAULT_APPROVAL_REACTION:?}, \
                 not a codepoint — got {:?}",
                self.approval.reaction
            )));
        }
        Ok(())
    }

    /// Ready-poll interval as a [`Duration`].
    pub fn ready_interval(&self) -> Duration {
        Duration::from_secs(self.poll.ready_interval_secs.max(1))
    }

    /// Approval-poll interval as a [`Duration`].
    pub fn approval_interval(&self) -> Duration {
        Duration::from_secs(self.poll.approval_interval_secs.max(1))
    }

    /// Reconciliation-sweep interval as a [`Duration`].
    pub fn reconcile_interval(&self) -> Duration {
        Duration::from_secs(self.poll.reconcile_interval_secs.max(1))
    }
}

fn default_request_timeout_secs() -> u64 {
    30
}
fn default_max_retries() -> u32 {
    3
}
fn default_retry_backoff_ms() -> u64 {
    250
}
fn default_hermes_binary() -> String {
    "hermes".into()
}
fn default_robot_binary() -> String {
    "gitea-robot".into()
}
fn default_created_by() -> String {
    "gitea-automations".into()
}
fn default_base_branch() -> String {
    "main".into()
}
fn default_blocked_label() -> String {
    DEFAULT_BLOCKED_LABEL.into()
}
fn default_ready_interval_secs() -> u64 {
    60
}
fn default_reconcile_interval_secs() -> u64 {
    300
}
fn default_max_tasks_per_sweep() -> usize {
    25
}
fn default_approval_interval_secs() -> u64 {
    60
}
fn default_approval_reaction() -> String {
    DEFAULT_APPROVAL_REACTION.into()
}
fn default_true() -> bool {
    true
}

impl Default for GiteaConfig {
    fn default() -> Self {
        Self {
            base_url: String::new(),
            token: None,
            request_timeout_secs: default_request_timeout_secs(),
            max_retries: default_max_retries(),
            retry_backoff_ms: default_retry_backoff_ms(),
        }
    }
}

impl Default for KanbanConfig {
    fn default() -> Self {
        Self {
            binary: default_hermes_binary(),
            board: None,
            assignee: None,
            workspace: None,
            max_runtime: None,
            max_retries: None,
            created_by: default_created_by(),
            tenant: None,
            require_approval: true,
        }
    }
}

impl Default for RobotConfig {
    fn default() -> Self {
        Self {
            binary: default_robot_binary(),
            base_branch: default_base_branch(),
            blocked_label: default_blocked_label(),
            draft_pulls: false,
        }
    }
}

impl Default for PollConfig {
    fn default() -> Self {
        Self {
            ready_interval_secs: default_ready_interval_secs(),
            approval_interval_secs: default_approval_interval_secs(),
            skip_in_progress: true,
            reconcile_interval_secs: default_reconcile_interval_secs(),
            max_tasks_per_sweep: default_max_tasks_per_sweep(),
        }
    }
}

impl Default for ApprovalConfig {
    fn default() -> Self {
        Self {
            reaction: default_approval_reaction(),
            require_write_permission: true,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn minimal() -> &'static str {
        "gitea:\n  base_url: https://git.example.org\nrepos:\n  - owner: terraphim\n    repo: gitea\n"
    }

    #[test]
    fn minimal_config_fills_in_defaults() {
        let cfg: Config = serde_norway::from_str(minimal()).expect("parses");
        cfg.validate().expect("valid");
        assert_eq!(cfg.approval.reaction, DEFAULT_APPROVAL_REACTION);
        assert!(cfg.approval.require_write_permission);
        assert_eq!(cfg.robot.blocked_label, DEFAULT_BLOCKED_LABEL);
        assert_eq!(cfg.kanban.binary, "hermes");
        assert_eq!(cfg.robot.binary, "gitea-robot");
        assert!(cfg.poll.skip_in_progress);
        assert_eq!(cfg.repos[0].slug(), "terraphim/gitea");
        // The approval gate is only reachable because tasks are created blocked: kanban
        // creates `ready` by default, which would dispatch an agent with no human in it.
        assert!(cfg.kanban.require_approval);
        assert_eq!(cfg.reconcile_interval().as_secs(), 300);
        // The ready endpoint is unpaged, so an absent cap must not mean "no cap".
        assert_eq!(cfg.poll.max_tasks_per_sweep, 25);
    }

    #[test]
    fn an_uncapped_sweep_is_rejected() {
        let cfg: Config = serde_norway::from_str(&format!("{}poll:\n  max_tasks_per_sweep: 0\n", minimal()))
            .expect("parses");
        let err = cfg.validate().expect_err("must reject");
        assert!(err.to_string().contains("max_tasks_per_sweep"), "{err}");
    }

    /// A mistyped key must not become a default.
    ///
    /// `boad: f4` is the sharp case: `board` stays `None`, `--board` is dropped from every
    /// argv, and the bridge drives kanban's default board instead of the configured one —
    /// creating, promoting and commenting on the wrong board, with `check` reporting only
    /// `board None`.
    #[test]
    fn an_unknown_key_is_rejected_rather_than_ignored() {
        let appended = [
            "kanban:\n  boad: f4\n",
            "poll:\n  ready_interval_sec: 30\n",
            "approval:\n  reactions: honeybee\n",
            "robot:\n  blocked_labels: status/blocked\n",
            "rules_fil: rules.yaml\n",
        ];
        let whole = [
            // Sections that `minimal()` already spells, so they cannot simply be appended.
            "gitea:\n  base_url: https://git.example.org\n  tokens: abc\nrepos:\n  - owner: a\n    repo: b\n",
            "gitea:\n  base_url: https://git.example.org\nrepos:\n  - owner: a\n    repo: b\n    branch: main\n",
        ];
        for yaml in appended
            .iter()
            .map(|y| format!("{}{y}", minimal()))
            .chain(whole.iter().map(|y| (*y).to_string()))
        {
            let err = serde_norway::from_str::<Config>(&yaml).expect_err("an unknown key must not parse");
            assert!(err.to_string().contains("unknown field"), "{yaml}: {err}");
        }
    }

    /// The shipped example must stay loadable — `deny_unknown_fields` makes a stale key in it
    /// a hard failure for anyone who copies it, and nothing else would catch that.
    #[test]
    fn the_example_config_parses_and_validates() {
        let cfg: Config =
            serde_norway::from_str(include_str!("../bridge.example.yaml")).expect("example parses");
        cfg.validate().expect("example is valid");
    }

    #[test]
    fn a_slashed_base_branch_is_rejected() {
        // Only {head} is a catch-all on GET /pulls/{base}/{head}, so `release/1.0` would
        // make every existence probe ask about a branch that is not the configured base.
        let cfg: Config =
            serde_norway::from_str(&format!("{}robot:\n  base_branch: release/1.0\n", minimal()))
                .expect("parses");
        let err = cfg.validate().expect_err("must reject");
        assert!(err.to_string().contains("release/1.0"), "{err}");
        assert!(err.to_string().contains("catch-all"), "{err}");
    }

    /// A blocked label that cannot be applied silences four of the five terminal kinds:
    /// it is action 0 of their plan, and `edit-issue` fails hard on a label the repository
    /// does not have, so the reason comment behind it is never posted.
    #[test]
    fn a_blocked_label_that_could_never_be_applied_is_rejected() {
        for (yaml, needle) in [
            ("robot:\n  blocked_label: \"\"\n", "required"),
            ("robot:\n  blocked_label: needs,triage\n", "comma"),
        ] {
            let cfg: Config = serde_norway::from_str(&format!("{}{yaml}", minimal())).expect("parses");
            let err = cfg.validate().expect_err("must reject");
            assert!(err.to_string().contains(needle), "{err}");
        }
    }

    #[test]
    fn empty_repos_is_rejected() {
        let cfg: Config = serde_norway::from_str("gitea:\n  base_url: https://git.example.org\nrepos: []\n")
            .expect("parses");
        let err = cfg.validate().expect_err("must reject");
        assert!(err.to_string().contains("repos"), "{err}");
    }

    #[test]
    fn non_http_base_url_is_rejected() {
        let cfg: Config = serde_norway::from_str(
            "gitea:\n  base_url: git.example.org\nrepos:\n  - owner: a\n    repo: b\n",
        )
        .expect("parses");
        assert!(cfg.validate().is_err());
    }

    #[test]
    fn raw_codepoint_reaction_is_rejected() {
        // The reactions API returns the alias, and the ReactionsLookup allowlist stores the
        // alias. A configured codepoint would silently never match.
        let cfg: Config = serde_norway::from_str(
            "gitea:\n  base_url: https://git.example.org\nrepos:\n  - owner: a\n    repo: b\napproval:\n  reaction: \"\u{1f41d}\"\n",
        )
        .expect("parses");
        let err = cfg.validate().expect_err("must reject");
        assert!(err.to_string().contains("alias"), "{err}");
    }

    #[test]
    fn load_reports_the_path_it_could_not_read() {
        let err = Config::load(Path::new("/nonexistent/bridge.yaml")).expect_err("must fail");
        assert!(err.to_string().contains("/nonexistent/bridge.yaml"), "{err}");
    }

    #[test]
    fn load_round_trips_a_file() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("bridge.yaml");
        std::fs::write(&path, minimal()).expect("write");
        let cfg = Config::load(&path).expect("loads");
        assert_eq!(cfg.repos.len(), 1);
        assert_eq!(cfg.ready_interval().as_secs(), 60);
    }
}
