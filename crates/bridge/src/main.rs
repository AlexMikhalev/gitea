// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//! `gitea-automations` — the F4 bridge daemon.
//!
//! Four concurrent legs:
//!
//! * inbound — poll ready issues; hold them at the approval gate, or create their kanban
//!   tasks directly when the gate is off (deduplicated by idempotency key);
//! * outbound — watch the five terminal kanban events, return PRs / blocks to Gitea;
//! * approval — poll 🐝 reactions; create the task a human blessed, and release the tasks
//!   kanban itself blocked (the latter by id, never by a `list`: see
//!   [`bridge::hermes::UNLISTABLE_STATUS`]);
//! * reconcile — replay the terminal tasks whose Gitea feedback never landed.
//!
//! Nothing here owns liveness. Claims, heartbeats, reclaim and the circuit breaker are
//! kanban's, so killing this process orphans nothing and restarting it duplicates nothing.
//! The one piece of durable state this process does own is the approval gate — see
//! [`bridge::state::PendingApprovals`], and the reason it cannot be kanban's.

use std::path::PathBuf;
use std::sync::Arc;

use anyhow::{Context, Result};
use clap::{Parser, Subcommand};

use bridge::approval::{Approval, ApprovalOutcome, Preflight, evaluate, preflight};
use bridge::config::{Config, RepoRef};
use bridge::gitea::GiteaClient;
use bridge::hermes::{CreateTask, Kanban, RECONCILE_STATUSES, Task, TaskDetail, UNLISTABLE_STATUS};
use bridge::inbound::{GiteaRef, poll_once};
use bridge::outbound::{
    OutboundPlan, PlanError, PlannedAction, TerminalKind, WatchLine, classify_watch_line, escalation_actions,
    latest_terminal_kind, plan,
};
use bridge::robot::{LabelCheck, Robot, check_blocked_label};
use bridge::rules::{Action, RuleSet};
use bridge::state::{BridgeState, ESCALATE_AFTER, MARKER_ATTEMPTS, PendingApprovals};

/// Bridge between the Gitea shared board and the Hermes kanban execution fabric.
#[derive(Debug, Parser)]
#[command(name = "gitea-automations", version, about)]
struct Cli {
    /// Path to the daemon config.
    #[arg(short, long, default_value = "bridge.yaml")]
    config: PathBuf,

    /// Log filter, e.g. `info`, `bridge=debug`.
    #[arg(long, default_value = "info", env = "GITEA_AUTOMATIONS_LOG")]
    log: String,

    #[command(subcommand)]
    command: Command,
}

#[derive(Debug, Subcommand)]
enum Command {
    /// Run all four legs until interrupted.
    Run,
    /// Run one inbound sweep and exit. Safe to repeat: the idempotency key dedups.
    PollOnce,
    /// Run one approval sweep and exit.
    ApprovalOnce,
    /// Run one reconciliation sweep and exit: re-drive terminal tasks whose Gitea feedback
    /// never landed. Safe to repeat — the marker comments make it a no-op once it has.
    ReconcileOnce,
    /// Validate the config (and the rules file, when one is configured) and exit.
    Check,
    /// Validate a rules file and print the actions it uses.
    CheckRules {
        /// Rules file to validate.
        path: PathBuf,
    },
}

#[tokio::main]
async fn main() -> Result<()> {
    let cli = Cli::parse();
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::new(&cli.log))
        .with_target(false)
        .init();

    // `check-rules` deliberately needs no daemon config: an operator must be able to
    // validate a rules file before the bridge is configured at all.
    if let Command::CheckRules { path } = &cli.command {
        let set = RuleSet::load(path).with_context(|| format!("rules file {}", path.display()))?;
        for rule in &set.rules {
            println!("{}\t{}\t{}", rule.name, rule.trigger.kind(), rule.action.name());
        }
        println!("{} rule(s) OK", set.rules.len());
        println!("{}", RuleSet::VALIDATION_ONLY_NOTICE);
        return Ok(());
    }

    let cfg = Config::load(&cli.config).with_context(|| format!("config {}", cli.config.display()))?;
    if let Some(path) = &cfg.rules_file {
        RuleSet::load(path).with_context(|| format!("rules file {}", path.display()))?;
    }

    let gitea = GiteaClient::new(&cfg.gitea).context("building the gitea client")?;
    let kanban = Kanban::new(&cfg.kanban);
    let robot = Robot::new(&cfg.robot);
    // Loaded before anything runs, and fatal if it will not load: this file *is* the approval
    // gate, so a corrupt one that quietly became an empty gate would forget which issues are
    // waiting on a human and re-create them all on the next 🐝 sweep.
    let pending = Arc::new(
        PendingApprovals::load(&cfg.state_file)
            .with_context(|| format!("approval gate state {}", cfg.state_file.display()))?,
    );
    // `None` is what turns the gate off, so the branch is taken once, here, rather than
    // re-derived per sweep.
    let gate = cfg.kanban.require_approval.then(|| pending.clone());

    match cli.command {
        Command::CheckRules { .. } => unreachable!("handled above"),
        Command::Check => {
            // The cap is stated because it is the only bound on how much work the daemon puts
            // in flight: the ready endpoint is unpaged, so everything downstream of inbound
            // costs whatever this number is, per repository, per sweep, indefinitely.
            println!(
                "config OK: {} repo(s), board {:?}, at most {} task(s) per inbound sweep per repo",
                cfg.repos.len(),
                cfg.kanban.board,
                cfg.poll.max_tasks_per_sweep
            );
            // The daemon cannot read app.ini, and an unconfigured reaction is invisible
            // rather than rejected, so this is the only place it can be said out loud.
            println!(
                "reminder: `{}` must appear in [ui] REACTIONS in app.ini, or the reaction is \
                 invisible to the API rather than merely unwritable",
                cfg.approval.reaction
            );
            if let Some(path) = &cfg.rules_file {
                println!(
                    "rules file {} OK — {}",
                    path.display(),
                    RuleSet::VALIDATION_ONLY_NOTICE
                );
            }
            // Whether a human sees an issue before an agent does hangs entirely on this one
            // setting, and getting it wrong is silent in both directions — so it is stated
            // rather than assumed, along with *where* the gate is, because that is the part
            // an operator would otherwise have to read the source to learn.
            if cfg.kanban.require_approval {
                println!(
                    "approval gate ON (held by this bridge, not by kanban): a ready issue is \
                     recorded in {} and NO kanban task is created for it until a {:?} reaction \
                     from a writer arrives; {} issue(s) held right now",
                    cfg.state_file.display(),
                    cfg.approval.reaction,
                    pending.len()
                );
                println!(
                    "note: the gate cannot be a kanban status — on hermes v0.19.0 `hermes kanban \
                     list` promotes a blocked task by reading it, and the approval sweep's first \
                     call is a `kanban list`, so a task created --initial-status blocked was \
                     released by the very sweep looking for one to release. Creating nothing is \
                     what closes that: there is no task for kanban to promote"
                );
            } else {
                println!(
                    "warning: kanban.require_approval is off — every ready issue becomes a kanban \
                     task at once, and `hermes kanban create` defaults to `ready`, so an agent is \
                     dispatched with no human approval. The 🐝 leg then only ever sees tasks \
                     kanban itself blocked"
                );
            }
            // Independent of the gate, and stated for the same reason: it is a rule about
            // *this daemon's* reach that an operator cannot infer from the board. A blocked
            // task the daemon did not watch block is one no 🐝 will move.
            println!(
                "note: no sweep lists {UNLISTABLE_STATUS:?} — on hermes v0.19.0 `hermes kanban \
                 list --status {UNLISTABLE_STATUS}` promotes what it lists, so a timed sweep \
                 was releasing every task a worker had stopped for a human. Blocked tasks are \
                 reached by id instead, which is in-process: after a restart, a task that was \
                 already blocked needs `hermes kanban unblock <id>` by hand"
            );
            // Not just "is a token set": a token that is neither site admin nor an admin
            // of the repo cannot query anybody else's permission at all
            // (`routers/api/v1/repo/collaborators.go:279`), and the only symptom would be
            // every 🐝 resolving to Undetermined at runtime.
            match preflight(&gitea, &cfg.approval, &cfg.repos).await {
                Preflight::Usable { reason } => println!("approval OK: {reason}"),
                Preflight::Unusable { reason } => {
                    println!("warning: approvals will fail closed — {reason}")
                }
            }
            // The label is action 0 of every non-`completed` plan and `edit-issue` fails hard
            // on one the repository does not have, so a missing label does not merely skip the
            // label: it stops the reason comment behind it from ever being posted. Nothing in
            // the bridge creates repository labels, so this is the only place it can be said
            // before the fact rather than once per sweep in the daemon's log.
            match check_blocked_label(&gitea, robot.blocked_label(), &cfg.repos).await {
                LabelCheck::Present => println!(
                    "blocked label OK: {:?} exists in every configured repository",
                    robot.blocked_label()
                ),
                LabelCheck::Missing { reason } => println!("warning: {reason}"),
                LabelCheck::Unknown { reason } => {
                    println!("warning: cannot verify robot.blocked_label — {reason}")
                }
            }
            Ok(())
        }
        Command::PollOnce => {
            for repo in &cfg.repos {
                inbound_sweep(&gitea, &kanban, &cfg, repo, gate.as_deref()).await;
            }
            Ok(())
        }
        Command::ApprovalOnce => {
            approval_sweep(&gitea, &kanban, &cfg, &BridgeState::new(), &pending).await;
            Ok(())
        }
        Command::ReconcileOnce => {
            reconcile_sweep(&cfg, &kanban, &robot, &BridgeState::new()).await;
            Ok(())
        }
        Command::Run => run(Arc::new(cfg), gitea, kanban, robot, pending, gate).await,
    }
}

async fn run(
    cfg: Arc<Config>,
    gitea: GiteaClient,
    kanban: Kanban,
    robot: Robot,
    pending: Arc<PendingApprovals>,
    gate: Option<Arc<PendingApprovals>>,
) -> Result<()> {
    // Said once at startup rather than once per reaction. A token that cannot query
    // permissions leaves the approval leg permanently dead while every other leg, and the
    // process itself, looks healthy.
    match preflight(&gitea, &cfg.approval, &cfg.repos).await {
        Preflight::Usable { reason } => tracing::info!(%reason, "approval preflight passed"),
        Preflight::Unusable { reason } => tracing::warn!(
            %reason,
            "approval preflight failed: every approval reaction will resolve to Undetermined"
        ),
    }

    // Said once at startup for the same reason: a `status/blocked` the repository does not
    // have fails the *first* action of every blocked/gave_up/crashed/timed_out plan, so the
    // reason comment behind it never posts and the issue stays silent. The daemon still runs —
    // `completed` is unaffected, and the escalation comment reports the failure on the issue
    // once it has retried — but this is the line that says why.
    match check_blocked_label(&gitea, robot.blocked_label(), &cfg.repos).await {
        LabelCheck::Present => {
            tracing::info!(label = robot.blocked_label(), "blocked-label preflight passed")
        }
        LabelCheck::Missing { reason } => tracing::error!(
            %reason,
            "blocked-label preflight failed: every non-completed terminal event will fail at its \
             first action until the label exists"
        ),
        LabelCheck::Unknown { reason } => {
            tracing::warn!(%reason, "cannot verify robot.blocked_label")
        }
    }

    // The third startup disclosure, and the one with no server-side probe behind it: whether a
    // human sees an issue before an agent does. It is said out loud because the gate is *not*
    // where a reader of `hermes kanban` would look for it — a kanban status cannot hold it, so
    // this daemon holds it, and the file it holds it in is the thing to back up and to watch.
    if cfg.kanban.require_approval {
        tracing::info!(
            state_file = %cfg.state_file.display(),
            held = pending.len(),
            "approval gate ON: no kanban task is created for a ready issue until an authorized \
             🐝 lands. The hold is bridge-side because `hermes kanban list` promotes a blocked \
             task by reading it (hermes v0.19.0), which is the approval sweep's own first call"
        );
    } else {
        tracing::warn!(
            "kanban.require_approval is off: every ready issue becomes a `ready` kanban task at \
             once and is dispatched to an agent with no human approval"
        );
    }

    let mut inbound = {
        let (cfg, gitea, kanban, gate) = (cfg.clone(), gitea.clone(), kanban.clone(), gate.clone());
        tokio::spawn(async move {
            let mut ticker = interval(cfg.ready_interval());
            loop {
                ticker.tick().await;
                for repo in &cfg.repos {
                    inbound_sweep(&gitea, &kanban, &cfg, repo, gate.as_deref()).await;
                }
            }
        })
    };

    // The only in-process state in the daemon, shared by the three legs that write durable
    // markers. It holds no truth of its own — kanban stays the durable side — but it is what
    // stops a marker write that failed *after its retries* from becoming an endless loop.
    let state = Arc::new(BridgeState::new());

    let mut approval = {
        let (cfg, gitea, kanban, state, pending) = (
            cfg.clone(),
            gitea.clone(),
            kanban.clone(),
            state.clone(),
            pending.clone(),
        );
        tokio::spawn(async move {
            let mut ticker = interval(cfg.approval_interval());
            loop {
                ticker.tick().await;
                approval_sweep(&gitea, &kanban, &cfg, &state, &pending).await;
            }
        })
    };

    let mut outbound = {
        let (cfg, kanban, robot, state) = (cfg.clone(), kanban.clone(), robot.clone(), state.clone());
        tokio::spawn(async move { outbound_loop(cfg, kanban, robot, state).await })
    };

    // The fourth leg, and the one that makes the other three's failure modes recoverable.
    // `watch` is a live stream and a terminal event fires exactly once, so a single failed
    // `gitea-robot` call — or a daemon that was down when the event fired — would otherwise
    // drop that issue's feedback for good.
    let mut reconcile = {
        let (cfg, kanban, robot, state) = (cfg.clone(), kanban.clone(), robot.clone(), state.clone());
        tokio::spawn(async move {
            let mut ticker = interval(cfg.reconcile_interval());
            loop {
                ticker.tick().await;
                reconcile_sweep(&cfg, &kanban, &robot, &state).await;
            }
        })
    };

    // Each leg is an endless loop, so *any* completion is a panic or an abort — and none of
    // them is visible from outside: the process stays up, reports nothing, and a dead outbound
    // or reconcile leg looks exactly like an idle board. That is the same failure
    // `outbound_loop` guards against internally, so it is guarded around as well. Exiting
    // non-zero hands the restart to the supervisor, which is the only thing here that can
    // actually rebuild the leg's state.
    let died = tokio::select! {
        signal = tokio::signal::ctrl_c() => {
            signal.context("waiting for ctrl-c")?;
            None
        }
        outcome = &mut inbound => Some(("inbound", outcome)),
        outcome = &mut approval => Some(("approval", outcome)),
        outcome = &mut outbound => Some(("outbound", outcome)),
        outcome = &mut reconcile => Some(("reconcile", outcome)),
    };
    inbound.abort();
    approval.abort();
    outbound.abort();
    reconcile.abort();
    match died {
        None => {
            tracing::info!("shutting down; kanban keeps every claim and heartbeat");
            Ok(())
        }
        Some((leg, outcome)) => {
            let how = match &outcome {
                Ok(()) => "returned".to_string(),
                Err(err) if err.is_panic() => "panicked".to_string(),
                Err(err) => format!("ended ({err})"),
            };
            tracing::error!(leg, how, "a bridge leg stopped; shutting the daemon down");
            Err(anyhow::anyhow!(
                "the {leg} leg {how} and cannot be resumed in place; exiting so the supervisor \
                 restarts the daemon rather than leaving it running with a dead leg"
            ))
        }
    }
}

/// A ticker that does **not** burst.
///
/// [`tokio::time::interval`] defaults to [`tokio::time::MissedTickBehavior::Burst`], which replays every
/// tick a slow sweep overran back to back with no delay. An unwarmed reconcile sweep is nine
/// `kanban list` calls plus a `kanban show` subprocess per unsettled task, so overrunning is
/// most likely exactly when the board is already slow — and bursting answers that by stacking
/// more subprocesses onto it. Delaying is the intended semantics for every timed leg: the
/// sweeps are idempotent, so a skipped tick costs nothing.
fn interval(period: std::time::Duration) -> tokio::time::Interval {
    let mut ticker = tokio::time::interval(period);
    ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    ticker
}

async fn inbound_sweep(
    gitea: &GiteaClient,
    kanban: &Kanban,
    cfg: &Config,
    repo: &RepoRef,
    gate: Option<&PendingApprovals>,
) {
    match poll_once(
        gitea,
        kanban,
        repo,
        &cfg.gitea.base_url,
        cfg.poll.skip_in_progress,
        cfg.poll.max_tasks_per_sweep,
        gate,
    )
    .await
    {
        Ok(report) => tracing::info!(
            repo = %repo.slug(),
            resolved = report.resolved.len(),
            held = report.held.len(),
            failed = report.failed.len(),
            deferred = report.deferred,
            "inbound sweep complete"
        ),
        Err(err) => tracing::error!(repo = %repo.slug(), error = %err, "inbound sweep failed"),
    }
}

/// The 🐝 leg: both halves of it.
///
/// 1. [`gate_sweep`] — the ready issues this bridge is *holding*, which have no kanban task at
///    all. An authorized 🐝 is what creates one. This is the approval gate.
/// 2. [`release_sweep`] — the tasks kanban itself moved to `blocked` or `todo`, i.e. work that
///    already ran and stopped for a human. An authorized 🐝 unblocks or promotes them.
///
/// They are separate because the first is a safety property and the second is a convenience:
/// the gate decides whether an agent runs at all, and it is deliberately outside kanban, where
/// nothing can lift it by accident. See [`bridge::state::PendingApprovals`].
async fn approval_sweep(
    gitea: &GiteaClient,
    kanban: &Kanban,
    cfg: &Config,
    state: &BridgeState,
    pending: &PendingApprovals,
) {
    gate_sweep(gitea, kanban, cfg, state, pending).await;
    release_sweep(gitea, kanban, cfg, state).await;
}

/// Creates the kanban task for every held issue a human has approved.
///
/// This is the gate, and the whole of it: until this function runs, an issue admitted by the
/// inbound leg has no task, so there is nothing for a worker to claim and nothing for kanban to
/// promote. The previous design — create the task `--initial-status blocked` and unblock it
/// here — does not hold on hermes v0.19.0, where `hermes kanban list` promotes a blocked task
/// by reading it: [`release_sweep`]'s own first call released every gated task, with no 🐝 and
/// no log line. Nothing in this crate could fix that, so the gate moved out of kanban.
///
/// Each step is idempotent, which is what makes a failure anywhere in the middle safe to retry
/// on the next sweep: `create` dedups on the idempotency key and returns the same task, the
/// consumed markers are matched line-exactly so they are written at most once, and releasing an
/// issue that is no longer held is a no-op.
///
/// Every entry is revalidated against the issue itself before it is evaluated, because nothing
/// else would ever drop one. A held entry is refreshed by being re-held on each inbound sweep,
/// for as long as `/robot/ready` keeps offering the issue — and an issue that is closed, or
/// newly blocked by a dependency, or newly labelled, simply stops being offered. Nothing about
/// that reaches the gate: the entry stays, carrying a title and body frozen at the last hold,
/// waiting for an approval that would then dispatch an agent onto work nobody wants any more.
/// A 🐝 on a closed issue is not a stale entry's fault — the gate only has to notice.
async fn gate_sweep(
    gitea: &GiteaClient,
    kanban: &Kanban,
    cfg: &Config,
    state: &BridgeState,
    pending: &PendingApprovals,
) {
    let held = pending.held();
    let (mut released, mut waiting, mut dropped) = (0u64, 0u64, 0u64);
    for entry in &held {
        // The gate outlives a config change, so an issue held for a repository this bridge no
        // longer owns stays held rather than being created by whoever inherits the file.
        if !cfg
            .repos
            .iter()
            .any(|r| r.owner == entry.owner && r.repo == entry.repo)
        {
            continue;
        }
        // Asked *before* the reactions poll, so a dead entry costs one request rather than
        // two, and so that no path from here to `kanban.create` can run for an issue that is
        // no longer open. Fails closed on everything it cannot answer: an entry is dropped
        // only on a definite `closed` or a 404, never on a transport error and never on a
        // `state` the decoder does not recognise, because emptying the approval gate by
        // accident is the one outcome nothing downstream would report.
        match gitea.issue(&entry.owner, &entry.repo, entry.index).await {
            Ok(issue) if issue.is_open() => {}
            Ok(issue) if issue.is_closed() => {
                drop_held(pending, entry, "the issue is closed");
                dropped += 1;
                continue;
            }
            Ok(issue) => {
                tracing::warn!(
                    issue = entry.index, state = %issue.state,
                    "held issue: the issue API reported a state this bridge does not recognise; \
                     leaving it held and creating nothing"
                );
                continue;
            }
            Err(bridge::gitea::GiteaError::NotFound { .. }) => {
                drop_held(pending, entry, "the issue no longer exists");
                dropped += 1;
                continue;
            }
            Err(err) => {
                tracing::warn!(
                    issue = entry.index, error = %err,
                    "held issue: cannot confirm the issue is still open; leaving it held"
                );
                continue;
            }
        }
        match evaluate(gitea, &cfg.approval, &entry.owner, &entry.repo, entry.index).await {
            Ok(ApprovalOutcome::Approved { approvals }) => {
                let req = CreateTask {
                    title: entry.title.clone(),
                    body: entry.body.clone(),
                    idempotency_key: entry.idempotency_key.clone(),
                };
                let task_id = match kanban.create(&req).await {
                    Ok(id) => id,
                    Err(err) => {
                        tracing::error!(
                            issue = entry.index, key = %entry.idempotency_key, error = %err,
                            "approved, but kanban create failed; the issue stays held and the \
                             next sweep retries it"
                        );
                        continue;
                    }
                };
                let approver = approvals.first().map(|a| a.by.as_str()).unwrap_or("-");
                tracing::info!(
                    task = %task_id, issue = entry.index, by = %approver,
                    "approval released a held issue into a kanban task"
                );
                // Read back rather than assumed empty: `create` returns the *existing* task
                // when this entry is a re-hold of an issue already released, and writing its
                // markers a second time would be comment spam on that task.
                let detail = match kanban.show(&task_id).await {
                    Ok(detail) => detail,
                    Err(err) => {
                        tracing::error!(
                            task = %task_id, error = %err,
                            "cannot resolve the task just created; leaving the issue held so the \
                             next sweep records the approvals against it (create is idempotent)"
                        );
                        continue;
                    }
                };
                note_status(state, &detail);
                consume_approvals(kanban, state, &task_id, &approvals, Some(&detail)).await;
                if let Err(err) = pending.release(&entry.idempotency_key) {
                    // The task exists, so the gate has already done its job; the cost of this
                    // is one idempotent `create` per sweep until the file is writable again.
                    tracing::error!(
                        task = %task_id, key = %entry.idempotency_key, error = %err,
                        "cannot record the release in the approval gate state"
                    );
                }
                released += 1;
            }
            Ok(ApprovalOutcome::NotRequested) => waiting += 1,
            Ok(ApprovalOutcome::NotAuthorized { reactors }) => tracing::info!(
                issue = entry.index,
                ?reactors,
                "held issue: ignoring approval reaction, no reactor has write permission"
            ),
            Ok(ApprovalOutcome::Undetermined { reason, reactors }) => tracing::warn!(
                issue = entry.index, ?reactors, %reason,
                "held issue: approval undetermined; failing closed"
            ),
            Err(err) => tracing::error!(
                issue = entry.index, error = %err,
                "held issue: reaction poll failed"
            ),
        }
    }
    // Info rather than debug, unlike the release sweep: "how many issues is a human sitting on"
    // is the number an operator actually wants, and it is the only place the gate is visible
    // without reading the state file. It is also why `dropped` is reported: without the
    // revalidation above, `held` counted issues nobody was waiting on and nobody could act on.
    tracing::info!(
        held = held.len(),
        released,
        waiting,
        dropped,
        state_file = %pending.path().display(),
        "approval gate sweep complete"
    );
}

/// Drops one entry from the approval gate, saying why on the way out.
///
/// Info rather than debug: an issue leaving the gate un-created is a decision about somebody's
/// work, and the only record of it is this line — the state file will not say what it used to
/// hold. A failed save is tolerated for the same reason a failed release is: nothing was
/// created, so the next sweep reaches the same conclusion and tries again.
fn drop_held(pending: &PendingApprovals, entry: &bridge::state::PendingTask, why: &str) {
    tracing::info!(
        issue = entry.index, key = %entry.idempotency_key, why,
        "dropping a held issue from the approval gate; no kanban task was ever created for it, \
         and a 🐝 on it from here on does nothing"
    );
    if let Err(err) = pending.release(&entry.idempotency_key) {
        tracing::error!(
            key = %entry.idempotency_key, error = %err,
            "cannot record the drop in the approval gate state; it stays held until the file is \
             writable again"
        );
    }
}

/// Records **every** currently-authorized approval on the issue as consumed on this task.
///
/// Not merely the one that did the releasing, and that is the fix rather than an
/// afterthought. A reaction is durable and Gitea records nothing about it having been used, so
/// each unconsumed 🐝 is a stored release. With one marker per release, two maintainers
/// approving issue #57 *before* it ever ran meant: alex's 🐝 released it and was marked; the
/// worker later blocked it with `needs_input`; the next 60s sweep found bo's still-unconsumed
/// 🐝 from before the run and released it again — re-dispatching the agent onto a question
/// nobody had answered, and invisibly, because [`bridge::outbound::BLOCK_MARKER`] suppresses
/// the repeat Gitea comment.
///
/// The anti-shadowing property this is built beside still holds: a genuinely *new* 🐝 — removed
/// and re-added, or a second maintainer reacting after the block — carries a different
/// `created_at` and so a different fingerprint, which nothing here has written.
async fn consume_approvals(
    kanban: &Kanban,
    state: &BridgeState,
    task_id: &str,
    approvals: &[Approval],
    detail: Option<&TaskDetail>,
) {
    for approval in unconsumed(approvals, state, detail) {
        if let Err(err) = kanban
            .comment_with_retry(task_id, &approval.consumed_marker(), MARKER_ATTEMPTS)
            .await
        {
            tracing::error!(
                task = %task_id, fingerprint = %approval.fingerprint,
                attempts = MARKER_ATTEMPTS, error = %err,
                "cannot record the consumed approval after retries; suppressing this fingerprint \
                 for the lifetime of this process to avoid a release loop. A fresh 🐝 (removed \
                 and re-added) still releases the task, and a restart costs at most one extra \
                 release"
            );
            state.suppress_fingerprint(&approval.fingerprint);
        }
    }
}

/// The authorized approvals this task does not already record as acted on.
///
/// Split out from [`consume_approvals`] so the property that fix rests on — *all* of them, not
/// the one that did the releasing — is testable without a board behind it.
fn unconsumed<'a>(
    approvals: &'a [Approval],
    state: &BridgeState,
    detail: Option<&TaskDetail>,
) -> Vec<&'a Approval> {
    approvals
        .iter()
        .filter(|a| !detail.is_some_and(|d| a.is_consumed(d)))
        // Suppressed means an earlier marker write for this fingerprint failed after its
        // retries; re-attempting it every 60s is the loop the suppression exists to stop.
        .filter(|a| !state.is_fingerprint_suppressed(&a.fingerprint))
        .collect()
}

/// The statuses [`release_sweep`] enumerates with a `kanban list`.
///
/// `todo` and `blocked` are the two statuses a human decision can move; `triage` is the
/// specifier's, not ours. Only one of them is here, and the absence of the other is the R7 P1:
/// on hermes v0.19.0 a `list` of [`UNLISTABLE_STATUS`] *promotes* every task it returns, so
/// this sweep — a 60s timer — was releasing every task a worker had stopped for a human, then
/// evaluating the empty list it got back. The 🐝 it was looking for never entered into it.
///
/// Blocked tasks are still swept, by id rather than by status; see
/// [`BridgeState::remember_unlisted`].
const RELEASE_STATUSES: [&str; 1] = ["todo"];

/// What a 🐝 does to a task, which is a function of the status it is in.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Release {
    /// `todo` → `ready`.
    Promote,
    /// `blocked` → `ready`. Only ever reached by id.
    Unblock,
}

impl Release {
    fn name(self) -> &'static str {
        match self {
            Self::Promote => "promote",
            Self::Unblock => "unblock",
        }
    }
}

/// Sweeps 🐝 reactions for every bridge task kanban itself stopped for a human.
///
/// This is **not** the approval gate — see [`gate_sweep`] — because a kanban status cannot
/// hold one. What this sweep does is act on a 🐝 for a task that already ran and stopped.
///
/// It reaches its candidates two ways, and the split is the whole of the R7 P1 fix:
///
/// * [`RELEASE_STATUSES`] by `kanban list`, which is safe for every status but one;
/// * [`UNLISTABLE_STATUS`] by `kanban show` on the ids the daemon has learned, because
///   *listing* that status releases what it lists (hermes v0.19.0) — a 60s timer that undid
///   the human decision it existed to wait for.
async fn release_sweep(gitea: &GiteaClient, kanban: &Kanban, cfg: &Config, state: &BridgeState) {
    // Same reason the reconcile sweep counts these: the loop below `continue`s past every task
    // whose body carries no `gitea-ref:` trailer, and that skip is bare — a leg evaluating no
    // task at all is otherwise indistinguishable in the log from a board with no 🐝 on it.
    let (mut candidates, mut trailered) = (0u64, 0u64);
    for status in RELEASE_STATUSES {
        let tasks = match kanban.list(Some(status)).await {
            Ok(tasks) => tasks,
            Err(err) => {
                tracing::error!(status, error = %err, "kanban list failed");
                continue;
            }
        };
        for task in tasks {
            candidates += 1;
            if consider_release(gitea, kanban, cfg, state, &task, Release::Promote, None).await {
                trailered += 1;
            }
        }
    }

    // The other half, and the one that must never become a `list`. `show` does not promote —
    // the same probe that found the promote-on-read behaviour confirmed repeated `show` calls
    // leave a blocked task alone — so a blocked task is reachable exactly as long as this
    // process knows its id. See `BridgeState::remember_unlisted` for what a restart costs.
    let mut unlisted = 0u64;
    for task_id in state.unlisted() {
        let detail = match kanban.show(&task_id).await {
            Ok(detail) => detail,
            Err(err) => {
                tracing::error!(task = %task_id, error = %err, "cannot resolve a blocked task by id");
                continue;
            }
        };
        note_status(state, &detail);
        if detail.task.status != UNLISTABLE_STATUS {
            // It moved between sweeps; `note_status` has already dropped it, and whatever
            // status it holds now is one a `list` reaches.
            continue;
        }
        unlisted += 1;
        candidates += 1;
        let task = detail.task.clone();
        if consider_release(gitea, kanban, cfg, state, &task, Release::Unblock, Some(detail)).await {
            trailered += 1;
        }
    }

    // Debug rather than info: this runs every 60s, and unlike the reconcile sweep it has
    // nothing to say when it did its job. It is here so that "no bridge task was evaluated at
    // all" is answerable without attaching a debugger to a daemon that looks healthy.
    tracing::debug!(candidates, trailered, unlisted, "approval release sweep complete");
}

/// Evaluates one already-running task's 🐝 and releases it if a human blessed it.
///
/// Returns whether the task carried a `gitea-ref:` trailer, which is what the sweep counts.
///
/// `detail` is a detail already in hand — the by-id path has one, because resolving it is how
/// that path found the task at all — or `None` to pay for a `kanban show` only if an
/// authorized approval turns up.
async fn consider_release(
    gitea: &GiteaClient,
    kanban: &Kanban,
    cfg: &Config,
    state: &BridgeState,
    task: &Task,
    release: Release,
    detail: Option<TaskDetail>,
) -> bool {
    let task_id = task.id.as_str();
    let Some(gref) = task.body.as_deref().and_then(GiteaRef::parse_from_body) else {
        return false;
    };
    if !cfg
        .repos
        .iter()
        .any(|r| r.owner == gref.owner && r.repo == gref.repo)
    {
        return true;
    }
    match evaluate(gitea, &cfg.approval, &gref.owner, &gref.repo, gref.index).await {
        Ok(ApprovalOutcome::Approved { approvals }) => {
            // A reaction is durable and Gitea records nothing about it having been used, so
            // without this the same 🐝 unblocks the task on every sweep: it runs, blocks with
            // `transient`, is unblocked again, forever — and invisibly, because BLOCK_MARKER
            // suppresses the repeat Gitea comment.
            let detail = match detail {
                Some(detail) => detail,
                None => match kanban.show(task_id).await {
                    Ok(detail) => {
                        note_status(state, &detail);
                        detail
                    }
                    Err(err) => {
                        tracing::error!(task = %task_id, error = %err, "cannot resolve task detail");
                        return true;
                    }
                },
            };
            // …and the in-process guard covers the case where that durable record could not be
            // written at all: without it a marker write that failed after its retries releases
            // the task again on every sweep.
            //
            // `unconsumed` rather than an open-coded predicate, so what decides to release and
            // what gets marked afterwards cannot drift apart: an approval acted on but not
            // marked is a stored release.
            let standing = unconsumed(&approvals, state, Some(&detail));
            let Some(approval) = standing.first() else {
                tracing::debug!(
                    task = %task_id, issue = gref.index,
                    "every approval reaction on this issue has already been acted on \
                     (or suppressed); a new 🐝 is needed to release it again"
                );
                return true;
            };
            let result = match release {
                Release::Promote => kanban.promote(task_id).await,
                Release::Unblock => kanban.unblock(task_id).await,
            };
            match result {
                Ok(()) => {
                    tracing::info!(
                        task = %task_id, by = %approval.by, action = release.name(),
                        "approval promoted task"
                    );
                    // The task is claimable again, so it is no longer one the sweeps have to
                    // reach by id. Recorded here rather than waiting for the next `show`,
                    // because until it is, this leg pays a `show` for a task that is `ready`.
                    state.forget_unlisted(task_id);
                    // Written after the move, never before: a marker without a move would
                    // strand the task, whereas a move without a marker is retried on the next
                    // sweep. And written for *every* authorized approval, not only the one
                    // that released this task — a surplus pre-run 🐝 left pending is a stored
                    // release that would unblock the next worker block with no human decision
                    // behind it. See `consume_approvals`.
                    consume_approvals(kanban, state, task_id, &approvals, Some(&detail)).await;
                }
                Err(err) => {
                    tracing::error!(task = %task_id, action = release.name(), error = %err, "promotion failed")
                }
            }
        }
        Ok(ApprovalOutcome::NotRequested) => {}
        Ok(ApprovalOutcome::NotAuthorized { reactors }) => tracing::info!(
            task = %task_id, issue = gref.index, ?reactors,
            "ignoring approval reaction: no reactor has write permission"
        ),
        Ok(ApprovalOutcome::Undetermined { reason, reactors }) => tracing::warn!(
            task = %task_id, issue = gref.index, ?reactors, %reason,
            "approval undetermined; failing closed"
        ),
        Err(err) => {
            tracing::error!(task = %task_id, issue = gref.index, error = %err, "reaction poll failed")
        }
    }
    true
}

/// Records whether a task the daemon just read holds the one status no sweep may enumerate.
///
/// Called after **every** `kanban show` in this binary, which is what keeps the set honest for
/// free: `show` reports a status, `show` does not promote, and the watch leg shows every task
/// the instant it emits its `blocked` event. See [`BridgeState::remember_unlisted`].
fn note_status(state: &BridgeState, detail: &TaskDetail) {
    if detail.task.status == UNLISTABLE_STATUS {
        state.remember_unlisted(&detail.task.id);
    } else {
        state.forget_unlisted(&detail.task.id);
    }
}

/// Follows `kanban watch` and applies each terminal event to Gitea.
///
/// The watcher is restarted if it exits, because a dead watcher is silent, and silence here
/// looks exactly like an idle board.
async fn outbound_loop(cfg: Arc<Config>, kanban: Kanban, robot: Robot, state: Arc<BridgeState>) {
    loop {
        match kanban.watch().await {
            Ok(mut watcher) => {
                tracing::info!("watching kanban terminal events");
                // The `watch` line format is a terminal display, not a documented interface.
                // Counting what parsed is what turns a change to it into a log line instead
                // of a daemon that reports "watching" and then never acts again.
                let (mut parsed, mut unparsed) = (0u64, 0u64);
                loop {
                    match watcher.lines.next_line().await {
                        Ok(Some(line)) => match classify_watch_line(&line) {
                            WatchLine::Event(event) => {
                                parsed += 1;
                                apply_event(&cfg, &kanban, &robot, &state, &event.task_id, event.kind).await;
                            }
                            WatchLine::UnexpectedKind { kind } => {
                                // `watch` is invoked with `--kinds`, so this cannot happen
                                // unless the filter stopped being honoured or the kind names
                                // moved. Either way the event is being dropped.
                                unparsed += 1;
                                tracing::warn!(
                                    %kind,
                                    "kanban watch emitted a kind outside the --kinds filter; dropping it"
                                );
                            }
                            WatchLine::Unrecognised => {
                                unparsed += 1;
                                tracing::debug!(line = %line.trim_end(), "ignoring a non-event watch line");
                            }
                        },
                        Ok(None) => break,
                        Err(err) => {
                            tracing::error!(error = %err, "watch stream failed");
                            break;
                        }
                    }
                }
                let exit = watcher.finish().await;
                if parsed == 0 && unparsed > 0 {
                    tracing::warn!(
                        unparsed,
                        "kanban watch produced output but no line matched the expected event \
                         format; the outbound leg acted on nothing"
                    );
                }
                // The silent case: no stdout at all. Nothing above can fire, so without the
                // exit code and stderr this is a `watch exited; restarting` line every five
                // seconds and a permanently dead outbound leg — an unsupported `--kinds`, a
                // missing board or version skew all look identical to an idle board.
                if parsed == 0 && unparsed == 0 {
                    tracing::error!(
                        code = ?exit.code,
                        stderr = %exit.stderr,
                        "kanban watch exited without emitting a single line; the outbound leg is \
                         dead until this is fixed (reconcile still covers correctness)"
                    );
                }
                tracing::warn!(
                    code = ?exit.code,
                    stderr = %exit.stderr,
                    parsed,
                    unparsed,
                    "kanban watch exited; restarting"
                );
            }
            Err(err) => tracing::error!(error = %err, "cannot start kanban watch"),
        }
        tokio::time::sleep(std::time::Duration::from_secs(5)).await;
    }
}

/// Sweeps tasks that reached a terminal state but whose Gitea feedback never landed.
///
/// This is what makes "a half-applied plan is safe to replay" reachable rather than merely
/// true. `outbound_loop` is driven by `kanban watch`, a live stream on which each terminal
/// event fires exactly once, so a single `gitea-robot` failure — a 500, a DNS blip — or a
/// daemon that was simply down at the time would otherwise drop that issue's feedback for
/// good: no pull request, no comment, no label, one `error!` line and nothing to re-drive it.
///
/// The sweep keys on the task's *event trail*, not its status. A task can hold a status it
/// never ran into — `blocked` with only a `created` event, `ready` after a crash-reclaim — and
/// labelling its issue `status/blocked` would be a lie about work that never ran.
///
/// It enumerates [`RECONCILE_STATUSES`] — every documented status but one — rather than the
/// two a terminal event was *observed* to leave behind. Nothing pins where `crashed`,
/// `gave_up` or `timed_out` land, and kanban's crash-reclaim exists to return a dead worker's
/// task to a claimable status, so a `done`/`blocked` scan silently covered two of the five
/// kinds and dropped the other three's feedback permanently — for the kinds most likely to
/// coincide with the infrastructure trouble that made this sweep necessary in the first place.
///
/// The one status it does *not* list is [`UNLISTABLE_STATUS`], and that is the R7 P1: on
/// hermes v0.19.0 a `list` of it promotes what it returns, so this sweep — a 300s timer,
/// running whether or not anything needed reconciling — was releasing every worker-blocked
/// task back to a claimable status, silently. Those tasks are reconciled by id instead, from
/// [`BridgeState::unlisted`], which costs one `show` each and promotes nothing.
///
/// Enumerating widely costs nothing extra per task, because the expensive step — one
/// `kanban show` subprocess each — is skipped for tasks whose report is already *confirmed*
/// in the status they were listed in. Without that cache the sweep's cost would grow without
/// bound with board history: `done` accumulates for the lifetime of the board and nothing here
/// archives it, and every one of those tasks carries a durable marker that cannot un-write
/// itself. A task with nothing to report is not cached — see [`BridgeState::mark_reported`] —
/// so the residual per-sweep cost is one `show` per idle bridge task, bounded by work in
/// flight rather than by history.
async fn reconcile_sweep(cfg: &Config, kanban: &Kanban, robot: &Robot, state: &BridgeState) {
    let mut counts = ReconcileCounts::default();
    let mut seen = std::collections::HashSet::new();
    let (mut failed_statuses, mut first_error): (Vec<&str>, Option<String>) = (Vec::new(), None);
    for status in RECONCILE_STATUSES {
        let tasks = match kanban.list(Some(status)).await {
            Ok(tasks) => tasks,
            Err(err) => {
                // Aggregated below rather than logged per status: a `--status` value kanban
                // does not accept is a question about the filter, not about the board, and
                // one recurring error line per sweep per status would drown the sweep that
                // actually found something. The first error is kept so the aggregate still
                // says *why*.
                failed_statuses.push(status);
                first_error.get_or_insert_with(|| err.to_string());
                continue;
            }
        };
        for task in tasks {
            // A task can be listed under only one status, but the same board is enumerated
            // eight times and kanban may move a task between two of those listings.
            if !seen.insert(task.id.clone()) {
                continue;
            }
            // Filter on the trailer before paying for a `show`: the board is shared, and
            // this sweep walks all of it rather than only what the watcher happened to see.
            let Some(gref) = task.body.as_deref().and_then(GiteaRef::parse_from_body) else {
                continue;
            };
            counts.trailered += 1;
            if !cfg
                .repos
                .iter()
                .any(|r| r.owner == gref.owner && r.repo == gref.repo)
            {
                continue;
            }
            match triage(state, &task.id, &task.status) {
                Triage::Reported => {
                    counts.reported += 1;
                    continue;
                }
                Triage::Suppressed => {
                    counts.suppressed += 1;
                    continue;
                }
                Triage::Resolve => {}
            }
            let detail = match kanban.show(&task.id).await {
                Ok(detail) => detail,
                Err(err) => {
                    tracing::error!(task = %task.id, error = %err, "cannot resolve task detail");
                    continue;
                }
            };
            note_status(state, &detail);
            reconcile_resolved(cfg, kanban, robot, state, &detail, &mut counts).await;
        }
    }

    // The tasks no `list` above could return, reached by id. Without this leg the whole
    // `blocked` terminal kind would lose its safety net: `watch` fires each terminal event
    // exactly once, so a `gitea-robot` failure on a block — or a daemon that was down when it
    // fired — would leave the issue permanently silent about work that ran and stopped.
    for task_id in state.unlisted() {
        if !seen.insert(task_id.clone()) {
            continue;
        }
        let detail = match kanban.show(&task_id).await {
            Ok(detail) => detail,
            Err(err) => {
                tracing::error!(task = %task_id, error = %err, "cannot resolve a blocked task by id");
                continue;
            }
        };
        note_status(state, &detail);
        counts.unlisted += 1;
        let Some(gref) = detail.task.body.as_deref().and_then(GiteaRef::parse_from_body) else {
            continue;
        };
        counts.trailered += 1;
        if !cfg
            .repos
            .iter()
            .any(|r| r.owner == gref.owner && r.repo == gref.repo)
        {
            continue;
        }
        match triage(state, &task_id, &detail.task.status) {
            Triage::Reported => {
                counts.reported += 1;
                continue;
            }
            Triage::Suppressed => {
                counts.suppressed += 1;
                continue;
            }
            Triage::Resolve => {}
        }
        reconcile_resolved(cfg, kanban, robot, state, &detail, &mut counts).await;
    }

    let ReconcileCounts {
        checked,
        replayed,
        reported,
        idle,
        suppressed,
        trailered,
        unlisted,
    } = counts;
    let error = first_error.unwrap_or_default();
    if failed_statuses.len() == RECONCILE_STATUSES.len() {
        tracing::error!(%error, "kanban list failed for every status; this sweep reconciled nothing at all");
    } else if !failed_statuses.is_empty() {
        tracing::warn!(?failed_statuses, %error, "kanban list failed for some statuses");
    }
    tracing::info!(
        checked,
        replayed,
        reported,
        idle,
        suppressed,
        trailered,
        unlisted,
        candidates = seen.len(),
        "reconciliation sweep complete"
    );
}

/// What one reconciliation sweep did, so the two ways it reaches a task can share the tally.
///
/// `trailered` is counted apart from `candidates` on purpose. `candidates` is every task the
/// sweep saw, *before* the trailer filter, so on its own it cannot tell a board with nothing to
/// do from a list payload in which no task carries a `gitea-ref:` trailer at all — the state in
/// which this leg reports nothing while its summary line reads perfectly healthy.
/// [`Kanban::list`] fails loudly if the `body` projection disappears; this is what says so from
/// the sweep's side, including for the cases the decoder cannot see (a trailer the bridge no
/// longer writes, a board that is simply not ours).
///
/// `unlisted` is the tasks reached by id rather than by `list`. It is reported because those
/// tasks are invisible to every other diagnostic here: nothing else in the log distinguishes
/// "no task is blocked" from "this process has forgotten which tasks are blocked", which is
/// what a restart leaves behind. See [`BridgeState::remember_unlisted`].
#[derive(Debug, Default)]
struct ReconcileCounts {
    /// Tasks whose event trail was examined.
    checked: u64,
    /// Terminal tasks whose plan was replayed.
    replayed: u64,
    /// Tasks whose Gitea report is confirmed landed.
    reported: u64,
    /// Tasks with no terminal event yet.
    idle: u64,
    /// Dead-lettered tasks, skipped for their own reason.
    suppressed: u64,
    /// Tasks carrying a `gitea-ref:` trailer.
    trailered: u64,
    /// Tasks reached by id because no `list` may return them.
    unlisted: u64,
}

/// Acts on one task the sweep has already resolved with a `kanban show`.
///
/// Shared by the two halves of [`reconcile_sweep`] — the listed statuses and the by-id pass —
/// so a blocked task, which only the second can reach, is reconciled by exactly the same rules
/// as every other one.
async fn reconcile_resolved(
    cfg: &Config,
    kanban: &Kanban,
    robot: &Robot,
    state: &BridgeState,
    detail: &TaskDetail,
    counts: &mut ReconcileCounts,
) {
    let task_id = detail.task.id.as_str();
    match resolve(state, detail, robot.blocked_label()) {
        Resolution::Idle => counts.idle += 1,
        Resolution::Reported => {
            counts.checked += 1;
            counts.reported += 1;
        }
        Resolution::Replay(kind, plan) => {
            counts.checked += 1;
            counts.replayed += 1;
            tracing::info!(
                task = %task_id, kind = kind.event_kind(),
                "terminal task has no report marker; replaying its plan"
            );
            apply_plan(cfg, kanban, robot, state, task_id, &plan, detail).await;
        }
        Resolution::Unplannable(err) => {
            counts.checked += 1;
            tracing::error!(task = %task_id, error = %err, "cannot plan outbound actions");
        }
    }
}

/// What the sweep does with one listed task *before* paying for its `kanban show`.
///
/// Extracted so the one decision that can silently drop a task's Gitea feedback forever is
/// testable without a board behind it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Triage {
    /// Its report is confirmed landed in kanban, in this status. A durable marker cannot
    /// un-write itself, so there is nothing left to find.
    Reported,
    /// Its marker could not be written after retries. The task is deliberately unmarked —
    /// exactly what this sweep looks for — so acting again reposts the same Gitea comment.
    Suppressed,
    /// Pay for the `show` and decide on the event trail.
    Resolve,
}

fn triage(state: &BridgeState, task_id: &str, status: &str) -> Triage {
    if state.is_reported(task_id, status) {
        Triage::Reported
    } else if state.is_task_suppressed(task_id) {
        Triage::Suppressed
    } else {
        Triage::Resolve
    }
}

/// What the sweep does with a task once `kanban show` has resolved its *event trail*.
///
/// The trail, not the status: a task can hold `blocked` with only a `created` event on its
/// trail, and labelling its issue `status/blocked` would be a lie about work that never ran.
#[derive(Debug)]
enum Resolution {
    /// No terminal event yet — queued, claimed, or still running.
    Idle,
    /// Its terminal event is confirmed already reported to Gitea.
    Reported,
    /// A terminal event with no report marker behind it. Apply this.
    Replay(TerminalKind, OutboundPlan),
    /// A terminal event the sweep cannot turn into actions — in practice a body whose
    /// `gitea-ref:` trailer went away between the `list` that selected the task and the `show`
    /// that resolved it.
    ///
    /// Note what is *not* here: a `completed` task whose branch was never pushed. Planning is
    /// offline and cannot ask a git remote whether a head exists, so that case plans normally
    /// and surfaces where it is knowable — `create-pull` fails, the failure is counted, and
    /// `escalate_plan_failure` reports it on the issue.
    Unplannable(PlanError),
}

/// Classifies a resolved task, and records the one outcome that is safe to remember.
///
/// Which outcome that is, is the whole point, and getting it wrong is silent: see
/// [`BridgeState::mark_reported`]. `Reported` is stable — a durable kanban marker cannot
/// un-write itself. `Idle` is **not**: a task with nothing to report can acquire something to
/// report and come back to the same status, so caching it blinds the sweep to that task for
/// the lifetime of the process, and nothing invalidates the cache.
fn resolve(state: &BridgeState, detail: &TaskDetail, blocked_label: &str) -> Resolution {
    let Some(kind) = latest_terminal_kind(detail) else {
        return Resolution::Idle;
    };
    match plan(detail, kind, blocked_label, true) {
        Ok(plan) => Resolution::Replay(kind, plan),
        // The overwhelmingly common case: the watcher already handled it.
        Err(PlanError::AlreadyReported { .. }) => {
            state.mark_reported(&detail.task.id, &detail.task.status);
            Resolution::Reported
        }
        Err(err) => Resolution::Unplannable(err),
    }
}

async fn apply_event(
    cfg: &Config,
    kanban: &Kanban,
    robot: &Robot,
    state: &BridgeState,
    task_id: &str,
    kind: TerminalKind,
) {
    if state.is_task_suppressed(task_id) {
        tracing::warn!(
            task = task_id,
            kind = kind.event_kind(),
            "ignoring a terminal event for a task whose report marker could not be written; \
             restart the daemon once kanban accepts comments again"
        );
        return;
    }
    let detail = match kanban.show(task_id).await {
        Ok(detail) => detail,
        Err(err) => {
            tracing::error!(task = task_id, error = %err, "cannot resolve task detail");
            return;
        }
    };
    // The load-bearing one. `watch` is live, so this call happens within seconds of the block
    // that a `list` must never go looking for, and it is what puts the task within reach of
    // both sweeps for as long as it stays blocked. See `note_status`.
    note_status(state, &detail);
    // Matched exhaustively on `PlanError`, with no catch-all arm: a new variant is a new way
    // for a terminal event to produce nothing on the user's issue, and it should have to be
    // answered here rather than swept into one error line.
    let plan = match plan(&detail, kind, robot.blocked_label(), true) {
        Ok(plan) => plan,
        Err(PlanError::NoGiteaRef { .. }) => {
            // Warn, not debug. `watch` is filtered by `--assignee`, and the multi-box
            // deployment this daemon documents gives each bridge its own, so a terminal
            // event with no trailer on *this* stream is anomalous rather than routine — and
            // the shape it would take if `show --json` ever changed is exactly this one.
            tracing::warn!(
                task = task_id,
                kind = kind.event_kind(),
                "terminal event for a task with no `gitea-ref:` trailer; nothing reported to gitea"
            );
            return;
        }
        Err(PlanError::AlreadyReported { .. }) => {
            tracing::info!(
                task = task_id,
                kind = kind.event_kind(),
                "already reported; ignoring replay"
            );
            return;
        }
    };
    apply_plan(cfg, kanban, robot, state, task_id, &plan, &detail).await;
}

/// Applies one planned response to Gitea, then marks the task once all of it landed.
///
/// Serialised per task. `outbound_loop` and the reconcile ticker are independent tokio tasks
/// over one `Arc<BridgeState>`, and the window between planning and the marker landing spans a
/// `create-pull` subprocess (up to `RUN_TIMEOUT`), the comment, and the marker's retries. A
/// sweep arriving inside that window sees an unmarked task and plans the same actions:
/// `create-pull` probes for an existing pull request and adding a label twice is a no-op, but
/// the reason comment is not idempotent, so the user's issue would carry it twice.
///
/// The claim closes the *overlapping* case. It cannot close the sequential one on its own: the
/// plan was computed from a `TaskDetail` read before the claim was taken — the reconcile sweep
/// pays a `kanban show` subprocess in between — so the other leg can have finished the whole
/// apply, written its marker and dropped its claim inside that gap, leaving a plan built from a
/// detail that is now stale. So the task is re-read *under* the claim and the marker this plan
/// would write is checked again before anything is applied.
async fn apply_plan(
    cfg: &Config,
    kanban: &Kanban,
    robot: &Robot,
    state: &BridgeState,
    task_id: &str,
    plan: &OutboundPlan,
    detail: &TaskDetail,
) {
    let Some(_claim) = state.try_claim_apply(task_id) else {
        tracing::info!(
            task = task_id,
            "another leg is already applying this task's plan; skipping this one rather than \
             posting its reason comment a second time"
        );
        return;
    };
    if !cfg
        .repos
        .iter()
        .any(|r| r.owner == plan.gitea_ref.owner && r.repo == plan.gitea_ref.repo)
    {
        tracing::debug!(task = task_id, repo = %format!("{}/{}", plan.gitea_ref.owner, plan.gitea_ref.repo),
            "task references a repo this bridge does not own; ignoring");
        return;
    }
    // Re-read under the claim: see this function's own note. The check is the marker rather
    // than a re-plan because the marker is exactly what `plan()` would refuse on, and it is
    // the one line of the comment that must not be posted twice.
    if let Some(marker) = plan_marker(plan) {
        match kanban.show(task_id).await {
            Ok(fresh) if fresh.has_marker(marker) => {
                note_status(state, &fresh);
                tracing::info!(
                    task = task_id,
                    marker,
                    "another leg finished reporting this task while this plan was being prepared; \
                     not applying it a second time"
                );
                return;
            }
            Ok(fresh) => note_status(state, &fresh),
            Err(err) => {
                // Deliberately a skip rather than a best-effort apply. The plan in hand may
                // already have landed, and the action that is not idempotent is the
                // user-visible one; the reconcile sweep comes back for anything left unmarked,
                // so waiting costs one interval and guessing costs a duplicate comment on
                // somebody's issue.
                tracing::warn!(
                    task = task_id, error = %err,
                    "cannot re-read the task before applying its plan; leaving it for the \
                     reconciliation sweep rather than risking a duplicate reason comment"
                );
                return;
            }
        }
    }

    let (owner, repo, index) = (&plan.gitea_ref.owner, &plan.gitea_ref.repo, plan.gitea_ref.index);
    match apply_actions(robot, task_id, owner, repo, index, &plan.actions).await {
        Ok(marker) => {
            state.clear_plan_failures(task_id);
            if let Some(marker) = marker {
                record_marker(kanban, state, task_id, &marker).await;
            }
        }
        Err((action, err)) => {
            let failures = state.record_plan_failure(task_id);
            // Two failures reach here and neither one ever stops on its own: a `completed`
            // task whose head branch was never pushed, and — for every other terminal kind —
            // a `status/blocked` the repository does not have, which `edit-issue` refuses
            // outright at action 0, taking the reason comment behind it with it. The retry has
            // no ceiling either way: the only signal is a recurring `error!` line in the
            // daemon's log, while the Gitea issue stays silent about work that ran and
            // stopped — indistinguishable, to a human watching the board, from an issue nobody
            // picked up. After enough failures, say so where they are looking.
            //
            // Gated on the *count*, not on which action failed: gating on `OpenPr` left the
            // label case — four of the five terminal kinds — with no escalation at all.
            if failures >= ESCALATE_AFTER {
                let failure = PlanFailure {
                    action,
                    failures,
                    error: &err,
                };
                escalate_plan_failure(kanban, robot, state, plan, detail, failure).await;
            }
        }
    }
}

/// Applies planned actions in order, stopping at the first failure.
///
/// On success, returns the marker to record — the first line of the first comment that
/// landed. On failure, returns which action failed and why; the task is deliberately left
/// unmarked, because an unmarked task is what `reconcile_sweep` looks for, and that sweep —
/// not the next terminal event — is what actually replays this plan: a terminal kanban event
/// fires once, so nothing else would ever come back for this task.
///
/// Replaying re-runs the actions that already landed, so the two that can land before a
/// failure are idempotent on the Gitea side: `create-pull` probes `GET /pulls/{base}/{head}`
/// and reports an existing *open, unmerged* pull request as success
/// (`cmd/gitea-robot/write.go`), and adding a label the issue already carries is a no-op
/// (`models/issues/issue_label.go:125-130`). Without that probe a `completed` plan whose
/// audit comment failed would wedge forever — Gitea refuses a second pull request for the
/// same base and head, so every retry would fail on the first action and the comment would
/// never be posted. The comment itself is last in every plan, so a replay repeats it only
/// when it was the action that failed.
async fn apply_actions(
    robot: &Robot,
    task_id: &str,
    owner: &str,
    repo: &str,
    index: i64,
    actions: &[PlannedAction],
) -> Result<Option<String>, (Action, String)> {
    let mut marker: Option<String> = None;
    for action in actions {
        let outcome = match action {
            PlannedAction::OpenPull(pr) => robot.create_pull(owner, repo, pr).await.map(|_| ()),
            PlannedAction::Comment(body) => robot.comment(owner, repo, index, body).await.map(|_| ()),
            PlannedAction::Label(labels) => robot.add_labels(owner, repo, index, labels).await.map(|_| ()),
        };
        match outcome {
            Ok(()) => {
                tracing::info!(task = task_id, %owner, %repo, index, action = action.action().name(), "applied");
                if let PlannedAction::Comment(body) = action {
                    marker = marker.or_else(|| first_line(body).map(str::to_string));
                }
            }
            Err(err) => {
                tracing::error!(
                    task = task_id, %owner, %repo, index,
                    action = action.action().name(), error = %err,
                    "action failed; leaving the task unmarked for the reconciliation sweep to replay"
                );
                return Err((action.action(), err.to_string()));
            }
        }
    }
    Ok(marker)
}

/// Records a durable marker, retrying, and dead-letters the task if it will not land.
///
/// The marker is the whole idempotence story: while it is missing, `plan()` keeps returning
/// a plan and `reconcile_sweep` keeps applying it — posting the same comment on a
/// user-visible Gitea issue every 300 seconds. Suppressing the task bounds that to one
/// duplicate per process lifetime instead of one per sweep, forever.
async fn record_marker(kanban: &Kanban, state: &BridgeState, task_id: &str, marker: &str) {
    if let Err(err) = kanban.comment_with_retry(task_id, marker, MARKER_ATTEMPTS).await {
        tracing::error!(
            task = task_id, marker, attempts = MARKER_ATTEMPTS, error = %err,
            "cannot record the kanban dedup marker after retries; suppressing this task for the \
             lifetime of this process so its gitea comment is not reposted every sweep. Run \
             `reconcile-once` once kanban accepts comments again to finish recording it"
        );
        state.suppress_task(task_id);
    }
}

/// One repeatedly-failing action of a plan, as the escalation path needs it.
struct PlanFailure<'a> {
    /// The action that failed — the plan stops at it, so it is also the *first* failure.
    action: Action,
    /// Consecutive failed applications of this task's plan.
    failures: u32,
    /// What the failing call said.
    error: &'a str,
}

/// Surfaces a repeatedly-failing report on the Gitea issue itself.
///
/// The retry is not abandoned — push the missing head branch, or create the missing label, and
/// the original plan applies on a subsequent sweep — but the escalation comment is posted
/// exactly once per failing action, guarded by its own durable marker so it survives a restart
/// and does not collide with a genuine later report.
///
/// That "the retry is not abandoned" is load-bearing, because the comment this posts says so
/// out loud to whoever is reading the issue. So the escalation marker is *not* routed through
/// [`record_marker`]: dead-lettering the task there would drop it from both the watch leg and
/// the sweep for the process lifetime, which is precisely the promise the comment just made
/// being broken silently. The escalation's own failure mode is one duplicate comment, and
/// [`BridgeState::suppress_escalation`] bounds it to that.
///
/// The escalation is a comment and nothing else. That is what makes it reachable for the case
/// it most needs to cover: when the *label* is what cannot be applied, an escalation leading
/// with a label would be retrying the one call that is broken.
async fn escalate_plan_failure(
    kanban: &Kanban,
    robot: &Robot,
    state: &BridgeState,
    plan: &OutboundPlan,
    detail: &TaskDetail,
    failure: PlanFailure<'_>,
) {
    let PlanFailure {
        action,
        failures,
        error,
    } = failure;
    let task_id = detail.task.id.as_str();
    // Its marker never landed, so `escalation_actions` would plan the comment again on every
    // sweep. The comment is already on the issue and says what it needs to say.
    if state.is_escalation_suppressed(task_id) {
        return;
    }
    // The one fact that tells a human which failure this is — the head branch, or the label —
    // read off the action that actually failed rather than assumed.
    let context = plan.actions.iter().find_map(|a| match (action, a) {
        (Action::OpenPr, PlannedAction::OpenPull(pr)) => Some(pr.head.clone()),
        (Action::Label, PlannedAction::Label(labels)) => Some(labels.join(", ")),
        _ => None,
    });
    let actions = match escalation_actions(detail, action, context.as_deref(), failures, error) {
        Ok(actions) => actions,
        // Already escalated: keep retrying quietly rather than commenting again.
        Err(_) => return,
    };
    let (owner, repo, index) = (&plan.gitea_ref.owner, &plan.gitea_ref.repo, plan.gitea_ref.index);
    tracing::warn!(
        task = task_id, %owner, %repo, index, failures,
        action = action.name(), context = context.as_deref().unwrap_or("-"),
        "a terminal task's report to gitea has failed repeatedly; reporting that on the issue"
    );
    if let Ok(Some(marker)) = apply_actions(robot, task_id, owner, repo, index, &actions).await
        && let Err(err) = kanban.comment_with_retry(task_id, &marker, MARKER_ATTEMPTS).await
    {
        tracing::error!(
            task = task_id, marker, attempts = MARKER_ATTEMPTS, error = %err,
            "cannot record the escalation marker after retries; not re-posting this escalation \
             for the lifetime of this process. The task itself stays in both legs, because the \
             comment just posted on the issue promises the bridge will keep retrying — fixing \
             the branch or the label still resolves it on a later sweep"
        );
        state.suppress_escalation(task_id);
    }
}

/// The first line of a comment body — the marker the dedup guard looks for.
fn first_line(body: &str) -> Option<&str> {
    body.lines().next().map(str::trim).filter(|s| !s.is_empty())
}

/// The durable marker this plan would write once every one of its actions has landed.
///
/// The first comment in the plan, because that is what [`record_marker`] records and what
/// [`plan`] refuses on. Every plan ends in a comment, so this is `None` only for a plan with
/// no actions at all.
fn plan_marker(plan: &OutboundPlan) -> Option<&str> {
    plan.actions.iter().find_map(|action| match action {
        PlannedAction::Comment(body) => first_line(body),
        _ => None,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use bridge::config::{ApprovalConfig, GiteaConfig, KanbanConfig, PollConfig, RobotConfig};
    use bridge::hermes::{TaskComment, TaskEvent};
    use bridge::outbound::{
        BLOCK_MARKER, PR_MARKER, blocked_comment, completed_comment, escalation_comment, escalation_marker,
    };
    use bridge::state::PendingTask;
    use wiremock::matchers::{method, path, query_param};
    use wiremock::{Mock, MockServer, ResponseTemplate};

    fn task_detail(events: &[(&str, i64)], comments: &[&str]) -> TaskDetail {
        TaskDetail {
            task: Task {
                id: "t_1".into(),
                title: "probe".into(),
                body: Some("gitea-ref: terraphim/gitea#57".into()),
                status: "blocked".into(),
                ..Task::default()
            },
            comments: comments
                .iter()
                .map(|body| TaskComment {
                    author: Some("bridge".into()),
                    body: (*body).to_string(),
                })
                .collect(),
            events: events
                .iter()
                .map(|(kind, at)| TaskEvent {
                    kind: (*kind).to_string(),
                    payload: Default::default(),
                    created_at: *at,
                })
                .collect(),
            ..TaskDetail::default()
        }
    }

    /// The three cases `reconcile_sweep` decides between, in one place.
    ///
    /// A terminal kanban event fires exactly once on a live `watch` stream, so without this
    /// sweep a single `gitea-robot` failure — or a daemon that was down at the time — drops
    /// that issue's feedback permanently. The sweep is only safe because it keys on the
    /// event trail rather than the status.
    #[test]
    fn reconciliation_replays_only_unreported_terminal_tasks() {
        let label = "status/blocked";

        // 1. Blocked by a worker, never reported: replay it.
        let unreported = task_detail(&[("created", 1), ("blocked", 2)], &[]);
        let kind = latest_terminal_kind(&unreported).expect("a terminal event");
        assert_eq!(kind, TerminalKind::Blocked);
        assert!(plan(&unreported, kind, label, true).is_ok());

        // 2. Already reported: the durable marker makes the sweep a no-op, which is what
        //    keeps it from re-labelling and re-commenting every 5 minutes forever.
        let reported = task_detail(
            &[("created", 1), ("blocked", 2)],
            &[&blocked_comment("t_1", TerminalKind::Blocked, None, None)],
        );
        assert!(matches!(
            plan(&reported, kind, label, true),
            Err(PlanError::AlreadyReported { .. })
        ));

        // 3. `blocked` with nothing but a `created` event: it never ran. Keying on status
        //    would label the issue and comment about work that has not started.
        let gated = task_detail(&[("created", 1)], &[]);
        assert_eq!(latest_terminal_kind(&gated), None);
    }

    /// The sweep used to enumerate `done` and `blocked`, which covers two of the five
    /// terminal kinds. kanban's crash-reclaim returns a dead worker's task to a claimable
    /// status, so a `crashed` task plausibly sits in `ready` — never listed, never labelled,
    /// never commented on, for the kind most likely to coincide with infrastructure trouble.
    #[test]
    fn a_crashed_task_reclaimed_to_a_claimable_status_is_still_reconciled() {
        let mut reclaimed = task_detail(&[("created", 1), ("claimed", 2), ("crashed", 3)], &[]);
        reclaimed.task.status = "ready".into();
        assert!(
            RECONCILE_STATUSES.contains(&reclaimed.task.status.as_str()),
            "the sweep must list the status a reclaimed task lands in"
        );
        let kind = latest_terminal_kind(&reclaimed).expect("a terminal event");
        assert_eq!(kind, TerminalKind::Crashed);
        assert!(plan(&reclaimed, kind, "status/blocked", true).is_ok());
    }

    /// The R3 P1 this closes: the sweep's cache used to remember "nothing to report", which
    /// is not a stable fact.
    ///
    /// Every bridge task passes through an idle status on its way in, so this is the *normal*
    /// path, not a corner:
    ///
    /// 1. the task is created and sits unclaimed; the sweep lists it, finds only a `created`
    ///    event, and used to cache `(t_1, <that status>)`;
    /// 2. a worker claims and runs it, then blocks it — now with a `blocked` event on the
    ///    trail, in a status it may well have been idle in before;
    /// 3. the watch leg's apply fails (the R2 case: `status/blocked` missing, `edit-issue`
    ///    refuses at action 0), leaving the task deliberately unmarked;
    /// 4. the next sweep hit the cache and skipped it — for the process lifetime, while its
    ///    own "healthy" log line counted it as settled, and with the failure counter frozen
    ///    below `ESCALATE_AFTER` so the escalation could never fire either.
    ///
    /// The same shape covers the reclaim case `RECONCILE_STATUSES` was widened for: idle in
    /// `ready`, claimed, crashed, and returned to `ready` by kanban's crash-reclaim.
    #[test]
    fn a_task_that_returns_to_a_status_it_was_idle_in_is_still_reconciled() {
        let state = BridgeState::new();
        let label = "status/blocked";

        // Sweep 1 — created, unclaimed, idle in `blocked`. Nothing to report, and — the fix —
        // nothing the sweep is allowed to remember.
        assert_eq!(triage(&state, "t_1", "blocked"), Triage::Resolve);
        let gated = task_detail(&[("created", 1)], &[]);
        assert!(matches!(resolve(&state, &gated, label), Resolution::Idle));

        // Sweep 2 — released, run, and blocked by the worker. Same status, new trail. This
        // is the assertion the old status-keyed cache failed: it returned `Reported` here
        // and the block was never reported to the issue at all.
        assert_eq!(
            triage(&state, "t_1", "blocked"),
            Triage::Resolve,
            "an idle task must not be cached: it can acquire a terminal event and come back \
             to the status it was idle in"
        );
        let blocked = task_detail(
            &[("created", 1), ("unblocked", 2), ("claimed", 3), ("blocked", 4)],
            &[],
        );
        assert!(matches!(
            resolve(&state, &blocked, label),
            Resolution::Replay(TerminalKind::Blocked, _)
        ));

        // What the cache *is* for still works: once the report is confirmed by a durable
        // marker it cannot become unreported, so the next sweep skips the `show`.
        let reported = task_detail(
            &[("created", 1), ("blocked", 2)],
            &[&blocked_comment("t_1", TerminalKind::Blocked, None, None)],
        );
        assert!(matches!(resolve(&state, &reported, label), Resolution::Reported));
        assert_eq!(triage(&state, "t_1", "blocked"), Triage::Reported);

        // …and a dead-lettered task is skipped for its own reason, not this one.
        state.suppress_task("t_2");
        assert_eq!(triage(&state, "t_2", "blocked"), Triage::Suppressed);
    }

    /// The P1 this closes, stated as the shape that caused it.
    ///
    /// Escalation used to be gated on `action == Action::OpenPr`. Four of the five terminal
    /// kinds never produce that action — they lead with `Label`, and `apply_actions` stops at
    /// the first failure — so a `status/blocked` the repository does not have failed at action
    /// 0 with no escalation possible: no label, no reason comment, nothing on the issue, and
    /// the reconcile sweep replaying it every 300s forever.
    #[test]
    fn the_escalation_gate_covers_the_action_every_non_completed_plan_leads_with() {
        for kind in [
            TerminalKind::Blocked,
            TerminalKind::GaveUp,
            TerminalKind::Crashed,
            TerminalKind::TimedOut,
        ] {
            let detail = task_detail(&[("created", 1), (kind.event_kind(), 2)], &[]);
            let plan = plan(&detail, kind, "status/blocked", true).expect("plans");
            assert_eq!(
                plan.actions[0].action(),
                Action::Label,
                "{} leads with a label, so gating escalation on OpenPr cannot fire",
                kind.event_kind()
            );
            // …and that action can escalate, by comment alone — the verb with no
            // repository-side precondition, which is the one thing the label case can rely on.
            let actions = escalation_actions(&detail, Action::Label, Some("status/blocked"), 3, "boom")
                .expect("escalates");
            assert_eq!(actions.len(), 1);
            assert_eq!(actions[0].action(), Action::Comment);
        }
    }

    /// The R6 P2 this closes: one marker per release left every *other* authorized 🐝 pending
    /// forever, and a pending 🐝 is a stored release.
    ///
    /// Two maintainers approve issue #57 before it runs. alex's releases it and is marked; the
    /// worker later blocks it with `needs_input`; the next 60s sweep finds bo's still-unconsumed
    /// 🐝 from *before* the run and releases it again, re-dispatching the agent onto a question
    /// nobody answered — invisibly, because `BLOCK_MARKER` suppresses the repeat Gitea comment.
    #[test]
    fn releasing_a_task_consumes_every_approval_standing_at_the_time() {
        let state = BridgeState::new();
        let at = "2026-08-04T16:23:00Z";
        let approvals = vec![
            Approval {
                by: "alex".into(),
                fingerprint: format!("alex@{at}"),
            },
            Approval {
                by: "bo".into(),
                fingerprint: format!("bo@{at}"),
            },
        ];

        // Nothing recorded yet: both are written, not just the one that does the releasing.
        let fresh = task_detail(&[("created", 1)], &[]);
        assert_eq!(
            unconsumed(&approvals, &state, Some(&fresh))
                .iter()
                .map(|a| a.by.as_str())
                .collect::<Vec<_>>(),
            vec!["alex", "bo"],
        );

        // After the release both are marked, so the worker's later block is *not* released by
        // a reaction that was standing before the task ever ran.
        let released = task_detail(
            &[("created", 1), ("unblocked", 2), ("claimed", 3), ("blocked", 4)],
            &[&approvals[0].consumed_marker(), &approvals[1].consumed_marker()],
        );
        assert!(unconsumed(&approvals, &state, Some(&released)).is_empty());

        // …and the anti-shadowing property is untouched: a genuinely new 🐝 — removed and
        // re-added, or a third maintainer reacting *after* the block — is a new fingerprint,
        // so a human can still release the task without restarting anything.
        let mut with_fresh = approvals.clone();
        with_fresh.push(Approval {
            by: "alex".into(),
            fingerprint: "alex@2026-08-05T09:00:00Z".into(),
        });
        assert_eq!(
            unconsumed(&with_fresh, &state, Some(&released))
                .iter()
                .map(|a| a.fingerprint.as_str())
                .collect::<Vec<_>>(),
            vec!["alex@2026-08-05T09:00:00Z"],
        );

        // A fingerprint whose marker could not be written stays out, or the dead-letter guard
        // would be re-attempted every sweep — which is the loop it exists to stop.
        state.suppress_fingerprint(&approvals[1].fingerprint);
        assert_eq!(
            unconsumed(&approvals, &state, Some(&fresh))
                .iter()
                .map(|a| a.by.as_str())
                .collect::<Vec<_>>(),
            vec!["alex"],
        );

        // With no detail at all — the freshly created task on the gate path — everything
        // unsuppressed is written.
        assert_eq!(unconsumed(&approvals, &state, None).len(), 1);
    }

    /// The R6 P2 this closes: the apply-claim is taken *after* the plan was computed, so the
    /// other leg can have finished the whole apply inside the gap. `create-pull` and the label
    /// are idempotent; the reason comment is not.
    ///
    /// `apply_plan` re-reads the task under the claim and checks this marker, so what has to
    /// hold is that the marker it checks is the one the plan would write.
    #[test]
    fn the_marker_rechecked_under_the_claim_is_the_one_the_plan_would_write() {
        for (kind, marker) in [
            (TerminalKind::Completed, PR_MARKER),
            (TerminalKind::Blocked, BLOCK_MARKER),
            (TerminalKind::GaveUp, BLOCK_MARKER),
            (TerminalKind::Crashed, BLOCK_MARKER),
            (TerminalKind::TimedOut, BLOCK_MARKER),
        ] {
            let detail = task_detail(&[("created", 1), (kind.event_kind(), 2)], &[]);
            let planned = plan(&detail, kind, "status/blocked", true).expect("plans");
            assert_eq!(
                plan_marker(&planned),
                Some(marker),
                "{} must recheck the marker it writes",
                kind.event_kind()
            );

            // …and that marker is exactly what makes the second application a no-op: a task
            // the other leg already reported plans nothing at all.
            let reported = task_detail(&[("created", 1), (kind.event_kind(), 2)], &[marker]);
            assert!(reported.has_marker(marker));
            assert!(matches!(
                plan(&reported, kind, "status/blocked", true),
                Err(PlanError::AlreadyReported { .. })
            ));
        }
    }

    #[test]
    fn cli_parses_the_documented_invocations() {
        let cli = Cli::try_parse_from(["gitea-automations", "--config", "/etc/bridge.yaml", "run"])
            .expect("parses");
        assert!(matches!(cli.command, Command::Run));
        assert_eq!(cli.config, PathBuf::from("/etc/bridge.yaml"));

        assert!(matches!(
            Cli::try_parse_from(["gitea-automations", "poll-once"])
                .expect("parses")
                .command,
            Command::PollOnce
        ));
        assert!(matches!(
            Cli::try_parse_from(["gitea-automations", "check-rules", "rules.yaml"])
                .expect("parses")
                .command,
            Command::CheckRules { .. }
        ));
        assert!(
            Cli::try_parse_from(["gitea-automations"]).is_err(),
            "a subcommand is required"
        );
    }

    #[test]
    fn the_marker_written_back_is_the_one_the_guard_looks_for() {
        // If these drifted apart the guard would never fire and every restart would
        // re-open a pull request.
        assert_eq!(first_line(&completed_comment("t_1", Some("s"))), Some(PR_MARKER));
        assert_eq!(
            first_line(&blocked_comment(
                "t_1",
                TerminalKind::Blocked,
                Some("needs_input"),
                None
            )),
            Some(BLOCK_MARKER)
        );
        // The escalation runs the same round trip — `record_marker` writes `first_line` of
        // what was posted, and `escalation_actions` looks for `escalation_marker` — so a
        // drift here would repost the escalation comment on every sweep.
        for action in [Action::OpenPr, Action::Label, Action::Comment] {
            assert_eq!(
                first_line(&escalation_comment("t_1", action, Some("ctx"), 3, "boom")),
                Some(escalation_marker(action).as_str()),
                "{}",
                action.name()
            );
        }
    }

    #[test]
    fn first_line_skips_leading_blanks() {
        assert_eq!(first_line("\nsecond"), None);
        assert_eq!(first_line(""), None);
        assert_eq!(first_line("only"), Some("only"));
    }

    // ---------------------------------------------------------------------------------------
    // Sweep-level tests.
    //
    // The three functions below — `gate_sweep`, `release_sweep`, `reconcile_sweep` — are the
    // only code in the crate that *dispatches an agent* or *moves a task*, and until now they
    // were covered only through their extracted helpers. A stub `hermes` records every
    // invocation, so the assertions are about what kanban was asked, not merely about what a
    // function returned; wiremock plays the reactions and issue endpoints.
    // ---------------------------------------------------------------------------------------

    /// A stub `hermes` and a stub `gitea-robot`, plus the files that drive them.
    ///
    /// Every invocation is appended to `calls.log`; `listed` records the `--status` of every
    /// `kanban list`, which is the whole subject of the R7 P1 regression test.
    struct Stub {
        dir: tempfile::TempDir,
        hermes: PathBuf,
        robot: PathBuf,
    }

    const STUB_HERMES: &str = r#"#!/bin/sh
set -eu
root="$(dirname "$0")"
{ printf '%s ' "$@" | tr '\n' ' '; printf '\n'; } >> "$root/calls.log"
verb="${2:-}"
case "$verb" in
  list)
    status="__unfiltered__"
    prev=""
    for arg in "$@"; do
      if [ "$prev" = "--status" ]; then status="$arg"; fi
      prev="$arg"
    done
    printf '%s\n' "$status" >> "$root/listed"
    if [ -f "$root/lists/$status" ]; then cat "$root/lists/$status"; else printf '[]\n'; fi
    ;;
  show)
    id="${3:-}"
    if [ -f "$root/tasks/$id.json" ]; then cat "$root/tasks/$id.json"
    else printf 'stub: no such task %s\n' "$id" >&2; exit 3; fi
    ;;
  create)
    if [ -f "$root/create-fails" ]; then printf 'stub: create refused\n' >&2; exit 5; fi
    printf '{"id":"%s","title":"stub","status":"ready"}\n' "$(cat "$root/create-id" 2>/dev/null || printf 't_new')"
    ;;
  comment|promote|unblock)
    printf '%s %s\n' "$verb" "${3:-}" >> "$root/moves"
    printf 'ok\n'
    ;;
  *)
    printf 'stub: unsupported verb %s\n' "$verb" >&2
    exit 4
    ;;
esac
"#;

    const STUB_ROBOT: &str = r#"#!/bin/sh
set -eu
root="$(dirname "$0")"
{ printf '%s ' "$@" | tr '\n' ' '; printf '\n'; } >> "$root/robot.log"
printf 'ok\n'
"#;

    impl Stub {
        fn new() -> Self {
            let dir = tempfile::tempdir().expect("tempdir");
            let hermes = Self::write(dir.path(), "hermes", STUB_HERMES);
            let robot = Self::write(dir.path(), "gitea-robot", STUB_ROBOT);
            Self { dir, hermes, robot }
        }

        fn write(dir: &std::path::Path, name: &str, body: &str) -> PathBuf {
            let script = dir.join(name);
            std::fs::write(&script, body).expect("write stub");
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt;
                std::fs::set_permissions(&script, std::fs::Permissions::from_mode(0o755)).expect("chmod");
            }
            script
        }

        /// What `kanban show <id>` returns.
        fn task(&self, id: &str, json: &str) {
            let dir = self.dir.path().join("tasks");
            std::fs::create_dir_all(&dir).expect("tasks dir");
            std::fs::write(dir.join(format!("{id}.json")), json).expect("write task");
        }

        /// What `kanban list --status <status>` returns.
        fn list(&self, status: &str, json: &str) {
            let dir = self.dir.path().join("lists");
            std::fs::create_dir_all(&dir).expect("lists dir");
            std::fs::write(dir.join(status), json).expect("write list");
        }

        fn create_fails(&self) {
            std::fs::write(self.dir.path().join("create-fails"), "").expect("write sentinel");
        }

        fn lines(&self, name: &str) -> Vec<String> {
            std::fs::read_to_string(self.dir.path().join(name))
                .unwrap_or_default()
                .lines()
                .map(str::to_string)
                .collect()
        }

        /// The `--status` of every `kanban list` this daemon made.
        fn listed(&self) -> Vec<String> {
            self.lines("listed")
        }

        /// Every task kanban was asked to move or comment on.
        fn moves(&self) -> Vec<String> {
            self.lines("moves")
        }

        fn called(&self) -> bool {
            self.dir.path().join("calls.log").exists()
        }

        fn kanban(&self) -> Kanban {
            Kanban::new(&KanbanConfig {
                binary: self.hermes.to_string_lossy().into_owned(),
                ..KanbanConfig::default()
            })
        }

        fn robot(&self) -> Robot {
            Robot::new(&RobotConfig {
                binary: self.robot.to_string_lossy().into_owned(),
                ..RobotConfig::default()
            })
        }

        fn gate(&self) -> PendingApprovals {
            PendingApprovals::load(&self.dir.path().join("gate.json"))
                .expect("an absent file is an empty gate")
        }
    }

    /// One `TaskDetail` payload as `hermes kanban show --json` returns it.
    fn task_json(status: &str, events: &[(&str, i64)], comments: &[&str]) -> String {
        let events: Vec<String> = events
            .iter()
            .map(|(kind, at)| format!(r#"{{"kind":"{kind}","payload":{{}},"created_at":{at}}}"#))
            .collect();
        let comments: Vec<String> = comments
            .iter()
            .map(|body| format!(r#"{{"author":"bridge","body":{}}}"#, serde_json::json!(body)))
            .collect();
        format!(
            r#"{{"task":{{"id":"t_1","title":"probe","body":"gitea-ref: terraphim/gitea#57","status":"{status}"}},
                 "events":[{}],"comments":[{}]}}"#,
            events.join(","),
            comments.join(",")
        )
    }

    /// A config whose gate is `state_file` and whose only repo is `terraphim/gitea`.
    ///
    /// `require_write_permission` is off so the sweeps under test need only the reactions
    /// endpoint; the permission lookup itself is pinned end to end in `wiremock_gitea.rs`.
    fn config(server: &MockServer, state_file: PathBuf) -> Config {
        Config {
            gitea: GiteaConfig {
                base_url: server.uri(),
                token: None,
                request_timeout_secs: 5,
                max_retries: 0,
                retry_backoff_ms: 1,
            },
            repos: vec![RepoRef::new("terraphim", "gitea")],
            kanban: KanbanConfig::default(),
            robot: RobotConfig::default(),
            poll: PollConfig::default(),
            approval: ApprovalConfig {
                require_write_permission: false,
                ..ApprovalConfig::default()
            },
            rules_file: None,
            state_file,
        }
    }

    fn gitea_client(server: &MockServer) -> GiteaClient {
        GiteaClient::new(&GiteaConfig {
            base_url: server.uri(),
            token: None,
            request_timeout_secs: 5,
            max_retries: 0,
            retry_backoff_ms: 1,
        })
        .expect("client")
    }

    /// Mounts both pages the reaction walk asks for: only an *empty* page ends it.
    async fn mount_reactions(server: &MockServer, index: i64, first_page: &str) {
        let route = format!("/api/v1/repos/terraphim/gitea/issues/{index}/reactions");
        Mock::given(method("GET"))
            .and(path(route.clone()))
            .and(query_param("page", "1"))
            .respond_with(ResponseTemplate::new(200).set_body_string(first_page))
            .mount(server)
            .await;
        Mock::given(method("GET"))
            .and(path(route))
            .and(query_param("page", "2"))
            .respond_with(ResponseTemplate::new(200).set_body_string("[]"))
            .mount(server)
            .await;
    }

    async fn mount_issue(server: &MockServer, index: i64, status: u16, body: &str) {
        Mock::given(method("GET"))
            .and(path(format!("/api/v1/repos/terraphim/gitea/issues/{index}")))
            .respond_with(ResponseTemplate::new(status).set_body_string(body))
            .mount(server)
            .await;
    }

    const A_BEE: &str = r#"[{"user":{"login":"alex"},"content":"honeybee",
                             "created_at":"2026-08-04T16:23:00Z"}]"#;

    fn held_issue() -> PendingTask {
        PendingTask {
            owner: "terraphim".into(),
            repo: "gitea".into(),
            index: 57,
            title: "automations daemon".into(),
            body: "gitea-ref: terraphim/gitea#57".into(),
            idempotency_key: "gitea:terraphim/gitea#57".into(),
        }
    }

    /// **The R7 P1.** Neither timed leg may ask kanban to list the one status a `list` moves.
    ///
    /// On hermes v0.19.0 `hermes kanban list --status blocked` *promotes* what it returns, so
    /// the approval sweep (60s) and the reconciliation sweep (300s) were between them
    /// releasing every task a worker had stopped for a human — within a minute of it stopping.
    /// A worker blocks `t_trap` with `needs_input` ("waiting for spec"); the sweep lists it,
    /// hermes hands it back to a worker, and the agent re-runs on the unanswered question with
    /// no human, no log line, and no Gitea comment, because `BLOCK_MARKER` suppresses the
    /// repeat.
    ///
    /// So the assertion is on the *call*, not on the outcome: the stub board is loaded with a
    /// blocked task that has everything a sweep would act on, and the sweeps must never ask
    /// for it. An unfiltered `list` is checked too — it promotes blocked tasks just the same.
    #[tokio::test]
    async fn no_sweep_asks_kanban_for_the_status_that_listing_would_release() {
        // The set-level half, at the two lines a regression would edit.
        assert!(
            !RECONCILE_STATUSES.contains(&UNLISTABLE_STATUS),
            "the reconciliation sweep must not enumerate {UNLISTABLE_STATUS:?}"
        );
        assert!(
            !RELEASE_STATUSES.contains(&UNLISTABLE_STATUS),
            "the approval sweep must not enumerate {UNLISTABLE_STATUS:?}"
        );

        let server = MockServer::start().await;
        mount_issue(&server, 57, 200, r#"{"state":"open"}"#).await;
        mount_reactions(&server, 57, A_BEE).await;

        let stub = Stub::new();
        // A trap, not a fixture. If either sweep ever lists `blocked`, this is what it finds:
        // a bridge task, with a trailer, an authorized 🐝 standing on its issue, and a
        // terminal event with no report marker — everything needed for both legs to act.
        stub.list(
            UNLISTABLE_STATUS,
            r#"[{"id":"t_1","title":"probe","body":"gitea-ref: terraphim/gitea#57","status":"blocked"}]"#,
        );
        stub.task(
            "t_1",
            &task_json("blocked", &[("created", 1), ("claimed", 2), ("blocked", 3)], &[]),
        );

        let cfg = config(&server, stub.dir.path().join("gate.json"));
        let state = BridgeState::new();
        release_sweep(&gitea_client(&server), &stub.kanban(), &cfg, &state).await;
        reconcile_sweep(&cfg, &stub.kanban(), &stub.robot(), &state).await;

        let listed = stub.listed();
        assert_eq!(
            listed.len(),
            RELEASE_STATUSES.len() + RECONCILE_STATUSES.len(),
            "both sweeps must actually have listed something, or this proves nothing: {listed:?}"
        );
        assert!(
            !listed.iter().any(|s| s == UNLISTABLE_STATUS),
            "a `list` of {UNLISTABLE_STATUS:?} releases the tasks it lists: {listed:?}"
        );
        assert!(
            !listed.iter().any(|s| s == "__unfiltered__"),
            "an unfiltered `list` promotes blocked tasks too: {listed:?}"
        );
        assert!(
            stub.moves().is_empty(),
            "nothing on the board may be touched by a sweep that found nothing: {:?}",
            stub.moves()
        );
    }

    /// The other half of the R7 P1 fix: not listing `blocked` must not mean *abandoning*
    /// blocked tasks.
    ///
    /// `kanban show` does not promote — the probe that found the promote-on-read behaviour
    /// confirmed repeated `show` calls leave a blocked task alone — so a blocked task stays
    /// reachable by id. Without this the 🐝 that unblocks a `needs_input` task would simply
    /// stop working, which is the same outcome as the bug with better manners.
    #[tokio::test]
    async fn a_blocked_task_is_released_by_id_rather_than_by_listing_it() {
        let server = MockServer::start().await;
        mount_reactions(&server, 57, A_BEE).await;

        let stub = Stub::new();
        stub.task(
            "t_1",
            &task_json("blocked", &[("created", 1), ("claimed", 2), ("blocked", 3)], &[]),
        );

        let state = BridgeState::new();
        // What the watch leg records, seconds after the worker's `blocked` event fires.
        state.remember_unlisted("t_1");

        let cfg = config(&server, stub.dir.path().join("gate.json"));
        release_sweep(&gitea_client(&server), &stub.kanban(), &cfg, &state).await;

        let moves = stub.moves();
        assert!(
            moves.iter().any(|m| m == "unblock t_1"),
            "an authorized 🐝 must still release a blocked task: {moves:?}"
        );
        assert!(
            moves.iter().any(|m| m == "comment t_1"),
            "and spend the reaction, or the same 🐝 releases it again every 60s: {moves:?}"
        );
        assert!(
            !stub.listed().iter().any(|s| s == UNLISTABLE_STATUS),
            "reached by id, never by a list: {:?}",
            stub.listed()
        );
        assert!(
            state.unlisted().is_empty(),
            "a released task is claimable, so a `list` reaches it again and paying a `show` \
             for it every sweep would be waste"
        );
    }

    /// A blocked task whose Gitea report never landed still gets replayed.
    ///
    /// `watch` fires each terminal event exactly once, so without this the whole `blocked`
    /// kind would lose its safety net: one failed `gitea-robot` call, or a daemon that was
    /// down when the block fired, and the issue stays permanently silent about work that ran
    /// and stopped. The sweep used to reach it by listing `blocked`, at the cost of releasing
    /// it; it reaches it by id instead.
    #[tokio::test]
    async fn a_blocked_task_whose_report_never_landed_is_reconciled_by_id() {
        let server = MockServer::start().await;
        let stub = Stub::new();
        stub.task(
            "t_1",
            &task_json("blocked", &[("created", 1), ("claimed", 2), ("blocked", 3)], &[]),
        );

        let state = BridgeState::new();
        state.remember_unlisted("t_1");

        let cfg = config(&server, stub.dir.path().join("gate.json"));
        reconcile_sweep(&cfg, &stub.kanban(), &stub.robot(), &state).await;

        let robot_log = std::fs::read_to_string(stub.dir.path().join("robot.log")).unwrap_or_default();
        assert!(
            robot_log.contains("edit-issue") && robot_log.contains("--add-labels status/blocked"),
            "the block must reach the issue as a label: {robot_log}"
        );
        assert!(
            robot_log.contains("comment --owner terraphim") && robot_log.contains(BLOCK_MARKER),
            "…and as the reason comment behind it: {robot_log}"
        );
        assert!(
            stub.moves().iter().any(|m| m == "comment t_1"),
            "and the durable marker is written back, or the next sweep reposts it: {:?}",
            stub.moves()
        );
        assert!(
            !stub.listed().iter().any(|s| s == UNLISTABLE_STATUS),
            "{:?}",
            stub.listed()
        );
        assert!(
            state.unlisted().contains(&"t_1".to_string()),
            "still blocked, so still only reachable by id"
        );
    }

    /// **The R7 P2.** The release half of the gate — the only code in the crate that
    /// dispatches an agent — had no test at all.
    ///
    /// What has to hold is the ordering: `create` before `release`. Swapped, a `create` that
    /// failed would drop the issue from the gate with no task behind it, and nothing would
    /// ever come back for it — the inbound leg re-offers only issues `/robot/ready` still
    /// returns, and a released entry is simply gone.
    #[tokio::test]
    async fn an_approved_hold_becomes_a_task_before_it_leaves_the_gate() {
        let server = MockServer::start().await;
        mount_issue(&server, 57, 200, r#"{"state":"open"}"#).await;
        mount_reactions(&server, 57, A_BEE).await;

        let stub = Stub::new();
        stub.task("t_new", &task_json("ready", &[("created", 1)], &[]));
        let gate = stub.gate();
        gate.hold(held_issue()).expect("holds");

        let cfg = config(&server, stub.dir.path().join("gate.json"));
        gate_sweep(
            &gitea_client(&server),
            &stub.kanban(),
            &cfg,
            &BridgeState::new(),
            &gate,
        )
        .await;

        let calls = stub.lines("calls.log");
        let create = calls
            .iter()
            .position(|c| c.contains(" create "))
            .expect("the approval must create the task");
        assert!(
            calls[create].contains("--idempotency-key gitea:terraphim/gitea#57"),
            "{}",
            calls[create]
        );
        // The gate is not a kanban status, and passing one would be actively wrong: a `list`
        // of it promotes what it lists.
        assert!(!calls[create].contains("--initial-status"), "{}", calls[create]);
        assert!(
            stub.moves().iter().any(|m| m == "comment t_new"),
            "the 🐝 is spent on the task it created: {:?}",
            stub.moves()
        );
        assert!(gate.is_empty(), "…and only then does the issue leave the gate");
    }

    /// The same path, failing closed, three ways. None of them may create a task.
    ///
    /// A regression that hoisted `kanban.create` above the `evaluate` match would pass every
    /// other test in this crate: the helpers it is built from would all still be correct.
    #[tokio::test]
    async fn an_unapproved_hold_creates_nothing_and_stays_held() {
        for (name, reactions, approval) in [
            // Nobody reacted.
            ("not requested", "[]", ApprovalConfig::default()),
            // A 🐝 from somebody with no write permission — with the lookup on and no token,
            // this is `Undetermined`, which is the shape a 403 takes in production.
            (
                "undetermined",
                A_BEE,
                ApprovalConfig {
                    require_write_permission: true,
                    ..ApprovalConfig::default()
                },
            ),
        ] {
            let server = MockServer::start().await;
            mount_issue(&server, 57, 200, r#"{"state":"open"}"#).await;
            mount_reactions(&server, 57, reactions).await;

            let stub = Stub::new();
            let gate = stub.gate();
            gate.hold(held_issue()).expect("holds");

            let mut cfg = config(&server, stub.dir.path().join("gate.json"));
            cfg.approval = approval;
            gate_sweep(
                &gitea_client(&server),
                &stub.kanban(),
                &cfg,
                &BridgeState::new(),
                &gate,
            )
            .await;

            assert!(
                !stub.called(),
                "{name}: hermes must not be invoked at all — a task that is never created \
                 cannot be claimed"
            );
            assert_eq!(gate.len(), 1, "{name}: and the issue stays held");
        }

        // …and the failure that happens *after* the decision to create: the task does not
        // exist, so the issue must stay held for the next sweep to retry (create is
        // idempotent, so retrying costs nothing).
        let server = MockServer::start().await;
        mount_issue(&server, 57, 200, r#"{"state":"open"}"#).await;
        mount_reactions(&server, 57, A_BEE).await;
        let stub = Stub::new();
        stub.create_fails();
        let gate = stub.gate();
        gate.hold(held_issue()).expect("holds");
        let cfg = config(&server, stub.dir.path().join("gate.json"));
        gate_sweep(
            &gitea_client(&server),
            &stub.kanban(),
            &cfg,
            &BridgeState::new(),
            &gate,
        )
        .await;
        assert_eq!(gate.len(), 1, "a failed create must not empty the gate");
        assert!(stub.moves().is_empty(), "and nothing is marked as consumed");
    }

    /// **The R7 P2.** A held entry was only ever removed by a successful release, so an issue
    /// that stopped being ready stayed in the gate forever, carrying a title and body frozen
    /// at the last hold.
    ///
    /// The failure it caused: #57 is held; a writer 🐝s it; that day's permission lookup 403s
    /// so it stays `Undetermined`; the issue is closed as wontfix; the token is fixed a week
    /// later; the next sweep creates a kanban task and dispatches an agent onto a closed issue
    /// with a stale title. Nothing else would ever drop the entry — `/robot/ready` stops
    /// offering a closed issue, so it is never re-held either.
    #[tokio::test]
    async fn a_held_issue_that_is_no_longer_open_is_dropped_rather_than_dispatched() {
        for (name, status, body) in [
            ("closed", 200u16, r#"{"state":"closed"}"#),
            ("deleted", 404, r#"{"message":"Not Found"}"#),
        ] {
            let server = MockServer::start().await;
            mount_issue(&server, 57, status, body).await;
            // Standing and authorized. The point is that it is never even looked at.
            mount_reactions(&server, 57, A_BEE).await;

            let stub = Stub::new();
            let gate = stub.gate();
            gate.hold(held_issue()).expect("holds");

            let cfg = config(&server, stub.dir.path().join("gate.json"));
            gate_sweep(
                &gitea_client(&server),
                &stub.kanban(),
                &cfg,
                &BridgeState::new(),
                &gate,
            )
            .await;

            assert!(!stub.called(), "{name}: no agent may be dispatched onto it");
            assert!(
                gate.is_empty(),
                "{name}: and the entry is gone, not merely skipped"
            );
        }
    }

    /// …and the converse, which is what keeps the drop from becoming its own outage: the gate
    /// is emptied only on a *definite* answer.
    ///
    /// `Issue::state` is `#[serde(default)]`, so a payload the decoder does not recognise
    /// yields `""`. Treating that as "not open" would silently discard every held issue on the
    /// first Gitea upgrade that renamed the field — the one failure mode with no diagnostic
    /// anywhere, since a gate that holds nothing looks exactly like a board nobody has filed
    /// against.
    #[tokio::test]
    async fn a_gitea_answer_the_gate_cannot_read_leaves_the_issue_held() {
        for (name, status, body) in [
            ("unrecognised state", 200u16, r#"{"reason":"who knows"}"#),
            ("server error", 500, r#"{"message":"boom"}"#),
        ] {
            let server = MockServer::start().await;
            mount_issue(&server, 57, status, body).await;
            mount_reactions(&server, 57, A_BEE).await;

            let stub = Stub::new();
            let gate = stub.gate();
            gate.hold(held_issue()).expect("holds");

            let cfg = config(&server, stub.dir.path().join("gate.json"));
            gate_sweep(
                &gitea_client(&server),
                &stub.kanban(),
                &cfg,
                &BridgeState::new(),
                &gate,
            )
            .await;

            assert_eq!(gate.len(), 1, "{name}: the gate must not empty on a non-answer");
            assert!(!stub.called(), "{name}: and nothing is created on one either");
        }
    }
}
