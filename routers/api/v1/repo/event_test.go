// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"testing"

	auth_model "code.gitea.io/gitea/models/auth"
	"code.gitea.io/gitea/models/unittest"
	"code.gitea.io/gitea/services/contexttest"
	"code.gitea.io/gitea/services/repoevent"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The stream spans several units, so the route is gated on "may read something in this repository"
// - AccessTokenScopeCategoryRepository. But the endpoints that own plain issue and pull request
// comment bodies are behind AccessTokenScopeCategoryIssue, so serving the comment kind to a
// repository-scoped token would hand it every comment body in the repository through a route its
// scope was never meant to reach. tokenRequiresScopes is any-of and cannot draw that line; this is
// where it is drawn.
func TestEventKindsWithinTokenScope(t *testing.T) {
	unittest.PrepareTestEnv(t)

	kindsFor := func(t *testing.T, scope auth_model.AccessTokenScope, asked ...repoevent.Kind) []repoevent.Kind {
		t.Helper()
		ctx, _ := contexttest.MockAPIContext(t, "user2/repo1")
		ctx.Data["IsApiToken"] = true
		ctx.Data["ApiTokenScope"] = scope
		kinds, err := kindsWithinTokenScope(ctx, asked)
		require.NoError(t, err)
		return kinds
	}

	t.Run("a repository-scoped token gets the stream without comments", func(t *testing.T) {
		kinds := kindsFor(t, auth_model.AccessTokenScopeReadRepository)
		assert.NotContains(t, kinds, repoevent.KindComment,
			"a read:repository token read comment bodies it has no other route to")
		// The rest of the stream is still served: review, status and the audit trail are
		// owned by endpoints inside this same repository-scoped group.
		assert.Equal(t, []repoevent.Kind{
			repoevent.KindAction, repoevent.KindAgentAudit, repoevent.KindReview, repoevent.KindStatus,
		}, kinds)
	})

	t.Run("read:issue restores the comment kind", func(t *testing.T) {
		kinds := kindsFor(t, "read:repository,read:issue")
		assert.Equal(t, repoevent.AllKinds, kinds)
	})

	t.Run("a broader scope contains the narrower one", func(t *testing.T) {
		// `all` and `read:issue` are different strings for the same permission here, and the
		// bitmap - not string equality - is what has to answer that.
		assert.Equal(t, repoevent.AllKinds, kindsFor(t, auth_model.AccessTokenScopeAll))
	})

	t.Run("an explicit kinds filter is narrowed, not widened", func(t *testing.T) {
		// The trap: ListOptions.Kinds empty means *every* kind, so a filter that dropped the
		// caller's only kind and handed the empty slice on would serve the whole stream.
		assert.Empty(t, kindsFor(t, auth_model.AccessTokenScopeReadRepository, repoevent.KindComment))
		assert.Equal(t, []repoevent.Kind{repoevent.KindReview},
			kindsFor(t, auth_model.AccessTokenScopeReadRepository, repoevent.KindComment, repoevent.KindReview))
	})

	t.Run("a caller that is not a token is unrestricted", func(t *testing.T) {
		// Scopes are a property of tokens. A session, basic auth or an Actions task has none,
		// and reading the absent scope as "no scopes" would empty their stream instead.
		ctx, _ := contexttest.MockAPIContext(t, "user2/repo1")
		kinds, err := kindsWithinTokenScope(ctx, nil)
		require.NoError(t, err)
		assert.Nil(t, kinds, "every kind, which ListOptions spells as the empty slice")

		ctx, _ = contexttest.MockAPIContext(t, "user2/repo1")
		ctx.Data["ApiTokenScope"] = auth_model.AccessTokenScopeReadRepository // but IsApiToken is not set
		kinds, err = kindsWithinTokenScope(ctx, nil)
		require.NoError(t, err)
		assert.Nil(t, kinds)
	})

	t.Run("an unparsable scope is an error, not an open door", func(t *testing.T) {
		ctx, _ := contexttest.MockAPIContext(t, "user2/repo1")
		ctx.Data["IsApiToken"] = true
		ctx.Data["ApiTokenScope"] = auth_model.AccessTokenScope("read:nonsense")
		_, err := kindsWithinTokenScope(ctx, nil)
		require.Error(t, err)
	})
}
