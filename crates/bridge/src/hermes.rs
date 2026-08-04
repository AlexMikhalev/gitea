// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//! Hermes kanban wrapper.
//!
//! The execution fabric is inherited, not rebuilt: atomic claims, heartbeats, crash
//! reclaim, the circuit breaker, isolated workspaces and the `task_events` audit all live
//! in kanban. This module only *speaks* to it.
//!
//! Every invocation is an `argv` vector handed to [`tokio::process::Command`]. There is no
//! `sh -c` anywhere in this crate, so no argument can turn into shell syntax; the argv
//! builders are pure functions and [`tests`](self) asserts the property directly.

use std::collections::BTreeMap;
use std::process::Stdio;

use serde::{Deserialize, Deserializer, Serialize};
use tokio::io::{AsyncBufReadExt, BufReader, Lines};
use tokio::process::{ChildStdout, Command};

use crate::config::KanbanConfig;

/// Deserializes a field that may be present-but-`null` into its default.
///
/// `#[serde(default)]` covers an *absent* field only; an explicit `null` is still a type
/// error. kanban emits both — an `unblocked` event is recorded as
/// `{"kind":"unblocked","payload":null}` — and that one null used to fail the whole `show`
/// decode, which would have left every task the approval leg released unreportable.
fn null_as_default<'de, D, T>(de: D) -> Result<T, D::Error>
where
    D: Deserializer<'de>,
    T: Deserialize<'de> + Default,
{
    Ok(Option::<T>::deserialize(de)?.unwrap_or_default())
}

/// The five terminal event kinds the bridge subscribes to.
///
/// `completed` returns work to Gitea as a pull request; the other four return it as a
/// `status/blocked` label plus a reason comment.
pub const TERMINAL_KINDS: [&str; 5] = ["completed", "blocked", "gave_up", "crashed", "timed_out"];

/// Every status a task can hold, as [`Task::status`] documents them.
///
/// The reconciliation sweep enumerates *all* of these rather than the two a terminal event
/// was observed to leave behind (`done` after `complete`, `blocked` after `block`). Nothing
/// pins the status of a `crashed`, `gave_up` or `timed_out` task, and kanban's crash-reclaim
/// exists precisely to return a dead worker's task to a claimable status — most plausibly
/// `ready`. A sweep keyed on a hand-maintained pair of statuses would never list those, and
/// their Gitea feedback would be dropped for good, for the three kinds most likely to
/// coincide with the infrastructure trouble that made the sweep necessary.
///
/// Enumerating widely is safe because the discrimination is done by
/// [`crate::outbound::latest_terminal_kind`] on the task's *event trail*, not by its status:
/// a task created `blocked` to await a 🐝 has no terminal event and is skipped whichever
/// status listed it.
pub const RECONCILE_STATUSES: [&str; 9] = [
    "triage",
    "todo",
    "ready",
    "running",
    "review",
    "blocked",
    "scheduled",
    "done",
    "archived",
];

/// A task as `hermes kanban show --json` reports it.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct Task {
    /// Kanban task id, e.g. `t_ea5c4ee9`.
    #[serde(default, deserialize_with = "null_as_default")]
    pub id: String,
    /// Task title.
    #[serde(default, deserialize_with = "null_as_default")]
    pub title: String,
    /// Opening post. The bridge stores the Gitea coordinates here as a trailer, because
    /// `show --json` does not echo the idempotency key back.
    #[serde(default)]
    pub body: Option<String>,
    /// `triage | todo | ready | running | review | blocked | scheduled | done | archived`.
    #[serde(default, deserialize_with = "null_as_default")]
    pub status: String,
    /// Profile the task is assigned to.
    #[serde(default)]
    pub assignee: Option<String>,
    /// Branch name for worktree tasks.
    #[serde(default)]
    pub branch_name: Option<String>,
}

/// One entry of a task's `task_events` audit trail.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct TaskEvent {
    /// Event kind, e.g. `created`, `completed`, `blocked`.
    #[serde(default, deserialize_with = "null_as_default")]
    pub kind: String,
    /// Kind-specific payload. `blocked` carries `{reason, kind, recurrences}`;
    /// `completed` carries `{result_len, summary}`.
    #[serde(default, deserialize_with = "null_as_default")]
    pub payload: BTreeMap<String, serde_json::Value>,
    /// Unix timestamp.
    #[serde(default, deserialize_with = "null_as_default")]
    pub created_at: i64,
}

impl TaskEvent {
    /// Reads a payload field as a string, tolerating non-string JSON.
    pub fn payload_str(&self, key: &str) -> Option<String> {
        match self.payload.get(key)? {
            serde_json::Value::Null => None,
            serde_json::Value::String(s) if s.is_empty() => None,
            serde_json::Value::String(s) => Some(s.clone()),
            other => Some(other.to_string()),
        }
    }
}

/// A comment on a kanban task.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct TaskComment {
    /// Comment author.
    #[serde(default)]
    pub author: Option<String>,
    /// Comment body.
    #[serde(default, deserialize_with = "null_as_default")]
    pub body: String,
}

/// Full `hermes kanban show --json` payload.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct TaskDetail {
    /// The task itself.
    #[serde(default, deserialize_with = "null_as_default")]
    pub task: Task,
    /// Latest run summary, when the task has one.
    #[serde(default)]
    pub latest_summary: Option<String>,
    /// Comments, oldest first.
    #[serde(default, deserialize_with = "null_as_default")]
    pub comments: Vec<TaskComment>,
    /// Events, oldest first.
    #[serde(default, deserialize_with = "null_as_default")]
    pub events: Vec<TaskEvent>,
}

impl TaskDetail {
    /// Rejects a payload that decoded but says nothing.
    ///
    /// Every field here is `#[serde(default)]`, which is deliberate — kanban may add fields
    /// — but it also means a *differently shaped* payload decodes to an empty struct rather
    /// than failing. Without this check the outbound leg would report "watching kanban
    /// terminal events" and then quietly plan nothing, forever, because an empty body has no
    /// `gitea-ref:` trailer. A task id is the one field kanban has always returned
    /// (`hermes kanban show --json` → `task.id`), so its absence means the shape moved.
    pub fn validate(&self) -> Result<(), String> {
        if self.task.id.trim().is_empty() {
            return Err(
                "no task.id in the payload — `hermes kanban show --json` did not return the \
                 expected shape, or returned an error document"
                    .into(),
            );
        }
        Ok(())
    }

    /// The most recent event of the given kind.
    pub fn last_event(&self, kind: &str) -> Option<&TaskEvent> {
        self.events.iter().rev().find(|e| e.kind == kind)
    }

    /// The most recent event whose kind is one of [`TERMINAL_KINDS`].
    ///
    /// This is what the reconciliation sweep keys on rather than the task's *status*: a task
    /// created with `--initial-status blocked` to await a 🐝 is `blocked` but has only a
    /// `created` event, and reporting it to Gitea as a block would be a lie.
    pub fn last_terminal_event(&self) -> Option<&TaskEvent> {
        self.events
            .iter()
            .rev()
            .find(|e| TERMINAL_KINDS.contains(&e.kind.as_str()))
    }

    /// Whether any comment carries `marker` as a line of its own.
    ///
    /// This is how the bridge stays idempotent across a restart without keeping in-memory
    /// state: the marker lives in kanban, which is the durable side.
    ///
    /// Matched line-exactly rather than by substring, for the same reason
    /// [`crate::approval::Approval::is_consumed`] is — and this is the guard with the larger
    /// blast radius, because it decides whether a pull request is opened at all. Under a
    /// substring match a human (or another agent) commenting *"did the bridge ever post
    /// gitea-bridge: pr-opened for this?"* would suppress that task's pull request
    /// permanently, and both the watch path and the reconcile path would read the suppression
    /// as the routine already-handled case and say nothing.
    pub fn has_marker(&self, marker: &str) -> bool {
        let marker = marker.trim();
        self.comments
            .iter()
            .any(|c| c.body.lines().any(|l| l.trim() == marker))
    }
}

/// First delay between marker-write attempts; doubled on each further attempt.
const RETRY_BACKOFF: std::time::Duration = std::time::Duration::from_millis(200);

/// A request to create a kanban task.
#[derive(Debug, Clone, PartialEq)]
pub struct CreateTask {
    /// Task title — the Gitea issue title.
    pub title: String,
    /// Opening post, carrying the `gitea-ref:` trailer.
    pub body: String,
    /// Dedup key. If a non-archived task with this key exists, its id is returned instead
    /// of a duplicate being created.
    pub idempotency_key: String,
}

/// Failures of the kanban wrapper.
#[derive(Debug, thiserror::Error)]
pub enum KanbanError {
    /// The binary could not be spawned.
    #[error("cannot run {binary}: {source}")]
    Spawn {
        /// Executable name.
        binary: String,
        /// Underlying error.
        #[source]
        source: std::io::Error,
    },
    /// The command ran and failed.
    #[error("{binary} {args} exited with {code}: {stderr}")]
    Exit {
        /// Executable name.
        binary: String,
        /// Joined arguments, for the log.
        args: String,
        /// Exit status, or -1 when killed by a signal.
        code: i32,
        /// Truncated stderr.
        stderr: String,
    },
    /// The `--json` output did not parse.
    #[error("cannot decode `{args}` output: {source}")]
    Decode {
        /// Joined arguments.
        args: String,
        /// Underlying error.
        #[source]
        source: serde_json::Error,
    },
    /// The `--json` output parsed but is not the shape the bridge relies on.
    #[error("`{args}` returned an unexpected shape: {reason}")]
    Shape {
        /// Joined arguments.
        args: String,
        /// What was missing.
        reason: String,
    },
    /// A created task came back without an id.
    #[error("kanban create returned no task id")]
    NoTaskId,
}

/// Handle on one kanban board.
#[derive(Debug, Clone)]
pub struct Kanban {
    binary: String,
    board: Option<String>,
    assignee: Option<String>,
    workspace: Option<String>,
    max_runtime: Option<String>,
    max_retries: Option<u32>,
    created_by: String,
    tenant: Option<String>,
    require_approval: bool,
}

impl Kanban {
    /// Builds a handle from config.
    pub fn new(cfg: &KanbanConfig) -> Self {
        Self {
            binary: cfg.binary.clone(),
            board: cfg.board.clone(),
            assignee: cfg.assignee.clone(),
            workspace: cfg.workspace.clone(),
            max_runtime: cfg.max_runtime.clone(),
            max_retries: cfg.max_retries,
            created_by: cfg.created_by.clone(),
            tenant: cfg.tenant.clone(),
            require_approval: cfg.require_approval,
        }
    }

    /// Whether created tasks wait for a 🐝 before a worker can claim them.
    pub fn require_approval(&self) -> bool {
        self.require_approval
    }

    /// The executable this handle runs.
    pub fn binary(&self) -> &str {
        &self.binary
    }

    /// `hermes kanban [--board X] …` prefix shared by every subcommand.
    fn prefix(&self) -> Vec<String> {
        let mut argv = vec!["kanban".to_string()];
        if let Some(board) = self.board.as_deref().filter(|b| !b.is_empty()) {
            argv.push("--board".into());
            argv.push(board.into());
        }
        argv
    }

    /// Builds the argv for `kanban create`.
    ///
    /// `--idempotency-key` is what makes acceptance criterion 1 hold: two poll cycles over
    /// one ready issue return the same task id, because kanban returns the existing task
    /// rather than creating a duplicate.
    ///
    /// `--initial-status blocked` is what makes acceptance criterion 5 hold. `create` has no
    /// `--status` flag and its default is **`ready`** — verified against the shipped CLI, and
    /// pinned by `create_lands_in_a_status_the_approval_sweep_covers` in `live_kanban.rs`.
    /// A `ready` task is immediately claimable, so without this flag the task is dispatched
    /// to an agent before any human sees it, and the approval sweep — which covers `blocked`
    /// and `todo` — never has a bridge-created task to act on.
    pub fn create_args(&self, req: &CreateTask) -> Vec<String> {
        let mut argv = self.prefix();
        argv.push("create".into());
        argv.push(req.title.clone());
        if self.require_approval {
            argv.push("--initial-status".into());
            argv.push("blocked".into());
        }
        argv.push("--body".into());
        argv.push(req.body.clone());
        argv.push("--idempotency-key".into());
        argv.push(req.idempotency_key.clone());
        argv.push("--created-by".into());
        argv.push(self.created_by.clone());
        if let Some(a) = self.assignee.as_deref().filter(|s| !s.is_empty()) {
            argv.push("--assignee".into());
            argv.push(a.into());
        }
        if let Some(w) = self.workspace.as_deref().filter(|s| !s.is_empty()) {
            argv.push("--workspace".into());
            argv.push(w.into());
        }
        if let Some(m) = self.max_runtime.as_deref().filter(|s| !s.is_empty()) {
            argv.push("--max-runtime".into());
            argv.push(m.into());
        }
        if let Some(n) = self.max_retries {
            argv.push("--max-retries".into());
            argv.push(n.to_string());
        }
        if let Some(t) = self.tenant.as_deref().filter(|s| !s.is_empty()) {
            argv.push("--tenant".into());
            argv.push(t.into());
        }
        argv.push("--json".into());
        argv
    }

    /// Builds the argv for `kanban show --json`.
    pub fn show_args(&self, task_id: &str) -> Vec<String> {
        let mut argv = self.prefix();
        argv.push("show".into());
        argv.push(task_id.to_string());
        argv.push("--json".into());
        argv
    }

    /// Builds the argv for `kanban list --json`, optionally filtered by status.
    pub fn list_args(&self, status: Option<&str>) -> Vec<String> {
        let mut argv = self.prefix();
        argv.push("list".into());
        if let Some(status) = status {
            argv.push("--status".into());
            argv.push(status.to_string());
        }
        if let Some(a) = self.assignee.as_deref().filter(|s| !s.is_empty()) {
            argv.push("--assignee".into());
            argv.push(a.into());
        }
        if let Some(t) = self.tenant.as_deref().filter(|s| !s.is_empty()) {
            argv.push("--tenant".into());
            argv.push(t.into());
        }
        argv.push("--json".into());
        argv
    }

    /// Builds the argv for `kanban comment`.
    pub fn comment_args(&self, task_id: &str, body: &str) -> Vec<String> {
        let mut argv = self.prefix();
        argv.push("comment".into());
        argv.push(task_id.to_string());
        argv.push(body.to_string());
        argv
    }

    /// Builds the argv for `kanban promote` — the 🐝 approval path.
    pub fn promote_args(&self, task_id: &str) -> Vec<String> {
        let mut argv = self.prefix();
        argv.push("promote".into());
        argv.push(task_id.to_string());
        argv
    }

    /// Builds the argv for `kanban unblock`.
    pub fn unblock_args(&self, task_id: &str) -> Vec<String> {
        let mut argv = self.prefix();
        argv.push("unblock".into());
        argv.push(task_id.to_string());
        argv
    }

    /// Builds the argv for `kanban watch`, restricted to the five terminal kinds.
    pub fn watch_args(&self) -> Vec<String> {
        let mut argv = self.prefix();
        argv.push("watch".into());
        argv.push("--kinds".into());
        argv.push(TERMINAL_KINDS.join(","));
        if let Some(a) = self.assignee.as_deref().filter(|s| !s.is_empty()) {
            argv.push("--assignee".into());
            argv.push(a.into());
        }
        if let Some(t) = self.tenant.as_deref().filter(|s| !s.is_empty()) {
            argv.push("--tenant".into());
            argv.push(t.into());
        }
        argv
    }

    /// Creates (or re-resolves) a task and returns its id.
    pub async fn create(&self, req: &CreateTask) -> Result<String, KanbanError> {
        let args = self.create_args(req);
        let out = self.run(&args).await?;
        let value: serde_json::Value = serde_json::from_str(&out).map_err(|source| KanbanError::Decode {
            args: redact(&args),
            source,
        })?;
        value
            .get("id")
            .and_then(|v| v.as_str())
            .map(str::to_string)
            .ok_or(KanbanError::NoTaskId)
    }

    /// Resolves a task's full detail.
    pub async fn show(&self, task_id: &str) -> Result<TaskDetail, KanbanError> {
        let args = self.show_args(task_id);
        let out = self.run(&args).await?;
        let detail: TaskDetail = serde_json::from_str(&out).map_err(|source| KanbanError::Decode {
            args: redact(&args),
            source,
        })?;
        detail.validate().map_err(|reason| KanbanError::Shape {
            args: redact(&args),
            reason,
        })?;
        Ok(detail)
    }

    /// Lists tasks, optionally filtered by status.
    pub async fn list(&self, status: Option<&str>) -> Result<Vec<Task>, KanbanError> {
        let args = self.list_args(status);
        let out = self.run(&args).await?;
        let tasks: Vec<Task> = serde_json::from_str(&out).map_err(|source| KanbanError::Decode {
            args: redact(&args),
            source,
        })?;
        // Same reasoning as `TaskDetail::validate`: an every-field-defaulted struct turns a
        // shape change into a list of blanks rather than an error.
        if let Some(i) = tasks.iter().position(|t| t.id.trim().is_empty()) {
            return Err(KanbanError::Shape {
                args: redact(&args),
                reason: format!(
                    "entry {i} has no id — `kanban list --json` did not return the expected shape"
                ),
            });
        }
        Ok(tasks)
    }

    /// Appends a comment to a task.
    pub async fn comment(&self, task_id: &str, body: &str) -> Result<(), KanbanError> {
        self.run(&self.comment_args(task_id, body)).await.map(|_| ())
    }

    /// Appends a comment, retrying a failure a bounded number of times.
    ///
    /// Every marker this bridge relies on is written by this call, and each one is written
    /// *after* the move it records — deliberately, because a marker without a move would
    /// strand the task whereas a move without a marker is retried on the next sweep. That
    /// ordering is only safe while the marker eventually lands: an unwritten
    /// `approval-consumed` marker releases the task again on every 60s sweep, and an unwritten
    /// `pr-opened` / `blocked-reported` marker makes the reconcile sweep repost the same
    /// user-visible Gitea comment every 300s, forever.
    ///
    /// The call is a local subprocess, so a few immediate retries cost nothing and cover the
    /// failure this is actually for: a momentarily locked board, a transient spawn failure.
    /// A failure that survives them is *not* transient, and the caller is expected to treat
    /// the `Err` as load-bearing rather than log it and move on — see
    /// [`crate::state::BridgeState`].
    pub async fn comment_with_retry(
        &self,
        task_id: &str,
        body: &str,
        attempts: u32,
    ) -> Result<(), KanbanError> {
        let attempts = attempts.max(1);
        let mut backoff = RETRY_BACKOFF;
        for attempt in 1..=attempts {
            match self.comment(task_id, body).await {
                Ok(()) => return Ok(()),
                Err(err) if attempt == attempts => return Err(err),
                Err(err) => {
                    tracing::warn!(
                        task = task_id, attempt, attempts, error = %err,
                        "cannot record a kanban marker; retrying"
                    );
                    tokio::time::sleep(backoff).await;
                    backoff *= 2;
                }
            }
        }
        unreachable!("the loop returns on the final attempt")
    }

    /// Moves a todo/blocked task to ready.
    pub async fn promote(&self, task_id: &str) -> Result<(), KanbanError> {
        self.run(&self.promote_args(task_id)).await.map(|_| ())
    }

    /// Returns a blocked/scheduled task to ready.
    pub async fn unblock(&self, task_id: &str) -> Result<(), KanbanError> {
        self.run(&self.unblock_args(task_id)).await.map(|_| ())
    }

    /// Spawns `kanban watch` and returns its stdout, line by line.
    ///
    /// The child is returned alongside the reader so the caller owns its lifetime; dropping
    /// the child kills the watcher.
    pub async fn watch(&self) -> Result<(tokio::process::Child, Lines<BufReader<ChildStdout>>), KanbanError> {
        let args = self.watch_args();
        let mut child = Command::new(&self.binary)
            .args(&args)
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .kill_on_drop(true)
            .spawn()
            .map_err(|source| KanbanError::Spawn {
                binary: self.binary.clone(),
                source,
            })?;
        let stdout = child.stdout.take().expect("stdout is piped");
        Ok((child, BufReader::new(stdout).lines()))
    }

    async fn run(&self, args: &[String]) -> Result<String, KanbanError> {
        run_argv(&self.binary, args).await
    }
}

/// Runs `binary` with `args` and returns stdout.
///
/// `args` is passed as an argv vector — no shell is involved, so no argument can be
/// interpreted as shell syntax.
pub(crate) async fn run_argv(binary: &str, args: &[String]) -> Result<String, KanbanError> {
    let out = Command::new(binary)
        .args(args)
        .stdin(Stdio::null())
        .output()
        .await
        .map_err(|source| KanbanError::Spawn {
            binary: binary.to_string(),
            source,
        })?;
    if !out.status.success() {
        return Err(KanbanError::Exit {
            binary: binary.to_string(),
            args: redact(args),
            code: out.status.code().unwrap_or(-1),
            stderr: String::from_utf8_lossy(&out.stderr).chars().take(512).collect(),
        });
    }
    Ok(String::from_utf8_lossy(&out.stdout).into_owned())
}

/// Joins argv for an error message without echoing long bodies back into the log.
fn redact(args: &[String]) -> String {
    args.iter()
        .map(|a| {
            if a.len() > 64 {
                format!("{}…", &a[..floor_char_boundary(a, 64)])
            } else {
                a.clone()
            }
        })
        .collect::<Vec<_>>()
        .join(" ")
}

/// Largest index `<= i` that is a char boundary of `s`.
fn floor_char_boundary(s: &str, i: usize) -> usize {
    let mut i = i.min(s.len());
    while i > 0 && !s.is_char_boundary(i) {
        i -= 1;
    }
    i
}

#[cfg(test)]
mod tests {
    use super::*;

    fn kanban() -> Kanban {
        Kanban::new(&KanbanConfig {
            binary: "hermes".into(),
            board: Some("f4".into()),
            assignee: Some("codex".into()),
            workspace: Some("worktree".into()),
            max_runtime: Some("30m".into()),
            max_retries: Some(2),
            created_by: "gitea-automations".into(),
            tenant: None,
            require_approval: true,
        })
    }

    #[test]
    fn create_args_carry_the_idempotency_key() {
        let argv = kanban().create_args(&CreateTask {
            title: "automations daemon".into(),
            body: "gitea-ref: terraphim/gitea#57".into(),
            idempotency_key: "gitea:terraphim/gitea#57".into(),
        });
        let joined = argv.join(" ");
        assert!(
            joined.starts_with("kanban --board f4 create automations daemon"),
            "{joined}"
        );
        assert!(
            joined.contains("--idempotency-key gitea:terraphim/gitea#57"),
            "{joined}"
        );
        assert!(joined.contains("--assignee codex"), "{joined}");
        assert!(joined.contains("--workspace worktree"), "{joined}");
        assert!(joined.contains("--max-runtime 30m"), "{joined}");
        assert!(joined.contains("--max-retries 2"), "{joined}");
        assert!(argv.last().map(String::as_str) == Some("--json"), "{joined}");
    }

    #[test]
    fn create_blocks_the_task_so_the_approval_gate_is_reachable() {
        // `hermes kanban create` has no `--status` and defaults to `ready`, which is
        // immediately claimable. Without this flag AC5's promote/unblock branch is dead code
        // and every ready issue reaches an agent unreviewed.
        let argv = kanban().create_args(&CreateTask {
            title: "t".into(),
            body: "gitea-ref: o/r#1".into(),
            idempotency_key: "gitea:o/r#1".into(),
        });
        let i = argv
            .iter()
            .position(|a| a == "--initial-status")
            .expect("--initial-status present");
        assert_eq!(argv[i + 1], "blocked");

        let open = Kanban::new(&KanbanConfig {
            require_approval: false,
            ..KanbanConfig::default()
        });
        assert!(
            !open
                .create_args(&CreateTask {
                    title: "t".into(),
                    body: "b".into(),
                    idempotency_key: "k".into(),
                })
                .iter()
                .any(|a| a == "--initial-status"),
            "opting out must leave kanban's own default alone"
        );
    }

    #[test]
    fn a_payload_of_the_wrong_shape_is_rejected_rather_than_defaulted() {
        // Every field is #[serde(default)], so a differently-shaped document decodes to an
        // empty struct. Undetected, that makes the outbound leg silently plan nothing: an
        // empty body has no `gitea-ref:` trailer, so every event looks like somebody else's
        // task.
        let empty: TaskDetail = serde_json::from_str("{}").expect("decodes");
        assert!(empty.validate().is_err());
        let wrong: TaskDetail = serde_json::from_str(r#"{"error":"no such task"}"#).expect("decodes");
        assert!(wrong.validate().is_err());
        let ok: TaskDetail = serde_json::from_str(r#"{"task":{"id":"t_1"}}"#).expect("decodes");
        assert!(ok.validate().is_ok());
    }

    #[test]
    fn only_a_terminal_event_counts_as_one() {
        // The reconciliation sweep keys on this rather than on status: a task created with
        // `--initial-status blocked` to await a 🐝 is `blocked` but has only a `created`
        // event, and must not be reported to Gitea as a block.
        let gated: TaskDetail = serde_json::from_str(
            r#"{"task":{"id":"t_1","status":"blocked"},
                "events":[{"kind":"created","payload":{"status":"blocked"},"created_at":1}]}"#,
        )
        .expect("decodes");
        assert!(gated.last_terminal_event().is_none());

        let real: TaskDetail = serde_json::from_str(
            r#"{"task":{"id":"t_1","status":"blocked"},
                "events":[{"kind":"created","payload":{},"created_at":1},
                          {"kind":"claimed","payload":{},"created_at":2},
                          {"kind":"blocked","payload":{"kind":"transient"},"created_at":3}]}"#,
        )
        .expect("decodes");
        assert_eq!(
            real.last_terminal_event().map(|e| e.kind.as_str()),
            Some("blocked")
        );
    }

    #[test]
    fn watch_args_subscribe_to_exactly_the_five_terminal_kinds() {
        let argv = kanban().watch_args();
        let i = argv.iter().position(|a| a == "--kinds").expect("--kinds present");
        assert_eq!(argv[i + 1], "completed,blocked,gave_up,crashed,timed_out");
    }

    #[test]
    fn no_argv_builder_ever_invokes_a_shell() {
        // The action space is an allowlist and the transport is argv. If either regressed,
        // a shell token would show up here.
        let k = kanban();
        let bodies = [
            k.create_args(&CreateTask {
                title: "t; rm -rf /".into(),
                body: "$(id)".into(),
                idempotency_key: "gitea:o/r#1".into(),
            }),
            k.show_args("t_1"),
            k.comment_args("t_1", "`id`"),
            k.promote_args("t_1"),
            k.unblock_args("t_1"),
            k.watch_args(),
        ];
        for argv in bodies {
            assert_eq!(argv.first().map(String::as_str), Some("kanban"), "{argv:?}");
            for shell in ["sh", "bash", "zsh", "-c", "eval", "system"] {
                assert!(
                    !argv.iter().any(|a| a == shell),
                    "no shell token may appear: {argv:?}"
                );
            }
        }
        // Metacharacters survive verbatim as *data* — that is the point of argv.
        let argv = k.comment_args("t_1", "; rm -rf / `id`");
        assert_eq!(argv.last().expect("body"), "; rm -rf / `id`");
    }

    #[test]
    fn board_is_omitted_when_unset() {
        let k = Kanban::new(&KanbanConfig::default());
        assert_eq!(k.show_args("t_1"), vec!["kanban", "show", "t_1", "--json"]);
    }

    #[test]
    fn show_json_decodes_the_probed_shape() {
        let json = r#"{
          "task": {"id":"t_3b49a6e0","title":"probe","body":"gitea-ref: o/r#42","status":"blocked",
                   "assignee":null,"branch_name":null},
          "latest_summary": null,
          "comments": [{"author":"default","body":"BLOCKED: waiting for spec","created_at":1}],
          "events": [
            {"kind":"created","payload":{"status":"ready"},"created_at":1},
            {"kind":"blocked","payload":{"reason":"waiting for spec","kind":"needs_input","recurrences":1},
             "created_at":2}
          ]
        }"#;
        let detail: TaskDetail = serde_json::from_str(json).expect("decodes");
        assert_eq!(detail.task.id, "t_3b49a6e0");
        let blocked = detail.last_event("blocked").expect("blocked event");
        assert_eq!(blocked.payload_str("kind").as_deref(), Some("needs_input"));
        assert_eq!(blocked.payload_str("reason").as_deref(), Some("waiting for spec"));
        assert!(detail.has_marker("BLOCKED: waiting for spec"));
        assert!(!detail.has_marker("gitea-bridge: pr-opened"));
    }

    #[test]
    fn a_marker_is_matched_line_exactly_not_by_substring() {
        // The guard that decides whether a pull request is opened at all must not be
        // trippable by somebody talking *about* the marker: under a substring match this
        // comment would suppress the PR permanently, and both the watch path and the
        // reconcile path would read the suppression as the routine already-handled case.
        let detail = TaskDetail {
            comments: vec![
                TaskComment {
                    author: Some("alex".into()),
                    body: "did the bridge ever post gitea-bridge: pr-opened for this?".into(),
                },
                TaskComment {
                    author: Some("bridge".into()),
                    body: "gitea-bridge: blocked-reported".into(),
                },
            ],
            ..TaskDetail::default()
        };
        assert!(!detail.has_marker("gitea-bridge: pr-opened"));
        assert!(detail.has_marker("gitea-bridge: blocked-reported"));

        // A marker that leads a multi-line body still counts: that is the shape of the
        // Gitea-side comment, and `first_line` is what gets written back to kanban.
        let multi = TaskDetail {
            comments: vec![TaskComment {
                author: Some("bridge".into()),
                body: "gitea-bridge: pr-opened\n\nKanban task `t_1` completed.".into(),
            }],
            ..TaskDetail::default()
        };
        assert!(multi.has_marker("gitea-bridge: pr-opened"));
    }

    #[test]
    fn the_reconcile_status_set_covers_every_documented_status() {
        // The sweep used to enumerate `done` and `blocked` only. Nothing pins the status a
        // `crashed`, `gave_up` or `timed_out` task lands in, and crash-reclaim plausibly
        // returns it to `ready` — which that pair does not list, so those three kinds were
        // never reconciled.
        for status in ["done", "blocked", "ready", "todo", "running"] {
            assert!(RECONCILE_STATUSES.contains(&status), "{status}");
        }
        let mut sorted = RECONCILE_STATUSES.to_vec();
        sorted.sort_unstable();
        sorted.dedup();
        assert_eq!(sorted.len(), RECONCILE_STATUSES.len(), "no duplicates");
    }

    #[tokio::test]
    async fn a_marker_write_is_retried_before_it_is_given_up_on() {
        // A missing binary is the persistent case: it must exhaust its attempts and surface
        // the error rather than being swallowed, because the caller keys its dead-letter
        // guard on exactly this `Err`.
        let k = Kanban::new(&KanbanConfig {
            binary: "gitea-automations-no-such-binary".into(),
            ..KanbanConfig::default()
        });
        let err = k
            .comment_with_retry("t_1", "gitea-bridge: pr-opened", 2)
            .await
            .expect_err("a missing binary cannot be retried into success");
        assert!(matches!(err, KanbanError::Spawn { .. }), "{err:?}");
    }

    #[test]
    fn completed_payload_exposes_the_summary() {
        let json = r#"{"task":{"id":"t_1"},"events":[{"kind":"completed",
            "payload":{"result_len":0,"summary":"done"},"created_at":3}]}"#;
        let detail: TaskDetail = serde_json::from_str(json).expect("decodes");
        assert_eq!(
            detail
                .last_event("completed")
                .and_then(|e| e.payload_str("summary"))
                .as_deref(),
            Some("done")
        );
    }

    #[test]
    fn a_null_payload_does_not_fail_the_whole_decode() {
        // kanban records an `unblocked` event as {"kind":"unblocked","payload":null}, and
        // #[serde(default)] covers an absent field only — an explicit null is still a type
        // error. Undetected, that one null failed the entire `show` decode, which made every
        // task the approval leg had ever released unreportable to Gitea.
        let json = r#"{
          "task": {"id":"t_1","title":"probe","body":"gitea-ref: o/r#42","status":"blocked"},
          "comments": null,
          "events": [
            {"kind":"created","payload":{"status":"blocked"},"created_at":1},
            {"kind":"unblocked","payload":null,"created_at":2},
            {"kind":"blocked","payload":{"kind":"transient"},"created_at":3}
          ]
        }"#;
        let detail: TaskDetail = serde_json::from_str(json).expect("a null payload must decode");
        assert!(detail.validate().is_ok());
        assert!(detail.comments.is_empty());
        assert_eq!(detail.events.len(), 3);
        assert_eq!(
            detail.last_terminal_event().map(|e| e.kind.as_str()),
            Some("blocked")
        );
    }

    #[test]
    fn redact_truncates_on_char_boundaries() {
        let long = "🐝".repeat(40);
        let out = redact(&[long]);
        assert!(out.ends_with('…'));
    }
}
