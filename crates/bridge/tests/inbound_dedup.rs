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

    let first = poll_once(&gitea, &kanban, &repo, &server.uri(), true)
        .await
        .expect("first sweep");
    let second = poll_once(&gitea, &kanban, &repo, &server.uri(), true)
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
        // Without this the approval gate is unreachable: kanban creates `ready`, which is
        // immediately claimable, so the issue would reach an agent before any human saw it
        // and the approval sweep — which covers `blocked` and `todo` — would never see a
        // bridge-created task at all.
        assert!(call.contains("--initial-status blocked"), "{call}");
    }
    // Exactly one task materialised, no matter how many times we asked.
    let keys = std::fs::read_dir(dir.path().join("keys"))
        .expect("keys dir")
        .count();
    assert_eq!(keys, 1);
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
    )
    .await
    .expect("the sweep itself survives");
    assert!(report.resolved.is_empty());
    assert_eq!(report.failed.len(), 1);
    assert_eq!(report.failed[0].0, 57);
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
