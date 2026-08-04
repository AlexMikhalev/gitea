// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//! In-process guards that bound what a *failed durable write* can cost.
//!
//! Every idempotence guarantee this bridge makes lives in a kanban comment, and every one of
//! those markers is written after the move it records — a marker without a move would strand
//! the task, whereas a move without a marker is retried on the next sweep. The failure mode
//! that ordering leaves open is a marker that never lands at all: the "retry on the next
//! sweep" then never stops.
//!
//! * approval — the 🐝 is durable on the Gitea issue and nothing there records that it was
//!   used, so an unwritten `approval-consumed` marker releases the same task on every 60s
//!   sweep: it runs, blocks, is released again, forever, and invisibly, because
//!   [`crate::outbound::BLOCK_MARKER`] correctly suppresses the repeat Gitea comment.
//! * outbound — an unwritten `pr-opened` / `blocked-reported` marker makes the reconcile
//!   sweep replay the whole plan every 300s, posting the same comment on a *user-visible*
//!   Gitea issue each time.
//!
//! Nothing here is a source of truth: kanban stays the durable side, and losing this state on
//! restart costs at most one extra release or one extra replay — the same cost the durable
//! marker already bounds. It exists so that a marker write which failed *after its retries*
//! becomes a dead letter rather than a loop.
//!
//! The `reported` cache is the same idea pointed at cost rather than correctness: a task whose
//! Gitea feedback is *confirmed landed in kanban* cannot become unreported, so the reconcile
//! sweep need not pay a `kanban show` subprocess for it on every sweep for the lifetime of the
//! board. It deliberately does **not** cover the other cheap outcome — a task with nothing to
//! report yet — because that one is not stable: see [`BridgeState::mark_reported`].
//!
//! [`PendingApprovals`] is the one thing here that *is* a source of truth, and it is here
//! because it cannot be anywhere else: it is the approval gate. See its own documentation.

use std::collections::{BTreeMap, HashMap, HashSet};
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use serde::{Deserialize, Serialize};

/// Attempts a marker write gets before it is treated as a dead letter.
pub const MARKER_ATTEMPTS: u32 = 3;

/// Consecutive failed applications of one task's plan before the failure is escalated to the
/// Gitea issue itself.
pub const ESCALATE_AFTER: u32 = 3;

/// Upper bound on the reported-task cache.
///
/// One short string per reported task is nothing beside the subprocess it saves, but the board
/// grows for its whole lifetime and this daemon is meant to run for months. At the cap the
/// cache is dropped whole: the next sweep is as expensive as an unwarmed one and then warms
/// again, which is a bounded cost, whereas retaining the map is not.
const REPORTED_CAP: usize = 50_000;

/// Upper bound on the plan-failure counters.
///
/// Same argument as [`REPORTED_CAP`], and it bites sooner: an entry is removed only by
/// [`BridgeState::clear_plan_failures`], on full success, so a task that can *never* succeed —
/// a head branch nobody ever pushes, a label nobody ever creates — keeps its counter for the
/// lifetime of a process meant to run for months. At the cap the counters are dropped whole:
/// the cost is that an already-escalated task counts up to [`ESCALATE_AFTER`] again, and its
/// escalation is a no-op the second time because that marker is durable in kanban.
const FAILURES_CAP: usize = 10_000;

/// Upper bound on each of the three suppression sets.
///
/// Same argument again, and the asymmetry with the two caches above was the only thing keeping
/// it out: nothing removes a suppression, by design — a fingerprint or task id stays in its set
/// for the lifetime of the process — so under a kanban that will not accept comments, each set
/// grows one entry per distinct fingerprint or task, which is board-scaled exactly like
/// [`REPORTED_CAP`].
///
/// Smaller than the caches because these are pathological entries rather than routine ones: an
/// id only ever lands here when a marker write failed *after* [`MARKER_ATTEMPTS`] retries, and
/// a suppressed task stops re-inserting itself. Reaching 10k of them means the durable side has
/// been rejecting writes for a very long time.
///
/// At the cap the set is dropped whole, and what that costs is bounded by the same durable
/// marker each suppression stands in for — it is the restart case, without the restart:
///
/// * a dropped fingerprint costs at most one extra release of one task by an approval already
///   given (and the release is retried only while kanban still refuses the marker);
/// * a dropped task id costs at most one extra replay, i.e. one repeated Gitea comment;
/// * a dropped escalation id costs at most one repeated escalation comment.
///
/// Each of those is a single event, and any of them recurring is the failure the suppression
/// was for, which re-suppresses on the spot. Retaining the sets forever is not bounded by
/// anything.
const SUPPRESSION_CAP: usize = 10_000;

/// Upper bound on the set of tasks the sweeps must reach by id.
///
/// Bounded by work a human has been asked about and has not answered, not by board history —
/// an entry leaves the moment any `kanban show` reports the task in some other status. The cap
/// is here for the same reason the others are: nothing else bounds it, and this daemon is meant
/// to run for months. Dropping it whole costs one `hermes kanban unblock` by hand for each
/// forgotten task, exactly like a restart, and is loud rather than silent — see
/// [`BridgeState::remember_unlisted`].
const UNLISTED_CAP: usize = 10_000;

/// Per-process guards shared by the approval, outbound and reconcile legs.
#[derive(Debug, Default)]
pub struct BridgeState {
    inner: Mutex<Inner>,
}

#[derive(Debug, Default)]
struct Inner {
    /// Approval fingerprints whose consumed-marker could not be recorded.
    fingerprints: HashSet<String>,
    /// Task ids whose report marker could not be recorded.
    tasks: HashSet<String>,
    /// Task ids whose *escalation* marker could not be recorded.
    escalations: HashSet<String>,
    /// Task ids whose plan is being applied right now, by one leg or the other.
    applying: HashSet<String>,
    /// `<task id>\0<status>` pairs whose Gitea report is confirmed landed.
    reported: HashSet<String>,
    /// Consecutive failed applications of a task's plan.
    failures: HashMap<String, u32>,
    /// Task ids last seen holding [`crate::hermes::UNLISTABLE_STATUS`].
    unlisted: HashSet<String>,
}

impl BridgeState {
    /// A state with nothing suppressed.
    pub fn new() -> Self {
        Self::default()
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, Inner> {
        // A panic while holding this lock would otherwise turn a guard into an outage; the
        // data is a set of ids, so there is no invariant a poisoned guard could break.
        self.inner.lock().unwrap_or_else(|err| err.into_inner())
    }

    /// Stops this process acting on an approval whose consumption could not be recorded.
    ///
    /// Bounds the damage to one extra release across a restart rather than one per minute
    /// forever. A human can always re-approve: removing and re-adding the 🐝 produces a new
    /// fingerprint, which this set does not hold.
    pub fn suppress_fingerprint(&self, fingerprint: &str) {
        insert_capped(&mut self.lock().fingerprints, fingerprint, SUPPRESSION_CAP);
    }

    /// Whether this approval has been suppressed for the lifetime of this process.
    pub fn is_fingerprint_suppressed(&self, fingerprint: &str) -> bool {
        self.lock().fingerprints.contains(fingerprint)
    }

    /// Stops this process re-acting on a task whose report marker could not be recorded.
    ///
    /// Without it the unmarked task is exactly what the reconcile sweep looks for, so the
    /// user-visible Gitea comment is posted again on every sweep.
    pub fn suppress_task(&self, task_id: &str) {
        insert_capped(&mut self.lock().tasks, task_id, SUPPRESSION_CAP);
    }

    /// Whether this task has been suppressed for the lifetime of this process.
    pub fn is_task_suppressed(&self, task_id: &str) -> bool {
        self.lock().tasks.contains(task_id)
    }

    /// Stops this process re-posting an escalation comment whose marker could not be recorded.
    ///
    /// Deliberately *not* [`Self::suppress_task`]: that one retires the task from both the
    /// watch leg and the reconcile sweep, and the escalation comment this marker belongs to
    /// has just told the reader on the Gitea issue that "the bridge will keep retrying".
    /// Retiring the task there would make that a lie — push the missing branch, create the
    /// missing label, and nothing would pick it up until a restart. The escalation comment's
    /// own failure mode is far cheaper: without its marker it would be re-posted on every
    /// sweep, so this bounds it to the one that was already posted.
    pub fn suppress_escalation(&self, task_id: &str) {
        insert_capped(&mut self.lock().escalations, task_id, SUPPRESSION_CAP);
    }

    /// Whether this task's escalation comment has been suppressed for this process.
    pub fn is_escalation_suppressed(&self, task_id: &str) -> bool {
        self.lock().escalations.contains(task_id)
    }

    /// Claims a task for one application of its plan, or returns `None` if a leg holds it.
    ///
    /// The watch leg and the reconcile sweep run as independent tasks over the same
    /// `Arc<BridgeState>`, and the window between reading a task's detail and recording its
    /// marker spans a `create-pull` subprocess plus the comment plus the marker's retries — up
    /// to a couple of minutes. A sweep landing inside that window sees an unmarked task, plans
    /// the same actions and applies them: `create-pull` and the label are idempotent, but the
    /// reason comment is *not*, so the user's issue gets it twice.
    ///
    /// The guard releases on drop, so an early return or a panic cannot strand the claim.
    pub fn try_claim_apply(&self, task_id: &str) -> Option<ApplyGuard<'_>> {
        if !self.lock().applying.insert(task_id.to_string()) {
            return None;
        }
        Some(ApplyGuard {
            state: self,
            task_id: task_id.to_string(),
        })
    }

    /// Records that this task's Gitea report, in this status, is confirmed landed.
    ///
    /// Only that. The reconcile sweep's other cheap outcome — a task with *no* terminal event
    /// to report — must not come here, and the distinction is the whole point of the cache:
    ///
    /// * a durable kanban marker cannot un-write itself, so a task whose report is confirmed
    ///   cannot become unreported while it stands still;
    /// * a task with nothing to report can *acquire* something to report and come back to the
    ///   same status. A task created `blocked` to await a 🐝 is idle in `blocked`; the 🐝
    ///   releases it, a worker runs it, the worker blocks it, and it is `blocked` again — now
    ///   with a `blocked` event and nothing on the issue. Same shape for `ready`: idle,
    ///   claimed, crashed, and returned to `ready` by kanban's crash-reclaim.
    ///
    /// Caching that second case keyed on `(id, status)` blinded the sweep to those tasks for
    /// the lifetime of the process, and nothing invalidates this cache. So the sweep pays a
    /// `kanban show` per idle task per sweep instead. That cost is bounded by work in flight,
    /// whereas the history this cache exists for — `done` and `archived`, which accumulate for
    /// the lifetime of the board — always carries a terminal event and a durable marker, and
    /// so still lands here.
    ///
    /// The status stays part of the key: a task that *has* moved can have new work to report —
    /// a `done` task that later blocks still needs its block reported, and only
    /// [`crate::outbound::BLOCK_MARKER`] suppresses that.
    pub fn mark_reported(&self, task_id: &str, status: &str) {
        let mut inner = self.lock();
        if inner.reported.len() >= REPORTED_CAP {
            inner.reported.clear();
        }
        inner.reported.insert(reported_key(task_id, status));
    }

    /// Whether this task's report was confirmed landed *while holding this status*.
    pub fn is_reported(&self, task_id: &str, status: &str) -> bool {
        self.lock().reported.contains(&reported_key(task_id, status))
    }

    /// Counts one failed application of a task's plan and returns the running total.
    pub fn record_plan_failure(&self, task_id: &str) -> u32 {
        let mut inner = self.lock();
        if inner.failures.len() >= FAILURES_CAP && !inner.failures.contains_key(task_id) {
            inner.failures.clear();
        }
        let counter = inner.failures.entry(task_id.to_string()).or_insert(0);
        *counter = counter.saturating_add(1);
        *counter
    }

    /// Forgets a task's failures — its plan applied cleanly.
    pub fn clear_plan_failures(&self, task_id: &str) {
        self.lock().failures.remove(task_id);
    }

    /// Records that this task holds [`crate::hermes::UNLISTABLE_STATUS`].
    ///
    /// This set exists because a `list` is the wrong instrument for that status: on hermes
    /// v0.19.0 `hermes kanban list` *promotes* a blocked task by reading it, so a sweep that
    /// enumerated it released every task a worker had stopped for a human — the R7 P1. No
    /// sweep enumerates it now, which leaves the sweeps needing some *other* way to see a
    /// blocked task at all, or the 🐝 that unblocks one and the reconciliation of one whose
    /// Gitea report never landed would both simply stop happening.
    ///
    /// So the legs remember what they learn. Every `kanban show` in the daemon reports a
    /// status, and `show` does not promote (same probe), so the ids arrive for free from calls
    /// already being made — above all from the watch leg, which shows every task the moment it
    /// emits its `blocked` event. The sweeps then reach those tasks by id.
    ///
    /// It is in-process, and what that costs is worth stating plainly: **a restart forgets
    /// which tasks are blocked**, and nothing re-learns a task that is already blocked and
    /// therefore emits no further event. Such a task needs `hermes kanban unblock` by hand.
    /// That is a liveness cost, not a safety one — nothing is released without a 🐝, and no
    /// duplicate lands on a Gitea issue — and it is the strictly better half of the trade the
    /// alternative offered: releasing *every* blocked task on a timer, with no human at all.
    pub fn remember_unlisted(&self, task_id: &str) {
        insert_capped(&mut self.lock().unlisted, task_id, UNLISTED_CAP);
    }

    /// Forgets a task that is listable again — any status but the unlistable one.
    ///
    /// Called from the same places as [`Self::remember_unlisted`], on the other branch, so the
    /// set cannot accumulate tasks that moved on: a `show` reporting `ready` is proof that a
    /// `list` of that status will return the task, and reaching it by id as well would cost a
    /// second `show` per sweep forever.
    pub fn forget_unlisted(&self, task_id: &str) {
        self.lock().unlisted.remove(task_id);
    }

    /// The tasks the sweeps must reach by id, in id order.
    ///
    /// Sorted so a sweep's work is deterministic rather than dependent on hash iteration
    /// order, which is what makes a partial sweep — one cut short by a `show` failure — repeat
    /// the same prefix rather than a fresh random sample.
    pub fn unlisted(&self) -> Vec<String> {
        let mut ids: Vec<String> = self.lock().unlisted.iter().cloned().collect();
        ids.sort_unstable();
        ids
    }
}

/// Upper bound on the approval gate's held set.
///
/// Unlike the caches above, dropping this whole is *cheap and self-healing*: every held entry
/// is rebuilt from `GET /api/v1/robot/ready` by the next inbound sweep, because an issue whose
/// task has not been created is still ready. So the cap costs at most one ready-poll interval
/// of gate memory, and it is what stops a board whose issues are never approved (and are then
/// closed, so they never come back) from growing the file for the lifetime of the deployment.
const PENDING_CAP: usize = 10_000;

/// One ready Gitea issue held at the approval gate — everything needed to create its kanban
/// task later, without going back to the ready endpoint.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct PendingTask {
    /// Repository owner.
    pub owner: String,
    /// Repository name.
    pub repo: String,
    /// Issue index.
    pub index: i64,
    /// Task title — the Gitea issue title.
    pub title: String,
    /// Opening post, carrying the `gitea-ref:` trailer.
    pub body: String,
    /// `gitea:<owner>/<repo>#<index>`, and the key of this entry.
    pub idempotency_key: String,
}

/// Failures of the approval gate's durable store.
#[derive(Debug, thiserror::Error)]
pub enum PendingError {
    /// The state file could not be read.
    #[error("cannot read the approval gate state {path}: {source}")]
    Read {
        /// Path that failed to open.
        path: PathBuf,
        /// Underlying I/O error.
        #[source]
        source: std::io::Error,
    },
    /// The state file could not be written.
    #[error("cannot write the approval gate state {path}: {source}")]
    Write {
        /// Path that failed to write.
        path: PathBuf,
        /// Underlying I/O error.
        #[source]
        source: std::io::Error,
    },
    /// The state file is not the JSON the gate wrote.
    #[error("cannot parse the approval gate state {path}: {source}")]
    Parse {
        /// Path that failed to parse.
        path: PathBuf,
        /// Underlying JSON error.
        #[source]
        source: serde_json::Error,
    },
}

/// The approval gate: ready issues the bridge is deliberately *not* turning into kanban tasks.
///
/// The gate used to be a kanban status — inbound created the task with `--initial-status
/// blocked` and the 🐝 released it. That cannot work: on the deployed hermes (v0.19.0, probed
/// twice on 2026-08-04) **`hermes kanban list` promotes a blocked task by reading it**, both
/// plain and with `--status blocked`. `approval_sweep`'s very first call is a `kanban list`, so
/// the sweep looking for a task to release was itself what released every gated task to a
/// worker, with no human involved and nothing in the log to say so.
///
/// So the gate moved to the only side that can hold it: this one. Inbound does not create the
/// kanban task at all; it records the issue here, and the approval sweep creates the task the
/// moment an authorized 🐝 appears. Until then no task exists, so there is nothing for kanban
/// to promote, and no worker can claim work no board is carrying.
///
/// It is a file rather than a `HashSet` because it is the gate: `approval-once` must be able to
/// release an issue a *previous* process held, and a daemon that restarts between the reaction
/// and the next inbound sweep must still act on it. Losing the file is nevertheless not a
/// safety failure in either direction — nothing is released that was not approved, and the held
/// set is rebuilt by the next inbound sweep, because an issue with no task is still ready.
///
/// Both mutations write the whole map through a temporary file and a rename, so a crash mid-save
/// leaves the previous state rather than a truncated one. A save that fails after the in-memory
/// map moved is reported and tolerated: the pair (create, release) is idempotent — `create`
/// dedups on the idempotency key and the consumed markers are matched line-exactly — so a stale
/// entry replayed after a restart resolves to the same task and writes no second marker.
#[derive(Debug)]
pub struct PendingApprovals {
    path: PathBuf,
    held: Mutex<BTreeMap<String, PendingTask>>,
}

impl PendingApprovals {
    /// Loads the gate from `path`. A file that is not there yet is an empty gate, not an error.
    pub fn load(path: &Path) -> Result<Self, PendingError> {
        let held = match std::fs::read_to_string(path) {
            Ok(raw) if raw.trim().is_empty() => BTreeMap::new(),
            Ok(raw) => serde_json::from_str(&raw).map_err(|source| PendingError::Parse {
                path: path.to_path_buf(),
                source,
            })?,
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => BTreeMap::new(),
            Err(source) => {
                return Err(PendingError::Read {
                    path: path.to_path_buf(),
                    source,
                });
            }
        };
        Ok(Self {
            path: path.to_path_buf(),
            held: Mutex::new(held),
        })
    }

    /// An in-memory gate, for tests and for the `check` path that only reports its size.
    pub fn ephemeral() -> Self {
        Self {
            path: PathBuf::new(),
            held: Mutex::new(BTreeMap::new()),
        }
    }

    /// The file this gate persists to.
    pub fn path(&self) -> &Path {
        &self.path
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, BTreeMap<String, PendingTask>> {
        self.held.lock().unwrap_or_else(|err| err.into_inner())
    }

    /// Holds one ready issue at the gate. Returns whether it was not already held.
    ///
    /// Re-holding an issue is the ordinary steady state, not an anomaly: an issue whose task
    /// has not been created is still ready, so every inbound sweep offers it again.
    pub fn hold(&self, entry: PendingTask) -> Result<bool, PendingError> {
        let mut held = self.lock();
        let key = entry.idempotency_key.clone();
        if held.len() >= PENDING_CAP && !held.contains_key(&key) {
            held.clear();
        }
        let fresh = held.insert(key, entry).is_none();
        self.persist(&held)?;
        Ok(fresh)
    }

    /// Releases one issue from the gate — its kanban task now exists.
    ///
    /// Returns whether it was held. Persisting is attempted even when it was not, so a caller
    /// that retries after a failed save converges.
    pub fn release(&self, idempotency_key: &str) -> Result<bool, PendingError> {
        let mut held = self.lock();
        let was_held = held.remove(idempotency_key).is_some();
        self.persist(&held)?;
        Ok(was_held)
    }

    /// Everything held right now, in idempotency-key order.
    pub fn held(&self) -> Vec<PendingTask> {
        self.lock().values().cloned().collect()
    }

    /// How many issues are waiting on a 🐝.
    pub fn len(&self) -> usize {
        self.lock().len()
    }

    /// Whether nothing is waiting on a 🐝.
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    /// Writes the whole map through a temporary file and a rename.
    ///
    /// Whole-file rather than append-only because the map is small and bounded by
    /// [`PENDING_CAP`], and rename-based because a half-written gate that failed to parse would
    /// take the daemon down on its next start.
    fn persist(&self, held: &BTreeMap<String, PendingTask>) -> Result<(), PendingError> {
        if self.path.as_os_str().is_empty() {
            return Ok(());
        }
        let write_err = |source| PendingError::Write {
            path: self.path.clone(),
            source,
        };
        if let Some(parent) = self.path.parent().filter(|p| !p.as_os_str().is_empty()) {
            std::fs::create_dir_all(parent).map_err(write_err)?;
        }
        let raw = serde_json::to_string_pretty(held).map_err(|source| PendingError::Parse {
            path: self.path.clone(),
            source,
        })?;
        let tmp = self.path.with_extension("tmp");
        std::fs::write(&tmp, raw).map_err(write_err)?;
        std::fs::rename(&tmp, &self.path).map_err(write_err)?;
        Ok(())
    }
}

/// Adds one entry to a bounded set — [`SUPPRESSION_CAP`] or [`UNLISTED_CAP`].
///
/// Dropped whole at the cap rather than evicted one by one: there is no recency to evict on —
/// every entry is equally permanent by design — and dropping whole makes the cost the same
/// bounded one a restart already has.
fn insert_capped(set: &mut HashSet<String>, value: &str, cap: usize) {
    if set.len() >= cap && !set.contains(value) {
        set.clear();
    }
    set.insert(value.to_string());
}

/// Cache key. `\0` cannot occur in a kanban id or status, so it cannot be spelled by data.
fn reported_key(task_id: &str, status: &str) -> String {
    format!("{task_id}\0{status}")
}

/// Holds one task's apply-claim for as long as its plan is being applied.
///
/// See [`BridgeState::try_claim_apply`]. Released on drop so no path out of the apply — an
/// early return, an error, a panic — can leak the claim and wedge the task for the process.
#[derive(Debug)]
pub struct ApplyGuard<'a> {
    state: &'a BridgeState,
    task_id: String,
}

impl Drop for ApplyGuard<'_> {
    fn drop(&mut self) {
        self.state.lock().applying.remove(&self.task_id);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The approval loop this closes: a released task whose consumed-marker write failed is
    /// released again by the same 🐝 on every sweep, forever, and invisibly.
    #[test]
    fn a_suppressed_fingerprint_is_not_acted_on_again() {
        let state = BridgeState::new();
        assert!(!state.is_fingerprint_suppressed("alex@2026-08-04T16:23:00Z"));
        state.suppress_fingerprint("alex@2026-08-04T16:23:00Z");
        assert!(state.is_fingerprint_suppressed("alex@2026-08-04T16:23:00Z"));

        // A fresh 🐝 — removed and re-added — is a different fingerprint, so a human can
        // still re-approve the task without restarting the daemon.
        assert!(!state.is_fingerprint_suppressed("alex@2026-08-05T09:00:00Z"));
        // …and so is another maintainer's.
        assert!(!state.is_fingerprint_suppressed("bo@2026-08-04T16:23:00Z"));
    }

    /// The outbound loop this closes: an unmarked task is what the reconcile sweep looks
    /// for, so a failed marker write reposts the Gitea comment every 300s.
    #[test]
    fn a_suppressed_task_is_not_replayed_again() {
        let state = BridgeState::new();
        assert!(!state.is_task_suppressed("t_1"));
        state.suppress_task("t_1");
        assert!(state.is_task_suppressed("t_1"));
        assert!(!state.is_task_suppressed("t_2"));
    }

    /// The escalation comment says on the user's issue that the bridge will keep retrying, so
    /// a failed escalation *marker* must not retire the task from both legs the way a failed
    /// *plan* marker does. The two suppressions are separate sets for exactly that reason.
    #[test]
    fn a_suppressed_escalation_does_not_retire_the_task_itself() {
        let state = BridgeState::new();
        state.suppress_escalation("t_1");
        assert!(state.is_escalation_suppressed("t_1"));
        // …and the task stays in both legs: push the missing branch or create the missing
        // label and the original plan still applies on the next sweep.
        assert!(!state.is_task_suppressed("t_1"));
        // The converse too — dead-lettering a task says nothing about its escalation.
        state.suppress_task("t_2");
        assert!(!state.is_escalation_suppressed("t_2"));
    }

    /// The watch leg and the reconcile sweep share this state and can reach the same task's
    /// plan at once; `create-pull` and the label are idempotent but the reason comment is not.
    #[test]
    fn only_one_leg_at_a_time_can_apply_a_task_s_plan() {
        let state = BridgeState::new();
        let held = state.try_claim_apply("t_1").expect("the first claim wins");
        assert!(
            state.try_claim_apply("t_1").is_none(),
            "the second leg must not apply the same plan concurrently"
        );
        // A different task is unaffected — the guard is per task, not a global lock.
        assert!(state.try_claim_apply("t_2").is_some());
        drop(held);
        assert!(
            state.try_claim_apply("t_1").is_some(),
            "the claim must be released on drop, or one apply wedges the task for the process"
        );
    }

    #[test]
    fn a_reported_task_is_cached_per_status_not_per_id() {
        let state = BridgeState::new();
        state.mark_reported("t_1", "done");
        assert!(state.is_reported("t_1", "done"));
        // A task that moved may have a new terminal event to report — a completed task that
        // later blocks needs its block reported, and the PR marker does not suppress that.
        assert!(!state.is_reported("t_1", "blocked"));
        assert!(!state.is_reported("t_2", "done"));
    }

    #[test]
    fn the_reported_cache_is_bounded() {
        let state = BridgeState::new();
        for i in 0..REPORTED_CAP + 10 {
            state.mark_reported(&format!("t_{i}"), "done");
        }
        assert!(state.lock().reported.len() <= REPORTED_CAP);
    }

    #[test]
    fn plan_failures_count_up_and_reset_on_success() {
        let state = BridgeState::new();
        assert_eq!(state.record_plan_failure("t_1"), 1);
        assert_eq!(state.record_plan_failure("t_1"), 2);
        assert_eq!(state.record_plan_failure("t_2"), 1);
        state.clear_plan_failures("t_1");
        assert_eq!(state.record_plan_failure("t_1"), 1);
    }

    /// Nothing ever removes a suppression, so without a bound these three sets are the one
    /// board-scaled structure in the module with no ceiling — the asymmetry with the two
    /// caches above, which are capped for exactly this reason.
    #[test]
    fn the_suppression_sets_are_bounded() {
        let state = BridgeState::new();
        for i in 0..SUPPRESSION_CAP + 10 {
            state.suppress_fingerprint(&format!("alex@{i}"));
            state.suppress_task(&format!("t_{i}"));
            state.suppress_escalation(&format!("t_{i}"));
        }
        let inner = state.lock();
        assert!(inner.fingerprints.len() <= SUPPRESSION_CAP);
        assert!(inner.tasks.len() <= SUPPRESSION_CAP);
        assert!(inner.escalations.len() <= SUPPRESSION_CAP);
        drop(inner);

        // …and the bound is the only thing that changed: a suppression made after the cap was
        // hit still suppresses.
        state.suppress_task("t_0");
        assert!(state.is_task_suppressed("t_0"));
    }

    /// The R7 P1's other half: with no sweep enumerating `blocked`, this set is the only way
    /// a blocked task is reachable at all, so it has to hold exactly the tasks that are
    /// blocked *now* — no more (a stale id costs a `show` per sweep forever) and no fewer (a
    /// missing id is a task no 🐝 can release and no sweep can reconcile).
    #[test]
    fn a_task_is_reachable_by_id_for_exactly_as_long_as_it_is_blocked() {
        let state = BridgeState::new();
        assert!(state.unlisted().is_empty());

        state.remember_unlisted("t_2");
        state.remember_unlisted("t_1");
        assert_eq!(state.unlisted(), vec!["t_1", "t_2"], "id order, not hash order");
        // Remembering twice is the steady state — every sweep re-shows a task that is still
        // blocked — and must not double it up.
        state.remember_unlisted("t_1");
        assert_eq!(state.unlisted().len(), 2);

        // Unblocked, or run, or completed: whatever moved it, a `list` reaches it again, and
        // paying a second `show` for it every sweep forever is exactly what this avoids.
        state.forget_unlisted("t_1");
        assert_eq!(state.unlisted(), vec!["t_2"]);
        // Forgetting one that was never held is a no-op: `forget` runs on every non-blocked
        // `show` in the daemon, which is most of them.
        state.forget_unlisted("t_9");
        assert_eq!(state.unlisted(), vec!["t_2"]);
    }

    #[test]
    fn the_unlisted_set_is_bounded() {
        let state = BridgeState::new();
        for i in 0..UNLISTED_CAP + 10 {
            state.remember_unlisted(&format!("t_{i}"));
        }
        assert!(state.unlisted().len() <= UNLISTED_CAP);
        // …and the bound is the only thing that changed.
        state.remember_unlisted("t_0");
        assert!(state.unlisted().contains(&"t_0".to_string()));
    }

    fn pending(index: i64) -> PendingTask {
        PendingTask {
            owner: "terraphim".into(),
            repo: "gitea".into(),
            index,
            title: "automations daemon".into(),
            body: format!("gitea-ref: terraphim/gitea#{index}"),
            idempotency_key: format!("gitea:terraphim/gitea#{index}"),
        }
    }

    /// The R6 P1 this closes: the gate cannot live in a kanban status, because `hermes kanban
    /// list` promotes a blocked task by reading it — so the sweep looking for a task to release
    /// released every one of them. The gate is this file instead, and the property it has to
    /// have is that a *restart* does not open it.
    #[test]
    fn the_approval_gate_survives_a_restart() {
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("state").join("bridge-state.json");

        let gate = PendingApprovals::load(&path).expect("an absent file is an empty gate");
        assert!(gate.is_empty());
        assert!(gate.hold(pending(57)).expect("holds"), "newly held");
        assert!(
            !gate.hold(pending(57)).expect("holds"),
            "an issue with no task is still ready, so every inbound sweep re-offers it"
        );
        gate.hold(pending(58)).expect("holds");

        // The moral equivalent of a restart: nothing of the gate is in this process any more.
        let reloaded = PendingApprovals::load(&path).expect("reloads");
        assert_eq!(reloaded.len(), 2);
        assert_eq!(
            reloaded.held().iter().map(|p| p.index).collect::<Vec<_>>(),
            vec![57, 58],
            "held in idempotency-key order, and carrying what `create` needs"
        );
        assert_eq!(reloaded.held()[0].body, "gitea-ref: terraphim/gitea#57");

        // Releasing is what a 🐝 does, and it is durable too — otherwise the next sweep would
        // re-create the task it just created.
        assert!(reloaded.release("gitea:terraphim/gitea#57").expect("releases"));
        assert!(
            !reloaded.release("gitea:terraphim/gitea#57").expect("releases"),
            "releasing twice is a no-op, not an error: a save that failed is retried"
        );
        let again = PendingApprovals::load(&path).expect("reloads");
        assert_eq!(again.len(), 1);
        assert_eq!(again.held()[0].index, 58);
    }

    #[test]
    fn a_corrupt_gate_is_a_named_error_rather_than_an_open_gate() {
        // Failing closed is not an option here — there is nothing to fail closed *to*, since
        // the gate holds work that has not been created. So it must be loud: an unparsable
        // file that silently became an empty gate would re-create every held task on the next
        // 🐝 sweep and forget which issues were waiting.
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("bridge-state.json");
        std::fs::write(&path, "{not json").expect("write");
        let err = PendingApprovals::load(&path).expect_err("must not decode");
        assert!(matches!(err, PendingError::Parse { .. }), "{err:?}");
        assert!(err.to_string().contains("bridge-state.json"), "{err}");

        // An empty file is an empty gate: that is what a half-finished first write looks like.
        std::fs::write(&path, "  \n").expect("write");
        assert!(PendingApprovals::load(&path).expect("loads").is_empty());
    }

    /// Same argument as the suppression sets, with one extra property that makes the cap free:
    /// every dropped entry is rebuilt by the next inbound sweep, because an issue with no task
    /// is still ready.
    #[test]
    fn the_approval_gate_is_bounded() {
        let dir = tempfile::tempdir().expect("tempdir");
        let gate = PendingApprovals::load(&dir.path().join("bridge-state.json")).expect("loads");
        for i in 0..PENDING_CAP + 10 {
            gate.hold(pending(i as i64)).expect("holds");
        }
        assert!(gate.len() <= PENDING_CAP);
        // …and the bound is the only thing that changed.
        gate.hold(pending(1)).expect("holds");
        assert!(gate.held().iter().any(|p| p.index == 1));
    }

    /// Counters are only removed on success, so the permanently-failing tasks — the ones this
    /// map exists for — are exactly the entries that would never leave it.
    #[test]
    fn the_failure_counters_are_bounded() {
        let state = BridgeState::new();
        for i in 0..FAILURES_CAP + 10 {
            state.record_plan_failure(&format!("t_{i}"));
        }
        assert!(state.lock().failures.len() <= FAILURES_CAP);
    }
}
