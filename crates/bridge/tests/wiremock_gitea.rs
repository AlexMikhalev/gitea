// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//! Wiremock coverage of the Gitea read leg.
//!
//! The response bodies here are the shapes verified against the Go source, not invented
//! ones: `ReadyResponse`/`ReadyIssue` from `routers/api/v1/robot/ready_graph.go:25-42`,
//! `Reaction` from `modules/structs/issue_reaction.go:17-25` and
//! `RepoCollaboratorPermission` from `modules/structs/repo_collaborator.go:14-21`.

use bridge::approval::{ApprovalOutcome, evaluate, preflight};
use bridge::config::{ApprovalConfig, GiteaConfig, RepoRef};
use bridge::gitea::{GiteaClient, GiteaError};
use wiremock::matchers::{header, method, path, query_param};
use wiremock::{Mock, MockServer, ResponseTemplate};

fn config(server: &MockServer) -> GiteaConfig {
    GiteaConfig {
        base_url: server.uri(),
        token: None,
        request_timeout_secs: 5,
        max_retries: 3,
        retry_backoff_ms: 1,
    }
}

fn with_token(server: &MockServer) -> GiteaConfig {
    GiteaConfig {
        token: Some("s3cr3t".into()),
        ..config(server)
    }
}

const READY_BODY: &str = r#"{
  "repo_id": 7,
  "repo_name": "gitea",
  "total_count": 2,
  "ready_issues": [
    {"id": 91, "index": 57, "title": "automations daemon", "page_rank": 0.4237,
     "priority": 2, "is_blocked": false, "blocker_count": 0},
    {"id": 92, "index": 58, "title": "second", "page_rank": 0.11,
     "priority": 1, "is_blocked": false, "blocker_count": 0}
  ]
}"#;

#[tokio::test]
async fn ready_returns_the_verified_shape() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/v1/robot/ready"))
        .and(query_param("owner", "terraphim"))
        .and(query_param("repo", "gitea"))
        .and(query_param("skip_in_progress", "true"))
        .respond_with(ResponseTemplate::new(200).set_body_string(READY_BODY))
        .expect(1)
        .mount(&server)
        .await;

    let client = GiteaClient::new(&config(&server)).expect("client");
    let ready = client.ready("terraphim", "gitea", true).await.expect("ready");
    assert_eq!(ready.repo_name, "gitea");
    assert_eq!(ready.total_count, 2);
    assert_eq!(ready.ready_issues.len(), 2);
    assert_eq!(ready.ready_issues[0].index, 57);
    assert_eq!(ready.ready_issues[0].title, "automations daemon");
    assert!(!ready.ready_issues[0].is_blocked);
}

#[tokio::test]
async fn ready_handles_an_empty_board() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/v1/robot/ready"))
        .respond_with(
            ResponseTemplate::new(200)
                .set_body_string(r#"{"repo_id":7,"repo_name":"gitea","total_count":0,"ready_issues":[]}"#),
        )
        .mount(&server)
        .await;

    let client = GiteaClient::new(&config(&server)).expect("client");
    let ready = client.ready("terraphim", "gitea", true).await.expect("ready");
    assert!(ready.ready_issues.is_empty());
}

#[tokio::test]
async fn ready_404_is_distinguishable_and_not_retried() {
    // `[issue_graph] ENABLED = false` makes Ready return APIErrorNotFound
    // (`ready_graph.go:47-51`) — the same 404 a missing repo gives. It must surface as
    // NotFound, and must not be retried: retrying a disabled feature flag is a busy loop.
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/v1/robot/ready"))
        .respond_with(ResponseTemplate::new(404).set_body_string(r#"{"message":"Not Found"}"#))
        .expect(1)
        .mount(&server)
        .await;

    let client = GiteaClient::new(&config(&server)).expect("client");
    let err = client.ready("terraphim", "gitea", true).await.expect_err("404");
    assert!(matches!(err, GiteaError::NotFound { .. }), "{err:?}");
}

#[tokio::test]
async fn a_5xx_is_retried_and_then_succeeds() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/v1/robot/ready"))
        .respond_with(ResponseTemplate::new(503))
        .up_to_n_times(2)
        .expect(2)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/api/v1/robot/ready"))
        .respond_with(ResponseTemplate::new(200).set_body_string(READY_BODY))
        .expect(1)
        .mount(&server)
        .await;

    let client = GiteaClient::new(&config(&server)).expect("client");
    let ready = client
        .ready("terraphim", "gitea", true)
        .await
        .expect("succeeds after retry");
    assert_eq!(ready.ready_issues.len(), 2);
}

#[tokio::test]
async fn a_persistent_5xx_gives_up_after_the_configured_retries() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/v1/robot/ready"))
        .respond_with(ResponseTemplate::new(500).set_body_string("boom"))
        // max_retries = 2 means three attempts in total.
        .expect(3)
        .mount(&server)
        .await;

    let cfg = GiteaConfig {
        max_retries: 2,
        ..config(&server)
    };
    let client = GiteaClient::new(&cfg).expect("client");
    let err = client
        .ready("terraphim", "gitea", true)
        .await
        .expect_err("gives up");
    assert!(matches!(err, GiteaError::Status { status: 500, .. }), "{err:?}");
}

#[tokio::test]
async fn a_configured_token_is_sent() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/v1/robot/ready"))
        .and(header("authorization", "token s3cr3t"))
        .respond_with(ResponseTemplate::new(200).set_body_string(READY_BODY))
        .expect(1)
        .mount(&server)
        .await;

    let client = GiteaClient::new(&with_token(&server)).expect("client");
    client.ready("terraphim", "gitea", true).await.expect("ready");
}

/// Mounts a paged reactions response: `body` as page 1, an empty page 2.
///
/// The bridge pages the reactions endpoint because the handler applies
/// `utils.GetListOptions(ctx)` (`routers/api/v1/repo/issue_reaction.go:313`) and defaults
/// to 30 items (`modules/setting/api.go:26`). Only an empty page ends the walk, so every
/// test that answers reactions must answer a second page too.
async fn mount_reactions(server: &MockServer, body: &str) {
    Mock::given(method("GET"))
        .and(path("/api/v1/repos/terraphim/gitea/issues/57/reactions"))
        .and(query_param("page", "1"))
        .respond_with(ResponseTemplate::new(200).set_body_string(body.to_string()))
        .mount(server)
        .await;
    Mock::given(method("GET"))
        .and(path("/api/v1/repos/terraphim/gitea/issues/57/reactions"))
        .and(query_param("page", "2"))
        .respond_with(ResponseTemplate::new(200).set_body_string("[]"))
        .mount(server)
        .await;
}

/// Mounts a reactions response plus a permission response for one user.
async fn mount_approval(server: &MockServer, reactor: &str, content: &str, permission: &str) {
    mount_reactions(
        server,
        &format!(
            r#"[{{"user":{{"login":"{reactor}"}},"content":"{content}","created_at":"2026-08-04T10:00:00Z"}}]"#
        ),
    )
    .await;
    Mock::given(method("GET"))
        .and(path(format!(
            "/api/v1/repos/terraphim/gitea/collaborators/{reactor}/permission"
        )))
        .respond_with(ResponseTemplate::new(200).set_body_string(format!(
            r#"{{"permission":"{permission}","role_name":"{permission}","user":{{"login":"{reactor}"}}}}"#
        )))
        .mount(server)
        .await;
}

#[tokio::test]
async fn a_bee_from_a_writer_approves() {
    let server = MockServer::start().await;
    mount_approval(&server, "alex", "honeybee", "write").await;
    let client = GiteaClient::new(&with_token(&server)).expect("client");
    let outcome = evaluate(&client, &ApprovalConfig::default(), "terraphim", "gitea", 57)
        .await
        .expect("evaluates");
    assert_eq!(outcome.approver(), Some("alex"), "{outcome:?}");
}

#[tokio::test]
async fn a_bee_from_a_non_writer_is_ignored() {
    let server = MockServer::start().await;
    mount_approval(&server, "mallory", "honeybee", "read").await;
    let client = GiteaClient::new(&with_token(&server)).expect("client");
    let outcome = evaluate(&client, &ApprovalConfig::default(), "terraphim", "gitea", 57)
        .await
        .expect("evaluates");
    assert_eq!(
        outcome,
        ApprovalOutcome::NotAuthorized {
            reactors: vec!["mallory".into()]
        }
    );
    assert!(!outcome.is_approved());
}

/// A stranger — an existing user who is not a collaborator — is answered with **200 and
/// `permission: "none"`**, not 404: `GetRepoPermissions` calls `GetUserRepoPermission`
/// unconditionally (`routers/api/v1/repo/collaborators.go:294-301`).
#[tokio::test]
async fn a_non_collaborator_reaction_is_ignored() {
    let server = MockServer::start().await;
    mount_approval(&server, "stranger", "honeybee", "none").await;

    let client = GiteaClient::new(&with_token(&server)).expect("client");
    let outcome = evaluate(&client, &ApprovalConfig::default(), "terraphim", "gitea", 57)
        .await
        .expect("evaluates");
    assert_eq!(
        outcome,
        ApprovalOutcome::NotAuthorized {
            reactors: vec!["stranger".into()]
        }
    );
}

/// 404 is the *other* case: the username itself does not resolve
/// (`collaborators.go:286-288`), e.g. a renamed or deleted account. It is still ignored,
/// but for a different reason than a non-collaborator.
#[tokio::test]
async fn a_reaction_from_an_unresolvable_username_is_ignored() {
    let server = MockServer::start().await;
    mount_reactions(
        &server,
        r#"[{"user":{"login":"ghost"},"content":"honeybee","created_at":null}]"#,
    )
    .await;
    Mock::given(method("GET"))
        .and(path(
            "/api/v1/repos/terraphim/gitea/collaborators/ghost/permission",
        ))
        .respond_with(ResponseTemplate::new(404).set_body_string(r#"{"message":"Not Found"}"#))
        .mount(&server)
        .await;

    let client = GiteaClient::new(&with_token(&server)).expect("client");
    let outcome = evaluate(&client, &ApprovalConfig::default(), "terraphim", "gitea", 57)
        .await
        .expect("evaluates");
    assert_eq!(
        outcome,
        ApprovalOutcome::NotAuthorized {
            reactors: vec!["ghost".into()]
        }
    );
}

/// The dominant real-world response for a token that is neither site admin nor repo
/// admin: querying *another* user's permission is 403 (`collaborators.go:279`). It must
/// fail closed as Undetermined — never be mistaken for "not a writer", which would look
/// like a decision rather than an inability to decide.
#[tokio::test]
async fn a_403_from_a_non_admin_token_is_undetermined() {
    let server = MockServer::start().await;
    mount_reactions(
        &server,
        r#"[{"user":{"login":"alex"},"content":"honeybee","created_at":null}]"#,
    )
    .await;
    Mock::given(method("GET"))
        .and(path("/api/v1/repos/terraphim/gitea/collaborators/alex/permission"))
        .respond_with(ResponseTemplate::new(403).set_body_string(
            r#"{"message":"Only admins can query all permissions, repo admins can query all repo permissions, collaborators can query only their own"}"#,
        ))
        .mount(&server)
        .await;

    let client = GiteaClient::new(&with_token(&server)).expect("client");
    let outcome = evaluate(&client, &ApprovalConfig::default(), "terraphim", "gitea", 57)
        .await
        .expect("evaluates");
    match outcome {
        ApprovalOutcome::Undetermined { reactors, .. } => assert_eq!(reactors, vec!["alex".to_string()]),
        other => panic!("expected Undetermined, got {other:?}"),
    }
}

#[tokio::test]
async fn no_bee_means_no_request() {
    let server = MockServer::start().await;
    mount_reactions(
        &server,
        r#"[{"user":{"login":"alex"},"content":"+1","created_at":null}]"#,
    )
    .await;
    let client = GiteaClient::new(&with_token(&server)).expect("client");
    let outcome = evaluate(&client, &ApprovalConfig::default(), "terraphim", "gitea", 57)
        .await
        .expect("evaluates");
    assert_eq!(outcome, ApprovalOutcome::NotRequested);
}

#[tokio::test]
async fn a_raw_codepoint_reaction_never_matches() {
    // If 🐝 is absent from `[ui] REACTIONS` the API cannot return it at all
    // (`models/issues/reaction.go:165,223`). Matching on the codepoint would look like it
    // handles the case while being dead code — so it must not match.
    let server = MockServer::start().await;
    mount_reactions(
        &server,
        "[{\"user\":{\"login\":\"alex\"},\"content\":\"\u{1f41d}\",\"created_at\":null}]",
    )
    .await;
    let client = GiteaClient::new(&with_token(&server)).expect("client");
    let outcome = evaluate(&client, &ApprovalConfig::default(), "terraphim", "gitea", 57)
        .await
        .expect("evaluates");
    assert_eq!(outcome, ApprovalOutcome::NotRequested);
}

#[tokio::test]
async fn approval_fails_closed_without_a_token() {
    // The permission endpoint sits behind reqToken() (`routers/api/v1/api.go:1466`).
    let server = MockServer::start().await;
    mount_reactions(
        &server,
        r#"[{"user":{"login":"alex"},"content":"honeybee","created_at":null}]"#,
    )
    .await;
    let client = GiteaClient::new(&config(&server)).expect("client");
    let outcome = evaluate(&client, &ApprovalConfig::default(), "terraphim", "gitea", 57)
        .await
        .expect("evaluates");
    match outcome {
        ApprovalOutcome::Undetermined { reactors, .. } => assert_eq!(reactors, vec!["alex".to_string()]),
        other => panic!("expected Undetermined, got {other:?}"),
    }
}

#[tokio::test]
async fn a_permission_lookup_failure_is_undetermined_not_approved() {
    let server = MockServer::start().await;
    mount_reactions(
        &server,
        r#"[{"user":{"login":"alex"},"content":"honeybee","created_at":null}]"#,
    )
    .await;
    Mock::given(method("GET"))
        .and(path(
            "/api/v1/repos/terraphim/gitea/collaborators/alex/permission",
        ))
        .respond_with(ResponseTemplate::new(500).set_body_string("boom"))
        .mount(&server)
        .await;

    let cfg = GiteaConfig {
        max_retries: 0,
        ..with_token(&server)
    };
    let client = GiteaClient::new(&cfg).expect("client");
    let outcome = evaluate(&client, &ApprovalConfig::default(), "terraphim", "gitea", 57)
        .await
        .expect("evaluates");
    assert!(
        matches!(outcome, ApprovalOutcome::Undetermined { .. }),
        "{outcome:?}"
    );
    assert!(!outcome.is_approved());
}

#[tokio::test]
async fn one_writer_among_several_reactors_is_enough() {
    let server = MockServer::start().await;
    mount_reactions(
        &server,
        r#"[{"user":{"login":"mallory"},"content":"honeybee","created_at":null},
                {"user":{"login":"alex"},"content":"honeybee","created_at":null}]"#,
    )
    .await;
    Mock::given(method("GET"))
        .and(path(
            "/api/v1/repos/terraphim/gitea/collaborators/mallory/permission",
        ))
        .respond_with(
            ResponseTemplate::new(200).set_body_string(r#"{"permission":"read","role_name":"read"}"#),
        )
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path(
            "/api/v1/repos/terraphim/gitea/collaborators/alex/permission",
        ))
        .respond_with(
            ResponseTemplate::new(200).set_body_string(r#"{"permission":"admin","role_name":"admin"}"#),
        )
        .mount(&server)
        .await;

    let client = GiteaClient::new(&with_token(&server)).expect("client");
    let outcome = evaluate(&client, &ApprovalConfig::default(), "terraphim", "gitea", 57)
        .await
        .expect("evaluates");
    assert_eq!(outcome.approver(), Some("alex"), "{outcome:?}");
}

/// A 🐝 past the first page must still be seen. Reading one page would resolve it to
/// `NotRequested` — indistinguishable from nobody having approved, which is exactly the
/// silent failure the approval leg exists to avoid.
#[tokio::test]
async fn an_approval_on_a_later_page_is_found() {
    let server = MockServer::start().await;
    let filler: Vec<String> = (0..50)
        .map(|i| format!(r#"{{"user":{{"login":"noise{i}"}},"content":"+1","created_at":null}}"#))
        .collect();
    Mock::given(method("GET"))
        .and(path("/api/v1/repos/terraphim/gitea/issues/57/reactions"))
        .and(query_param("page", "1"))
        .and(query_param("limit", "50"))
        .respond_with(ResponseTemplate::new(200).set_body_string(format!("[{}]", filler.join(","))))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/api/v1/repos/terraphim/gitea/issues/57/reactions"))
        .and(query_param("page", "2"))
        .respond_with(
            ResponseTemplate::new(200)
                .set_body_string(r#"[{"user":{"login":"alex"},"content":"honeybee","created_at":null}]"#),
        )
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/api/v1/repos/terraphim/gitea/issues/57/reactions"))
        .and(query_param("page", "3"))
        .respond_with(ResponseTemplate::new(200).set_body_string("[]"))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path(
            "/api/v1/repos/terraphim/gitea/collaborators/alex/permission",
        ))
        .respond_with(ResponseTemplate::new(200).set_body_string(r#"{"permission":"write"}"#))
        .mount(&server)
        .await;

    let client = GiteaClient::new(&with_token(&server)).expect("client");
    let outcome = evaluate(&client, &ApprovalConfig::default(), "terraphim", "gitea", 57)
        .await
        .expect("evaluates");
    assert_eq!(outcome.approver(), Some("alex"), "{outcome:?}");
}

/// A short page is not the last page: an instance with a lower `MAX_RESPONSE_ITEMS`
/// returns one for *every* page, so stopping on a short page would stop after the first.
#[tokio::test]
async fn a_short_page_does_not_end_the_walk() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/v1/repos/terraphim/gitea/issues/57/reactions"))
        .and(query_param("page", "1"))
        .respond_with(
            ResponseTemplate::new(200)
                .set_body_string(r#"[{"user":{"login":"carol"},"content":"+1","created_at":null}]"#),
        )
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/api/v1/repos/terraphim/gitea/issues/57/reactions"))
        .and(query_param("page", "2"))
        .respond_with(
            ResponseTemplate::new(200)
                .set_body_string(r#"[{"user":{"login":"alex"},"content":"honeybee","created_at":null}]"#),
        )
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/api/v1/repos/terraphim/gitea/issues/57/reactions"))
        .and(query_param("page", "3"))
        .respond_with(ResponseTemplate::new(200).set_body_string("[]"))
        .mount(&server)
        .await;

    let client = GiteaClient::new(&with_token(&server)).expect("client");
    let reactions = client
        .issue_reactions("terraphim", "gitea", 57)
        .await
        .expect("pages");
    assert_eq!(reactions.len(), 2, "both pages, not just the first");
}

/// A list that never ends is an error, not a prefix: answering from a truncated reaction
/// list would silently report "nobody approved".
#[tokio::test]
async fn an_endless_reaction_list_is_refused_rather_than_truncated() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/api/v1/repos/terraphim/gitea/issues/57/reactions"))
        .respond_with(
            ResponseTemplate::new(200)
                .set_body_string(r#"[{"user":{"login":"a"},"content":"+1","created_at":null}]"#),
        )
        .mount(&server)
        .await;

    let client = GiteaClient::new(&with_token(&server)).expect("client");
    let err = client
        .issue_reactions("terraphim", "gitea", 57)
        .await
        .expect_err("refuses");
    assert!(matches!(err, GiteaError::Truncated { .. }), "{err:?}");
}

/// Mounts `GET /api/v1/user`.
async fn mount_whoami(server: &MockServer, login: &str, is_admin: bool) {
    Mock::given(method("GET"))
        .and(path("/api/v1/user"))
        .respond_with(
            ResponseTemplate::new(200)
                .set_body_string(format!(r#"{{"login":"{login}","is_admin":{is_admin}}}"#)),
        )
        .mount(server)
        .await;
}

fn repos() -> Vec<RepoRef> {
    vec![RepoRef::new("terraphim", "gitea")]
}

/// The check `check` was missing: a token that is neither site admin nor repo admin can
/// never query another user's permission (`routers/api/v1/repo/collaborators.go:279`), so
/// reporting it as configured would promise an approval leg that can only fail closed.
#[tokio::test]
async fn preflight_rejects_a_plain_write_token() {
    let server = MockServer::start().await;
    mount_whoami(&server, "bridge-bot", false).await;
    Mock::given(method("GET"))
        .and(path(
            "/api/v1/repos/terraphim/gitea/collaborators/bridge-bot/permission",
        ))
        .respond_with(ResponseTemplate::new(200).set_body_string(r#"{"permission":"write"}"#))
        .mount(&server)
        .await;

    let client = GiteaClient::new(&with_token(&server)).expect("client");
    let outcome = preflight(&client, &ApprovalConfig::default(), &repos()).await;
    assert!(!outcome.is_usable(), "{outcome:?}");
    assert!(outcome.reason().contains("write"), "{}", outcome.reason());
}

#[tokio::test]
async fn preflight_accepts_a_repo_admin() {
    let server = MockServer::start().await;
    mount_whoami(&server, "bridge-bot", false).await;
    Mock::given(method("GET"))
        .and(path(
            "/api/v1/repos/terraphim/gitea/collaborators/bridge-bot/permission",
        ))
        .respond_with(ResponseTemplate::new(200).set_body_string(r#"{"permission":"admin"}"#))
        .mount(&server)
        .await;

    let client = GiteaClient::new(&with_token(&server)).expect("client");
    assert!(
        preflight(&client, &ApprovalConfig::default(), &repos())
            .await
            .is_usable()
    );
}

#[tokio::test]
async fn preflight_accepts_a_site_admin_without_asking_per_repo() {
    let server = MockServer::start().await;
    mount_whoami(&server, "root", true).await;
    // No permission mock: a site admin passes `collaborators.go:279` outright, so asking
    // would be a request the daemon does not need to make.
    let client = GiteaClient::new(&with_token(&server)).expect("client");
    assert!(
        preflight(&client, &ApprovalConfig::default(), &repos())
            .await
            .is_usable()
    );
}

#[tokio::test]
async fn preflight_rejects_a_missing_token() {
    let server = MockServer::start().await;
    let client = GiteaClient::new(&config(&server)).expect("client");
    let outcome = preflight(&client, &ApprovalConfig::default(), &repos()).await;
    assert!(!outcome.is_usable());
    assert!(outcome.reason().contains("gitea.token"), "{}", outcome.reason());
}
