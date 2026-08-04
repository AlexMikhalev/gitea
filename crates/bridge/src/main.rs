// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//! `gitea-automations` — the F4 bridge daemon.
//!
//! Three concurrent legs, all of them stateless in this process:
//!
//! * inbound — poll ready issues, create kanban tasks (deduplicated by idempotency key);
//! * outbound — watch the five terminal kanban events, return PRs / blocks to Gitea;
//! * approval — poll 🐝 reactions, promote the tasks a human blessed.
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
use bridge::hermes::Kanban;
use bridge::inbound::{GiteaRef, poll_once};
use bridge::outbound::{
    OutboundPlan, PlanError, PlannedAction, TerminalKind, WatchLine, classify_watch_line,
    latest_terminal_kind, plan,
};
use bridge::robot::Robot;
use bridge::rules::RuleSet;

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
    /// Run all three legs until interrupted.
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
            Ok(())
        }
        Command::PollOnce => {
            for repo in &cfg.repos {
                inbound_sweep(&gitea, &kanban, &cfg, repo).await;
            }
            Ok(())
        }
        Command::ApprovalOnce => {
            approval_sweep(&gitea, &kanban, &cfg).await;
            Ok(())
        }
        Command::ReconcileOnce => {
            reconcile_sweep(&cfg, &kanban, &robot).await;
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

    let inbound = {
        let (cfg, gitea, kanban) = (cfg.clone(), gitea.clone(), kanban.clone());
        tokio::spawn(async move {
            let mut ticker = tokio::time::interval(cfg.ready_interval());
            loop {
                ticker.tick().await;
                for repo in &cfg.repos {
                    inbound_sweep(&gitea, &kanban, &cfg, repo).await;
                }
            }
        })
    };

    let approval = {
        let (cfg, gitea, kanban) = (cfg.clone(), gitea.clone(), kanban.clone());
        tokio::spawn(async move {
            let mut ticker = tokio::time::interval(cfg.approval_interval());
            loop {
                ticker.tick().await;
                approval_sweep(&gitea, &kanban, &cfg).await;
            }
        })
    };

    let outbound = {
        let (cfg, kanban, robot) = (cfg.clone(), kanban.clone(), robot.clone());
        tokio::spawn(async move { outbound_loop(cfg, kanban, robot).await })
    };

    // The fourth leg, and the one that makes the other three's failure modes recoverable.
    // `watch` is a live stream and a terminal event fires exactly once, so a single failed
    // `gitea-robot` call — or a daemon that was down when the event fired — would otherwise
    // drop that issue's feedback for good.
    let reconcile = {
        let (cfg, kanban, robot) = (cfg.clone(), kanban.clone(), robot.clone());
        tokio::spawn(async move {
            let mut ticker = tokio::time::interval(cfg.reconcile_interval());
            loop {
                ticker.tick().await;
                reconcile_sweep(&cfg, &kanban, &robot).await;
            }
        })
    };

    tokio::signal::ctrl_c().await.context("waiting for ctrl-c")?;
    tracing::info!("shutting down; kanban keeps every claim and heartbeat");
    inbound.abort();
    approval.abort();
    outbound.abort();
    reconcile.abort();
    Ok(())
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
async fn approval_sweep(gitea: &GiteaClient, kanban: &Kanban, cfg: &Config) {
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
                    let Some(approval) = approvals.iter().find(|a| !a.is_consumed(&detail)) else {
                        tracing::debug!(
                            task = %task.id, issue = gref.index,
                            "every approval reaction on this issue has already been acted on; \
                             a new 🐝 is needed to release it again"
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
                            // retried on the next sweep.
                            if let Err(err) = kanban.comment(&task.id, &approval.consumed_marker()).await {
                                tracing::warn!(
                                    task = %task.id, error = %err,
                                    "cannot record the consumed approval; this 🐝 may release the task again"
                                );
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
async fn outbound_loop(cfg: Arc<Config>, kanban: Kanban, robot: Robot) {
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
                                apply_event(&cfg, &kanban, &robot, &event.task_id, event.kind).await;
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
async fn reconcile_sweep(cfg: &Config, kanban: &Kanban, robot: &Robot) {
    let (mut checked, mut replayed) = (0u64, 0u64);
    for status in ["done", "blocked"] {
        let tasks = match kanban.list(Some(status)).await {
            Ok(tasks) => tasks,
            Err(err) => {
                tracing::error!(status, error = %err, "kanban list failed");
                continue;
            }
        };
        for task in tasks {
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
            let detail = match kanban.show(&task.id).await {
                Ok(detail) => detail,
                Err(err) => {
                    tracing::error!(task = %task.id, error = %err, "cannot resolve task detail");
                    continue;
                }
            };
            let Some(kind) = latest_terminal_kind(&detail) else {
                continue;
            };
            checked += 1;
            match plan(&detail, kind, robot.blocked_label(), true) {
                Ok(plan) => {
                    tracing::info!(
                        task = %task.id, kind = kind.event_kind(),
                        "terminal task has no report marker; replaying its plan"
                    );
                    replayed += 1;
                    apply_plan(cfg, kanban, robot, &task.id, &plan).await;
                }
                // The overwhelmingly common case: the watcher already handled it.
                Err(PlanError::AlreadyReported { .. }) => {}
                Err(err) => {
                    tracing::error!(task = %task.id, error = %err, "cannot plan outbound actions")
                }
            }
        }
    }
    tracing::info!(checked, replayed, "reconciliation sweep complete");
}

async fn apply_event(cfg: &Config, kanban: &Kanban, robot: &Robot, task_id: &str, kind: TerminalKind) {
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
    apply_plan(cfg, kanban, robot, task_id, &plan).await;
}

/// Applies one planned response to Gitea, then marks the task once all of it landed.
async fn apply_plan(cfg: &Config, kanban: &Kanban, robot: &Robot, task_id: &str, plan: &OutboundPlan) {
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
    let mut marker: Option<&str> = None;
    for action in &plan.actions {
        let outcome = match action {
            PlannedAction::OpenPull(pr) => robot.create_pull(owner, repo, pr).await.map(|_| ()),
            PlannedAction::Comment(body) => robot.comment(owner, repo, index, body).await.map(|_| ()),
            PlannedAction::Label(labels) => robot.add_labels(owner, repo, index, labels).await.map(|_| ()),
        };
        match outcome {
            Ok(()) => {
                tracing::info!(task = task_id, %owner, %repo, index, action = action.action().name(), "applied");
                if let PlannedAction::Comment(body) = action {
                    marker = marker.or(first_line(body));
                }
            }
            Err(err) => {
                // Stop at the first failure and leave the task unmarked. The unmarked task
                // is what `reconcile_sweep` looks for, and that sweep — not the next
                // terminal event — is what actually replays this plan: a terminal kanban
                // event fires once, so nothing else would ever come back for this task.
                //
                // Replaying re-runs the actions that already landed, so the two that can
                // land before a failure are idempotent on the Gitea side: `create-pull`
                // probes `GET /pulls/{base}/{head}` and reports an existing *open, unmerged*
                // pull request as success (`cmd/gitea-robot/write.go`), and adding a label
                // the issue already carries is a no-op
                // (`models/issues/issue_label.go:125-130`). Without that probe a `completed`
                // plan whose audit comment failed would wedge forever — Gitea refuses a
                // second pull request for the same base and head, so every retry would fail
                // on the first action and the comment would never be posted. The comment
                // itself is last in every plan, so a replay repeats it only when it was the
                // action that failed.
                tracing::error!(
                    task = task_id, %owner, %repo, index,
                    action = action.action().name(), error = %err,
                    "action failed; leaving the task unmarked for the reconciliation sweep to replay"
                );
                return;
            }
        }
    }

    if let Some(marker) = marker
        && let Err(err) = kanban.comment(task_id, marker).await
    {
        tracing::warn!(task = task_id, error = %err, "cannot write the kanban dedup marker");
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
    use bridge::outbound::{BLOCK_MARKER, PR_MARKER, blocked_comment, completed_comment};

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
    }

    #[test]
    fn first_line_skips_leading_blanks() {
        assert_eq!(first_line("\nsecond"), None);
        assert_eq!(first_line(""), None);
        assert_eq!(first_line("only"), Some("only"));
    }
}
