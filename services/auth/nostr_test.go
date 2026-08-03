// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"

	agent_model "code.gitea.io/gitea/models/agent"
	auth_model "code.gitea.io/gitea/models/auth"
	"code.gitea.io/gitea/models/db"
	"code.gitea.io/gitea/models/unittest"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/json"
	"code.gitea.io/gitea/modules/nostr"
	"code.gitea.io/gitea/modules/reqctx"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/test"
	"code.gitea.io/gitea/services/agentauth"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A fixed keypair; a test vector, never to be used for anything real.
const nostrTestSecretKey = "0000000000000000000000000000000000000000000000000000000000000007"

func nostrTestRequest(t *testing.T, method, path, body, authHeader string) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://localhost:3000"+path, reader)
	require.NoError(t, err)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	return req
}

// nostrSignedHeader signs a NIP-98 event for the given request and returns the header value.
//
// Every call produces a distinct event, because created_at moves and, when it does not, the nonce
// tag does: the server spends each event id exactly once, so two tests that signed byte-identical
// events would see the second one rejected as a replay for reasons that have nothing to do with
// what they are testing. The nonce is also mandatory - VerifyCredential refuses an event without
// one - so passing an empty one is how a test asks for that refusal.
func nostrSignedHeader(t *testing.T, secretKey, method, rawURL, body string) string {
	t.Helper()
	return nostrSignedHeaderNonced(t, secretKey, method, rawURL, body, t.Name())
}

func nostrSignedHeaderNonced(t *testing.T, secretKey, method, rawURL, body, nonce string) string {
	t.Helper()
	tags := nostr.Tags{
		nostr.Tag{"u", rawURL},
		nostr.Tag{"method", strings.ToUpper(method)},
	}
	if body != "" {
		sum := sha256.Sum256([]byte(body))
		tags = append(tags, nostr.Tag{"payload", hex.EncodeToString(sum[:])})
	}
	if nonce != "" {
		tags = append(tags, nostr.Tag{"nonce", nonce})
	}
	event := nostr.Event{Kind: agentauth.EventKind, CreatedAt: nostr.Now(), Tags: tags}
	require.NoError(t, event.Sign(secretKey))
	raw, err := json.Marshal(event)
	require.NoError(t, err)
	return "Nostr " + base64.StdEncoding.EncodeToString(raw)
}

// TestNostrVerifyNotApplicable pins the invariant the whole feature rests on: a request that
// does not carry a NIP-98 credential must leave this method with (nil, nil), so PAT, OAuth2 and
// Basic behave exactly as they did before. Any error here would shadow them all.
func TestNostrVerifyNotApplicable(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())

	cases := []struct {
		name   string
		path   string
		header string
	}{
		{name: "no authorization header", path: "/api/v1/version"},
		{name: "personal access token", path: "/api/v1/version", header: "token 0123456789abcdef"},
		{name: "basic auth", path: "/api/v1/version", header: "Basic dXNlcjpwYXNz"},
		{name: "bearer token", path: "/api/v1/version", header: "Bearer 0123456789abcdef"},
		{name: "scheme with no credential", path: "/api/v1/version", header: "Nostr"},
		{name: "nostr credential outside the api", path: "/user/login", header: "Nostr eyJ9"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := nostrTestRequest(t, "GET", tc.path, "", tc.header)
			store := reqctx.ContextData{}

			u, err := (&Nostr{}).Verify(req, nil, store, nil)

			assert.NoError(t, err)
			assert.Nil(t, u)
			assert.Empty(t, store)
		})
	}
}

// nostrTestFixture registers an agent key with a given scope and returns it.
func nostrTestFixture(t *testing.T, scope auth_model.AccessTokenScope) (*user_model.User, *agent_model.Key) {
	t.Helper()
	ctx := t.Context()

	agentUser := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})
	agentUser.IsAgent = true
	require.NoError(t, user_model.UpdateUserCols(ctx, agentUser, "is_agent"))

	pubKeyHex, err := nostr.PubKeyFromSecretKey(nostrTestSecretKey)
	require.NoError(t, err)
	pubKey, npub, err := agent_model.PubKeyFromInput(pubKeyHex)
	require.NoError(t, err)

	key := &agent_model.Key{
		OwnerUserID: agentUser.ID,
		AgentUserID: agentUser.ID,
		PubKey:      pubKey,
		Npub:        npub,
		Scope:       scope,
	}
	require.NoError(t, agent_model.RegisterKey(ctx, key))
	return agentUser, key
}

func TestNostrVerify(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	defer test.MockVariableValue(&setting.AppURL, "http://localhost:3000/")()

	ctx := t.Context()
	agentUser, key := nostrTestFixture(t, auth_model.AccessTokenScopeWriteIssue)

	const path = "/api/v1/repos/user2/repo1/issues"
	// Deliberately not JSON: the `payload` tag commits to the exact bytes, and nothing on the
	// authentication path may parse or re-encode them.
	const body = "raw agent payload, byte-for-byte"
	fullURL := "http://localhost:3000" + path

	t.Run("accepted signed request writes exactly one audit row", func(t *testing.T) {
		before, err := db.GetEngine(ctx).Count(&agent_model.AuditEvent{})
		require.NoError(t, err)

		req := nostrTestRequest(t, "POST", path, body, nostrSignedHeader(t, nostrTestSecretKey, "POST", fullURL, body))
		store := reqctx.ContextData{}

		u, err := (&Nostr{}).Verify(req, nil, store, nil)
		require.NoError(t, err)
		require.NotNil(t, u)
		assert.Equal(t, agentUser.ID, u.ID)
		assert.Equal(t, NostrMethodName, store["LoginMethod"])

		after, err := db.GetEngine(ctx).Count(&agent_model.AuditEvent{})
		require.NoError(t, err)
		assert.Equal(t, before+1, after)

		events, total, err := agent_model.FindAuditEvents(ctx, agent_model.FindAuditEventsOptions{
			ListOptions: db.ListOptionsAll,
			AgentUserID: agentUser.ID,
		})
		require.NoError(t, err)
		require.NotZero(t, total)
		latest := events[0]
		assert.Equal(t, "POST", latest.Method)
		assert.Equal(t, key.ID, latest.AgentKeyID)
		assert.Equal(t, key.PubKey, latest.PubKey)
		// The repository and the outcome are attached later, by the middleware in
		// routers/api/v1, once the handler has actually run.
		assert.Zero(t, latest.ResponseStatus, "authentication must not claim an outcome")

		// The handler downstream still needs the body the verifier hashed.
		got, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		assert.Equal(t, body, string(got))
	})

	// Scope enforcement lives in routers/api/v1's tokenRequiresScopes, and it only constrains a
	// request that arrives carrying a scope. If these two keys are missing, every scope check in
	// the API silently passes for NIP-98 - which would make a signed request strictly more
	// powerful than any personal access token of the same user.
	t.Run("the key's scope is presented the way a token's is", func(t *testing.T) {
		req := nostrTestRequest(t, "POST", path, body, nostrSignedHeader(t, nostrTestSecretKey, "POST", fullURL, body))
		store := reqctx.ContextData{}

		u, err := (&Nostr{}).Verify(req, nil, store, nil)
		require.NoError(t, err)
		require.NotNil(t, u)

		assert.Equal(t, true, store["IsApiToken"], "tokenRequiresScopes early-returns without this")
		scope, ok := store["ApiTokenScope"].(auth_model.AccessTokenScope)
		require.True(t, ok, "ApiTokenScope must be an auth_model.AccessTokenScope, got %T", store["ApiTokenScope"])
		assert.Equal(t, key.Scope, scope)

		// And the scope really is the narrow one the key was registered with, not "all".
		allowed, err := scope.HasScope(auth_model.AccessTokenScopeWriteIssue)
		require.NoError(t, err)
		assert.True(t, allowed)
		allowed, err = scope.HasScope(auth_model.AccessTokenScopeWriteAdmin)
		require.NoError(t, err)
		assert.False(t, allowed, "a write:issue key must not satisfy an admin scope check")
	})

	// The half of replay protection that the URL binding cannot provide: the identical request,
	// sent twice, inside the freshness window.
	t.Run("the same event replayed verbatim is rejected", func(t *testing.T) {
		header := nostrSignedHeaderNonced(t, nostrTestSecretKey, "POST", fullURL, body, "verbatim-replay")

		first := nostrTestRequest(t, "POST", path, body, header)
		u, err := (&Nostr{}).Verify(first, nil, reqctx.ContextData{}, nil)
		require.NoError(t, err)
		require.NotNil(t, u)

		before, err := db.GetEngine(ctx).Count(&agent_model.AuditEvent{})
		require.NoError(t, err)

		second := nostrTestRequest(t, "POST", path, body, header)
		u, err = (&Nostr{}).Verify(second, nil, reqctx.ContextData{}, nil)
		assert.Error(t, err, "an event id must only ever be spent once")
		assert.Nil(t, u)

		after, err := db.GetEngine(ctx).Count(&agent_model.AuditEvent{})
		require.NoError(t, err)
		assert.Equal(t, before, after, "a replay must not append to the audit trail either")
	})

	t.Run("replay at a different url is rejected and writes no audit row", func(t *testing.T) {
		before, err := db.GetEngine(ctx).Count(&agent_model.AuditEvent{})
		require.NoError(t, err)

		header := nostrSignedHeader(t, nostrTestSecretKey, "POST", fullURL, body)
		req := nostrTestRequest(t, "POST", "/api/v1/repos/user2/repo2/issues", body, header)

		u, err := (&Nostr{}).Verify(req, nil, reqctx.ContextData{}, nil)
		assert.Error(t, err)
		assert.Nil(t, u)

		after, err := db.GetEngine(ctx).Count(&agent_model.AuditEvent{})
		require.NoError(t, err)
		assert.Equal(t, before, after)
	})

	t.Run("unregistered key is rejected and writes no audit row", func(t *testing.T) {
		before, err := db.GetEngine(ctx).Count(&agent_model.AuditEvent{})
		require.NoError(t, err)

		const otherSecret = "0000000000000000000000000000000000000000000000000000000000000009"
		req := nostrTestRequest(t, "POST", path, body, nostrSignedHeader(t, otherSecret, "POST", fullURL, body))

		u, err := (&Nostr{}).Verify(req, nil, reqctx.ContextData{}, nil)
		assert.Error(t, err)
		assert.Nil(t, u)

		after, err := db.GetEngine(ctx).Count(&agent_model.AuditEvent{})
		require.NoError(t, err)
		assert.Equal(t, before, after)
	})

	t.Run("revoked key is rejected and writes no audit row", func(t *testing.T) {
		require.NoError(t, agent_model.RevokeKey(ctx, key.ID))
		defer func() {
			_, err := db.GetEngine(ctx).ID(key.ID).Cols("revoked_unix").Update(&agent_model.Key{})
			require.NoError(t, err)
		}()

		before, err := db.GetEngine(ctx).Count(&agent_model.AuditEvent{})
		require.NoError(t, err)

		req := nostrTestRequest(t, "POST", path, body, nostrSignedHeader(t, nostrTestSecretKey, "POST", fullURL, body))

		u, err := (&Nostr{}).Verify(req, nil, reqctx.ContextData{}, nil)
		assert.Error(t, err)
		assert.Nil(t, u)

		after, err := db.GetEngine(ctx).Count(&agent_model.AuditEvent{})
		require.NoError(t, err)
		assert.Equal(t, before, after)
	})

	// A scopeless key cannot be created through the API, but a row could still arrive from a
	// hand-edited database or a future migration bug. It must not authenticate: with no scope
	// every tokenRequiresScopes guard in the API would wave it through.
	t.Run("a key with no scope cannot authenticate", func(t *testing.T) {
		_, err := db.GetEngine(ctx).ID(key.ID).Cols("scope").Update(&agent_model.Key{Scope: ""})
		require.NoError(t, err)
		defer func() {
			_, err := db.GetEngine(ctx).ID(key.ID).Cols("scope").
				Update(&agent_model.Key{Scope: auth_model.AccessTokenScopeWriteIssue})
			require.NoError(t, err)
		}()

		req := nostrTestRequest(t, "POST", path, body, nostrSignedHeader(t, nostrTestSecretKey, "POST", fullURL, body))
		u, err := (&Nostr{}).Verify(req, nil, reqctx.ContextData{}, nil)
		assert.Error(t, err)
		assert.Nil(t, u)
	})

	t.Run("a user without the agent flag cannot authenticate", func(t *testing.T) {
		agentUser.IsAgent = false
		require.NoError(t, user_model.UpdateUserCols(ctx, agentUser, "is_agent"))
		defer func() {
			agentUser.IsAgent = true
			require.NoError(t, user_model.UpdateUserCols(ctx, agentUser, "is_agent"))
		}()

		req := nostrTestRequest(t, "POST", path, body, nostrSignedHeader(t, nostrTestSecretKey, "POST", fullURL, body))

		u, err := (&Nostr{}).Verify(req, nil, reqctx.ContextData{}, nil)
		assert.Error(t, err)
		assert.Nil(t, u)
	})

	// A GET leaves a trail too. "What did the agent read" is part of an audit trail, and the
	// table is documented as recording every authenticated signed request rather than only the
	// mutating ones - so pin that, in the direction the documentation claims.
	t.Run("a signed read is recorded as well", func(t *testing.T) {
		before, err := db.GetEngine(ctx).Count(&agent_model.AuditEvent{})
		require.NoError(t, err)

		const readPath = "/api/v1/repos/user2/repo1/issues"
		req := nostrTestRequest(t, "GET", readPath, "", nostrSignedHeader(t, nostrTestSecretKey, "GET", "http://localhost:3000"+readPath, ""))

		u, err := (&Nostr{}).Verify(req, nil, reqctx.ContextData{}, nil)
		require.NoError(t, err)
		require.NotNil(t, u)

		after, err := db.GetEngine(ctx).Count(&agent_model.AuditEvent{})
		require.NoError(t, err)
		assert.Equal(t, before+1, after)

		events, _, err := agent_model.FindAuditEvents(ctx, agent_model.FindAuditEventsOptions{
			ListOptions: db.ListOptionsAll,
			AgentUserID: agentUser.ID,
		})
		require.NoError(t, err)
		assert.Equal(t, "GET", events[0].Method)
		assert.Empty(t, events[0].PayloadHash)
	})
}

// TestNostrVerifyOwnerIsNotTheAgent exercises the configuration the whole design rests on and
// which every other test here happens to avoid: the human owner and the agent are *different*
// accounts. When they are the same user - as they are in the fixture above - the human's own
// account is the one that gets IsAgent and becomes NIP-98-authenticable, which is the opposite of
// the containment the feature is for. Only the refusal to bind someone else's account was pinned;
// this pins the case that is supposed to work.
func TestNostrVerifyOwnerIsNotTheAgent(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	defer test.MockVariableValue(&setting.AppURL, "http://localhost:3000/")()

	ctx := t.Context()

	// user1 (a site administrator in the fixtures) vouches for user4, who does the signing.
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 1})
	agentUser := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 4})
	agentUser.IsAgent = true
	require.NoError(t, user_model.UpdateUserCols(ctx, agentUser, "is_agent"))

	const secretKey = "000000000000000000000000000000000000000000000000000000000000001f"
	pubKeyHex, err := nostr.PubKeyFromSecretKey(secretKey)
	require.NoError(t, err)
	pubKey, npub, err := agent_model.PubKeyFromInput(pubKeyHex)
	require.NoError(t, err)

	key := &agent_model.Key{
		OwnerUserID: owner.ID,
		AgentUserID: agentUser.ID,
		PubKey:      pubKey,
		Npub:        npub,
		Scope:       auth_model.AccessTokenScopeWriteIssue,
	}
	require.NoError(t, agent_model.RegisterKey(ctx, key))
	require.NotEqual(t, key.OwnerUserID, key.AgentUserID, "this test is meaningless if they are equal")

	const path = "/api/v1/repos/user2/repo1/issues"
	const body = "signed by an agent its owner does not share an account with"
	req := nostrTestRequest(t, "POST", path, body,
		nostrSignedHeaderNonced(t, secretKey, "POST", "http://localhost:3000"+path, body, "split-ownership"))

	u, err := (&Nostr{}).Verify(req, nil, reqctx.ContextData{}, nil)
	require.NoError(t, err)
	require.NotNil(t, u)

	// The request is authenticated as the *agent*, never as the human who vouched for it. If
	// this were the owner, one registration would hand the agent its owner's permissions.
	assert.Equal(t, agentUser.ID, u.ID)
	assert.NotEqual(t, owner.ID, u.ID)

	// And the audit row records both ends of the chain, which is the only thing that makes the
	// trail answer "who is accountable for this" rather than just "which account acted".
	events, _, err := agent_model.FindAuditEvents(ctx, agent_model.FindAuditEventsOptions{
		ListOptions: db.ListOptionsAll,
		AgentUserID: agentUser.ID,
	})
	require.NoError(t, err)
	require.NotEmpty(t, events)
	assert.Equal(t, agentUser.ID, events[0].AgentUserID)
	assert.Equal(t, owner.ID, events[0].OwnerUserID, "the trail must name the human who vouched")
	assert.Equal(t, key.ID, events[0].AgentKeyID)
}

func TestExpectedRequestURL(t *testing.T) {
	defer test.MockVariableValue(&setting.AppURL, "https://gitea.example.com/")()

	req := nostrTestRequest(t, "GET", "/api/v1/version?x=1", "", "")
	// The Host header is attacker-controlled, so it must not influence the expected URL.
	req.Host = "evil.example.net"

	assert.Equal(t, "https://gitea.example.com/api/v1/version?x=1", expectedRequestURL(req))
}

// A sub-path deployment is the case that quietly breaks if only the scheme and host are taken
// from AppURL: modules/web/router.go has already stripped AppSubURL from req.URL.Path by the time
// this runs, so the request contributes "/api/v1/…" and the prefix can only come from AppURL. Get
// this wrong and NIP-98 can never authenticate on such an instance - the client signs the URL it
// posted to, the server computes one without the sub-path, and every request 401s with the
// deliberately opaque message.
func TestExpectedRequestURLSubPath(t *testing.T) {
	defer test.MockVariableValue(&setting.AppURL, "https://git.example/gitea/")()

	req := nostrTestRequest(t, "POST", "/api/v1/repos/user2/repo1/issues", "", "")
	req.Host = "evil.example.net"

	assert.Equal(t, "https://git.example/gitea/api/v1/repos/user2/repo1/issues", expectedRequestURL(req))
}
