// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//! `gitea-automations` — the F4 bridge daemon.
//!
//! Four concurrent legs, all of them stateless in this process:
//!
//! * inbound — poll ready issues, create kanban tasks (deduplicated by idempotency key);
//! * outbound — watch the five terminal kanban events, return PRs / blocks to Gitea;
//! * approval — poll 🐝 reactions, promote the tasks a human blessed;
//! * reconcile — replay the terminal tasks whose Gitea feedback never landed.
//!
//! Nothing here owns liveness. Claims, heartbeats, reclaim and the circuit breaker are
//! kanban's, so killing this process orphans nothing and restarting it duplicates nothing.

use std::path::PathBuf;
use std::sync::Arc;

use anyhow::{Context, Result};
use clap::{Parser, Subcommand};

use bridge::approval::{ApprovalOutcome, Preflight, evaluate, preflight};
use bridge::config::{Config, RepoRef};
use bridge::gitea::GiteaClient;
use bridge::hermes::{Kanban, RECONCILE_STATUSES, TaskDetail};
use bridge::inbound::{GiteaRef, poll_once};
use bridge::outbound::{
    OutboundPlan, PlanError, PlannedAction, TerminalKind, WatchLine, classify_watch_line, escalation_actions,
    latest_terminal_kind, plan,
};
use bridge::robot::{LabelCheck, Robot, check_blocked_label};
use bridge::rules::{Action, RuleSet};
use bridge::state::{BridgeState, ESCALATE_AFTER, MARKER_ATTEMPTS};

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

    match cli.command {
        Command::CheckRules { .. } => unreachable!("handled above"),
        Command::Check => {
            println!(
                "config OK: {} repo(s), board {:?}",
                cfg.repos.len(),
                cfg.kanban.board
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
            // `hermes kanban create` has no `--status` flag and defaults to `ready`, which
            // is immediately claimable. Whether a human sees an issue before an agent does
            // therefore hangs entirely on this one setting, and getting it wrong is silent
            // in both directions — so it is stated rather than assumed.
            if cfg.kanban.require_approval {
                println!(
                    "approval gate ON: tasks are created with --initial-status blocked, so a \
                     {:?} reaction from a writer is what releases each one to a worker",
                    cfg.approval.reaction
                );
            } else {
                println!(
                    "warning: kanban.require_approval is off — `hermes kanban create` defaults \
                     to `ready`, so every ready issue is dispatched to an agent with no human \
                     approval, and the 🐝 leg only ever sees tasks kanban itself blocked"
                );
            }
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
                inbound_sweep(&gitea, &kanban, &cfg, repo).await;
            }
            Ok(())
        }
        Command::ApprovalOnce => {
            approval_sweep(&gitea, &kanban, &cfg, &BridgeState::new()).await;
            Ok(())
        }
        Command::ReconcileOnce => {
            reconcile_sweep(&cfg, &kanban, &robot, &BridgeState::new()).await;
            Ok(())
        }
        Command::Run => run(Arc::new(cfg), gitea, kanban, robot).await,
    }
}

async fn run(cfg: Arc<Config>, gitea: GiteaClient, kanban: Kanban, robot: Robot) -> Result<()> {
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

    let mut inbound = {
        let (cfg, gitea, kanban) = (cfg.clone(), gitea.clone(), kanban.clone());
        tokio::spawn(async move {
            let mut ticker = interval(cfg.ready_interval());
            loop {
                ticker.tick().await;
                for repo in &cfg.repos {
                    inbound_sweep(&gitea, &kanban, &cfg, repo).await;
                }
            }
        })
    };

    // The only in-process state in the daemon, shared by the three legs that write durable
    // markers. It holds no truth of its own — kanban stays the durable side — but it is what
    // stops a marker write that failed *after its retries* from becoming an endless loop.
    let state = Arc::new(BridgeState::new());

    let mut approval = {
        let (cfg, gitea, kanban, state) = (cfg.clone(), gitea.clone(), kanban.clone(), state.clone());
        tokio::spawn(async move {
            let mut ticker = interval(cfg.approval_interval());
            loop {
                ticker.tick().await;
                approval_sweep(&gitea, &kanban, &cfg, &state).await;
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
/// [`tokio::time::interval`] defaults to [`MissedTickBehavior::Burst`], which replays every
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

async fn inbound_sweep(gitea: &GiteaClient, kanban: &Kanban, cfg: &Config, repo: &RepoRef) {
    match poll_once(
        gitea,
        kanban,
        repo,
        &cfg.gitea.base_url,
        cfg.poll.skip_in_progress,
    )
    .await
    {
        Ok(report) => tracing::info!(
            repo = %repo.slug(),
            resolved = report.resolved.len(),
            failed = report.failed.len(),
            "inbound sweep complete"
        ),
        Err(err) => tracing::error!(repo = %repo.slug(), error = %err, "inbound sweep failed"),
    }
}

/// Sweeps 🐝 reactions for every bridge-created task waiting on a human.
///
/// `todo` and `blocked` are the two statuses a human decision can move; `triage` is the
/// specifier's, not ours. With `kanban.require_approval` on, this is also the leg that
/// releases a freshly created task — inbound creates it `blocked` precisely so that it does.
async fn approval_sweep(gitea: &GiteaClient, kanban: &Kanban, cfg: &Config, state: &BridgeState) {
    for status in ["blocked", "todo"] {
        let tasks = match kanban.list(Some(status)).await {
            Ok(tasks) => tasks,
            Err(err) => {
                tracing::error!(status, error = %err, "kanban list failed");
                continue;
            }
        };
        for task in tasks {
            let Some(gref) = task.body.as_deref().and_then(GiteaRef::parse_from_body) else {
                continue;
            };
            if !cfg
                .repos
                .iter()
                .any(|r| r.owner == gref.owner && r.repo == gref.repo)
            {
                continue;
            }
            match evaluate(gitea, &cfg.approval, &gref.owner, &gref.repo, gref.index).await {
                Ok(ApprovalOutcome::Approved { approvals }) => {
                    // A reaction is durable and Gitea records nothing about it having been
                    // used, so without this the same 🐝 unblocks the task on every sweep: it
                    // runs, blocks with `transient`, is unblocked again, forever — and
                    // invisibly, because BLOCK_MARKER suppresses the repeat Gitea comment.
                    let detail = match kanban.show(&task.id).await {
                        Ok(detail) => detail,
                        Err(err) => {
                            tracing::error!(task = %task.id, error = %err, "cannot resolve task detail");
                            continue;
                        }
                    };
                    // …and the in-process guard covers the case where that durable record
                    // could not be written at all: without it a marker write that failed
                    // after its retries releases the task again on every sweep.
                    let Some(approval) = approvals.iter().find(|a| {
                        !a.is_consumed(&detail) && !state.is_fingerprint_suppressed(&a.fingerprint)
                    }) else {
                        tracing::debug!(
                            task = %task.id, issue = gref.index,
                            "every approval reaction on this issue has already been acted on \
                             (or suppressed); a new 🐝 is needed to release it again"
                        );
                        continue;
                    };
                    let action = if status == "blocked" { "unblock" } else { "promote" };
                    let result = if status == "blocked" {
                        kanban.unblock(&task.id).await
                    } else {
                        kanban.promote(&task.id).await
                    };
                    match result {
                        Ok(()) => {
                            tracing::info!(task = %task.id, by = %approval.by, action, "approval promoted task");
                            // Written after the move, never before: a marker without a move
                            // would strand the task, whereas a move without a marker is
                            // retried on the next sweep. That "retried on the next sweep" is
                            // only safe while the marker eventually lands, so it is retried,
                            // and a failure that survives the retries is a dead letter rather
                            // than a warning: the same 🐝 would otherwise release this task
                            // every 60 seconds forever, running an agent each time, while
                            // Gitea shows nothing because BLOCK_MARKER suppresses the repeat
                            // comment.
                            if let Err(err) = kanban
                                .comment_with_retry(&task.id, &approval.consumed_marker(), MARKER_ATTEMPTS)
                                .await
                            {
                                tracing::error!(
                                    task = %task.id, fingerprint = %approval.fingerprint,
                                    attempts = MARKER_ATTEMPTS, error = %err,
                                    "cannot record the consumed approval after retries; suppressing \
                                     this fingerprint for the lifetime of this process to avoid a \
                                     release loop. A fresh 🐝 (removed and re-added) still releases \
                                     the task, and a restart costs at most one extra release"
                                );
                                state.suppress_fingerprint(&approval.fingerprint);
                            }
                        }
                        Err(err) => {
                            tracing::error!(task = %task.id, action, error = %err, "promotion failed")
                        }
                    }
                }
                Ok(ApprovalOutcome::NotRequested) => {}
                Ok(ApprovalOutcome::NotAuthorized { reactors }) => tracing::info!(
                    task = %task.id, issue = gref.index, ?reactors,
                    "ignoring approval reaction: no reactor has write permission"
                ),
                Ok(ApprovalOutcome::Undetermined { reason, reactors }) => tracing::warn!(
                    task = %task.id, issue = gref.index, ?reactors, %reason,
                    "approval undetermined; failing closed"
                ),
                Err(err) => {
                    tracing::error!(task = %task.id, issue = gref.index, error = %err, "reaction poll failed")
                }
            }
        }
    }
}

/// Follows `kanban watch` and applies each terminal event to Gitea.
///
/// The watcher is restarted if it exits, because a dead watcher is silent, and silence here
/// looks exactly like an idle board.
async fn outbound_loop(cfg: Arc<Config>, kanban: Kanban, robot: Robot, state: Arc<BridgeState>) {
    loop {
        match kanban.watch().await {
            Ok((mut child, mut lines)) => {
                tracing::info!("watching kanban terminal events");
                // The `watch` line format is a terminal display, not a documented interface.
                // Counting what parsed is what turns a change to it into a log line instead
                // of a daemon that reports "watching" and then never acts again.
                let (mut parsed, mut unparsed) = (0u64, 0u64);
                loop {
                    match lines.next_line().await {
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
                let _ = child.kill().await;
                if parsed == 0 && unparsed > 0 {
                    tracing::warn!(
                        unparsed,
                        "kanban watch produced output but no line matched the expected event \
                         format; the outbound leg acted on nothing"
                    );
                }
                tracing::warn!("kanban watch exited; restarting");
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
/// The sweep keys on the task's *event trail*, not its status. A task created with
/// `--initial-status blocked` to await a 🐝 is `blocked` and has no terminal event, and
/// labelling its issue `status/blocked` would be a lie about work that never ran.
///
/// It enumerates [`RECONCILE_STATUSES`] — every documented status — rather than the two a
/// terminal event was *observed* to leave behind. Nothing pins where `crashed`, `gave_up` or
/// `timed_out` land, and kanban's crash-reclaim exists to return a dead worker's task to a
/// claimable status, so a `done`/`blocked` scan silently covered two of the five kinds and
/// dropped the other three's feedback permanently — for the kinds most likely to coincide
/// with the infrastructure trouble that made this sweep necessary in the first place.
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
    let (mut checked, mut replayed, mut reported, mut idle, mut suppressed) = (0u64, 0u64, 0u64, 0u64, 0u64);
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
            // nine times and kanban may move a task between two of those listings.
            if !seen.insert(task.id.clone()) {
                continue;
            }
            // Filter on the trailer before paying for a `show`: the board is shared, and
            // this sweep walks all of it rather than only what the watcher happened to see.
            let Some(gref) = task.body.as_deref().and_then(GiteaRef::parse_from_body) else {
                continue;
            };
            if !cfg
                .repos
                .iter()
                .any(|r| r.owner == gref.owner && r.repo == gref.repo)
            {
                continue;
            }
            match triage(state, &task.id, &task.status) {
                Triage::Reported => {
                    reported += 1;
                    continue;
                }
                Triage::Suppressed => {
                    suppressed += 1;
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
            match resolve(state, &detail, robot.blocked_label()) {
                Resolution::Idle => idle += 1,
                Resolution::Reported => {
                    checked += 1;
                    reported += 1;
                }
                Resolution::Replay(kind, plan) => {
                    checked += 1;
                    replayed += 1;
                    tracing::info!(
                        task = %task.id, kind = kind.event_kind(),
                        "terminal task has no report marker; replaying its plan"
                    );
                    apply_plan(cfg, kanban, robot, state, &task.id, &plan, &detail).await;
                }
                Resolution::Unplannable(err) => {
                    checked += 1;
                    tracing::error!(task = %task.id, error = %err, "cannot plan outbound actions");
                }
            }
        }
    }
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
        candidates = seen.len(),
        "reconciliation sweep complete"
    );
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
/// The trail, not the status: a task created `--initial-status blocked` to await a 🐝 is
/// `blocked` and has no terminal event, and labelling its issue `status/blocked` would be a
/// lie about work that never ran.
#[derive(Debug)]
enum Resolution {
    /// No terminal event yet — awaiting a 🐝, or still running.
    Idle,
    /// Its terminal event is confirmed already reported to Gitea.
    Reported,
    /// A terminal event with no report marker behind it. Apply this.
    Replay(TerminalKind, OutboundPlan),
    /// A terminal event the sweep cannot turn into actions.
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
        Err(err) => {
            tracing::error!(task = task_id, error = %err, "cannot plan outbound actions");
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

#[cfg(test)]
mod tests {
    use super::*;
    use bridge::hermes::{Task, TaskComment, TaskDetail, TaskEvent};
    use bridge::outbound::{
        BLOCK_MARKER, PR_MARKER, blocked_comment, completed_comment, escalation_comment, escalation_marker,
    };

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

        // 3. Created blocked to await a 🐝: `blocked`, but it never ran. Keying on status
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
    /// On the default config (`require_approval: true`) every bridge task passes through an
    /// idle `blocked` on its way in, so this is the *normal* path, not a corner:
    ///
    /// 1. inbound creates the task `--initial-status blocked`; the sweep lists it under
    ///    `blocked`, finds only a `created` event, and used to cache `(t_1, "blocked")`;
    /// 2. a 🐝 releases it → `ready` → `running`; the worker blocks it → `blocked` again,
    ///    now with a `blocked` event on the trail;
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

        // Sweep 1 — created blocked to await a 🐝. Nothing to report, and — the fix —
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
}
