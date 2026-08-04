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
//! The `settled` cache is the same idea pointed at cost rather than correctness: a task whose
//! Gitea feedback is confirmed done — or which has nothing to report at all — cannot become
//! unsettled while it stands still, so the reconcile sweep need not pay a `kanban show`
//! subprocess for it on every sweep for the lifetime of the board.

use std::collections::{HashMap, HashSet};
use std::sync::Mutex;

/// Attempts a marker write gets before it is treated as a dead letter.
pub const MARKER_ATTEMPTS: u32 = 3;

/// Consecutive failed applications of one task's plan before the failure is escalated to the
/// Gitea issue itself.
pub const ESCALATE_AFTER: u32 = 3;

/// Upper bound on the settled-task cache.
///
/// One short string per settled task is nothing beside the subprocess it saves, but the board
/// grows for its whole lifetime and this daemon is meant to run for months. At the cap the
/// cache is dropped whole: the next sweep is as expensive as an unwarmed one and then warms
/// again, which is a bounded cost, whereas retaining the map is not.
const SETTLED_CAP: usize = 50_000;

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
    /// `<task id>\0<status>` pairs the reconcile sweep has nothing left to do for.
    settled: HashSet<String>,
    /// Consecutive failed applications of a task's plan.
    failures: HashMap<String, u32>,
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
        self.lock().fingerprints.insert(fingerprint.to_string());
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
        self.lock().tasks.insert(task_id.to_string());
    }

    /// Whether this task has been suppressed for the lifetime of this process.
    pub fn is_task_suppressed(&self, task_id: &str) -> bool {
        self.lock().tasks.contains(task_id)
    }

    /// Records that this task, in this status, needs nothing further from the sweep.
    ///
    /// Either its terminal event is confirmed reported to Gitea, or it has no terminal event
    /// to report at all.
    pub fn mark_settled(&self, task_id: &str, status: &str) {
        let mut inner = self.lock();
        if inner.settled.len() >= SETTLED_CAP {
            inner.settled.clear();
        }
        inner.settled.insert(settled_key(task_id, status));
    }

    /// Whether this task was settled *while holding this status*.
    ///
    /// The status is part of the key on purpose. A durable marker cannot un-write itself and
    /// an event trail cannot un-happen, so a task that has not moved cannot have become
    /// unsettled — but a task that *has* moved can have new work to report: a `done` task
    /// that later blocks still needs its block reported, and only
    /// [`crate::outbound::BLOCK_MARKER`] suppresses that. So the cache must miss when the
    /// status changes, and every path from one terminal event to the next passes through a
    /// different status (kanban refuses to block an already-blocked task, so a second block
    /// is reached only via `unblock`).
    pub fn is_settled(&self, task_id: &str, status: &str) -> bool {
        self.lock().settled.contains(&settled_key(task_id, status))
    }

    /// Counts one failed application of a task's plan and returns the running total.
    pub fn record_plan_failure(&self, task_id: &str) -> u32 {
        let mut inner = self.lock();
        let counter = inner.failures.entry(task_id.to_string()).or_insert(0);
        *counter = counter.saturating_add(1);
        *counter
    }

    /// Forgets a task's failures — its plan applied cleanly.
    pub fn clear_plan_failures(&self, task_id: &str) {
        self.lock().failures.remove(task_id);
    }
}

/// Cache key. `\0` cannot occur in a kanban id or status, so it cannot be spelled by data.
fn settled_key(task_id: &str, status: &str) -> String {
    format!("{task_id}\0{status}")
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

    #[test]
    fn a_settled_task_is_cached_per_status_not_per_id() {
        let state = BridgeState::new();
        state.mark_settled("t_1", "done");
        assert!(state.is_settled("t_1", "done"));
        // A task that moved may have a new terminal event to report — a completed task that
        // later blocks needs its block reported, and the PR marker does not suppress that.
        assert!(!state.is_settled("t_1", "blocked"));
        assert!(!state.is_settled("t_2", "done"));
    }

    #[test]
    fn the_settled_cache_is_bounded() {
        let state = BridgeState::new();
        for i in 0..SETTLED_CAP + 10 {
            state.mark_settled(&format!("t_{i}"), "done");
        }
        assert!(state.lock().settled.len() <= SETTLED_CAP);
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
}
