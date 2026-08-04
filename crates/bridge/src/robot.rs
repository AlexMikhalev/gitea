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
//!
//! What that argv does *not* carry is where the write goes and who it is signed by: both are
//! environment variables of the child process ([`URL_VAR`], [`CREDENTIAL_VARS`]). Left to
//! inheritance they are ambient state nothing validates, and both failures are silent in the
//! direction that matters — a missing credential fails every write while both other preflights
//! stay green, and an unset `GITEA_URL` sends the writes to `gitea-robot`'s own
//! `http://localhost:3000` default while the reads keep coming from the configured instance.
//! So [`Robot`] names them, resolves them once, spawns every child with them explicitly, and
//! [`check_write_leg`] probes the result before the daemon does any work.

use crate::config::{RepoRef, RobotConfig};
use crate::gitea::GiteaClient;
use crate::hermes::{EnvVar, KanbanError, run_argv_env};

/// The variable `gitea-robot` reads its instance URL from (`cmd/gitea-robot/main.go:30`).
pub const URL_VAR: &str = "GITEA_URL";

/// The credentials `gitea-robot` accepts, in the order it prefers them.
///
/// `main()` exits 1 before it dispatches unless one of these is set
/// (`cmd/gitea-robot/main.go:165-168`), and `setRequestAuth` (`:64-75`) signs with the Nostr
/// key whenever it has one, falling back to the token — so this order is the CLI's, not a
/// preference of the bridge's. The Nostr key is the identity F1 exists for: a write carrying it
/// is attributable to the agent key and revocable with it.
pub const CREDENTIAL_VARS: [&str; 2] = ["GITEA_NOSTR_KEY", "GITEA_TOKEN"];

/// The argv [`check_write_leg`] probes with: a verb `gitea-robot` deliberately does not have.
///
/// The probe has to reach the dispatch table to prove the credential check passed, and every
/// real verb makes a request. An unknown command does neither: `main()` validates the
/// credential first and only then fails with `Unknown command:`
/// (`cmd/gitea-robot/main.go:178-186`), so the two answers are distinguishable and neither
/// touches the network. `TestBridgePreflightProbeStaysDistinguishable`
/// (`cmd/gitea-robot/write_test.go`) is what keeps this name from becoming a real verb, and
/// what pins the two messages `classify_preflight` reads.
pub const PREFLIGHT_VERB: &str = "gitea-automations-preflight";

/// Handle on the `gitea-robot` CLI.
#[derive(Debug, Clone)]
pub struct Robot {
    binary: String,
    base_url: String,
    credential: Option<RobotCredential>,
    base_branch: String,
    blocked_label: String,
    draft_pulls: bool,
    wip_prefix: String,
}

/// The credential the daemon hands `gitea-robot`, captured once at startup.
///
/// `Debug` is hand-written: this is a bearer token or a Nostr secret key, and [`Robot`] is
/// `Debug`-derived and reachable from error paths that log.
#[derive(Clone, PartialEq, Eq)]
pub struct RobotCredential {
    name: &'static str,
    value: String,
}

impl std::fmt::Debug for RobotCredential {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "RobotCredential({}: <redacted>)", self.name)
    }
}

impl RobotCredential {
    /// Builds one explicitly. `name` must be one of [`CREDENTIAL_VARS`].
    pub fn new(name: &'static str, value: impl Into<String>) -> Self {
        debug_assert!(
            CREDENTIAL_VARS.contains(&name),
            "{name} is not a credential variable"
        );
        Self {
            name,
            value: value.into(),
        }
    }

    /// The first of [`CREDENTIAL_VARS`] this process has a non-empty value for.
    ///
    /// Empty is treated as absent because `gitea-robot` treats it that way too — it compares
    /// against `""` — so an `Environment=GITEA_TOKEN=` in a unit file must not read as
    /// "configured" here and "missing" there.
    pub fn from_env() -> Option<Self> {
        CREDENTIAL_VARS.iter().find_map(|name| {
            let value = std::env::var(name).ok()?;
            (!value.trim().is_empty()).then(|| Self::new(name, value))
        })
    }

    /// Which variable this came from. Never the value.
    pub fn name(&self) -> &'static str {
        self.name
    }
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
    /// Builds a handle from config, taking the write credential from this process's
    /// environment ([`RobotCredential::from_env`]).
    ///
    /// `base_url` is the instance every write goes to; it is a parameter rather than a field of
    /// [`RobotConfig`] because its default is `gitea.base_url` — reads and writes address one
    /// instance unless an operator says otherwise. See [`crate::config::Config::robot_base_url`].
    pub fn new(cfg: &RobotConfig, base_url: &str) -> Self {
        Self::with_credential(cfg, base_url, RobotCredential::from_env())
    }

    /// [`Robot::new`] with the credential supplied rather than read from the environment.
    ///
    /// The seam the preflight tests need: "no credential" cannot be arranged by unsetting a
    /// variable in a test process (it is racy, and `std::env::remove_var` is unsafe), and the
    /// whole point of `Robot::env` is that the child's view does not depend on the ambient
    /// one anyway.
    pub fn with_credential(cfg: &RobotConfig, base_url: &str, credential: Option<RobotCredential>) -> Self {
        Self {
            binary: cfg.binary.clone(),
            base_url: base_url.trim().to_string(),
            credential,
            base_branch: cfg.base_branch.clone(),
            blocked_label: cfg.blocked_label.clone(),
            draft_pulls: cfg.draft_pulls,
            wip_prefix: cfg.wip_prefix.clone(),
        }
    }

    /// The executable this handle runs.
    pub fn binary(&self) -> &str {
        &self.binary
    }

    /// The instance every write from this handle goes to.
    pub fn base_url(&self) -> &str {
        &self.base_url
    }

    /// Which credential variable the child is given, if any. Never its value.
    pub fn credential_var(&self) -> Option<&'static str> {
        self.credential.as_ref().map(RobotCredential::name)
    }

    /// The environment overlay every `gitea-robot` child is spawned with.
    ///
    /// Two decisions, both of them the R9 P1:
    ///
    /// * [`URL_VAR`] is **set**, always. Inherited, an unset one leaves `gitea-robot` on its
    ///   documented `http://localhost:3000` default (`cmd/gitea-robot/main.go:152-154`), so a
    ///   box with a dev Gitea on that port reads from the configured instance and writes —
    ///   successfully, with the agent's identity, and with no diagnostic anywhere, because the
    ///   request worked — to the local one. Setting it also makes the NIP-98 signature's URL
    ///   the configured one: the signature commits to the absolute URL, so a `GITEA_URL` that
    ///   is not the server's `ROOT_URL` is rejected as an opaque 401.
    /// * every [`CREDENTIAL_VARS`] entry the daemon did *not* resolve is **removed**. The
    ///   child's identity is then exactly the one [`check_write_leg`] validated, rather than
    ///   whatever the unit file, the shell or a developer's dotfiles happened to export into
    ///   this process.
    fn env(&self) -> Vec<EnvVar<'_>> {
        let mut env = vec![(URL_VAR, Some(self.base_url.as_str()))];
        for name in CREDENTIAL_VARS {
            let value = self
                .credential
                .as_ref()
                .filter(|c| c.name == name)
                .map(|c| c.value.as_str());
            env.push((name, value));
        }
        env
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
    ///
    /// `--draft` is always paired with `--wip-prefix`, never sent alone. Gitea has no `draft`
    /// field on `CreatePullRequestOption` — draft-ness is inferred from the title prefix,
    /// matched against `PULL_REQUEST.WORK_IN_PROGRESS_PREFIXES` — and neither the bridge nor
    /// `gitea-robot` can read the instance's `app.ini`. Sending `--draft` on its own means
    /// accepting the CLI's default prefix, which on an instance that customised the setting is
    /// not a work-in-progress marker at all: the pull request opens as an ordinary,
    /// immediately-reviewable one while the config says draft, and the call succeeds, so
    /// nothing anywhere reports it. [`RobotConfig::wip_prefix`] is the escape hatch, and
    /// passing it unconditionally is what makes it reachable.
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
            argv.push("--wip-prefix".into());
            argv.push(self.wip_prefix.clone());
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
        run_argv_env(
            &self.binary,
            &self.comment_args(owner, repo, index, body),
            &self.env(),
        )
        .await
    }

    /// Adds labels to an issue, leaving existing labels in place.
    pub async fn add_labels(
        &self,
        owner: &str,
        repo: &str,
        index: i64,
        labels: &[String],
    ) -> Result<String, KanbanError> {
        run_argv_env(
            &self.binary,
            &self.add_labels_args(owner, repo, index, labels),
            &self.env(),
        )
        .await
    }

    /// Opens a pull request.
    pub async fn create_pull(
        &self,
        owner: &str,
        repo: &str,
        pr: &PullRequest,
    ) -> Result<String, KanbanError> {
        run_argv_env(&self.binary, &self.create_pull_args(owner, repo, pr), &self.env()).await
    }

    /// Runs [`PREFLIGHT_VERB`] with the environment a real write would get.
    ///
    /// Split from [`check_write_leg`] so the classification is testable without a binary.
    async fn preflight_probe(&self) -> Result<String, KanbanError> {
        run_argv_env(&self.binary, &[PREFLIGHT_VERB.to_string()], &self.env()).await
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

/// What the write-leg preflight concluded.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum WriteCheck {
    /// `gitea-robot` ran, accepted its environment, and reached its dispatch table.
    Ready {
        /// What was verified, including where writes go and which credential carries them.
        reason: String,
    },
    /// It will not write: no binary, or no credential.
    Broken {
        /// Human-readable explanation, naming the fix.
        reason: String,
    },
    /// The probe could not be made, or answered something unrecognised.
    Unknown {
        /// Why there is no answer either way.
        reason: String,
    },
}

impl WriteCheck {
    /// Whether the write leg is known to be usable.
    pub fn is_ready(&self) -> bool {
        matches!(self, Self::Ready { .. })
    }

    /// The explanation, whichever variant this is.
    pub fn reason(&self) -> &str {
        match self {
            Self::Ready { reason } | Self::Broken { reason } | Self::Unknown { reason } => reason,
        }
    }
}

/// Probes the write leg before it is needed: the binary, its credential, and its target.
///
/// This is the other half of [`check_blocked_label`], and it exists for the same reason. Every
/// Gitea write the daemon makes is a `gitea-robot` subprocess, and until it is spawned neither
/// of the two things it needs is checked anywhere: `main()` exits 1 with *"GITEA_TOKEN or
/// GITEA_NOSTR_KEY environment variable required"* **before** the dispatch table
/// (`cmd/gitea-robot/main.go:165-168`), and `giteaURL` falls back to `http://localhost:3000`
/// (`:152-154`). Undetected, the first is a daemon whose every terminal event fails at action 0
/// — the escalation comment included, since it goes through the same binary — while `check`
/// prints two green lines and the issue stays as silent as one nobody picked up.
///
/// `Robot::env` closes the second failure outright by setting [`URL_VAR`] on every child, so
/// what is probed here is the first: that the binary exists, is this CLI, and got past its own
/// credential check with the environment a real write will be given.
///
/// No request is made. [`PREFLIGHT_VERB`] is not a verb, so `main()` validates the credential
/// and then refuses to dispatch — which is precisely the pair of answers this needs.
pub async fn check_write_leg(robot: &Robot) -> WriteCheck {
    classify_preflight(robot, robot.preflight_probe().await)
}

/// Turns the probe's outcome into a verdict. Separated so every branch is testable.
fn classify_preflight(robot: &Robot, outcome: Result<String, KanbanError>) -> WriteCheck {
    let target = format!(
        "writes go to {} ({URL_VAR}, set explicitly on every call)",
        robot.base_url()
    );
    let credential = match robot.credential_var() {
        Some(name) => format!("signed with {name}"),
        None => format!(
            "no credential: none of {} is set in this process's environment — set one in the \
             daemon's unit file (`Environment=`), because the bridge passes exactly what it \
             resolved and removes the rest",
            CREDENTIAL_VARS.join(" or ")
        ),
    };
    match outcome {
        // A binary that accepts a command it cannot have is not the CLI this contract is with,
        // so nothing here can be concluded from its exit code either way.
        Ok(_) => WriteCheck::Unknown {
            reason: format!(
                "{} exited 0 for {PREFLIGHT_VERB:?}, which is not one of its commands; this may \
                 not be gitea-robot. {target}, {credential}",
                robot.binary()
            ),
        },
        Err(KanbanError::Spawn { binary, source }) => WriteCheck::Broken {
            reason: format!(
                "cannot run robot.binary {binary:?}: {source}. Every comment, label and pull \
                 request the bridge produces is a subprocess of it, so no terminal event will \
                 reach gitea until it is on PATH"
            ),
        },
        Err(KanbanError::Exit { stderr, code, .. }) => {
            // Ordered credential-first: the credential check runs before dispatch, so a build
            // that ever printed both would still be reported as the failure that comes first.
            if stderr.contains("environment variable required") {
                WriteCheck::Broken {
                    reason: format!(
                        "{} refuses to start: {credential}. Until it is set, every \
                         comment/edit-issue/create-pull fails at action 0 — including the \
                         escalation comment, which goes through the same binary, so the issue \
                         stays silent about work that ran and stopped",
                        robot.binary()
                    ),
                }
            } else if stderr.contains("Unknown command") {
                WriteCheck::Ready {
                    reason: format!("{}, {credential}", capitalise_first(&target)),
                }
            } else {
                WriteCheck::Unknown {
                    reason: format!(
                        "the write-leg probe `{} {PREFLIGHT_VERB}` exited {code} with an \
                         unrecognised message: {stderr}. {target}, {credential}",
                        robot.binary()
                    ),
                }
            }
        }
        Err(err) => WriteCheck::Unknown {
            reason: format!("the write-leg probe failed: {err}. {target}, {credential}"),
        },
    }
}

/// Uppercases the first character, so a reason reads as a sentence wherever it is printed.
fn capitalise_first(s: &str) -> String {
    let mut chars = s.chars();
    match chars.next() {
        Some(first) => first.to_uppercase().chain(chars).collect(),
        None => String::new(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const TEST_URL: &str = "https://git.example.org";

    fn robot() -> Robot {
        Robot::new(&RobotConfig::default(), TEST_URL)
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

    /// `--draft` alone is a no-op on an instance that changed
    /// `PULL_REQUEST.WORK_IN_PROGRESS_PREFIXES`: Gitea reads draft-ness off the title prefix,
    /// and neither binary can read `app.ini`. So the prefix travels with the flag.
    #[test]
    fn draft_flag_is_opt_in_and_carries_the_wip_prefix() {
        let pr = PullRequest {
            title: "t".into(),
            head: "h".into(),
            body: "Refs #1".into(),
        };
        let r = Robot::new(
            &RobotConfig {
                draft_pulls: true,
                ..RobotConfig::default()
            },
            TEST_URL,
        );
        let argv = r.create_pull_args("o", "r", &pr);
        assert!(argv.contains(&"--draft".to_string()));
        let i = argv
            .iter()
            .position(|a| a == "--wip-prefix")
            .expect("--draft must never travel alone: {argv:?}");
        assert_eq!(argv[i + 1], crate::config::DEFAULT_WIP_PREFIX);

        // …and a customised instance can actually reach it, which is the point.
        let custom = Robot::new(
            &RobotConfig {
                draft_pulls: true,
                wip_prefix: "[DRAFT]".into(),
                ..RobotConfig::default()
            },
            TEST_URL,
        );
        let argv = custom.create_pull_args("o", "r", &pr);
        let i = argv.iter().position(|a| a == "--wip-prefix").expect("prefix");
        assert_eq!(argv[i + 1], "[DRAFT]");

        // Off, neither flag appears — the prefix is meaningless without the draft flag.
        let plain = Robot::new(&RobotConfig::default(), TEST_URL);
        let argv = plain.create_pull_args("o", "r", &pr);
        assert!(
            !argv.iter().any(|a| a == "--draft" || a == "--wip-prefix"),
            "{argv:?}"
        );
    }

    /// The R9 P1, as the two variables it is about.
    ///
    /// The argv says nothing about where a write goes or who signs it; the child's environment
    /// does. So `GITEA_URL` is always set — inherited-and-unset means `gitea-robot`'s own
    /// `http://localhost:3000`, i.e. reads from the configured instance and writes to whatever
    /// is listening locally, successfully and silently — and a credential variable the daemon
    /// did not resolve is *removed*, so the write leg's identity cannot be ambient.
    #[test]
    fn every_write_carries_its_instance_and_only_the_resolved_credential() {
        let signing = Robot::with_credential(
            &RobotConfig::default(),
            TEST_URL,
            Some(RobotCredential::new("GITEA_NOSTR_KEY", "nsec1secret")),
        );
        let env = signing.env();
        assert_eq!(env[0], (URL_VAR, Some(TEST_URL)), "{env:?}");
        assert_eq!(env[1], ("GITEA_NOSTR_KEY", Some("nsec1secret")), "{env:?}");
        assert_eq!(
            env[2],
            ("GITEA_TOKEN", None),
            "an unresolved credential must be removed, not inherited: {env:?}"
        );

        // With none resolved, both are removed: the child fails its own credential check, which
        // is what `check_write_leg` reports, rather than picking up an identity from the
        // environment the daemon happens to have been started with.
        let uncredentialed = Robot::with_credential(&RobotConfig::default(), TEST_URL, None);
        let bare = uncredentialed.env();
        for name in CREDENTIAL_VARS {
            assert_eq!(
                bare.iter().find(|(n, _)| *n == name),
                Some(&(name, None)),
                "{bare:?}"
            );
        }
        assert_eq!(bare[0], (URL_VAR, Some(TEST_URL)));
    }

    /// A credential is never printed, only named — `Robot` is `Debug`-derived and logged.
    #[test]
    fn the_credential_is_redacted_in_debug_output() {
        let robot = Robot::with_credential(
            &RobotConfig::default(),
            TEST_URL,
            Some(RobotCredential::new("GITEA_TOKEN", "s3cr3t-token-value")),
        );
        let rendered = format!("{robot:?}");
        assert!(!rendered.contains("s3cr3t"), "{rendered}");
        assert!(rendered.contains("GITEA_TOKEN"), "{rendered}");
        assert_eq!(robot.credential_var(), Some("GITEA_TOKEN"));
    }

    /// Every answer the probe can give, including the two that are the whole point: a missing
    /// binary and a missing credential are `Broken`, and only reaching the dispatch table
    /// (`Unknown command:`) is `Ready`.
    #[test]
    fn the_write_preflight_reads_the_robots_own_refusals() {
        let exit = |stderr: &str| KanbanError::Exit {
            binary: "gitea-robot".into(),
            args: PREFLIGHT_VERB.into(),
            code: 1,
            stderr: stderr.into(),
        };
        let with_token = Robot::with_credential(
            &RobotConfig::default(),
            TEST_URL,
            Some(RobotCredential::new("GITEA_TOKEN", "t")),
        );
        let without = Robot::with_credential(&RobotConfig::default(), TEST_URL, None);

        let ready = classify_preflight(
            &with_token,
            Err(exit("Unknown command: gitea-automations-preflight")),
        );
        assert!(ready.is_ready(), "{ready:?}");
        assert!(ready.reason().contains(TEST_URL), "{ready:?}");
        assert!(ready.reason().contains("GITEA_TOKEN"), "{ready:?}");

        let missing = classify_preflight(
            &without,
            Err(exit(
                "Error: GITEA_TOKEN or GITEA_NOSTR_KEY environment variable required",
            )),
        );
        assert!(matches!(missing, WriteCheck::Broken { .. }), "{missing:?}");
        assert!(missing.reason().contains("escalation"), "{missing:?}");

        let absent = classify_preflight(
            &without,
            Err(KanbanError::Spawn {
                binary: "gitea-robot".into(),
                source: std::io::Error::from(std::io::ErrorKind::NotFound),
            }),
        );
        assert!(matches!(absent, WriteCheck::Broken { .. }), "{absent:?}");

        // Neither an unrecognised failure nor a binary that accepts a command it cannot have
        // is an answer: `check` must say it could not tell rather than print a green line.
        for outcome in [Ok("ok\n".to_string()), Err(exit("segmentation fault"))] {
            let unknown = classify_preflight(&with_token, outcome);
            assert!(matches!(unknown, WriteCheck::Unknown { .. }), "{unknown:?}");
            assert!(!unknown.is_ready());
        }
    }

    /// The classification above, reached through a real spawn rather than a synthesised error.
    ///
    /// A `robot.binary` that is not there is the cheapest way to have a daemon whose two other
    /// preflights are green and which cannot report anything at all, and it is the one branch
    /// of the probe that needs no `gitea-robot` to exercise — so it runs in CI, unlike
    /// `robot_cli_contract.rs`.
    #[tokio::test]
    async fn a_write_leg_with_no_binary_is_reported_before_any_work_is_done() {
        let robot = Robot::new(
            &RobotConfig {
                binary: "gitea-automations-no-such-binary".into(),
                ..RobotConfig::default()
            },
            TEST_URL,
        );
        let check = check_write_leg(&robot).await;
        assert!(matches!(check, WriteCheck::Broken { .. }), "{check:?}");
        assert!(
            check.reason().contains("gitea-automations-no-such-binary"),
            "{check:?}"
        );
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
