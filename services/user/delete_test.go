// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package user

import (
	"fmt"
	"testing"

	agent_model "code.gitea.io/gitea/models/agent"
	auth_model "code.gitea.io/gitea/models/auth"
	"code.gitea.io/gitea/models/db"
	"code.gitea.io/gitea/models/unittest"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/timeutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agentTestPubKey mints a distinct, well-formed 32-byte hex public key per test. Nothing in the
// deletion path verifies a signature, so these need only be valid hex of the right length and
// unique - which the `pub_key` UNIQUE index requires of them.
func agentTestPubKey(n int) string {
	return fmt.Sprintf("%064x", n)
}

// registerTestAgentKey enrols a key straight through the model, which is the shape the deletion
// path reads. A scope is mandatory; any one will do, because none of this is about scopes.
func registerTestAgentKey(t *testing.T, ownerID, agentID int64, pubKey string) *agent_model.Key {
	t.Helper()
	key := &agent_model.Key{
		OwnerUserID: ownerID,
		AgentUserID: agentID,
		PubKey:      pubKey,
		Scope:       auth_model.AccessTokenScopeWriteIssue,
	}
	require.NoError(t, agent_model.RegisterKey(t.Context(), key))
	return key
}

// flagAsAgent marks a fixture user as an agent account, the way registering its first key does.
func flagAsAgent(t *testing.T, userID int64) *user_model.User {
	t.Helper()
	u := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: userID})
	u.IsAgent = true
	require.NoError(t, user_model.UpdateUserCols(t.Context(), u, "is_agent"))
	return u
}

func assertIsAgent(t *testing.T, userID int64, want bool) {
	t.Helper()
	u := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: userID})
	assert.Equal(t, want, u.IsAgent, "user %d: is_agent", userID)
}

// assertAgentStateIsConsistent asserts the two invariants the agent half of deleteUser exists to
// keep, over the whole table rather than over the rows one case happened to create:
//
//   - No key row may outlive either end of its ownership chain. A key is never deleted anywhere
//     else in the codebase - revocation only sets revoked_unix - so a row left behind by deletion
//     parks its public key in the `pub_key` UNIQUE index with no code path able to clear it,
//     burning that keypair for the instance permanently.
//   - No account may stay flagged `is_agent` with no unrevoked key behind it. The flag is only
//     ever earned by holding one, and revocation takes it away on the last key for exactly this
//     reason; leaving it set is the half-undone enrolment revocation is written to avoid.
func assertAgentStateIsConsistent(t *testing.T) {
	t.Helper()

	keys := make([]*agent_model.Key, 0, 8)
	require.NoError(t, db.GetEngine(t.Context()).Find(&keys))
	for _, key := range keys {
		for _, end := range []struct {
			role string
			id   int64
		}{{"owner", key.OwnerUserID}, {"agent", key.AgentUserID}} {
			assert.Equal(t, 1, unittest.GetCount(t, &user_model.User{ID: end.id}),
				"agent key %d (%s) outlived its %s user %d, so its public key can never be unregistered",
				key.ID, key.PubKey, end.role, end.id)
		}
	}

	agents := make([]*user_model.User, 0, 4)
	require.NoError(t, db.GetEngine(t.Context()).Where("is_agent = ?", true).Find(&agents))
	for _, agent := range agents {
		active, err := agent_model.HasActiveKeyForAgent(t.Context(), agent.ID)
		require.NoError(t, err)
		assert.True(t, active, "user %d is still enrolled as an agent with no unrevoked key behind it", agent.ID)
	}
}

// Deleting an account has to take both ends of the NIP-98 ownership chain with it: the keys the
// account vouched for as the human owner, and the keys that authenticate *as* it. Neither is
// about authentication - services/auth.Nostr already refuses a key whose agent or owner cannot be
// loaded - it is that nothing else in the codebase ever deletes a key row, so what deletion leaves
// behind stays forever. See assertAgentStateIsConsistent for the two invariants at stake.
//
// The deletions below purge, which is what lets a fixture user with organization memberships and
// repositories be deleted at all - plain deletion refuses both. The agent block runs before and
// independently of that choice, so it is the same code either way. user2 stands in for a second
// human who is not being deleted.
func TestDeleteUserAgentKeys(t *testing.T) {
	t.Run("keys the deleted human vouched for go with them, and orphan the agent", func(t *testing.T) {
		require.NoError(t, unittest.PrepareTestDatabase())
		owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 4})
		flagAsAgent(t, 5)
		key := registerTestAgentKey(t, owner.ID, 5, agentTestPubKey(0x1a))

		require.NoError(t, DeleteUser(t.Context(), owner, true))

		unittest.AssertNotExistsBean(t, &agent_model.Key{ID: key.ID})
		assertIsAgent(t, 5, false)
		assertAgentStateIsConsistent(t)

		// The rationale in full: with the row gone, the same keypair can be enrolled again.
		require.NoError(t, agent_model.RegisterKey(t.Context(), &agent_model.Key{
			OwnerUserID: 2, AgentUserID: 5, PubKey: key.PubKey, Scope: auth_model.AccessTokenScopeWriteIssue,
		}), "the deleted owner's public key is still parked in the UNIQUE index")
	})

	// The other direction, and the one a `DeleteByBean(&Key{OwnerUserID: u.ID})` on its own would
	// miss entirely: the deleted account is the agent, and somebody else vouched for the key.
	t.Run("keys naming the deleted user as the agent go too, whoever vouched for them", func(t *testing.T) {
		require.NoError(t, unittest.PrepareTestDatabase())
		agent := flagAsAgent(t, 4)
		key := registerTestAgentKey(t, 2, agent.ID, agentTestPubKey(0x2a))

		require.NoError(t, DeleteUser(t.Context(), agent, true))

		unittest.AssertNotExistsBean(t, &agent_model.Key{ID: key.ID})
		assertAgentStateIsConsistent(t)

		require.NoError(t, agent_model.RegisterKey(t.Context(), &agent_model.Key{
			OwnerUserID: 2, AgentUserID: 5, PubKey: key.PubKey, Scope: auth_model.AccessTokenScopeWriteIssue,
		}), "the deleted agent's public key is still parked in the UNIQUE index")
	})

	// `is_agent` is cleared because the account was left with nothing, not because one of its keys
	// happened to be deleted - so an agent another human still vouches for keeps it.
	t.Run("an agent that still holds a key keeps the flag", func(t *testing.T) {
		require.NoError(t, unittest.PrepareTestDatabase())
		owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 4})
		flagAsAgent(t, 5)
		deleted := registerTestAgentKey(t, owner.ID, 5, agentTestPubKey(0x3a))
		survivor := registerTestAgentKey(t, 2, 5, agentTestPubKey(0x3b))

		require.NoError(t, DeleteUser(t.Context(), owner, true))

		unittest.AssertNotExistsBean(t, &agent_model.Key{ID: deleted.ID})
		unittest.AssertExistsAndLoadBean(t, &agent_model.Key{ID: survivor.ID})
		assertIsAgent(t, 5, true)
		assertAgentStateIsConsistent(t)
	})

	// A revoked key is not a key the account can still act with, so it must not hold the flag up
	// either - the same rule revocation itself applies when it takes the last one away.
	t.Run("a revoked key left behind does not hold the flag up", func(t *testing.T) {
		require.NoError(t, unittest.PrepareTestDatabase())
		owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 4})
		flagAsAgent(t, 5)
		registerTestAgentKey(t, owner.ID, 5, agentTestPubKey(0x4a))
		revoked := registerTestAgentKey(t, 2, 5, agentTestPubKey(0x4b))
		require.NoError(t, agent_model.RevokeKey(t.Context(), revoked.ID))

		require.NoError(t, DeleteUser(t.Context(), owner, true))

		// The revoked row belongs to neither end of the deleted chain, so it stays - audit rows
		// refer to it by id and must keep resolving.
		unittest.AssertExistsAndLoadBean(t, &agent_model.Key{ID: revoked.ID})
		assertIsAgent(t, 5, false)
		assertAgentStateIsConsistent(t)
	})

	// An account that vouched for itself is both ends of the chain at once. Nothing needs to clear
	// its flag - the row goes with the user - and the pass over orphaned agents must not try to,
	// because that user is about to cease to exist.
	t.Run("a self-owned agent key", func(t *testing.T) {
		require.NoError(t, unittest.PrepareTestDatabase())
		self := flagAsAgent(t, 11)
		key := registerTestAgentKey(t, self.ID, self.ID, agentTestPubKey(0x5a))

		require.NoError(t, DeleteUser(t.Context(), self, true))

		unittest.AssertNotExistsBean(t, &agent_model.Key{ID: key.ID})
		unittest.AssertNotExistsBean(t, &user_model.User{ID: 11})
		assertAgentStateIsConsistent(t)
	})

	// The trail is deliberately kept: every row carries the whole signed event, so it stays
	// checkable without agent_key, and deleting the account of an agent must not erase what that
	// agent did. Inserted directly rather than through InsertAuditEvent because what is under test
	// is whether deletion touches the row, not whether the row is well-formed.
	t.Run("the audit trail survives the account it describes", func(t *testing.T) {
		require.NoError(t, unittest.PrepareTestDatabase())
		agent := flagAsAgent(t, 4)
		key := registerTestAgentKey(t, 2, agent.ID, agentTestPubKey(0x6a))
		event := &agent_model.AuditEvent{
			AgentUserID:      agent.ID,
			OwnerUserID:      2,
			AgentKeyID:       key.ID,
			EventID:          agentTestPubKey(0x6b),
			PubKey:           key.PubKey,
			Method:           "POST",
			RequestURL:       "/api/v1/repos/user2/repo1/issues",
			EventCreatedUnix: timeutil.TimeStampNow(),
		}
		require.NoError(t, db.Insert(t.Context(), event))

		require.NoError(t, DeleteUser(t.Context(), agent, true))

		unittest.AssertExistsAndLoadBean(t, &agent_model.AuditEvent{ID: event.ID})
	})
}
