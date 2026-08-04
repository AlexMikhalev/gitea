// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//! Acceptance criterion 1, end to end without live dependencies.
//!
//! Wiremock plays `/api/v1/robot/ready`; a stub binary plays `hermes`, implementing the one
//! behaviour the bridge relies on — `--idempotency-key` returns the existing task id rather
//! than creating a duplicate. Two poll cycles over one ready issue must therefore produce
//! the same id, and the stub records every invocation so the assertion is on *what kanban
//! was asked*, not merely on what the bridge returned.

use std::path::Path;

use bridge::config::{GiteaConfig, KanbanConfig, RepoRef};
use bridge::gitea::GiteaClient;
use bridge::hermes::Kanban;
use bridge::inbound::poll_once;
use bridge::state::PendingApprovals;
use wiremock::matchers::{method, path};
use wiremock::{Mock, MockServer, ResponseTemplate};

const READY_BODY: &str = r#"{
  "repo_id": 7,
  "repo_name": "gitea",
  "total_count": 1,
  "ready_issues": [
    {"id": 91, "index": 57, "title": "automations daemon", "page_rank": 0.4237,
     "priority": 2, "is_blocked": false, "blocker_count": 0}
  ]
}"#;

/// Writes a stub that mimics `hermes kanban create --idempotency-key … --json`.
///
/// State lives in `<dir>/keys/<sanitised key>`, so the stub is as stateless across
/// invocations as the real thing: the key is the only memory.
fn write_stub_hermes(dir: &Path) -> std::path::PathBuf {
    let script = dir.join("hermes");
    let body = r#"#!/bin/sh
set -eu
root="$(dirname "$0")"
mkdir -p "$root/keys"
# One line per invocation: bodies contain newlines, so flatten them.
{ printf '%s ' "$@" | tr '\n' ' '; printf '\n'; } >> "$root/calls.log"
key=""
prev=""
# `hermes kanban create` has no --status and defaults to ready, so the stub does too:
# a task the bridge did not explicitly block is immediately claimable.
status="ready"
for arg in "$@"; do
  if [ "$prev" = "--idempotency-key" ]; then key="$arg"; fi
  if [ "$prev" = "--initial-status" ]; then status="$arg"; fi
  prev="$arg"
done
[ -n "$key" ] || { echo "stub: no --idempotency-key" >&2; exit 2; }
safe=$(printf '%s' "$key" | tr -c 'A-Za-z0-9' '_')
file="$root/keys/$safe"
if [ -f "$file" ]; then
  id=$(cat "$file")
else
  id="t_$(cat "$root/counter" 2>/dev/null || echo 0)"
  echo $(( $(cat "$root/counter" 2>/dev/null || echo 0) + 1 )) > "$root/counter"
  printf '%s' "$id" > "$file"
fi
printf '{"id": "%s", "title": "stub", "status": "%s"}\n' "$id" "$status"
"#;
    std::fs::write(&script, body).expect("write stub");
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&script, std::fs::Permissions::from_mode(0o755)).expect("chmod");
    }
    script
}

#[tokio::test]
async fn two_poll_cycles_over_one_ready_issue_create_exactly_one_task() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/v1/robot/ready"))
        .respond_with(ResponseTemplate::new(200).set_body_string(READY_BODY))
        .expect(2)
        .mount(&server)
        .await;

    let dir = tempfile::tempdir().expect("tempdir");
    let stub = write_stub_hermes(dir.path());

    let gitea = GiteaClient::new(&GiteaConfig {
        base_url: server.uri(),
        request_timeout_secs: 5,
        max_retries: 1,
        retry_backoff_ms: 1,
        ..GiteaConfig::default()
    })
    .expect("client");
    let kanban = Kanban::new(&KanbanConfig {
        binary: stub.to_string_lossy().into_owned(),
        board: Some("f4-test".into()),
        ..KanbanConfig::default()
    });
    let repo = RepoRef::new("terraphim", "gitea");

    let first = poll_once(&gitea, &kanban, &repo, &server.uri(), true, 25, None)
        .await
        .expect("first sweep");
    let second = poll_once(&gitea, &kanban, &repo, &server.uri(), true, 25, None)
        .await
        .expect("second sweep");

    assert_eq!(first.failed, vec![], "no issue may fail");
    assert_eq!(second.failed, vec![]);
    let a = first.task_for(57).expect("first cycle resolved issue 57");
    let b = second.task_for(57).expect("second cycle resolved issue 57");
    assert_eq!(a, b, "the same task id must come back, verifying dedup by key");

    let calls = std::fs::read_to_string(dir.path().join("calls.log")).expect("calls.log");
    let create_calls: Vec<_> = calls.lines().filter(|l| l.contains(" create ")).collect();
    assert_eq!(
        create_calls.len(),
        2,
        "both cycles call kanban; kanban is what dedups"
    );
    for call in &create_calls {
        assert!(
            call.contains("--idempotency-key gitea:terraphim/gitea#57"),
            "{call}"
        );
        assert!(call.contains("--board f4-test"), "{call}");
        // The gate is not a kanban status any more, and passing one would be actively wrong:
        // `hermes kanban list` promotes a blocked task by reading it, and this sweep ran with
        // the gate off — the operator asked for the task to be claimable.
        assert!(!call.contains("--initial-status"), "{call}");
    }
    // Exactly one task materialised, no matter how many times we asked.
    let keys = std::fs::read_dir(dir.path().join("keys"))
        .expect("keys dir")
        .count();
    assert_eq!(keys, 1);
}

/// The R6 P1, pinned where CI actually runs it: with the gate on, the inbound leg creates
/// **nothing**.
///
/// The gate used to be `hermes kanban create --initial-status blocked`, released by the 🐝
/// sweep. That hold does not exist on the deployed hermes: v0.19.0 *promotes* a blocked task on
/// any `kanban list` read — plain and with `--status blocked`, both probed live on 2026-08-04 —
/// and `approval_sweep`'s very first call is a `kanban list`. So the sweep looking for a task to
/// release was itself what released every gated task to a worker, unapproved, with nothing in
/// the log to say so.
///
/// Only one thing closes that from inside this repo: do not create the task. An issue with no
/// kanban task cannot be listed, promoted, or claimed. This asserts exactly that — the stub
/// records every invocation, and with the gate on there are none — plus the other half, that
/// what a 🐝 needs later is durably written down rather than left in this process.
#[tokio::test]
async fn a_gated_sweep_creates_no_kanban_task_at_all() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/v1/robot/ready"))
        .respond_with(ResponseTemplate::new(200).set_body_string(READY_BODY))
        .mount(&server)
        .await;

    let dir = tempfile::tempdir().expect("tempdir");
    let stub = write_stub_hermes(dir.path());
    let state_file = dir.path().join("gate").join("bridge-state.json");
    let gate = PendingApprovals::load(&state_file).expect("an absent file is an empty gate");

    let gitea = GiteaClient::new(&GiteaConfig {
        base_url: server.uri(),
        request_timeout_secs: 5,
        max_retries: 0,
        retry_backoff_ms: 1,
        ..GiteaConfig::default()
    })
    .expect("client");
    let kanban = Kanban::new(&KanbanConfig {
        binary: stub.to_string_lossy().into_owned(),
        require_approval: true,
        ..KanbanConfig::default()
    });
    let repo = RepoRef::new("terraphim", "gitea");

    let report = poll_once(&gitea, &kanban, &repo, &server.uri(), true, 25, Some(&gate))
        .await
        .expect("sweep");
    assert_eq!(report.held, vec![57], "the issue is held, not created");
    assert!(report.resolved.is_empty(), "and no task id exists to resolve");
    assert_eq!(report.failed, vec![]);
    assert!(
        !dir.path().join("calls.log").exists(),
        "hermes must not be invoked at all: a task that is never created cannot be promoted \
         out from under the gate by a `kanban list`"
    );

    // The other half: a 🐝 arriving after a restart must still find what to create.
    let reloaded = PendingApprovals::load(&state_file).expect("the gate is durable");
    let held = reloaded.held();
    assert_eq!(held.len(), 1);
    assert_eq!(held[0].idempotency_key, "gitea:terraphim/gitea#57");
    assert_eq!(held[0].index, 57);
    assert_eq!(held[0].title, "automations daemon");
    assert!(
        held[0].body.contains("gitea-ref: terraphim/gitea#57"),
        "the trailer both list-driven legs key on must survive the gate: {}",
        held[0].body
    );

    // A second sweep re-offers the same still-ready issue, and that is a no-op rather than a
    // second entry — which is also why losing the file only costs one ready interval.
    poll_once(&gitea, &kanban, &repo, &server.uri(), true, 25, Some(&gate))
        .await
        .expect("second sweep");
    assert_eq!(gate.len(), 1);
    assert!(!dir.path().join("calls.log").exists());
}

#[tokio::test]
async fn a_kanban_failure_is_collected_not_fatal() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/v1/robot/ready"))
        .respond_with(ResponseTemplate::new(200).set_body_string(READY_BODY))
        .mount(&server)
        .await;

    let gitea = GiteaClient::new(&GiteaConfig {
        base_url: server.uri(),
        request_timeout_secs: 5,
        max_retries: 0,
        retry_backoff_ms: 1,
        ..GiteaConfig::default()
    })
    .expect("client");
    let kanban = Kanban::new(&KanbanConfig {
        binary: "/nonexistent/hermes".into(),
        ..KanbanConfig::default()
    });

    let report = poll_once(
        &gitea,
        &kanban,
        &RepoRef::new("terraphim", "gitea"),
        &server.uri(),
        true,
        25,
        None,
    )
    .await
    .expect("the sweep itself survives");
    assert!(report.resolved.is_empty());
    assert_eq!(report.failed.len(), 1);
    assert_eq!(report.failed[0].0, 57);
}

/// The sweep must cost `max_tasks_per_sweep` subprocesses, not "however many issues are open".
///
/// `/api/v1/robot/ready` has no limit and no paging (`ready_graph.go:192-276`), so this is the
/// only thing standing between a busy board and one `hermes kanban create` per open issue
/// every cycle — plus a permanent per-task cost in the approval and reconcile sweeps behind it.
#[tokio::test]
async fn a_sweep_creates_at_most_max_tasks_and_takes_the_highest_page_rank_first() {
    let issues: Vec<String> = [(11, 0.10), (12, 0.90), (13, 0.30), (14, 0.70), (15, 0.50)]
        .iter()
        .map(|(index, rank)| {
            format!(
                r#"{{"id": {}, "index": {index}, "title": "issue {index}", "page_rank": {rank},
                     "priority": 1, "is_blocked": false, "blocker_count": 0}}"#,
                900 + index
            )
        })
        .collect();
    let body = format!(
        r#"{{"repo_id":7,"repo_name":"gitea","total_count":5,"ready_issues":[{}]}}"#,
        issues.join(",")
    );

    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/v1/robot/ready"))
        .respond_with(ResponseTemplate::new(200).set_body_string(body))
        .mount(&server)
        .await;

    let dir = tempfile::tempdir().expect("tempdir");
    let stub = write_stub_hermes(dir.path());
    let gitea = GiteaClient::new(&GiteaConfig {
        base_url: server.uri(),
        request_timeout_secs: 5,
        max_retries: 0,
        retry_backoff_ms: 1,
        ..GiteaConfig::default()
    })
    .expect("client");
    let kanban = Kanban::new(&KanbanConfig {
        binary: stub.to_string_lossy().into_owned(),
        ..KanbanConfig::default()
    });
    let repo = RepoRef::new("terraphim", "gitea");

    let first = poll_once(&gitea, &kanban, &repo, &server.uri(), true, 2, None)
        .await
        .expect("sweep");
    assert_eq!(first.resolved.len(), 2, "the cap bounds what one sweep creates");
    assert_eq!(first.deferred, 3, "and what it deferred is reported, not dropped");
    let mut picked: Vec<i64> = first.resolved.iter().map(|(i, _)| *i).collect();
    picked.sort_unstable();
    assert_eq!(picked, vec![12, 14], "PageRank descending, best first");

    let calls = std::fs::read_to_string(dir.path().join("calls.log")).expect("calls.log");
    assert_eq!(
        calls.lines().filter(|l| l.contains(" create ")).count(),
        2,
        "one subprocess per admitted issue, and no more"
    );

    // The selection is deterministic, so a second sweep over an unchanged board re-picks the
    // same two and kanban dedups them: the cap bounds the *board*, not merely one sweep.
    let second = poll_once(&gitea, &kanban, &repo, &server.uri(), true, 2, None)
        .await
        .expect("second sweep");
    assert_eq!(first.resolved, second.resolved);
    let keys = std::fs::read_dir(dir.path().join("keys"))
        .expect("keys dir")
        .count();
    assert_eq!(keys, 2, "two sweeps, still two tasks");
}

#[tokio::test]
async fn a_blocked_ready_issue_is_skipped() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/v1/robot/ready"))
        .respond_with(ResponseTemplate::new(200).set_body_string(
            r#"{"repo_id":7,"repo_name":"gitea","total_count":1,"ready_issues":[
                {"id":91,"index":57,"title":"t","page_rank":0.1,"priority":1,
                 "is_blocked":true,"blocker_count":2}]}"#,
        ))
        .mount(&server)
        .await;

    let dir = tempfile::tempdir().expect("tempdir");
    let stub = write_stub_hermes(dir.path());
    let gitea = GiteaClient::new(&GiteaConfig {
        base_url: server.uri(),
        request_timeout_secs: 5,
        max_retries: 0,
        retry_backoff_ms: 1,
        ..GiteaConfig::default()
    })
    .expect("client");
    let kanban = Kanban::new(&KanbanConfig {
        binary: stub.to_string_lossy().into_owned(),
        ..KanbanConfig::default()
    });

    let report = poll_once(
        &gitea,
        &kanban,
        &RepoRef::new("terraphim", "gitea"),
        &server.uri(),
        true,
        25,
        None,
    )
    .await
    .expect("sweep");
    assert!(
        report.resolved.is_empty(),
        "a blocked issue must not become a runnable task"
    );
    assert!(
        !dir.path().join("calls.log").exists(),
        "kanban must not be called at all"
    );
}
