// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//! Gitea read client.
//!
//! Reads only. Every write goes through `gitea-robot` so that it carries the NIP-98 agent
//! identity from F1 — see [`crate::robot`].

use std::time::Duration;

use serde::{Deserialize, Serialize};

use crate::config::GiteaConfig;

/// One entry of `ready_issues[]`.
///
/// Verified against `routers/api/v1/robot/ready_graph.go:25-42`: there is **no owner, body
/// or labels** here. `--extended` in `gitea-robot` enriches client-side only, so anything
/// beyond these fields has to be fetched or configured, never assumed.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ReadyIssue {
    /// Internal issue id.
    pub id: i64,
    /// Per-repository issue index — the `#N` a human types.
    pub index: i64,
    /// Issue title.
    pub title: String,
    /// PageRank score from the dependency graph.
    #[serde(default)]
    pub page_rank: f64,
    /// Priority tier.
    #[serde(default)]
    pub priority: i32,
    /// Whether the issue has open blockers.
    #[serde(default)]
    pub is_blocked: bool,
    /// How many blockers it has.
    #[serde(default)]
    pub blocker_count: i32,
}

/// Response body of `GET /api/v1/robot/ready` (`ready_graph.go:36-42`).
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ReadyResponse {
    /// Repository id.
    #[serde(default)]
    pub repo_id: i64,
    /// Repository name — *not* `owner/name`.
    #[serde(default)]
    pub repo_name: String,
    /// Total ready count.
    #[serde(default)]
    pub total_count: i32,
    /// The ready issues themselves. May be empty.
    #[serde(default)]
    pub ready_issues: Vec<ReadyIssue>,
}

/// A user as returned inside a reaction.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ReactionUser {
    /// Login name.
    #[serde(default)]
    pub login: String,
    /// Display name; some deployments only populate this.
    #[serde(default)]
    pub username: Option<String>,
}

impl ReactionUser {
    /// The name to use for a permission lookup.
    pub fn name(&self) -> &str {
        if !self.login.is_empty() {
            &self.login
        } else {
            self.username.as_deref().unwrap_or_default()
        }
    }
}

/// One reaction on an issue (`modules/structs/issue_reaction.go:17-25`).
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Reaction {
    /// Reactor.
    #[serde(default)]
    pub user: Option<ReactionUser>,
    /// The reaction content. This is the emoji **alias** (e.g. `honeybee`), not the
    /// codepoint.
    #[serde(rename = "content", default)]
    pub content: String,
    /// When it was added.
    #[serde(default)]
    pub created_at: Option<String>,
}

/// One repository label (`modules/structs/issue_label.go:13-22`).
///
/// Only the name is read. It is what `POST /issues/{index}/labels` resolves against —
/// `GetLabelIDsInRepoByNames` — and a name it cannot resolve is dropped in silence there,
/// which is why the bridge probes for it up front instead.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Label {
    /// Label name, as a human typed it.
    #[serde(default)]
    pub name: String,
}

/// The user a token authenticates as (`GET /api/v1/user`, `modules/structs/user.go`).
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct AuthenticatedUser {
    /// Login name (`json:"login"`).
    #[serde(rename = "login", default)]
    pub login: String,
    /// Whether this is a site administrator (`json:"is_admin"`).
    #[serde(default)]
    pub is_admin: bool,
}

/// Response of `GET /repos/{o}/{r}/collaborators/{u}/permission`
/// (`modules/structs/repo_collaborator.go:14-21`).
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct CollaboratorPermission {
    /// `none | read | write | admin | owner`.
    #[serde(default)]
    pub permission: String,
    /// Human-readable role.
    #[serde(default)]
    pub role_name: String,
}

impl CollaboratorPermission {
    /// Whether this permission level is enough to approve work.
    pub fn is_writer(&self) -> bool {
        permission_is_writer(&self.permission)
    }

    /// Whether this permission level makes the holder a *repo admin*.
    ///
    /// This is the level `GetRepoPermissions` demands before it will answer for anybody
    /// but the caller (`routers/api/v1/repo/collaborators.go:279`), so it is what decides
    /// whether the approval leg can work at all.
    pub fn is_repo_admin(&self) -> bool {
        matches!(
            self.permission.trim().to_ascii_lowercase().as_str(),
            "admin" | "owner"
        )
    }
}

/// Whether a `permission` string from the collaborator API grants write access.
pub fn permission_is_writer(permission: &str) -> bool {
    matches!(
        permission.trim().to_ascii_lowercase().as_str(),
        "write" | "admin" | "owner"
    )
}

/// Failures of the read client.
#[derive(Debug, thiserror::Error)]
pub enum GiteaError {
    /// A 404 response. On the ready endpoint this is also what `[issue_graph] ENABLED =
    /// false` looks like — `Ready` returns `APIErrorNotFound` when the feature is off
    /// (`ready_graph.go:47-51`), deliberately indistinguishable from a missing repo.
    #[error("gitea returned 404 for {path} (repo missing, or the feature flag is off)")]
    NotFound {
        /// Requested path.
        path: String,
    },
    /// 401/403.
    #[error("gitea denied {path} with {status} — a token with read access is required")]
    Unauthorized {
        /// Requested path.
        path: String,
        /// HTTP status.
        status: u16,
    },
    /// Any other non-success status, after retries.
    #[error("gitea returned {status} for {path}: {body}")]
    Status {
        /// Requested path.
        path: String,
        /// HTTP status.
        status: u16,
        /// Truncated response body.
        body: String,
    },
    /// Transport failure, after retries.
    #[error("request to {path} failed: {source}")]
    Transport {
        /// Requested path.
        path: String,
        /// Underlying error.
        #[source]
        source: reqwest::Error,
    },
    /// A paged listing did not end within the page cap.
    ///
    /// Returned rather than silently handing back a prefix: on the reactions endpoint a
    /// truncated list is indistinguishable from "nobody approved", which is the exact
    /// silent failure the approval leg exists to avoid.
    #[error("{path} did not end within {pages} pages; refusing to answer from a partial list")]
    Truncated {
        /// Requested path, without the paging parameters.
        path: String,
        /// How many pages were fetched before giving up.
        pages: u32,
    },
    /// The response was not the shape we verified against the Go source.
    #[error("cannot decode response from {path}: {source}")]
    Decode {
        /// Requested path.
        path: String,
        /// Underlying error.
        #[source]
        source: serde_json::Error,
    },
    /// The client could not be constructed.
    #[error("cannot build http client: {0}")]
    Build(#[source] reqwest::Error),
}

/// Page size requested when listing reactions. The server clamps it to
/// `setting.API.MaxResponseItems` (default 50, operator-tunable), so a short page says
/// nothing about whether more pages follow.
const REACTION_PAGE_SIZE: u32 = 50;

/// Page cap for the reaction walk. Reaching it is an error, not a short answer: a
/// truncated reaction list would read as "nobody approved".
const REACTION_PAGE_LIMIT: u32 = 100;

/// Page cap for the label walk. Same reasoning as [`REACTION_PAGE_LIMIT`]: a truncated
/// label list would read as "the label does not exist", which is the opposite of what a
/// preflight is for.
const LABEL_PAGE_LIMIT: u32 = 20;

/// Read-side Gitea client with bounded retry on 5xx and transport errors.
#[derive(Debug, Clone)]
pub struct GiteaClient {
    http: reqwest::Client,
    base_url: String,
    token: Option<String>,
    max_retries: u32,
    retry_backoff: Duration,
}

impl GiteaClient {
    /// Builds a client from config.
    pub fn new(cfg: &GiteaConfig) -> Result<Self, GiteaError> {
        let http = reqwest::Client::builder()
            .timeout(Duration::from_secs(cfg.request_timeout_secs.max(1)))
            .user_agent(concat!("gitea-automations/", env!("CARGO_PKG_VERSION")))
            .build()
            .map_err(GiteaError::Build)?;
        Ok(Self {
            http,
            base_url: cfg.base_url.trim_end_matches('/').to_string(),
            token: cfg.token.clone(),
            max_retries: cfg.max_retries,
            retry_backoff: Duration::from_millis(cfg.retry_backoff_ms.max(1)),
        })
    }

    /// Whether a token is configured. Without one the collaborator permission endpoint —
    /// which sits behind `reqToken()` — is unreachable and approval fails closed.
    pub fn has_token(&self) -> bool {
        self.token.as_ref().is_some_and(|t| !t.trim().is_empty())
    }

    /// `GET /api/v1/robot/ready?owner=&repo=&skip_in_progress=`.
    pub async fn ready(
        &self,
        owner: &str,
        repo: &str,
        skip_in_progress: bool,
    ) -> Result<ReadyResponse, GiteaError> {
        let path = format!(
            "/api/v1/robot/ready?owner={}&repo={}&skip_in_progress={}",
            urlencode(owner),
            urlencode(repo),
            if skip_in_progress { "true" } else { "false" }
        );
        self.get_json(&path).await
    }

    /// `GET /api/v1/repos/{owner}/{repo}/issues/{index}/reactions`
    /// (`routers/api/v1/api.go:1798`), every page of it.
    ///
    /// Paging is not optional here. The handler applies `utils.GetListOptions(ctx)`
    /// (`routers/api/v1/repo/issue_reaction.go:313`), so an unpaged request returns at
    /// most `setting.API.DefaultPagingNum` — 30 by default (`modules/setting/api.go:26`).
    /// Reading one page would make a maintainer's 🐝 on a busy issue resolve to
    /// `NotRequested`, indistinguishable from nobody having approved.
    ///
    /// Only an empty page ends the walk. A page shorter than requested is what an
    /// instance with a lower `MAX_RESPONSE_ITEMS` returns for *every* page, so treating a
    /// short page as the last one would stop after the first.
    pub async fn issue_reactions(
        &self,
        owner: &str,
        repo: &str,
        index: i64,
    ) -> Result<Vec<Reaction>, GiteaError> {
        let path = format!(
            "/api/v1/repos/{}/{}/issues/{index}/reactions",
            urlencode(owner),
            urlencode(repo)
        );
        let mut all: Vec<Reaction> = Vec::new();
        for page in 1..=REACTION_PAGE_LIMIT {
            let paged = format!("{path}?page={page}&limit={REACTION_PAGE_SIZE}");
            let batch: Vec<Reaction> = self.get_json(&paged).await?;
            if batch.is_empty() {
                return Ok(all);
            }
            all.extend(batch);
        }
        Err(GiteaError::Truncated {
            path,
            pages: REACTION_PAGE_LIMIT,
        })
    }

    /// `GET /api/v1/repos/{owner}/{repo}/labels` (`routers/api/v1/api.go`), every page of it.
    ///
    /// Paged for the same reason reactions are: the handler applies `utils.GetListOptions`,
    /// so an unpaged request answers with at most `setting.API.DefaultPagingNum` (30) and a
    /// repository with more labels than that would report the bridge's label as missing.
    pub async fn repo_labels(&self, owner: &str, repo: &str) -> Result<Vec<Label>, GiteaError> {
        let path = format!("/api/v1/repos/{}/{}/labels", urlencode(owner), urlencode(repo));
        let mut all: Vec<Label> = Vec::new();
        for page in 1..=LABEL_PAGE_LIMIT {
            let paged = format!("{path}?page={page}&limit={REACTION_PAGE_SIZE}");
            let batch: Vec<Label> = self.get_json(&paged).await?;
            if batch.is_empty() {
                return Ok(all);
            }
            all.extend(batch);
        }
        Err(GiteaError::Truncated {
            path,
            pages: LABEL_PAGE_LIMIT,
        })
    }

    /// `GET /api/v1/user` — the user this token authenticates as. Requires a token.
    pub async fn current_user(&self) -> Result<AuthenticatedUser, GiteaError> {
        self.get_json("/api/v1/user").await
    }

    /// `GET /api/v1/repos/{owner}/{repo}/collaborators/{user}/permission`
    /// (`routers/api/v1/api.go:1464`). Requires a token.
    pub async fn user_permission(
        &self,
        owner: &str,
        repo: &str,
        user: &str,
    ) -> Result<CollaboratorPermission, GiteaError> {
        let path = format!(
            "/api/v1/repos/{}/{}/collaborators/{}/permission",
            urlencode(owner),
            urlencode(repo),
            urlencode(user)
        );
        self.get_json(&path).await
    }

    async fn get_json<T: serde::de::DeserializeOwned>(&self, path: &str) -> Result<T, GiteaError> {
        let body = self.get_text(path).await?;
        serde_json::from_str(&body).map_err(|source| GiteaError::Decode {
            path: path.to_string(),
            source,
        })
    }

    async fn get_text(&self, path: &str) -> Result<String, GiteaError> {
        let url = format!("{}{path}", self.base_url);
        let mut backoff = self.retry_backoff;
        // `max_retries` retries means `max_retries + 1` attempts.
        for attempt in 0..=self.max_retries {
            let last = attempt == self.max_retries;
            let mut req = self.http.get(&url);
            if let Some(token) = self.token.as_deref().filter(|t| !t.trim().is_empty()) {
                req = req.header(reqwest::header::AUTHORIZATION, format!("token {token}"));
            }
            match req.send().await {
                Ok(resp) => {
                    let status = resp.status();
                    if status.is_success() {
                        return resp.text().await.map_err(|source| GiteaError::Transport {
                            path: path.to_string(),
                            source,
                        });
                    }
                    if status.as_u16() == 404 {
                        return Err(GiteaError::NotFound {
                            path: path.to_string(),
                        });
                    }
                    if matches!(status.as_u16(), 401 | 403) {
                        return Err(GiteaError::Unauthorized {
                            path: path.to_string(),
                            status: status.as_u16(),
                        });
                    }
                    let retryable = status.is_server_error() || status.as_u16() == 429;
                    if !retryable || last {
                        let body = resp.text().await.unwrap_or_default();
                        return Err(GiteaError::Status {
                            path: path.to_string(),
                            status: status.as_u16(),
                            body: truncate(&body, 512),
                        });
                    }
                    tracing::warn!(
                        path,
                        status = status.as_u16(),
                        attempt,
                        "gitea read failed, retrying"
                    );
                }
                Err(source) => {
                    if last {
                        return Err(GiteaError::Transport {
                            path: path.to_string(),
                            source,
                        });
                    }
                    tracing::warn!(path, attempt, error = %source, "gitea read failed, retrying");
                }
            }
            tokio::time::sleep(backoff).await;
            backoff = backoff.saturating_mul(2);
        }
        unreachable!("the loop returns on the last attempt")
    }
}

/// Minimal percent-encoding for a single path or query segment.
///
/// Owner and repo names are already constrained by Gitea, but the bridge takes them from
/// config and from reaction payloads, so they are encoded rather than trusted.
fn urlencode(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for b in s.bytes() {
        match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => out.push(b as char),
            _ => out.push_str(&format!("%{b:02X}")),
        }
    }
    out
}

fn truncate(s: &str, max: usize) -> String {
    if s.len() <= max {
        return s.to_string();
    }
    let mut end = max;
    while end > 0 && !s.is_char_boundary(end) {
        end -= 1;
    }
    format!("{}…", &s[..end])
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ready_response_decodes_the_verified_shape() {
        // Exactly the fields ready_graph.go:25-42 emits — no owner, body or labels.
        let json = r#"{
            "repo_id": 7,
            "repo_name": "gitea",
            "total_count": 1,
            "ready_issues": [
                {"id": 91, "index": 57, "title": "automations daemon",
                 "page_rank": 0.42, "priority": 2, "is_blocked": false, "blocker_count": 0}
            ]
        }"#;
        let parsed: ReadyResponse = serde_json::from_str(json).expect("decodes");
        assert_eq!(parsed.repo_name, "gitea");
        assert_eq!(parsed.ready_issues[0].index, 57);
        assert_eq!(parsed.ready_issues[0].title, "automations daemon");
    }

    #[test]
    fn empty_ready_issues_decodes() {
        let parsed: ReadyResponse =
            serde_json::from_str(r#"{"repo_id":7,"repo_name":"gitea","total_count":0,"ready_issues":[]}"#)
                .expect("decodes");
        assert!(parsed.ready_issues.is_empty());
    }

    #[test]
    fn reaction_content_is_the_alias() {
        let parsed: Vec<Reaction> = serde_json::from_str(
            r#"[{"user":{"login":"alex"},"content":"honeybee","created_at":"2026-08-04T00:00:00Z"}]"#,
        )
        .expect("decodes");
        assert_eq!(parsed[0].content, "honeybee");
        assert_eq!(parsed[0].user.as_ref().expect("user").name(), "alex");
    }

    #[test]
    fn writer_permissions_are_write_admin_owner() {
        for p in ["write", "admin", "owner", "Write", " ADMIN "] {
            assert!(permission_is_writer(p), "{p} should be a writer");
        }
        for p in ["read", "none", "", "reader"] {
            assert!(!permission_is_writer(p), "{p} should not be a writer");
        }
    }

    #[test]
    fn urlencode_escapes_separators() {
        assert_eq!(urlencode("terraphim"), "terraphim");
        assert_eq!(urlencode("a/b"), "a%2Fb");
        assert_eq!(urlencode("a b"), "a%20b");
    }

    #[test]
    fn truncate_respects_char_boundaries() {
        let s = "🐝🐝🐝";
        let out = truncate(s, 5);
        assert!(out.ends_with('…'));
        assert!(out.len() <= 5 + '…'.len_utf8());
    }
}
