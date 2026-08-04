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

use std::collections::{BTreeMap, VecDeque};
use std::process::Stdio;
use std::sync::{Arc, Mutex};

use serde::{Deserialize, Deserializer, Serialize};
use tokio::io::{AsyncBufReadExt, BufReader, Lines};
use tokio::process::{Child, ChildStdout, Command};

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
/// a task that has not run yet has no terminal event and is skipped whichever status listed
/// it.
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
    /// can hold a status it never ran into — `blocked` with only a `created` event on the
    /// trail — and reporting that to Gitea as a block would be a lie about work that has not
    /// started.
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

/// Deadline on one `hermes` / `gitea-robot` invocation.
///
/// Every subprocess this crate runs is a *request*, and both binaries talk to a server over
/// HTTP. Without a deadline a half-open connection is not an error: the child simply never
/// exits, and `run_argv` waits for it forever. That is worse here than a failure would be,
/// because the caller of the failed call is a loop that would otherwise retry — the outbound
/// leg wedges inside the `watch` line loop, and the "a dead watcher is silent" guard around it
/// never fires, because the watcher is not dead, it is merely never read from again.
///
/// A minute is far longer than any of these calls should take (a local kanban subprocess, or
/// one Gitea API round trip) and short enough that a wedged call is a log line rather than a
/// silently dead leg. The child is killed on the way out, not leaked: [`Command::kill_on_drop`].
const RUN_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(60);

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
    /// The command was still running when its deadline passed, and was killed.
    #[error("{binary} {args} did not finish within {seconds}s and was killed")]
    Timeout {
        /// Executable name.
        binary: String,
        /// Joined arguments, for the log.
        args: String,
        /// The deadline that passed.
        seconds: u64,
    },
    /// A created task came back without an id.
    #[error("kanban create returned no task id")]
    NoTaskId,
}

/// Stderr lines retained from a running `kanban watch`.
///
/// Enough to carry a usage error and its context out of a watcher that died on startup;
/// bounded so a watcher that logs forever cannot grow this without limit.
const WATCH_STDERR_TAIL: usize = 20;

/// How long [`Watcher::finish`] waits for an exit before killing the child.
///
/// Stdout is already at EOF by then, so the process has almost always exited; the wait is
/// only so the exit *status* can be reported instead of guessed.
const WATCH_EXIT_GRACE: std::time::Duration = std::time::Duration::from_secs(5);

/// A running `hermes kanban watch`.
///
/// Dropping it kills the watcher. [`Watcher::finish`] is the ordered path: it reports why the
/// watcher stopped, which is the difference between a restart loop an operator can act on and
/// one that only says it is looping.
#[derive(Debug)]
pub struct Watcher {
    child: Child,
    /// The watcher's stdout, line by line. Terminal events arrive here.
    pub lines: Lines<BufReader<ChildStdout>>,
    stderr: Arc<Mutex<VecDeque<String>>>,
    drain: tokio::task::JoinHandle<()>,
}

/// Why a [`Watcher`] stopped.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct WatchExit {
    /// Exit code, when the child exited on its own within `WATCH_EXIT_GRACE`.
    pub code: Option<i32>,
    /// The last `WATCH_STDERR_TAIL` stderr lines, newest last, joined by ` | `.
    ///
    /// Empty when the watcher said nothing — which is itself the diagnosis for a binary that
    /// is not there or a subcommand that produced no complaint.
    pub stderr: String,
}

impl Watcher {
    /// Waits for the watcher to exit, killing it if it outlives the grace period, and reports
    /// its exit code alongside whatever it wrote to stderr.
    pub async fn finish(mut self) -> WatchExit {
        let code = match tokio::time::timeout(WATCH_EXIT_GRACE, self.child.wait()).await {
            Ok(Ok(status)) => status.code(),
            // Waiting itself failed: there is no code to report.
            Ok(Err(_)) => None,
            // The child outlived the grace period, so it is killed — and a status read after
            // that reports only the signal we just sent, which says nothing about the exit.
            Err(_) => {
                let _ = self.child.kill().await;
                let _ = self.child.wait().await;
                None
            }
        };
        // The child is gone, so its stderr pipe is at EOF and the drain finishes promptly.
        // Joining it first is what stops the exit line from racing the very lines that explain
        // the exit. The tail is read either way if the drain somehow outlives the grace
        // period — a partial diagnosis still beats none.
        let _ = tokio::time::timeout(WATCH_EXIT_GRACE, &mut self.drain).await;
        let stderr = self
            .stderr
            .lock()
            .map(|tail| tail.iter().cloned().collect::<Vec<_>>().join(" | "))
            .unwrap_or_default();
        WatchExit { code, stderr }
    }
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

    /// Whether the bridge holds a ready issue until a 🐝 rather than creating its task.
    ///
    /// Nothing in the argv depends on this any more — see [`Kanban::create_args`]. The gate is
    /// [`crate::state::PendingApprovals`], and this is only how the inbound leg reads the
    /// setting off the handle it already has.
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
    /// It deliberately does **not** pass `--initial-status blocked` any more, and that is the
    /// whole of acceptance criterion 5's redesign. `create` has no `--status` flag and its
    /// default is `ready` — immediately claimable — so the gate used to be a blocked task the
    /// 🐝 released. On hermes v0.19.0 that hold does not exist: `hermes kanban list` **promotes
    /// a blocked task by reading it** (probed twice on 2026-08-04, plain and with `--status
    /// blocked`), and `approval_sweep`'s first act is a `kanban list`, so the sweep was what
    /// released every gated task, unapproved and unlogged.
    ///
    /// The gate is therefore bridge-side: with `kanban.require_approval` on, this argv is not
    /// built at all until a 🐝 has landed — the issue waits in
    /// [`crate::state::PendingApprovals`] and no task exists for kanban to promote. Once it is
    /// built, `ready` is the correct status: the human already approved.
    pub fn create_args(&self, req: &CreateTask) -> Vec<String> {
        let mut argv = self.prefix();
        argv.push("create".into());
        argv.push(req.title.clone());
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
    ///
    /// The payload is checked against `decode_list`'s contract before it is returned — `id`
    /// *and* `body`. Both callers branch on fields that are `#[serde(default)]`, so a shape
    /// change is otherwise silent rather than an error.
    pub async fn list(&self, status: Option<&str>) -> Result<Vec<Task>, KanbanError> {
        let args = self.list_args(status);
        let out = self.run(&args).await?;
        decode_list(&out).map_err(|err| match err {
            ListDecodeError::Decode(source) => KanbanError::Decode {
                args: redact(&args),
                source,
            },
            ListDecodeError::Shape(reason) => KanbanError::Shape {
                args: redact(&args),
                reason,
            },
        })
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

    /// Spawns `kanban watch` and returns a handle to it.
    ///
    /// The child is owned by the returned [`Watcher`]; dropping it kills the watcher.
    pub async fn watch(&self) -> Result<Watcher, KanbanError> {
        let args = self.watch_args();
        let mut child = Command::new(&self.binary)
            .args(&args)
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            // Piped, not null. This is the daemon's only long-lived subprocess, and the only
            // one whose failure is *silent*: a `watch` that exits immediately — an unsupported
            // `--kinds`, a missing board, version skew — writes nothing to stdout, so the
            // "output but nothing parsed" diagnostic cannot fire and the outbound leg is dead
            // behind a bare `watch exited; restarting` every five seconds. `run_argv` reports
            // stderr for every other call (`:637`); this one used to discard it.
            .stderr(Stdio::piped())
            .kill_on_drop(true)
            .spawn()
            .map_err(|source| KanbanError::Spawn {
                binary: self.binary.clone(),
                source,
            })?;
        let stdout = child.stdout.take().expect("stdout is piped");
        let stderr = child.stderr.take().expect("stderr is piped");
        // Drained concurrently rather than read at exit: a watcher that logs steadily to
        // stderr would otherwise fill the pipe buffer and block on its own diagnostics.
        let tail = Arc::new(Mutex::new(VecDeque::with_capacity(WATCH_STDERR_TAIL)));
        let sink = Arc::clone(&tail);
        let drain = tokio::spawn(async move {
            let mut lines = BufReader::new(stderr).lines();
            while let Ok(Some(line)) = lines.next_line().await {
                let mut tail = sink.lock().expect("stderr tail mutex");
                if tail.len() == WATCH_STDERR_TAIL {
                    tail.pop_front();
                }
                tail.push_back(line);
            }
        });
        Ok(Watcher {
            child,
            lines: BufReader::new(stdout).lines(),
            stderr: tail,
            drain,
        })
    }

    async fn run(&self, args: &[String]) -> Result<String, KanbanError> {
        run_argv(&self.binary, args).await
    }
}

/// Why a `kanban list --json` payload was rejected.
///
/// Split from [`KanbanError`] so [`decode_list`] can be a pure function the tests drive with
/// a payload string, and the argv is attached once, by the caller that knows it.
#[derive(Debug)]
pub(crate) enum ListDecodeError {
    /// The output is not JSON, or not a list of task objects.
    Decode(serde_json::Error),
    /// It decoded, but not into the shape the daemon branches on.
    Shape(String),
}

/// Decodes a `kanban list --json` payload and pins the fields the daemon *branches on*.
///
/// Every field of [`Task`] is `#[serde(default)]` — deliberately, so kanban can add fields —
/// which means a differently shaped payload decodes to a list of blanks rather than failing.
/// The same reasoning as [`TaskDetail::validate`], applied to the list path, where two of the
/// daemon's four legs live:
///
/// * `id` — without it nothing can be shown, promoted or unblocked.
/// * `body` — both list callers open with
///   `task.body.as_deref().and_then(GiteaRef::parse_from_body)` and `continue` on `None`
///   (`approval_sweep` and `reconcile_sweep` in `main.rs`). `body` is what carries the
///   `gitea-ref:` trailer, because `list`/`show` do not echo the idempotency key back, so a
///   list without it is a list in which *no task is a bridge task*: every inbound task sits
///   `blocked` awaiting a 🐝 that can never be seen, and the reconcile sweep reports nothing
///   while its own summary reads idle. Two legs dead, no error, both preflights green — the
///   exact failure class the `id` guard was written for.
///
/// The `body` check is on the **key**, not the value: a task legitimately has no body, and
/// `null`/`""` are perfectly good answers. Only a payload in which *no* row carries the key at
/// all is treated as a shape change — that is the signal that the field left the list
/// projection, as opposed to this particular set of tasks having nothing in it. A board whose
/// listed tasks are all bodyless *and* a kanban that omits the key rather than emitting null
/// would trip it; that is a loud, named error on a board with no bridge task on it, which is
/// the trade this whole guard exists to make.
pub(crate) fn decode_list(raw: &str) -> Result<Vec<Task>, ListDecodeError> {
    let rows: Vec<serde_json::Value> = serde_json::from_str(raw).map_err(ListDecodeError::Decode)?;
    let tasks = rows
        .iter()
        .cloned()
        .map(serde_json::from_value::<Task>)
        .collect::<Result<Vec<_>, _>>()
        .map_err(ListDecodeError::Decode)?;
    if let Some(i) = tasks.iter().position(|t| t.id.trim().is_empty()) {
        return Err(ListDecodeError::Shape(format!(
            "entry {i} has no id — `kanban list --json` did not return the expected shape"
        )));
    }
    let objects = rows.iter().filter(|r| r.is_object()).count();
    let with_body = rows
        .iter()
        .filter_map(serde_json::Value::as_object)
        .filter(|o| o.contains_key("body"))
        .count();
    if objects > 0 && with_body == 0 {
        return Err(ListDecodeError::Shape(format!(
            "none of the {objects} listed tasks carries a `body` field — `kanban list --json` \
             no longer projects it. The bridge finds its own tasks by the `{prefix}` trailer in \
             the body and skips every task without one, so this would silently kill both the \
             approval and the reconciliation legs rather than fail",
            prefix = crate::inbound::GITEA_REF_PREFIX
        )));
    }
    Ok(tasks)
}

/// Runs `binary` with `args` and returns stdout.
///
/// `args` is passed as an argv vector — no shell is involved, so no argument can be
/// interpreted as shell syntax. The call is bounded by [`RUN_TIMEOUT`].
pub(crate) async fn run_argv(binary: &str, args: &[String]) -> Result<String, KanbanError> {
    run_argv_within(binary, args, RUN_TIMEOUT).await
}

/// [`run_argv`] with an explicit deadline, so the timeout itself is testable.
pub(crate) async fn run_argv_within(
    binary: &str,
    args: &[String],
    limit: std::time::Duration,
) -> Result<String, KanbanError> {
    let spawn_err = |source| KanbanError::Spawn {
        binary: binary.to_string(),
        source,
    };
    // Spawned rather than `.output()`ed so the deadline has something to kill: dropping the
    // child at the timeout is what stops a wedged request from becoming an orphan process
    // holding the board's lock.
    let child = Command::new(binary)
        .args(args)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(true)
        .spawn()
        .map_err(spawn_err)?;
    let out = match tokio::time::timeout(limit, child.wait_with_output()).await {
        Ok(result) => result.map_err(spawn_err)?,
        Err(_) => {
            return Err(KanbanError::Timeout {
                binary: binary.to_string(),
                args: redact(args),
                seconds: limit.as_secs(),
            });
        }
    };
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

    /// The R6 P1, stated from the argv side: the gate is no longer a kanban status.
    ///
    /// `--initial-status blocked` was the whole gate, and on hermes v0.19.0 it holds nothing —
    /// `kanban list` promotes a blocked task by reading it, and the approval sweep opens with a
    /// `kanban list`. Passing it now would be worse than useless: it would put a freshly
    /// *approved* task into a status a human has to release a second time.
    #[test]
    fn create_never_asks_kanban_to_hold_the_gate() {
        for require_approval in [true, false] {
            let k = Kanban::new(&KanbanConfig {
                require_approval,
                ..KanbanConfig::default()
            });
            let argv = k.create_args(&CreateTask {
                title: "t".into(),
                body: "gitea-ref: o/r#1".into(),
                idempotency_key: "gitea:o/r#1".into(),
            });
            assert!(
                !argv.iter().any(|a| a == "--initial-status"),
                "the gate is bridge-side; kanban's own default is correct once approved: {argv:?}"
            );
            assert_eq!(
                k.require_approval(),
                require_approval,
                "the setting still reads back"
            );
        }
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

    /// The list path's equivalent, on the two fields the sweeps branch on.
    ///
    /// `body` is load-bearing exactly like `id`: it is where the `gitea-ref:` trailer lives,
    /// and both `approval_sweep` and `reconcile_sweep` `continue` past every task without one.
    /// A list projection that dropped it would leave those two legs dead and silent.
    #[test]
    fn a_list_without_the_body_projection_is_rejected_rather_than_skipped() {
        let dropped = r#"[{"id":"t_1","title":"a","status":"blocked"},
                          {"id":"t_2","title":"b","status":"todo"}]"#;
        let err = decode_list(dropped).expect_err("a list with no body field must not decode");
        let ListDecodeError::Shape(reason) = err else {
            panic!("must be a shape error, not a decode error")
        };
        assert!(reason.contains("body"), "{reason}");

        // The check is on the key, not the value: a task with no body is ordinary, and both
        // spellings of "no body" are answers rather than shape changes.
        for payload in [
            r#"[{"id":"t_1","body":null},{"id":"t_2","body":""}]"#,
            r#"[{"id":"t_1","body":"gitea-ref: o/r#1"},{"id":"t_2","title":"not a bridge task"}]"#,
        ] {
            assert!(decode_list(payload).is_ok(), "{payload}");
        }

        // An empty board is an empty board, not a regression.
        assert!(decode_list("[]").expect("decodes").is_empty());

        // …and the id guard still holds.
        let err = decode_list(r#"[{"body":"gitea-ref: o/r#1"}]"#).expect_err("no id");
        assert!(matches!(err, ListDecodeError::Shape(reason) if reason.contains("id")));

        // The trailer really does survive the round trip this guard protects.
        let tasks =
            decode_list(r#"[{"id":"t_1","body":"x\n\ngitea-ref: terraphim/gitea#63"}]"#).expect("decodes");
        assert_eq!(
            tasks[0]
                .body
                .as_deref()
                .and_then(crate::inbound::GiteaRef::parse_from_body),
            Some(crate::inbound::GiteaRef::new("terraphim", "gitea", 63))
        );
    }

    #[test]
    fn only_a_terminal_event_counts_as_one() {
        // The reconciliation sweep keys on this rather than on status: a task can be
        // `blocked` with only a `created` event on its trail, and must not be reported to
        // Gitea as a block.
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

    /// A subprocess that never exits must become an error, not a wedged leg.
    ///
    /// Both binaries this crate runs talk to a server over HTTP, and a half-open connection
    /// does not fail — it hangs. Unbounded, that hang happens *inside* the outbound leg's
    /// `watch` line loop, so the leg stops reading events while still looking alive.
    #[cfg(unix)]
    #[tokio::test]
    async fn a_subprocess_that_never_exits_is_killed_and_reported() {
        let err = run_argv_within(
            "/bin/sh",
            &["-c".into(), "sleep 30".into()],
            std::time::Duration::from_millis(150),
        )
        .await
        .expect_err("must not wait for the child forever");
        assert!(matches!(err, KanbanError::Timeout { .. }), "{err:?}");

        // …and the deadline does not truncate a call that answers within it.
        let out = run_argv_within(
            "/bin/sh",
            &["-c".into(), "printf ok".into()],
            std::time::Duration::from_secs(30),
        )
        .await
        .expect("a prompt command still succeeds");
        assert_eq!(out, "ok");
    }

    /// A watcher that dies on startup must say why.
    ///
    /// This is the only long-lived subprocess in the daemon and the only failure the outbound
    /// leg cannot infer: with no stdout there is nothing to count as unparsed, so before this
    /// the whole diagnosis was `watch exited; restarting`, every five seconds, forever, while
    /// the leg was dead.
    #[cfg(unix)]
    #[tokio::test]
    async fn a_watcher_that_exits_immediately_reports_its_code_and_stderr() {
        use std::os::unix::fs::PermissionsExt;

        let dir = tempfile::tempdir().expect("tempdir");
        let script = dir.path().join("hermes");
        std::fs::write(&script, "#!/bin/sh\necho 'unknown flag: --kinds' >&2\nexit 3\n").expect("write stub");
        std::fs::set_permissions(&script, std::fs::Permissions::from_mode(0o755)).expect("chmod");

        let k = Kanban::new(&KanbanConfig {
            binary: script.to_string_lossy().into_owned(),
            ..KanbanConfig::default()
        });
        let mut watcher = k.watch().await.expect("spawns");
        assert!(
            watcher.lines.next_line().await.expect("reads").is_none(),
            "the failure case emits nothing on stdout — which is exactly why stderr matters"
        );
        let exit = watcher.finish().await;
        assert_eq!(exit.code, Some(3));
        assert!(exit.stderr.contains("unknown flag: --kinds"), "{exit:?}");
    }

    /// …and the ordinary path is unaffected: stdout still streams, and a clean exit says so.
    #[cfg(unix)]
    #[tokio::test]
    async fn a_watcher_streams_stdout_and_reports_a_clean_exit() {
        use std::os::unix::fs::PermissionsExt;

        let dir = tempfile::tempdir().expect("tempdir");
        let script = dir.path().join("hermes");
        std::fs::write(&script, "#!/bin/sh\necho 'completed t_1'\n").expect("write stub");
        std::fs::set_permissions(&script, std::fs::Permissions::from_mode(0o755)).expect("chmod");

        let k = Kanban::new(&KanbanConfig {
            binary: script.to_string_lossy().into_owned(),
            ..KanbanConfig::default()
        });
        let mut watcher = k.watch().await.expect("spawns");
        assert_eq!(
            watcher.lines.next_line().await.expect("reads").as_deref(),
            Some("completed t_1")
        );
        assert!(watcher.lines.next_line().await.expect("reads").is_none());
        let exit = watcher.finish().await;
        assert_eq!(exit.code, Some(0));
        assert_eq!(exit.stderr, "");
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
