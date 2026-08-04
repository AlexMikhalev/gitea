// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

//! 🐝 = human approval.
//!
//! Polled from `GET /repos/{o}/{r}/issues/{index}/reactions`
//! (`routers/api/v1/api.go:1798`). Polling is the permanent path, not a stopgap:
//! `repoevent.Kind` is exactly `action, agent_audit, comment, review, status`
//! (`services/repoevent/event.go:29-37`), so a reaction is not observable through the F2
//! event stream at all, and F2 already landed (#55).
//!
//! Two things about 🐝 that are easy to get wrong, and both silently:
//!
//! 1. `content` is the emoji **alias** — `honeybee` — never the codepoint U+1F41D
//!    (`modules/emoji/emoji_data.go:191`).
//! 2. It requires an `app.ini` change. Writes are rejected unless the type is in
//!    `setting.UI.ReactionsLookup` (`models/issues/reaction.go:223`) and reads filter on
//!    the same allowlist (`:165`), so an unconfigured 🐝 is invisible, not merely
//!    unwritable. The default omits it (`app.example.ini:1361`); operators must set
//!    `[ui] REACTIONS = +1, -1, laugh, hooray, confused, heart, rocket, eyes, honeybee`.

use crate::config::{ApprovalConfig, RepoRef};
use crate::gitea::{GiteaClient, GiteaError, Reaction};
use crate::hermes::TaskDetail;

/// First line of the comment written on a kanban task once a 🐝 has been acted on.
///
/// A reaction is durable on the issue, and nothing on the Gitea side records that it was
/// already used. Without this marker a task approved once, which later blocks with
/// `needs_input` or `transient`, is unblocked again by the *same* 🐝 on the next sweep, runs,
/// blocks, and repeats forever — invisibly, because [`crate::outbound::BLOCK_MARKER`]
/// correctly suppresses the repeat Gitea comment. The marker lives in kanban for the same
/// reason the outbound markers do: it is the durable side, so a restart does not forget.
pub const CONSUMED_MARKER_PREFIX: &str = "gitea-bridge: approval-consumed";

/// One approval reaction, and the token that identifies it across sweeps.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Approval {
    /// Approving user.
    pub by: String,
    /// `<user>@<created_at>` — stable while the reaction stands, different once it is
    /// removed and re-added. A re-added 🐝 is therefore a *new* approval, which is how a
    /// human re-approves a task that blocked after running.
    pub fingerprint: String,
}

impl Approval {
    /// The marker comment recording that this approval has been acted on.
    pub fn consumed_marker(&self) -> String {
        format!("{CONSUMED_MARKER_PREFIX} {}", self.fingerprint)
    }

    /// Whether this task already records this exact approval as acted on.
    ///
    /// Matched line-exactly rather than by substring, so one fingerprint cannot be mistaken
    /// for a longer one that happens to start the same way.
    pub fn is_consumed(&self, detail: &TaskDetail) -> bool {
        let marker = self.consumed_marker();
        detail
            .comments
            .iter()
            .any(|c| c.body.lines().any(|l| l.trim() == marker))
    }
}

/// What a reaction sweep concluded for one issue.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ApprovalOutcome {
    /// One or more users with write permission approved, in API order.
    ///
    /// All of them are returned rather than just the first, so the caller can skip the ones
    /// it has already acted on: with only the first, a second maintainer's fresh 🐝 would be
    /// permanently shadowed by an earlier, already-consumed one.
    Approved {
        /// Authorized approvals, in API order.
        approvals: Vec<Approval>,
    },
    /// Nobody has reacted with the approval alias.
    ///
    /// Indistinguishable from "🐝 is missing from `[ui] REACTIONS`" — see the module note.
    NotRequested,
    /// The alias is present, but no reactor holds write permission.
    NotAuthorized {
        /// Users whose reaction was ignored.
        reactors: Vec<String>,
    },
    /// Write permission could not be established, so approval failed closed.
    ///
    /// The collaborator permission endpoint sits behind `reqToken()`
    /// (`routers/api/v1/api.go:1466`), and answers for *another* user only for a site
    /// admin or a repo admin (`routers/api/v1/repo/collaborators.go:279`). Without a
    /// token, or with one that is neither, the bridge cannot tell a maintainer from a
    /// passer-by, and refuses to guess. See [`preflight`], which says so before the fact
    /// instead of once per reaction.
    Undetermined {
        /// Why the check could not run.
        reason: String,
        /// Users whose reaction went unevaluated.
        reactors: Vec<String>,
    },
}

impl ApprovalOutcome {
    /// Whether the task may be promoted.
    pub fn is_approved(&self) -> bool {
        matches!(self, Self::Approved { .. })
    }

    /// The first approving user, for a log line.
    pub fn approver(&self) -> Option<&str> {
        match self {
            Self::Approved { approvals } => approvals.first().map(|a| a.by.as_str()),
            _ => None,
        }
    }
}

/// Whether a reaction's `content` is the configured approval alias.
///
/// Matching is on the alias, case-insensitively, and never on the codepoint: a codepoint
/// would never match a `content` the API returns, so accepting one would be a permanently
/// silent failure rather than a loud one.
pub fn is_approval_reaction(content: &str, alias: &str) -> bool {
    content.trim().eq_ignore_ascii_case(alias.trim())
}

/// The approval reactions on an issue, in API order, deduplicated by fingerprint.
///
/// `created_at` is part of the fingerprint and falls back to `-` when the API did not
/// return one. That fallback fails *closed*: without a timestamp a removed-and-re-added 🐝
/// is indistinguishable from the original, so it stays consumed rather than silently
/// unblocking the task again.
pub fn approval_reactions(reactions: &[Reaction], alias: &str) -> Vec<Approval> {
    let mut seen = std::collections::BTreeSet::new();
    let mut out = Vec::new();
    for r in reactions {
        if !is_approval_reaction(&r.content, alias) {
            continue;
        }
        let by = r.user.as_ref().map(|u| u.name().to_string()).unwrap_or_default();
        if by.is_empty() {
            continue;
        }
        let at = r.created_at.as_deref().map(str::trim).filter(|s| !s.is_empty());
        let fingerprint = format!("{by}@{}", at.unwrap_or("-"));
        if seen.insert(fingerprint.clone()) {
            out.push(Approval { by, fingerprint });
        }
    }
    out
}

/// The users who reacted with the approval alias, in API order, deduplicated.
pub fn approval_reactors(reactions: &[Reaction], alias: &str) -> Vec<String> {
    let mut seen = std::collections::BTreeSet::new();
    approval_reactions(reactions, alias)
        .into_iter()
        .filter(|a| seen.insert(a.by.clone()))
        .map(|a| a.by)
        .collect()
}

/// Polls one issue's reactions and decides whether it is approved.
pub async fn evaluate(
    gitea: &GiteaClient,
    cfg: &ApprovalConfig,
    owner: &str,
    repo: &str,
    index: i64,
) -> Result<ApprovalOutcome, GiteaError> {
    let reactions = gitea.issue_reactions(owner, repo, index).await?;
    let candidates = approval_reactions(&reactions, &cfg.reaction);
    if candidates.is_empty() {
        return Ok(ApprovalOutcome::NotRequested);
    }
    let reactors: Vec<String> = approval_reactors(&reactions, &cfg.reaction);

    if !cfg.require_write_permission {
        tracing::warn!(
            owner, repo, index, by = %candidates[0].by,
            "approval.require_write_permission is off — accepting a reaction without checking permission"
        );
        return Ok(ApprovalOutcome::Approved {
            approvals: candidates,
        });
    }

    if !gitea.has_token() {
        return Ok(ApprovalOutcome::Undetermined {
            reason: "no gitea.token configured; the collaborator permission endpoint requires one".into(),
            reactors,
        });
    }

    let mut undetermined: Option<String> = None;
    let mut approvals = Vec::new();
    for candidate in &candidates {
        let user = &candidate.by;
        match gitea.user_permission(owner, repo, user).await {
            Ok(perm) if perm.is_writer() => {
                tracing::info!(owner, repo, index, %user, permission = %perm.permission, "approval accepted");
                approvals.push(candidate.clone());
            }
            Ok(perm) => {
                // Acceptance criterion 5: ignored, and logged — not silently dropped.
                tracing::info!(
                    owner, repo, index, %user, permission = %perm.permission,
                    "ignoring approval reaction from a user without write permission"
                );
            }
            Err(GiteaError::NotFound { .. }) => {
                // 404 is *not* "not a collaborator": `GetRepoPermissions` calls
                // `GetUserRepoPermission` unconditionally and answers 200 with
                // `permission: "none"` for a stranger. 404 fires only when the username
                // itself does not resolve (`collaborators.go:286-288`) — a renamed or
                // deleted account, which cannot have write permission either way.
                tracing::info!(
                    owner, repo, index, %user,
                    "ignoring approval reaction from a username the instance does not resolve"
                );
            }
            Err(err @ GiteaError::Unauthorized { status: 403, .. }) => {
                // The dominant real-world failure, and the one worth naming precisely:
                // querying another user's permission needs site admin or repo admin
                // (`collaborators.go:279`). A plain write-scoped token gets 403 here, so
                // approval fails closed for every reactor, forever, until the token is
                // changed. `preflight` reports this at startup rather than per reaction.
                tracing::warn!(
                    owner, repo, index, %user,
                    "permission lookup forbidden: gitea.token must belong to a site admin \
                     or an admin of this repository to query another user's permission"
                );
                undetermined.get_or_insert_with(|| err.to_string());
            }
            Err(err) => {
                tracing::warn!(owner, repo, index, %user, error = %err, "permission lookup failed");
                undetermined.get_or_insert_with(|| err.to_string());
            }
        }
    }

    if !approvals.is_empty() {
        return Ok(ApprovalOutcome::Approved { approvals });
    }
    Ok(match undetermined {
        Some(reason) => ApprovalOutcome::Undetermined { reason, reactors },
        None => ApprovalOutcome::NotAuthorized { reactors },
    })
}

/// What the approval preflight concluded about the configured token.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Preflight {
    /// Approval can work. The reason names why, so `check` can print it.
    Usable {
        /// Human-readable explanation.
        reason: String,
    },
    /// Approval cannot work and every 🐝 will resolve to `Undetermined`.
    Unusable {
        /// Human-readable explanation, including what to change.
        reason: String,
    },
}

impl Preflight {
    /// Whether approval can work with this token.
    pub fn is_usable(&self) -> bool {
        matches!(self, Self::Usable { .. })
    }

    /// The explanation, whichever variant this is.
    pub fn reason(&self) -> &str {
        match self {
            Self::Usable { reason } | Self::Unusable { reason } => reason,
        }
    }
}

/// Probes whether the configured token can actually run the write-permission check.
///
/// "A token" is not enough, and that gap is silent: `GetRepoPermissions` answers for a
/// user other than the caller only if the caller is a site admin or an admin of the
/// repository (`routers/api/v1/repo/collaborators.go:279`). A plain write-scoped token
/// gets 403 for every reactor, so approval fails closed permanently while the daemon looks
/// healthy.
///
/// The probe asks two questions a token of *any* scope can answer: who am I
/// (`GET /api/v1/user`), and what is my own permission on each repo — the self-query is the
/// one case `collaborators.go:279` lets a non-admin through.
pub async fn preflight(gitea: &GiteaClient, cfg: &ApprovalConfig, repos: &[RepoRef]) -> Preflight {
    if !cfg.require_write_permission {
        return Preflight::Usable {
            reason: "approval.require_write_permission is off — any reactor's 🐝 promotes a task, \
                     and no permission lookup is made"
                .into(),
        };
    }
    if !gitea.has_token() {
        return Preflight::Unusable {
            reason: "no gitea.token configured; the collaborator permission endpoint sits behind \
                     reqToken() (routers/api/v1/api.go:1466)"
                .into(),
        };
    }

    let me = match gitea.current_user().await {
        Ok(me) => me,
        Err(err) => {
            return Preflight::Unusable {
                reason: format!("cannot identify the configured token via GET /api/v1/user: {err}"),
            };
        }
    };
    if me.is_admin {
        return Preflight::Usable {
            reason: format!("gitea.token belongs to site administrator {:?}", me.login),
        };
    }

    for repo in repos {
        match gitea.user_permission(&repo.owner, &repo.repo, &me.login).await {
            Ok(perm) if perm.is_repo_admin() => {}
            Ok(perm) => {
                return Preflight::Unusable {
                    reason: format!(
                        "gitea.token user {:?} has {:?} on {} — querying another user's permission \
                         needs site admin or repo admin (routers/api/v1/repo/collaborators.go:279), \
                         so every approval reaction will resolve to Undetermined",
                        me.login,
                        perm.permission,
                        repo.slug()
                    ),
                };
            }
            Err(err) => {
                return Preflight::Unusable {
                    reason: format!(
                        "cannot read the permission of {:?} on {}: {err}",
                        me.login,
                        repo.slug()
                    ),
                };
            }
        }
    }

    Preflight::Usable {
        reason: format!(
            "gitea.token user {:?} is an admin of every configured repository",
            me.login
        ),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::{DEFAULT_APPROVAL_REACTION, GiteaConfig};
    use crate::gitea::ReactionUser;

    fn reaction(user: &str, content: &str) -> Reaction {
        Reaction {
            user: Some(ReactionUser {
                login: user.into(),
                username: None,
            }),
            content: content.into(),
            created_at: None,
        }
    }

    #[test]
    fn the_alias_matches_and_the_codepoint_does_not() {
        assert!(is_approval_reaction("honeybee", DEFAULT_APPROVAL_REACTION));
        assert!(is_approval_reaction(" HoneyBee ", DEFAULT_APPROVAL_REACTION));
        // U+1F41D itself is never what the API returns, and accepting it would hide a
        // misconfiguration behind a match that can never fire.
        assert!(!is_approval_reaction("\u{1f41d}", DEFAULT_APPROVAL_REACTION));
        assert!(!is_approval_reaction("bee", DEFAULT_APPROVAL_REACTION));
        assert!(!is_approval_reaction("+1", DEFAULT_APPROVAL_REACTION));
        assert!(!is_approval_reaction("", DEFAULT_APPROVAL_REACTION));
    }

    #[test]
    fn reactors_are_collected_in_order_without_duplicates() {
        let reactions = vec![
            reaction("carol", "+1"),
            reaction("alex", "honeybee"),
            reaction("bo", "honeybee"),
            reaction("alex", "honeybee"),
            reaction("dee", "\u{1f41d}"),
        ];
        assert_eq!(
            approval_reactors(&reactions, DEFAULT_APPROVAL_REACTION),
            vec!["alex", "bo"]
        );
    }

    #[test]
    fn a_reaction_without_a_user_is_skipped() {
        let orphan = Reaction {
            user: None,
            content: "honeybee".into(),
            created_at: None,
        };
        assert!(approval_reactors(&[orphan], DEFAULT_APPROVAL_REACTION).is_empty());
    }

    #[test]
    fn username_is_used_when_login_is_absent() {
        let r = Reaction {
            user: Some(ReactionUser {
                login: String::new(),
                username: Some("alex".into()),
            }),
            content: "honeybee".into(),
            created_at: None,
        };
        assert_eq!(approval_reactors(&[r], DEFAULT_APPROVAL_REACTION), vec!["alex"]);
    }

    /// The finding this closes: a 🐝 is durable on the issue, so a task approved once, which
    /// later blocks, would be unblocked by the same reaction on every 60s sweep — running,
    /// blocking and re-running indefinitely while Gitea shows nothing, because BLOCK_MARKER
    /// suppresses the repeat comment.
    #[test]
    fn a_consumed_approval_does_not_unblock_the_task_again() {
        let at = "2026-08-04T16:23:00Z";
        let stale = Approval {
            by: "alex".into(),
            fingerprint: format!("alex@{at}"),
        };
        let mut detail = TaskDetail::default();
        assert!(!stale.is_consumed(&detail), "nothing has consumed it yet");

        detail.comments.push(crate::hermes::TaskComment {
            author: Some("bridge".into()),
            body: stale.consumed_marker(),
        });
        assert!(stale.is_consumed(&detail));

        // Removing and re-adding the reaction produces a new created_at, which is a new
        // approval — that is how a human re-approves a task that blocked after running.
        let fresh = Approval {
            by: "alex".into(),
            fingerprint: "alex@2026-08-05T09:00:00Z".into(),
        };
        assert!(!fresh.is_consumed(&detail));

        // …and so is a different maintainer's 🐝, which is why `Approved` carries every
        // authorized approval rather than only the first.
        let other = Approval {
            by: "bo".into(),
            fingerprint: format!("bo@{at}"),
        };
        assert!(!other.is_consumed(&detail));
    }

    #[test]
    fn the_fingerprint_is_matched_line_exactly() {
        // A substring match would let one fingerprint be shadowed by a longer one that
        // happens to start the same way.
        let a = Approval {
            by: "alex".into(),
            fingerprint: "alex@-".into(),
        };
        let detail = TaskDetail {
            comments: vec![crate::hermes::TaskComment {
                author: None,
                body: format!("{CONSUMED_MARKER_PREFIX} alex@-extra"),
            }],
            ..TaskDetail::default()
        };
        assert!(!a.is_consumed(&detail));
    }

    #[test]
    fn the_fingerprint_tracks_the_reaction_timestamp() {
        let with_time = |at: Option<&str>| Reaction {
            user: Some(ReactionUser {
                login: "alex".into(),
                username: None,
            }),
            content: "honeybee".into(),
            created_at: at.map(str::to_string),
        };
        let first = approval_reactions(
            &[with_time(Some("2026-08-04T16:23:00Z"))],
            DEFAULT_APPROVAL_REACTION,
        );
        let second = approval_reactions(
            &[with_time(Some("2026-08-05T09:00:00Z"))],
            DEFAULT_APPROVAL_REACTION,
        );
        assert_ne!(first[0].fingerprint, second[0].fingerprint);
        assert_eq!(first[0].by, "alex");

        // No timestamp: the fallback is stable, so an unrecorded reaction is consumed once
        // and stays consumed rather than re-firing forever.
        let a = approval_reactions(&[with_time(None)], DEFAULT_APPROVAL_REACTION);
        let b = approval_reactions(&[with_time(None)], DEFAULT_APPROVAL_REACTION);
        assert_eq!(a[0].fingerprint, b[0].fingerprint);
        assert_eq!(a[0].fingerprint, "alex@-");
    }

    #[test]
    fn outcome_is_approved_only_for_the_approved_variant() {
        assert!(
            ApprovalOutcome::Approved {
                approvals: vec![Approval {
                    by: "alex".into(),
                    fingerprint: "alex@-".into()
                }]
            }
            .is_approved()
        );
        assert!(!ApprovalOutcome::NotRequested.is_approved());
        assert!(
            !ApprovalOutcome::NotAuthorized {
                reactors: vec!["mallory".into()]
            }
            .is_approved()
        );
        assert!(
            !ApprovalOutcome::Undetermined {
                reason: "no token".into(),
                reactors: vec![]
            }
            .is_approved()
        );
    }

    /// With permission checks off there is nothing to preflight, and the reason has to say
    /// what was traded away rather than reporting a clean bill of health.
    #[tokio::test]
    async fn preflight_names_the_risk_when_permission_checks_are_off() {
        // Unreachable on purpose: this path must not make a request at all.
        let gitea = GiteaClient::new(&GiteaConfig {
            base_url: "http://127.0.0.1:1".into(),
            ..GiteaConfig::default()
        })
        .expect("client");
        let cfg = ApprovalConfig {
            require_write_permission: false,
            ..ApprovalConfig::default()
        };
        let outcome = preflight(&gitea, &cfg, &[RepoRef::new("terraphim", "gitea")]).await;
        assert!(outcome.is_usable(), "{outcome:?}");
        assert!(
            outcome.reason().contains("require_write_permission"),
            "{}",
            outcome.reason()
        );
    }
}
