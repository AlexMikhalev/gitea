// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package user

import (
	"context"
	"fmt"
	"time"

	_ "image/jpeg" // Needed for jpeg support

	actions_model "code.gitea.io/gitea/models/actions"
	activities_model "code.gitea.io/gitea/models/activities"
	agent_model "code.gitea.io/gitea/models/agent"
	asymkey_model "code.gitea.io/gitea/models/asymkey"
	auth_model "code.gitea.io/gitea/models/auth"
	"code.gitea.io/gitea/models/db"
	git_model "code.gitea.io/gitea/models/git"
	issues_model "code.gitea.io/gitea/models/issues"
	"code.gitea.io/gitea/models/organization"
	access_model "code.gitea.io/gitea/models/perm/access"
	pull_model "code.gitea.io/gitea/models/pull"
	repo_model "code.gitea.io/gitea/models/repo"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/setting"

	"xorm.io/builder"
)

// deleteUser deletes models associated to an user.
func deleteUser(ctx context.Context, u *user_model.User, purge bool) (err error) {
	e := db.GetEngine(ctx)

	// ***** START: Watch *****
	watchedRepoIDs, err := db.FindIDs(ctx, "watch", "watch.repo_id",
		builder.Eq{"watch.user_id": u.ID}.
			And(builder.Neq{"watch.mode": repo_model.WatchModeDont}))
	if err != nil {
		return fmt.Errorf("get all watches: %w", err)
	}
	if err = db.DecrByIDs(ctx, watchedRepoIDs, "num_watches", new(repo_model.Repository)); err != nil {
		return fmt.Errorf("decrease repository num_watches: %w", err)
	}
	// ***** END: Watch *****

	// ***** START: Star *****
	starredRepoIDs, err := db.FindIDs(ctx, "star", "star.repo_id",
		builder.Eq{"star.uid": u.ID})
	if err != nil {
		return fmt.Errorf("get all stars: %w", err)
	} else if err = db.DecrByIDs(ctx, starredRepoIDs, "num_stars", new(repo_model.Repository)); err != nil {
		return fmt.Errorf("decrease repository num_stars: %w", err)
	}
	// ***** END: Star *****

	// ***** START: Follow *****
	followeeIDs, err := db.FindIDs(ctx, "follow", "follow.follow_id",
		builder.Eq{"follow.user_id": u.ID})
	if err != nil {
		return fmt.Errorf("get all followees: %w", err)
	} else if err = db.DecrByIDs(ctx, followeeIDs, "num_followers", new(user_model.User)); err != nil {
		return fmt.Errorf("decrease user num_followers: %w", err)
	}

	followerIDs, err := db.FindIDs(ctx, "follow", "follow.user_id",
		builder.Eq{"follow.follow_id": u.ID})
	if err != nil {
		return fmt.Errorf("get all followers: %w", err)
	} else if err = db.DecrByIDs(ctx, followerIDs, "num_following", new(user_model.User)); err != nil {
		return fmt.Errorf("decrease user num_following: %w", err)
	}
	// ***** END: Follow *****

	if err = db.DeleteBeans(ctx,
		&auth_model.AccessToken{UID: u.ID},
		&repo_model.Collaboration{UserID: u.ID},
		&access_model.Access{UserID: u.ID},
		&repo_model.Watch{UserID: u.ID},
		&repo_model.Star{UID: u.ID},
		&user_model.Follow{UserID: u.ID},
		&user_model.Follow{FollowID: u.ID},
		&activities_model.Action{UserID: u.ID},
		&issues_model.IssueUser{UID: u.ID},
		&user_model.EmailAddress{UID: u.ID},
		&user_model.UserOpenID{UID: u.ID},
		&issues_model.Reaction{UserID: u.ID},
		&organization.TeamUser{UID: u.ID},
		&issues_model.Stopwatch{UserID: u.ID},
		&user_model.Setting{UserID: u.ID},
		&user_model.UserBadge{UserID: u.ID},
		&pull_model.AutoMerge{DoerID: u.ID},
		&pull_model.ReviewState{UserID: u.ID},
		&user_model.Redirect{RedirectUserID: u.ID},
		&actions_model.ActionRunner{OwnerID: u.ID},
		&user_model.Blocking{BlockerID: u.ID},
		&user_model.Blocking{BlockeeID: u.ID},
		&actions_model.ActionRunnerToken{OwnerID: u.ID},
	); err != nil {
		return fmt.Errorf("deleteBeans: %w", err)
	}

	// ***** START: Agent keys *****
	// Both ends of the NIP-98 ownership chain go with the account, alongside the access tokens
	// and SSH keys above: a key this user vouched for is a credential they are accountable for,
	// and a key that signs *as* this user would otherwise outlive the user itself.
	//
	// Authentication is not what this is for - services/auth.Nostr already refuses a key whose
	// agent user or owner cannot be loaded. It is that a key row is never otherwise deleted, so
	// leaving these behind would park a public key in the `pub_key` UNIQUE index with no path
	// left that can clear it, burning that keypair for the instance permanently.
	//
	// The audit trail is deliberately kept, and does not need these rows: every row carries the
	// whole signed event, and models/agent.VerifyEvent re-derives its id and re-checks its
	// signature without consulting agent_key at all.
	agentUserIDs, err := db.FindIDs(ctx, "agent_key", "agent_key.agent_user_id",
		builder.Eq{"agent_key.owner_user_id": u.ID})
	if err != nil {
		return fmt.Errorf("find agent keys owned by user: %w", err)
	}
	if _, err = db.DeleteByBean(ctx, &agent_model.Key{OwnerUserID: u.ID}); err != nil {
		return fmt.Errorf("deleteOwnedAgentKeys: %w", err)
	}
	if _, err = db.DeleteByBean(ctx, &agent_model.Key{AgentUserID: u.ID}); err != nil {
		return fmt.Errorf("deleteAgentKeys: %w", err)
	}
	// An agent account this user vouched for may now hold no key at all, and `is_agent` is only
	// earned by holding one - the revoke endpoint takes the flag away on the last key for exactly
	// this reason. Leaving it set would keep the account enrolled as an agent with nothing behind
	// it, which is the half-undo revocation is written to avoid.
	for _, agentUserID := range agentUserIDs {
		if agentUserID == u.ID {
			continue // deleted below with the user itself
		}
		stillActive, err := agent_model.HasActiveKeyForAgent(ctx, agentUserID)
		if err != nil {
			return fmt.Errorf("HasActiveKeyForAgent: %w", err)
		}
		if stillActive {
			continue
		}
		agentUser, err := user_model.GetUserByID(ctx, agentUserID)
		if err != nil {
			if user_model.IsErrUserNotExist(err) {
				continue
			}
			return fmt.Errorf("GetUserByID: %w", err)
		}
		if !agentUser.IsAgent {
			continue
		}
		agentUser.IsAgent = false
		if err := user_model.UpdateUserCols(ctx, agentUser, "is_agent"); err != nil {
			return fmt.Errorf("clear is_agent: %w", err)
		}
	}
	// ***** END: Agent keys *****

	if err := auth_model.DeleteOAuth2RelictsByUserID(ctx, u.ID); err != nil {
		return err
	}

	if purge || (setting.Service.UserDeleteWithCommentsMaxTime != 0 &&
		u.CreatedUnix.AsTime().Add(setting.Service.UserDeleteWithCommentsMaxTime).After(time.Now())) {
		// Delete Comments
		const batchSize = 50
		for {
			comments := make([]*issues_model.Comment, 0, batchSize)
			if err = e.Where("type=? AND poster_id=?", issues_model.CommentTypeComment, u.ID).Limit(batchSize, 0).Find(&comments); err != nil {
				return err
			}
			if len(comments) == 0 {
				break
			}

			for _, comment := range comments {
				if err = issues_model.DeleteComment(ctx, comment); err != nil {
					return err
				}
			}
		}

		// Delete Reactions
		if err = issues_model.DeleteReaction(ctx, &issues_model.ReactionOptions{DoerID: u.ID}); err != nil {
			return err
		}
	}

	// ***** START: Branch Protections *****
	{
		const batchSize = 50
		for start := 0; ; start += batchSize {
			protections := make([]*git_model.ProtectedBranch, 0, batchSize)
			// @perf: We can't filter on DB side by u.ID, as those IDs are serialized as JSON strings.
			//   We could filter down with `WHERE repo_id IN (reposWithPushPermission(u))`,
			//   though that query will be quite complex and tricky to maintain (compare `getRepoAssignees()`).
			// Also, as we didn't update branch protections when removing entries from `access` table,
			//   it's safer to iterate all protected branches.
			if err = e.Limit(batchSize, start).Find(&protections); err != nil {
				return fmt.Errorf("findProtectedBranches: %w", err)
			}
			if len(protections) == 0 {
				break
			}
			for _, p := range protections {
				if err := git_model.RemoveUserIDFromProtectedBranch(ctx, p, u.ID); err != nil {
					return err
				}
			}
		}
	}
	// ***** END: Branch Protections *****

	// ***** START: PublicKey *****
	if _, err = db.DeleteByBean(ctx, &asymkey_model.PublicKey{OwnerID: u.ID}); err != nil {
		return fmt.Errorf("deletePublicKeys: %w", err)
	}
	// ***** END: PublicKey *****

	// ***** START: GPGPublicKey *****
	keys, err := db.Find[asymkey_model.GPGKey](ctx, asymkey_model.FindGPGKeyOptions{
		OwnerID: u.ID,
	})
	if err != nil {
		return fmt.Errorf("ListGPGKeys: %w", err)
	}
	// Delete GPGKeyImport(s).
	for _, key := range keys {
		if _, err = db.DeleteByBean(ctx, &asymkey_model.GPGKeyImport{KeyID: key.KeyID}); err != nil {
			return fmt.Errorf("deleteGPGKeyImports: %w", err)
		}
	}
	if _, err = db.DeleteByBean(ctx, &asymkey_model.GPGKey{OwnerID: u.ID}); err != nil {
		return fmt.Errorf("deleteGPGKeys: %w", err)
	}
	// ***** END: GPGPublicKey *****

	// Clear assignee.
	if _, err = db.DeleteByBean(ctx, &issues_model.IssueAssignees{AssigneeID: u.ID}); err != nil {
		return fmt.Errorf("clear assignee: %w", err)
	}

	// ***** START: ExternalLoginUser *****
	if err = user_model.RemoveAllAccountLinks(ctx, u); err != nil {
		return fmt.Errorf("ExternalLoginUser: %w", err)
	}
	// ***** END: ExternalLoginUser *****

	if err := auth_model.DeleteAuthTokensByUserID(ctx, u.ID); err != nil {
		return fmt.Errorf("DeleteAuthTokensByUserID: %w", err)
	}

	if _, err = db.DeleteByID[user_model.User](ctx, u.ID); err != nil {
		return fmt.Errorf("delete: %w", err)
	}

	return nil
}
