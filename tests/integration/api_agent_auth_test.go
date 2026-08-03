// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	agent_model "code.gitea.io/gitea/models/agent"
	auth_model "code.gitea.io/gitea/models/auth"
	"code.gitea.io/gitea/models/organization"
	"code.gitea.io/gitea/models/perm"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/unittest"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/json"
	"code.gitea.io/gitea/modules/nostr"
	"code.gitea.io/gitea/modules/setting"
	api "code.gitea.io/gitea/modules/structs"
	"code.gitea.io/gitea/modules/test"
	"code.gitea.io/gitea/services/agentauth"
	"code.gitea.io/gitea/tests"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A fixed keypair; a test vector, never to be used for anything real.
const agentTestSecretKey = "000000000000000000000000000000000000000000000000000000000000000b"

// signNIP98 builds an `Authorization: Nostr ...` value for one request.
//
// The nonce tag makes every credential distinct even when the method, URL, body and second are
// identical. The server spends each event id exactly once, so without it two tests that signed
// the same request would collide with each other's replay guard rather than testing what they
// mean to test. It is also mandatory - the server refuses an event that carries no nonce - so a
// test passing an empty one is asking for that refusal.
func signNIP98(t *testing.T, secretKey, method, path, body string, createdAt time.Time, nonce string) string {
	t.Helper()

	base := strings.TrimSuffix(setting.AppURL, "/")
	tags := nostr.Tags{
		nostr.Tag{"u", base + path},
		nostr.Tag{"method", strings.ToUpper(method)},
	}
	if body != "" {
		sum := sha256.Sum256([]byte(body))
		tags = append(tags, nostr.Tag{"payload", hex.EncodeToString(sum[:])})
	}
	if nonce != "" {
		tags = append(tags, nostr.Tag{"nonce", nonce})
	}

	event := nostr.Event{
		Kind:      agentauth.EventKind,
		CreatedAt: nostr.Timestamp(createdAt.Unix()),
		Tags:      tags,
	}
	require.NoError(t, event.Sign(secretKey))

	raw, err := json.Marshal(event)
	require.NoError(t, err)
	return "Nostr " + base64.StdEncoding.EncodeToString(raw)
}

// agentPubKey derives the x-only public key of a secret key.
func agentPubKey(t *testing.T, secretKey string) string {
	t.Helper()
	pubKey, err := nostr.PubKeyFromSecretKey(secretKey)
	require.NoError(t, err)
	return pubKey
}

// registerAgentKey enrolls a Nostr key for the calling user via the public API.
func registerAgentKey(t *testing.T, token, pubKey string, scopes ...string) *api.AgentKey {
	t.Helper()
	return registerAgentKeyFor(t, token, pubKey, 0, scopes...)
}

// registerAgentKeyFor enrolls a Nostr key naming an explicit agent user. An agentUserID of 0
// means "the caller", which is what the API defaults to.
func registerAgentKeyFor(t *testing.T, token, pubKey string, agentUserID int64, scopes ...string) *api.AgentKey {
	t.Helper()

	req := NewRequestWithJSON(t, "POST", "/api/v1/agent/keys", &api.CreateAgentKeyOption{
		PublicKey:   pubKey,
		Scopes:      scopes,
		AgentUserID: agentUserID,
	}).AddTokenAuth(token)
	resp := MakeRequest(t, req, http.StatusCreated)

	key := new(api.AgentKey)
	DecodeJSON(t, resp, key)
	return key
}

func TestAPIAgentNIP98Auth(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	token := getUserToken(t, "user2", auth_model.AccessTokenScopeWriteUser, auth_model.AccessTokenScopeWriteIssue, auth_model.AccessTokenScopeWriteRepository)
	pubKey := agentPubKey(t, agentTestSecretKey)

	key := registerAgentKey(t, token, pubKey, string(auth_model.AccessTokenScopeWriteIssue))
	assert.Equal(t, pubKey, key.PublicKey)
	assert.True(t, strings.HasPrefix(key.Npub, "npub1"), "npub was %q", key.Npub)
	assert.Nil(t, key.Revoked)
	assert.Equal(t, []string{string(auth_model.AccessTokenScopeWriteIssue)}, key.Scopes)

	const issuePath = "/api/v1/repos/user2/repo1/issues"
	auditPath := "/api/v1/repos/user2/repo1/agent-audit"

	t.Run("a signed mutation is accepted", func(t *testing.T) {
		body := `{"title":"signed by an agent"}`
		req := NewRequestWithBody(t, "POST", issuePath, strings.NewReader(body)).
			SetHeader("Content-Type", "application/json").
			SetHeader("Authorization", signNIP98(t, agentTestSecretKey, "POST", issuePath, body, time.Now(), "accepted"))
		resp := MakeRequest(t, req, http.StatusCreated)

		issue := new(api.Issue)
		DecodeJSON(t, resp, issue)
		assert.Equal(t, "signed by an agent", issue.Title)
	})

	t.Run("the audit trail records it, repo-scoped, with the outcome", func(t *testing.T) {
		req := NewRequest(t, "GET", auditPath).AddTokenAuth(token)
		resp := MakeRequest(t, req, http.StatusOK)

		var events []*api.AgentAuditEvent
		DecodeJSON(t, resp, &events)
		require.NotEmpty(t, events)

		event := events[0]
		assert.Equal(t, "POST", event.Method)
		assert.Equal(t, pubKey, event.PublicKey)
		assert.Equal(t, key.ID, event.AgentKeyID)
		assert.NotEmpty(t, event.PayloadHash)
		assert.True(t, strings.HasSuffix(event.RequestURL, issuePath), "request url was %q", event.RequestURL)
		// The row records what actually happened, not merely that a credential was accepted.
		assert.Equal(t, http.StatusCreated, event.ResponseStatus)

		// Another repository must not see this repository's rows.
		req = NewRequest(t, "GET", "/api/v1/repos/user2/repo2/agent-audit").AddTokenAuth(token)
		resp = MakeRequest(t, req, http.StatusOK)
		var otherEvents []*api.AgentAuditEvent
		DecodeJSON(t, resp, &otherEvents)
		for _, other := range otherEvents {
			assert.NotEqual(t, event.ID, other.ID)
		}
	})

	// The half of replay protection the URL binding cannot give: the identical request, sent
	// twice, well inside the freshness window.
	t.Run("the identical request replayed verbatim is rejected", func(t *testing.T) {
		body := `{"title":"sent exactly twice"}`
		header := signNIP98(t, agentTestSecretKey, "POST", issuePath, body, time.Now(), "verbatim")

		send := func() *RequestWrapper {
			return NewRequestWithBody(t, "POST", issuePath, strings.NewReader(body)).
				SetHeader("Content-Type", "application/json").
				SetHeader("Authorization", header)
		}
		MakeRequest(t, send(), http.StatusCreated)

		rowsAfterFirst := unittest.GetCount(t, &agent_model.AuditEvent{})
		MakeRequest(t, send(), http.StatusUnauthorized)
		assert.Equal(t, rowsAfterFirst, unittest.GetCount(t, &agent_model.AuditEvent{}),
			"a replayed event must leave no audit row")

		// And the replay must not have created a second issue.
		req := NewRequest(t, "GET", issuePath+"?state=all").AddTokenAuth(token)
		resp := MakeRequest(t, req, http.StatusOK)
		var issues []*api.Issue
		DecodeJSON(t, resp, &issues)
		matches := 0
		for _, issue := range issues {
			if issue.Title == "sent exactly twice" {
				matches++
			}
		}
		assert.Equal(t, 1, matches, "the replayed mutation happened twice")
	})

	// The audit row is written *inside* authentication, so a credential that was refused must
	// never have reached it. Counted across the whole block of rejections below rather than
	// asserted per case, because the regression this guards against - a reordering that starts
	// recording attempts - would show up as a row from any one of them.
	rowsBeforeRejections := unittest.GetCount(t, &agent_model.AuditEvent{})

	t.Run("replaying the event at a different url is rejected", func(t *testing.T) {
		body := `{"title":"replayed"}`
		header := signNIP98(t, agentTestSecretKey, "POST", issuePath, body, time.Now(), "other-url")

		req := NewRequestWithBody(t, "POST", "/api/v1/repos/user2/repo2/issues", strings.NewReader(body)).
			SetHeader("Content-Type", "application/json").
			SetHeader("Authorization", header)
		MakeRequest(t, req, http.StatusUnauthorized)
	})

	t.Run("a tampered body is rejected", func(t *testing.T) {
		header := signNIP98(t, agentTestSecretKey, "POST", issuePath, `{"title":"one thing"}`, time.Now(), "tampered")

		req := NewRequestWithBody(t, "POST", issuePath, strings.NewReader(`{"title":"quite another"}`)).
			SetHeader("Content-Type", "application/json").
			SetHeader("Authorization", header)
		MakeRequest(t, req, http.StatusUnauthorized)
	})

	t.Run("a tampered signature is rejected", func(t *testing.T) {
		body := `{"title":"bad signature"}`
		header := signNIP98(t, agentTestSecretKey, "POST", issuePath, body, time.Now(), "bad-sig")

		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, "Nostr "))
		require.NoError(t, err)
		event := new(nostr.Event)
		require.NoError(t, json.Unmarshal(raw, event))

		// Flip one hex digit of the signature and nothing else: the id still matches the
		// serialization, so this isolates the signature check from every other one.
		if event.Sig[0] == '0' {
			event.Sig = "1" + event.Sig[1:]
		} else {
			event.Sig = "0" + event.Sig[1:]
		}
		raw, err = json.Marshal(event)
		require.NoError(t, err)

		req := NewRequestWithBody(t, "POST", issuePath, strings.NewReader(body)).
			SetHeader("Content-Type", "application/json").
			SetHeader("Authorization", "Nostr "+base64.StdEncoding.EncodeToString(raw))
		MakeRequest(t, req, http.StatusUnauthorized)
	})

	t.Run("a stale event is rejected", func(t *testing.T) {
		body := `{"title":"stale"}`
		stale := time.Now().Add(-agentauth.DefaultClockSkew - time.Minute)

		req := NewRequestWithBody(t, "POST", issuePath, strings.NewReader(body)).
			SetHeader("Content-Type", "application/json").
			SetHeader("Authorization", signNIP98(t, agentTestSecretKey, "POST", issuePath, body, stale, "stale"))
		MakeRequest(t, req, http.StatusUnauthorized)
	})

	t.Run("an unregistered key is rejected", func(t *testing.T) {
		const strangerKey = "000000000000000000000000000000000000000000000000000000000000000d"
		body := `{"title":"stranger"}`

		req := NewRequestWithBody(t, "POST", issuePath, strings.NewReader(body)).
			SetHeader("Content-Type", "application/json").
			SetHeader("Authorization", signNIP98(t, strangerKey, "POST", issuePath, body, time.Now(), "stranger"))
		MakeRequest(t, req, http.StatusUnauthorized)
	})

	t.Run("none of the rejections wrote an audit row", func(t *testing.T) {
		assert.Equal(t, rowsBeforeRejections, unittest.GetCount(t, &agent_model.AuditEvent{}),
			"a refused credential must leave no trace an attacker chose")
	})

	t.Run("token auth still works untouched", func(t *testing.T) {
		req := NewRequestWithJSON(t, "POST", issuePath, &api.CreateIssueOption{
			Title: "created with a token",
		}).AddTokenAuth(token)
		MakeRequest(t, req, http.StatusCreated)
	})
}

// The scope on the key has to bind, or a signed request would be able to do anything its user
// can - which is strictly more than the same user's personal access token may do, and the exact
// opposite of the "an agent's permissions are its user's" rule the design states.
func TestAPIAgentKeyScopeIsEnforced(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	token := getUserToken(t, "user2", auth_model.AccessTokenScopeWriteUser, auth_model.AccessTokenScopeWriteIssue)
	pubKey := agentPubKey(t, "0000000000000000000000000000000000000000000000000000000000000013")

	// A key that may only write issues.
	registerAgentKey(t, token, pubKey, string(auth_model.AccessTokenScopeWriteIssue))

	const issuePath = "/api/v1/repos/user2/repo1/issues"
	body := `{"title":"within scope"}`
	req := NewRequestWithBody(t, "POST", issuePath, strings.NewReader(body)).
		SetHeader("Content-Type", "application/json").
		SetHeader("Authorization", signNIP98(t, "0000000000000000000000000000000000000000000000000000000000000013", "POST", issuePath, body, time.Now(), "in-scope"))
	MakeRequest(t, req, http.StatusCreated)

	// The same key, same user, aimed at an endpoint the scope does not cover.
	const userPath = "/api/v1/user/emails"
	outOfScope := `{"emails":["agent@example.com"]}`
	req = NewRequestWithBody(t, "POST", userPath, strings.NewReader(outOfScope)).
		SetHeader("Content-Type", "application/json").
		SetHeader("Authorization", signNIP98(t, "0000000000000000000000000000000000000000000000000000000000000013", "POST", userPath, outOfScope, time.Now(), "out-of-scope"))
	MakeRequest(t, req, http.StatusForbidden)
}

// Enrolling a signing key is a human act. If an agent could enrol keys with the key it already
// holds, one leaked key would let its holder mint unlimited siblings and revoking the leaked one
// would accomplish nothing.
func TestAPIAgentCannotEnrolOrRevokeKeysWithItsOwnSignature(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	const secretKey = "0000000000000000000000000000000000000000000000000000000000000015"
	token := getUserToken(t, "user2", auth_model.AccessTokenScopeWriteUser)
	key := registerAgentKey(t, token, agentPubKey(t, secretKey), string(auth_model.AccessTokenScopeWriteUser))

	t.Run("enrolling a sibling key", func(t *testing.T) {
		const path = "/api/v1/agent/keys"
		sibling := agentPubKey(t, "0000000000000000000000000000000000000000000000000000000000000017")
		body := `{"public_key":"` + sibling + `","scopes":["write:user"]}`

		req := NewRequestWithBody(t, "POST", path, strings.NewReader(body)).
			SetHeader("Content-Type", "application/json").
			SetHeader("Authorization", signNIP98(t, secretKey, "POST", path, body, time.Now(), "self-enrol"))
		MakeRequest(t, req, http.StatusForbidden)
	})

	t.Run("revoking the key an operator is using to contain it", func(t *testing.T) {
		path := "/api/v1/agent/keys/" + strconv.FormatInt(key.ID, 10)

		req := NewRequest(t, "DELETE", path).
			SetHeader("Authorization", signNIP98(t, secretKey, "DELETE", path, "", time.Now(), "self-revoke"))
		MakeRequest(t, req, http.StatusForbidden)
	})

	t.Run("listing keys", func(t *testing.T) {
		const path = "/api/v1/agent/keys"

		req := NewRequest(t, "GET", path).
			SetHeader("Authorization", signNIP98(t, secretKey, "GET", path, "", time.Now(), "self-list"))
		MakeRequest(t, req, http.StatusForbidden)
	})
}

// A credential may only delegate what it already holds.
//
// TestAPIAgentKeyScopeIsEnforced pins that a key's scope constrains the key; this pins the other
// half, that the *creation* of the key is itself constrained. Without it POST /agent/keys is a
// scope-escalation primitive: a personal access token restricted to write:user could mint a
// signing credential for the same account carrying write:repository, and the token would have
// escalated itself. Gitea has already made this call for its own token endpoint, which sits
// behind reqBasicOrRevProxyAuth() so that a token cannot mint a token.
func TestAPIAgentKeyCannotExceedTheCallersScope(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	// Deliberately narrow: enough to reach the endpoint, nothing more.
	token := getUserToken(t, "user2", auth_model.AccessTokenScopeWriteUser)

	t.Run("a scope the caller does not hold is refused", func(t *testing.T) {
		req := NewRequestWithJSON(t, "POST", "/api/v1/agent/keys", &api.CreateAgentKeyOption{
			PublicKey: agentPubKey(t, "000000000000000000000000000000000000000000000000000000000000002b"),
			Scopes: []string{
				string(auth_model.AccessTokenScopeWriteUser),
				string(auth_model.AccessTokenScopeWriteRepository),
			},
		}).AddTokenAuth(token)
		MakeRequest(t, req, http.StatusForbidden)
	})

	t.Run("a read scope does not authorize minting the matching write scope", func(t *testing.T) {
		readToken := getUserToken(t, "user2", auth_model.AccessTokenScopeReadUser)
		req := NewRequestWithJSON(t, "POST", "/api/v1/agent/keys", &api.CreateAgentKeyOption{
			PublicKey: agentPubKey(t, "000000000000000000000000000000000000000000000000000000000000002d"),
			Scopes:    []string{string(auth_model.AccessTokenScopeWriteUser)},
		}).AddTokenAuth(readToken)
		MakeRequest(t, req, http.StatusForbidden)
	})

	// The check constrains delegation, not registration: what the caller does hold still works,
	// and so does anything narrower.
	t.Run("what the caller holds is still delegable", func(t *testing.T) {
		key := registerAgentKey(t, token, agentPubKey(t, "000000000000000000000000000000000000000000000000000000000000002f"),
			string(auth_model.AccessTokenScopeWriteUser))
		assert.Equal(t, []string{string(auth_model.AccessTokenScopeWriteUser)}, key.Scopes)

		narrower := registerAgentKey(t, token, agentPubKey(t, "0000000000000000000000000000000000000000000000000000000000000031"),
			string(auth_model.AccessTokenScopeReadUser))
		assert.Equal(t, []string{string(auth_model.AccessTokenScopeReadUser)}, narrower.Scopes)
	})
}

// reqHumanAuth() has to cover every endpoint that plants a credential outliving the signing key,
// not just the ones that look like key management. An Actions secret is handed to every later
// workflow run for the account, and a runner registration token yields a second, independent
// credential that keeps working after the Nostr key is revoked - in both cases revocation would
// be a half-undo.
//
// The pre-existing coverage tested the /agent group only, which is why these two were missed.
func TestAPIAgentCannotPlantCredentialsUnderUserActions(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	const secretKey = "0000000000000000000000000000000000000000000000000000000000000033"
	token := getUserToken(t, "user2", auth_model.AccessTokenScopeWriteUser)
	// write:user is exactly the scope that reaches these endpoints, so the refusals below are
	// the guard's doing and not the scope's.
	registerAgentKey(t, token, agentPubKey(t, secretKey), string(auth_model.AccessTokenScopeWriteUser))

	t.Run("planting a user-level Actions secret", func(t *testing.T) {
		const path = "/api/v1/user/actions/secrets/AGENT_PLANTED"
		body := `{"data":"a value every later workflow run would receive"}`
		req := NewRequestWithBody(t, "PUT", path, strings.NewReader(body)).
			SetHeader("Content-Type", "application/json").
			SetHeader("Authorization", signNIP98(t, secretKey, "PUT", path, body, time.Now(), "plant-secret"))
		MakeRequest(t, req, http.StatusForbidden)
	})

	t.Run("minting a runner registration token", func(t *testing.T) {
		const path = "/api/v1/user/actions/runners/registration-token"
		req := NewRequestWithBody(t, "POST", path, strings.NewReader("")).
			SetHeader("Authorization", signNIP98(t, secretKey, "POST", path, "", time.Now(), "mint-runner-token"))
		MakeRequest(t, req, http.StatusForbidden)
	})

	// Reading the current registration token is as good as minting one: the value is what
	// registers a runner, so the GET is guarded for the same reason as the POST.
	t.Run("reading a runner registration token", func(t *testing.T) {
		const path = "/api/v1/user/actions/runners/registration-token"
		req := NewRequest(t, "GET", path).
			SetHeader("Authorization", signNIP98(t, secretKey, "GET", path, "", time.Now(), "read-runner-token"))
		MakeRequest(t, req, http.StatusForbidden)
	})

	// Writing a user-level Actions variable is the same act as writing a secret, one confidence
	// level down: every later workflow run for the account reads it, and revoking the Nostr key
	// does not take it back out.
	t.Run("planting a user-level Actions variable", func(t *testing.T) {
		const path = "/api/v1/user/actions/variables/AGENT_PLANTED"
		body := `{"value":"a value every later workflow run would receive"}`
		req := NewRequestWithBody(t, "PUT", path, strings.NewReader(body)).
			SetHeader("Content-Type", "application/json").
			SetHeader("Authorization", signNIP98(t, secretKey, "PUT", path, body, time.Now(), "plant-variable"))
		MakeRequest(t, req, http.StatusForbidden)
	})

	// Reading one leaves nothing behind that outlives revocation, so it is deliberately still
	// reachable. Asserting it pins where the line is: this guard is about persistence, and a
	// later widening that swallowed the reads too would be a different decision, not a tidy-up.
	t.Run("reading Actions variables is still allowed", func(t *testing.T) {
		const path = "/api/v1/user/actions/variables"
		req := NewRequest(t, "GET", path).
			SetHeader("Authorization", signNIP98(t, secretKey, "GET", path, "", time.Now(), "read-variables"))
		MakeRequest(t, req, http.StatusOK)
	})

	// The guard is about the credential in the request, not about the endpoint being off
	// limits: the human holding a token still gets through.
	t.Run("the owner's own credential is unaffected", func(t *testing.T) {
		req := NewRequest(t, "GET", "/api/v1/user/actions/runners/registration-token").AddTokenAuth(token)
		MakeRequest(t, req, http.StatusOK)
	})
}

// The same artifact classes are reachable one level down, and the containment has to follow them
// there. A leaked key scoped write:repository for a user with admin on a repository could install
// a deploy key - an SSH credential with write access - or plant a repository Actions secret, and
// revoking the Nostr key afterwards would remove neither.
//
// This is the half of the claim the /user-only coverage above could not make: "a correctly-scoped
// agent may do what its scope allows" would have excused /user/keys just as readily, since
// write:user legitimately covers SSH-key management too, so scope cannot be the line.
func TestAPIAgentCannotPlantCredentialsUnderRepo(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	const secretKey = "0000000000000000000000000000000000000000000000000000000000000041"
	token := getUserToken(t, "user2", auth_model.AccessTokenScopeWriteRepository)
	registerAgentKey(t, token, agentPubKey(t, secretKey), string(auth_model.AccessTokenScopeWriteRepository))

	t.Run("installing a deploy key", func(t *testing.T) {
		const path = "/api/v1/repos/user2/repo1/keys"
		body := `{"title":"planted","key":"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQDWVj0fQ5N8wNc0LVNA41wDLYJ89ZIbejrPfg/avyj7u7BG8VjS/4Q==","read_only":false}`
		req := NewRequestWithBody(t, "POST", path, strings.NewReader(body)).
			SetHeader("Content-Type", "application/json").
			SetHeader("Authorization", signNIP98(t, secretKey, "POST", path, body, time.Now(), "plant-deploy-key"))
		MakeRequest(t, req, http.StatusForbidden)
	})

	t.Run("planting a repository Actions secret", func(t *testing.T) {
		const path = "/api/v1/repos/user2/repo1/actions/secrets/AGENT_PLANTED"
		body := `{"data":"a value every later workflow run would receive"}`
		req := NewRequestWithBody(t, "PUT", path, strings.NewReader(body)).
			SetHeader("Content-Type", "application/json").
			SetHeader("Authorization", signNIP98(t, secretKey, "PUT", path, body, time.Now(), "plant-repo-secret"))
		MakeRequest(t, req, http.StatusForbidden)
	})

	t.Run("minting a repository runner registration token", func(t *testing.T) {
		const path = "/api/v1/repos/user2/repo1/actions/runners/registration-token"
		req := NewRequestWithBody(t, "POST", path, strings.NewReader("")).
			SetHeader("Authorization", signNIP98(t, secretKey, "POST", path, "", time.Now(), "mint-repo-runner-token"))
		MakeRequest(t, req, http.StatusForbidden)
	})

	// PAT parity: the guard tests how the request authenticated, so the same scope carried by a
	// token still reaches the endpoint. Without this the change would read as a scope narrowing.
	t.Run("the owner's own credential is unaffected", func(t *testing.T) {
		req := NewRequest(t, "GET", "/api/v1/repos/user2/repo1/keys").AddTokenAuth(token)
		MakeRequest(t, req, http.StatusOK)
	})
}

// A git hook is the plainest case the containment test describes: PATCH writes a shell script that
// the server runs on every later push, as its own process user, with no NIP-98 signature anywhere
// near it. Revoking the agent's key leaves the script in place and running. reqGitHook() is not the
// line - it asks whether the agent *user* may edit git hooks (IsAdmin || AllowGitHook), which is
// true of a correctly-scoped agent for an administrator, exactly as reqAdmin() is for deploy keys.
func TestAPIAgentCannotPlantAGitHook(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	// Git hooks are off by default, which is what keeps this bounded on a stock instance; the
	// guard has to hold on the instances that turn them on, which is what this pins.
	defer test.MockVariableValue(&setting.DisableGitHooks, false)()

	const secretKey = "000000000000000000000000000000000000000000000000000000000000004b"
	// user1 is the site administrator, so reqAdmin() and reqGitHook() are both satisfied and the
	// refusal below is the guard's doing. write:user is what reaches /agent/keys to enrol at all.
	token := getUserToken(t, "user1", auth_model.AccessTokenScopeWriteRepository, auth_model.AccessTokenScopeWriteUser)
	registerAgentKey(t, token, agentPubKey(t, secretKey), string(auth_model.AccessTokenScopeWriteRepository))

	const path = "/api/v1/repos/user2/repo1/hooks/git/pre-receive"
	const hook = "#!/bin/bash\necho \"planted by an agent, run on every push\"\n"

	t.Run("writing a git hook", func(t *testing.T) {
		body := `{"content":"#!/bin/bash\necho \"planted by an agent, run on every push\"\n"}`
		req := NewRequestWithBody(t, "PATCH", path, strings.NewReader(body)).
			SetHeader("Content-Type", "application/json").
			SetHeader("Authorization", signNIP98(t, secretKey, "PATCH", path, body, time.Now(), "plant-git-hook"))
		MakeRequest(t, req, http.StatusForbidden)

		// The refusal has to be a refusal: nothing may have reached the hook file, or the next
		// push would run it whatever the response said.
		req = NewRequest(t, "GET", path).AddTokenAuth(token)
		resp := MakeRequest(t, req, http.StatusOK)
		var planted *api.GitHook
		DecodeJSON(t, resp, &planted)
		assert.False(t, planted.IsActive)
		assert.Empty(t, planted.Content)
	})

	// Reading a hook plants nothing, so it stays reachable - the same line the Actions secrets
	// draw between planting and reading.
	t.Run("reading the git hooks is still allowed", func(t *testing.T) {
		const listPath = "/api/v1/repos/user2/repo1/hooks/git"
		req := NewRequest(t, "GET", listPath).
			SetHeader("Authorization", signNIP98(t, secretKey, "GET", listPath, "", time.Now(), "list-git-hooks"))
		MakeRequest(t, req, http.StatusOK)
	})

	// PAT parity, and the non-vacuity of the refusal above: the identical request carrying the
	// administrator's own token writes the hook, so the 403 was the guard and not reqGitHook().
	t.Run("the administrator's own credential still writes it", func(t *testing.T) {
		req := NewRequestWithJSON(t, "PATCH", path, &api.EditGitHookOption{Content: hook}).AddTokenAuth(token)
		resp := MakeRequest(t, req, http.StatusOK)
		var written *api.GitHook
		DecodeJSON(t, resp, &written)
		assert.True(t, written.IsActive)
		assert.Equal(t, hook, written.Content)
	})
}

// The third class the containment has to cover: granting a *different account* standing access.
// A collaborator, a team member and a team's repository all reach the granted repository through
// their own passwords, PATs and SSH keys, so revoking the agent's Nostr key takes none of it back
// - the same half-undo the credential-planting guards exist to prevent, reached without planting
// anything. reqAdmin()/reqOrgOwnership() cannot draw this line: they ask whether the agent *user*
// may grant, which a correctly-scoped agent for a repository admin or an org owner may.
//
// These assertions exist because the guards are three words on three route lines, and a route
// refactor that dropped one would otherwise be silent.
func TestAPIAgentCannotGrantStandingAccess(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	const repoSecretKey = "0000000000000000000000000000000000000000000000000000000000000043"
	repoToken := getUserToken(t, "user2", auth_model.AccessTokenScopeWriteRepository)
	registerAgentKey(t, repoToken, agentPubKey(t, repoSecretKey), string(auth_model.AccessTokenScopeWriteRepository))

	// user2 owns org3 and is a member of its teams, so every refusal below is the guard's doing
	// rather than reqOrgOwnership()'s or reqTeamMembership()'s.
	const orgSecretKey = "0000000000000000000000000000000000000000000000000000000000000045"
	orgToken := getUserToken(t, "user2", auth_model.AccessTokenScopeWriteOrganization)
	registerAgentKey(t, orgToken, agentPubKey(t, orgSecretKey), string(auth_model.AccessTokenScopeWriteOrganization))

	t.Run("adding a repository collaborator", func(t *testing.T) {
		const path = "/api/v1/repos/user2/repo1/collaborators/user4"
		body := `{"permission":"admin"}`
		req := NewRequestWithBody(t, "PUT", path, strings.NewReader(body)).
			SetHeader("Content-Type", "application/json").
			SetHeader("Authorization", signNIP98(t, repoSecretKey, "PUT", path, body, time.Now(), "grant-collaborator"))
		MakeRequest(t, req, http.StatusForbidden)
	})

	t.Run("adding a team member", func(t *testing.T) {
		const path = "/api/v1/teams/1/members/user4"
		req := NewRequestWithBody(t, "PUT", path, strings.NewReader("")).
			SetHeader("Authorization", signNIP98(t, orgSecretKey, "PUT", path, "", time.Now(), "grant-team-member"))
		MakeRequest(t, req, http.StatusForbidden)
	})

	// Adding a repository to a team is the same grant made from the other end: it hands the
	// repository to every current member of the team at once.
	t.Run("adding a repository to a team", func(t *testing.T) {
		const path = "/api/v1/teams/2/repos/org3/repo21"
		req := NewRequestWithBody(t, "PUT", path, strings.NewReader("")).
			SetHeader("Authorization", signNIP98(t, orgSecretKey, "PUT", path, "", time.Now(), "grant-team-repo"))
		MakeRequest(t, req, http.StatusForbidden)
	})

	// The same grant reached from the repository side. Both paths land in
	// repo_service.TeamAddRepository, and this one asks for less to get there - write:repository
	// plus reqAdmin() on the repository, rather than organization scope plus team membership - so
	// guarding only the /teams path would have left the grant fully open at a lower bar. org3 owns
	// repo21 and team1 does not have it yet, so this is a real grant rather than a no-op.
	t.Run("adding a repository to a team from the repository side", func(t *testing.T) {
		const path = "/api/v1/repos/org3/repo21/teams/team1"
		req := NewRequestWithBody(t, "PUT", path, strings.NewReader("")).
			SetHeader("Authorization", signNIP98(t, repoSecretKey, "PUT", path, "", time.Now(), "grant-repo-team"))
		MakeRequest(t, req, http.StatusForbidden)
		unittest.AssertNotExistsBean(t, &organization.TeamRepo{TeamID: 2, RepoID: 32})
	})

	// Widening an existing team is the same grant in bulk: PATCH /teams/{teamid} sets Permission
	// and IncludesAllRepositories, which reach every current member and every current repository
	// of the organization at once - strictly more than the two per-item grants above, at the same
	// reqOrgOwnership() bar. team1 has two members and write on one repository, so an agent that
	// got this through would have handed both of them admin over all of org3.
	t.Run("widening an existing team", func(t *testing.T) {
		const path = "/api/v1/teams/2"
		body := `{"name":"team1","permission":"admin","includes_all_repositories":true}`
		req := NewRequestWithBody(t, "PATCH", path, strings.NewReader(body)).
			SetHeader("Content-Type", "application/json").
			SetHeader("Authorization", signNIP98(t, orgSecretKey, "PATCH", path, body, time.Now(), "widen-team"))
		MakeRequest(t, req, http.StatusForbidden)

		// A refusal, not a 403 after the widening: the team must still be write-only and must
		// still not include every repository.
		unittest.AssertExistsAndLoadBean(t, &organization.Team{ID: 2, AccessMode: perm.AccessModeWrite, IncludesAllRepositories: false})
	})

	// Transferring the repository is the widest form of the same grant: it hands over contents,
	// future writes and admin at once. org3 is an organization user2 can create repositories in,
	// so StartRepositoryTransfer would complete it immediately rather than leaving a request the
	// owner still has to accept - every owner-team member would reach the repository afterwards
	// with their own credential, and revoking the Nostr key would undo none of it.
	t.Run("transferring the repository away", func(t *testing.T) {
		const path = "/api/v1/repos/user2/repo1/transfer"
		body := `{"new_owner":"org3"}`
		req := NewRequestWithBody(t, "POST", path, strings.NewReader(body)).
			SetHeader("Content-Type", "application/json").
			SetHeader("Authorization", signNIP98(t, repoSecretKey, "POST", path, body, time.Now(), "transfer-repo"))
		MakeRequest(t, req, http.StatusForbidden)

		// A refusal, not a 403 after the fact: the repository must still be user2's, and there
		// must not be a pending transfer waiting for someone to accept it either.
		unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1, OwnerID: 2})
		unittest.AssertNotExistsBean(t, &repo_model.RepoTransfer{RepoID: 1})
	})

	// Reading who already has access grants nothing, so it stays reachable. Asserting it pins
	// where the line is: this guard is about handing out standing access, and a later widening
	// that swallowed the reads too would be a different decision, not a tidy-up.
	t.Run("reading the collaborator list is still allowed", func(t *testing.T) {
		const path = "/api/v1/repos/user2/repo1/collaborators"
		req := NewRequest(t, "GET", path).
			SetHeader("Authorization", signNIP98(t, repoSecretKey, "GET", path, "", time.Now(), "read-collaborators"))
		MakeRequest(t, req, http.StatusOK)
	})

	t.Run("reading the team member list is still allowed", func(t *testing.T) {
		const path = "/api/v1/teams/1/members"
		req := NewRequest(t, "GET", path).
			SetHeader("Authorization", signNIP98(t, orgSecretKey, "GET", path, "", time.Now(), "read-team-members"))
		MakeRequest(t, req, http.StatusOK)
	})

	// PAT parity: the guard tests how the request authenticated, so the same scope carried by a
	// token still makes the grant. Without this the change would read as a scope narrowing.
	t.Run("the owner's own credential still grants", func(t *testing.T) {
		permission := "write"
		req := NewRequestWithJSON(t, "PUT", "/api/v1/repos/user2/repo1/collaborators/user4",
			&api.AddCollaboratorOption{Permission: &permission}).AddTokenAuth(repoToken)
		MakeRequest(t, req, http.StatusNoContent)
	})

	// Parity for the repository-side alias, which is what makes its refusal non-vacuous: the same
	// request carrying the owner's own token goes through and makes the grant, so the 403 above
	// was reqHumanAuth() rather than reqAdmin() or a missing repository.
	t.Run("the owner's own credential still grants from the repository side", func(t *testing.T) {
		req := NewRequest(t, "PUT", "/api/v1/repos/org3/repo21/teams/team1").AddTokenAuth(repoToken)
		MakeRequest(t, req, http.StatusNoContent)
		unittest.AssertExistsAndLoadBean(t, &organization.TeamRepo{TeamID: 2, RepoID: 32})
	})

	// And parity for the team edit, for the same reason: reqOrgOwnership() is satisfied either
	// way, so a 403 that survived the token too would have meant the scope, not the guard.
	t.Run("the owner's own credential still widens the team", func(t *testing.T) {
		req := NewRequestWithJSON(t, "PATCH", "/api/v1/teams/2",
			&api.EditTeamOption{Name: "team1", Permission: "admin"}).AddTokenAuth(orgToken)
		MakeRequest(t, req, http.StatusOK)
		unittest.AssertExistsAndLoadBean(t, &organization.Team{ID: 2, AccessMode: perm.AccessModeAdmin})
	})

	// The same parity for the transfer, and it is what makes the refusal above non-vacuous: the
	// identical request carrying the owner's own token goes through and moves the repository, so
	// the 403 was the guard rather than reqOwner() or the scope. Last, because it does transfer.
	t.Run("the owner's own credential still transfers", func(t *testing.T) {
		req := NewRequestWithJSON(t, "POST", "/api/v1/repos/user2/repo1/transfer",
			&api.TransferRepoOption{NewOwner: "org3"}).AddTokenAuth(repoToken)
		MakeRequest(t, req, http.StatusAccepted)
		unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1, OwnerName: "org3"})
	})
}

// Editing an existing account is the strongest half-undo on the instance: PATCH
// /admin/users/{username} sets the password, the primary email, LoginName/LoginSource, IsAdmin and
// AllowGitHook, every one of which is an independent login that outlives the agent key. Taking
// over an existing administrator is strictly more than creating a fresh account, so guarding only
// the POST would buy nothing - which is what this pins.
func TestAPIAgentCannotEditAnExistingAccount(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	const secretKey = "0000000000000000000000000000000000000000000000000000000000000047"
	// user1 is the site administrator, so reqSiteAdmin() is satisfied and the refusals below are
	// the guard's doing. write:user is what reaches /agent/keys to enrol the key at all.
	token := getUserToken(t, "user1", auth_model.AccessTokenScopeWriteAdmin, auth_model.AccessTokenScopeWriteUser)
	registerAgentKey(t, token, agentPubKey(t, secretKey), string(auth_model.AccessTokenScopeWriteAdmin))

	t.Run("taking over an existing account", func(t *testing.T) {
		const path = "/api/v1/admin/users/user2"
		body := `{"login_name":"user2","source_id":0,"password":"agent-chosen-password-1","admin":true}`
		req := NewRequestWithBody(t, "PATCH", path, strings.NewReader(body)).
			SetHeader("Content-Type", "application/json").
			SetHeader("Authorization", signNIP98(t, secretKey, "PATCH", path, body, time.Now(), "take-over-account"))
		MakeRequest(t, req, http.StatusForbidden)
	})

	t.Run("creating a whole new account", func(t *testing.T) {
		const path = "/api/v1/admin/users"
		body := `{"username":"agentplanted","email":"agentplanted@example.com","password":"agent-chosen-password-2","must_change_password":false}`
		req := NewRequestWithBody(t, "POST", path, strings.NewReader(body)).
			SetHeader("Content-Type", "application/json").
			SetHeader("Authorization", signNIP98(t, secretKey, "POST", path, body, time.Now(), "create-account"))
		MakeRequest(t, req, http.StatusForbidden)
	})

	// Reading the user list leaves no credential behind, so it stays reachable.
	t.Run("listing users is still allowed", func(t *testing.T) {
		const path = "/api/v1/admin/users"
		req := NewRequest(t, "GET", path).
			SetHeader("Authorization", signNIP98(t, secretKey, "GET", path, "", time.Now(), "list-users"))
		MakeRequest(t, req, http.StatusOK)
	})

	// PAT parity, as above: the administrator holding their own token is unaffected.
	t.Run("the administrator's own credential is unaffected", func(t *testing.T) {
		website := "https://example.com/edited-by-a-human"
		req := NewRequestWithJSON(t, "PATCH", "/api/v1/admin/users/user2", &api.EditUserOption{
			LoginName: "user2",
			SourceID:  0,
			Website:   &website,
		}).AddTokenAuth(token)
		MakeRequest(t, req, http.StatusOK)
	})
}

// An added email address is an account-recovery path, which is a login by another name: it is
// stored already activated whenever REGISTER_EMAIL_CONFIRM is off - the default - and the
// forgot-password flow resolves any activated address to its user. So a write:user signature could
// plant an address it controls and still reset the account's password after the Nostr key is
// revoked. That is a durable independent login, which is why POST is guarded while the list and
// the delete, which plant nothing, are not.
func TestAPIAgentCannotPlantAnAccountRecoveryPath(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	const secretKey = "0000000000000000000000000000000000000000000000000000000000000049"
	token := getUserToken(t, "user2", auth_model.AccessTokenScopeWriteUser)
	// write:user is exactly the scope that reaches this endpoint, so the refusal below is the
	// guard's doing and not the scope's.
	registerAgentKey(t, token, agentPubKey(t, secretKey), string(auth_model.AccessTokenScopeWriteUser))

	t.Run("adding an email address", func(t *testing.T) {
		const path = "/api/v1/user/emails"
		body := `{"emails":["user2-agent@example.com"]}`
		req := NewRequestWithBody(t, "POST", path, strings.NewReader(body)).
			SetHeader("Content-Type", "application/json").
			SetHeader("Authorization", signNIP98(t, secretKey, "POST", path, body, time.Now(), "plant-email"))
		MakeRequest(t, req, http.StatusForbidden)

		// The refusal has to be a refusal, not a 403 after the write: the address must not be
		// on the account for the forgot-password flow to find later.
		unittest.AssertNotExistsBean(t, &user_model.EmailAddress{Email: "user2-agent@example.com"})
	})

	t.Run("listing email addresses is still allowed", func(t *testing.T) {
		const path = "/api/v1/user/emails"
		req := NewRequest(t, "GET", path).
			SetHeader("Authorization", signNIP98(t, secretKey, "GET", path, "", time.Now(), "list-emails"))
		MakeRequest(t, req, http.StatusOK)
	})

	// PAT parity: the human holding a write:user token still adds addresses.
	t.Run("the owner's own credential is unaffected", func(t *testing.T) {
		req := NewRequestWithJSON(t, "POST", "/api/v1/user/emails", &api.CreateEmailOption{
			Emails: []string{"user2-human@example.com"},
		}).AddTokenAuth(token)
		MakeRequest(t, req, http.StatusCreated)
	})
}

// The owner, holding their own credential, must be able to do all three - otherwise revocation is
// unreachable and the incident-response story is "edit the table by hand".
func TestAPIAgentKeyLifecycle(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	const secretKey = "0000000000000000000000000000000000000000000000000000000000000019"
	token := getUserToken(t, "user2", auth_model.AccessTokenScopeWriteUser, auth_model.AccessTokenScopeWriteIssue)
	key := registerAgentKey(t, token, agentPubKey(t, secretKey), string(auth_model.AccessTokenScopeWriteIssue))

	req := NewRequest(t, "GET", "/api/v1/agent/keys").AddTokenAuth(token)
	resp := MakeRequest(t, req, http.StatusOK)
	var keys []*api.AgentKey
	DecodeJSON(t, resp, &keys)
	require.Len(t, keys, 1)
	assert.Equal(t, key.ID, keys[0].ID)
	assert.Nil(t, keys[0].Revoked)

	// The key works before revocation...
	const issuePath = "/api/v1/repos/user2/repo1/issues"
	body := `{"title":"before revocation"}`
	signed := NewRequestWithBody(t, "POST", issuePath, strings.NewReader(body)).
		SetHeader("Content-Type", "application/json").
		SetHeader("Authorization", signNIP98(t, secretKey, "POST", issuePath, body, time.Now(), "before"))
	MakeRequest(t, signed, http.StatusCreated)

	req = NewRequest(t, "DELETE", "/api/v1/agent/keys/"+strconv.FormatInt(key.ID, 10)).AddTokenAuth(token)
	MakeRequest(t, req, http.StatusNoContent)

	// ...and not after.
	body = `{"title":"after revocation"}`
	signed = NewRequestWithBody(t, "POST", issuePath, strings.NewReader(body)).
		SetHeader("Content-Type", "application/json").
		SetHeader("Authorization", signNIP98(t, secretKey, "POST", issuePath, body, time.Now(), "after"))
	MakeRequest(t, signed, http.StatusUnauthorized)

	req = NewRequest(t, "GET", "/api/v1/agent/keys").AddTokenAuth(token)
	resp = MakeRequest(t, req, http.StatusOK)
	DecodeJSON(t, resp, &keys)
	require.Len(t, keys, 1)
	assert.NotNil(t, keys[0].Revoked, "revocation must be visible, not a silent delete")

	// Someone else's key is not theirs to revoke.
	otherToken := getUserToken(t, "user4", auth_model.AccessTokenScopeWriteUser)
	req = NewRequest(t, "DELETE", "/api/v1/agent/keys/"+strconv.FormatInt(key.ID, 10)).AddTokenAuth(otherToken)
	MakeRequest(t, req, http.StatusForbidden)
}

// A non-admin must not be able to name someone else's account as the agent: that would let any
// user mint a credential that authenticates as an administrator.
func TestAPIAgentKeyCannotBindAnotherUser(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	// The token holds write:issue as well, so the refusal below can only be about the binding:
	// a token that did not hold it would be refused for delegating a scope it lacks instead,
	// and this test would pass without ever reaching the check it is named after.
	token := getUserToken(t, "user2", auth_model.AccessTokenScopeWriteUser, auth_model.AccessTokenScopeWriteIssue)
	pubKey := agentPubKey(t, "000000000000000000000000000000000000000000000000000000000000000f")

	req := NewRequestWithJSON(t, "POST", "/api/v1/agent/keys", &api.CreateAgentKeyOption{
		PublicKey:   pubKey,
		Scopes:      []string{string(auth_model.AccessTokenScopeWriteIssue)},
		AgentUserID: 1, // user1 is a site administrator in the fixtures
	}).AddTokenAuth(token)
	MakeRequest(t, req, http.StatusForbidden)
}

// The configuration the whole design rests on, end to end: an administrator binds a key to a
// *separate* agent account, that account authenticates with it, and the owner - who is not the
// agent - revokes it.
//
// Every other test here has owner == agent, which means the human's own account is the one that
// gets is_agent and becomes NIP-98-authenticable. That is the opposite of the containment story,
// and it left the passing path of the ownership chain unexercised: only the refusal to bind
// someone else's account was pinned.
func TestAPIAgentKeyOwnerIsNotTheAgent(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	const secretKey = "0000000000000000000000000000000000000000000000000000000000000021"

	// user1 is a site administrator in the fixtures; user2 is the account that will do the
	// signing, and owns repo1.
	const agentUserID int64 = 2
	// write:issue is on the owner's token because a credential may only delegate what it holds,
	// and the key registered below is a write:issue key.
	ownerToken := getUserToken(t, "user1", auth_model.AccessTokenScopeWriteUser, auth_model.AccessTokenScopeWriteIssue)
	// Reading the audit trail needs repo admin, so this token carries the repository scope too.
	agentOwnToken := getUserToken(t, "user2", auth_model.AccessTokenScopeWriteUser, auth_model.AccessTokenScopeReadRepository)

	key := registerAgentKeyFor(t, ownerToken, agentPubKey(t, secretKey), agentUserID,
		string(auth_model.AccessTokenScopeWriteIssue))
	require.NotEqual(t, key.OwnerUserID, key.AgentUserID, "this test is meaningless if they are equal")
	assert.EqualValues(t, 1, key.OwnerUserID)
	assert.Equal(t, agentUserID, key.AgentUserID)

	const issuePath = "/api/v1/repos/user2/repo1/issues"

	t.Run("the agent account signs and is authenticated as itself", func(t *testing.T) {
		body := `{"title":"signed by a separate agent account"}`
		req := NewRequestWithBody(t, "POST", issuePath, strings.NewReader(body)).
			SetHeader("Content-Type", "application/json").
			SetHeader("Authorization", signNIP98(t, secretKey, "POST", issuePath, body, time.Now(), "split-accepted"))
		resp := MakeRequest(t, req, http.StatusCreated)

		issue := new(api.Issue)
		DecodeJSON(t, resp, issue)
		// The request acts as the agent, never as the administrator who vouched for it. If it
		// acted as the owner, one registration would hand the agent site-admin rights.
		assert.Equal(t, "user2", issue.Poster.UserName)
	})

	t.Run("the trail names both ends of the chain", func(t *testing.T) {
		req := NewRequest(t, "GET", "/api/v1/repos/user2/repo1/agent-audit").AddTokenAuth(agentOwnToken)
		resp := MakeRequest(t, req, http.StatusOK)

		var events []*api.AgentAuditEvent
		DecodeJSON(t, resp, &events)
		require.NotEmpty(t, events)
		assert.Equal(t, agentUserID, events[0].AgentUserID)
		assert.EqualValues(t, 1, events[0].OwnerUserID, "the trail must name the human who vouched")
	})

	// Containment is not secrecy. The key row says which public key can authenticate as user2,
	// and user2 is entitled to know that - listing only what the caller owns would leave the
	// account with no way to discover a credential signing on its behalf, since reqHumanAuth()
	// keeps it from looking with the agent's own signature either.
	t.Run("the agent user can see the key registered for it", func(t *testing.T) {
		req := NewRequest(t, "GET", "/api/v1/agent/keys").AddTokenAuth(agentOwnToken)
		resp := MakeRequest(t, req, http.StatusOK)

		var keys []*api.AgentKey
		DecodeJSON(t, resp, &keys)
		require.Len(t, keys, 1)
		assert.Equal(t, key.ID, keys[0].ID)
		assert.EqualValues(t, 1, keys[0].OwnerUserID, "it is still the owner's key, merely visible")
	})

	// The key lives on the agent's account but belongs to the owner. The agent holding its own
	// credential must not be able to unpick the containment.
	t.Run("the agent user cannot revoke its owner's key", func(t *testing.T) {
		req := NewRequest(t, "DELETE", "/api/v1/agent/keys/"+strconv.FormatInt(key.ID, 10)).
			AddTokenAuth(agentOwnToken)
		MakeRequest(t, req, http.StatusForbidden)
	})

	// ...and it stays usable, because that refusal must not have revoked anything.
	t.Run("the refused revocation changed nothing", func(t *testing.T) {
		body := `{"title":"still working"}`
		req := NewRequestWithBody(t, "POST", issuePath, strings.NewReader(body)).
			SetHeader("Content-Type", "application/json").
			SetHeader("Authorization", signNIP98(t, secretKey, "POST", issuePath, body, time.Now(), "split-still-ok"))
		MakeRequest(t, req, http.StatusCreated)
	})

	t.Run("the owner revokes a key belonging to another account", func(t *testing.T) {
		req := NewRequest(t, "DELETE", "/api/v1/agent/keys/"+strconv.FormatInt(key.ID, 10)).
			AddTokenAuth(ownerToken)
		MakeRequest(t, req, http.StatusNoContent)

		body := `{"title":"after revocation"}`
		req = NewRequestWithBody(t, "POST", issuePath, strings.NewReader(body)).
			SetHeader("Content-Type", "application/json").
			SetHeader("Authorization", signNIP98(t, secretKey, "POST", issuePath, body, time.Now(), "split-revoked"))
		MakeRequest(t, req, http.StatusUnauthorized)
	})

	// Revoking the last key has to take the enrolment away too, not just the key. If is_agent
	// survived, the account would stay eligible for NIP-98 authentication forever and revocation
	// would be only half an undo - so registering a fresh key must be what re-enables it.
	t.Run("revoking the last key un-enrols the account", func(t *testing.T) {
		user := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: agentUserID})
		assert.False(t, user.IsAgent, "revoking the last key must clear is_agent")

		const secondKey = "0000000000000000000000000000000000000000000000000000000000000023"
		registerAgentKeyFor(t, ownerToken, agentPubKey(t, secondKey), agentUserID,
			string(auth_model.AccessTokenScopeWriteIssue))

		user = unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: agentUserID})
		assert.True(t, user.IsAgent, "registering a key must re-enrol the account")

		body := `{"title":"re-enrolled"}`
		req := NewRequestWithBody(t, "POST", issuePath, strings.NewReader(body)).
			SetHeader("Content-Type", "application/json").
			SetHeader("Authorization", signNIP98(t, secondKey, "POST", issuePath, body, time.Now(), "split-reenrolled"))
		MakeRequest(t, req, http.StatusCreated)
	})
}

// An organization has no credentials of its own and the system users are not accounts anyone
// signs in as, so writing is_agent onto either just corrupts the row.
func TestAPIAgentKeyRejectsNonIndividualAgentUser(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	adminToken := getUserToken(t, "user1", auth_model.AccessTokenScopeWriteUser, auth_model.AccessTokenScopeWriteIssue)

	// org3 is an organization in the fixtures; -1 is the Ghost system user.
	cases := []struct {
		name        string
		agentUserID int64
		secretKey   string
	}{
		{"an organization", 3, "0000000000000000000000000000000000000000000000000000000000000025"},
		{"a system user", -1, "0000000000000000000000000000000000000000000000000000000000000027"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := NewRequestWithJSON(t, "POST", "/api/v1/agent/keys", &api.CreateAgentKeyOption{
				PublicKey:   agentPubKey(t, tc.secretKey),
				Scopes:      []string{string(auth_model.AccessTokenScopeWriteIssue)},
				AgentUserID: tc.agentUserID,
			}).AddTokenAuth(adminToken)
			MakeRequest(t, req, http.StatusUnprocessableEntity)
		})
	}
}

// Registering the same public key twice must fail rather than create a second binding.
func TestAPIAgentKeyRejectsDuplicates(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	token := getUserToken(t, "user2", auth_model.AccessTokenScopeWriteUser, auth_model.AccessTokenScopeWriteIssue)
	pubKey := agentPubKey(t, "0000000000000000000000000000000000000000000000000000000000000011")

	key := registerAgentKey(t, token, pubKey, string(auth_model.AccessTokenScopeWriteIssue))

	req := NewRequestWithJSON(t, "POST", "/api/v1/agent/keys", &api.CreateAgentKeyOption{
		PublicKey: pubKey,
		Scopes:    []string{string(auth_model.AccessTokenScopeWriteIssue)},
	}).AddTokenAuth(token)
	resp := MakeRequest(t, req, http.StatusUnprocessableEntity)
	assert.Contains(t, resp.Body.String(), "already registered")

	// Revoking does not free the keypair - pub_key is UNIQUE and revocation is deliberately
	// terminal - but "already registered" would then describe the wrong situation and send an
	// operator hunting for an active key the account does not have. The refusal is the same;
	// the reason it gives is not.
	req = NewRequest(t, "DELETE", "/api/v1/agent/keys/"+strconv.FormatInt(key.ID, 10)).AddTokenAuth(token)
	MakeRequest(t, req, http.StatusNoContent)

	req = NewRequestWithJSON(t, "POST", "/api/v1/agent/keys", &api.CreateAgentKeyOption{
		PublicKey: pubKey,
		Scopes:    []string{string(auth_model.AccessTokenScopeWriteIssue)},
	}).AddTokenAuth(token)
	resp = MakeRequest(t, req, http.StatusUnprocessableEntity)
	assert.Contains(t, resp.Body.String(), "revoked")
	assert.NotContains(t, resp.Body.String(), "already registered")
}

// A key with no scope would pass every scope check in the API, so registration has to refuse it.
func TestAPIAgentKeyRequiresAScope(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	token := getUserToken(t, "user2", auth_model.AccessTokenScopeWriteUser)
	pubKey := agentPubKey(t, "000000000000000000000000000000000000000000000000000000000000001b")

	req := NewRequestWithJSON(t, "POST", "/api/v1/agent/keys", &api.CreateAgentKeyOption{
		PublicKey: pubKey,
	}).AddTokenAuth(token)
	MakeRequest(t, req, http.StatusUnprocessableEntity)

	req = NewRequestWithJSON(t, "POST", "/api/v1/agent/keys", &api.CreateAgentKeyOption{
		PublicKey: pubKey,
		Scopes:    []string{"not-a-real-scope"},
	}).AddTokenAuth(token)
	MakeRequest(t, req, http.StatusUnprocessableEntity)
}

// The trail exposes every URL an agent touched, query strings included. Read access to the
// repository's contents does not imply access to that.
func TestAPIAgentAuditNeedsRepoAdmin(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	// user4 can read user2/repo1 in the fixtures but does not administer it.
	readerToken := getUserToken(t, "user4", auth_model.AccessTokenScopeReadRepository)
	req := NewRequest(t, "GET", "/api/v1/repos/user2/repo1/agent-audit").AddTokenAuth(readerToken)
	MakeRequest(t, req, http.StatusForbidden)

	ownerToken := getUserToken(t, "user2", auth_model.AccessTokenScopeReadRepository)
	req = NewRequest(t, "GET", "/api/v1/repos/user2/repo1/agent-audit").AddTokenAuth(ownerToken)
	MakeRequest(t, req, http.StatusOK)
}

// Anything an agent does outside a repository - a call against /user, an org-scoped one, or a
// refusal that lands before the router resolves a repository - is stored with repo_id 0, which is
// precisely what the repo-scoped endpoint filters out. Those rows would be recorded and readable
// by nothing at all without the owner's view.
func TestAPIAgentOwnedAuditShowsNonRepoRequests(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	const secretKey = "0000000000000000000000000000000000000000000000000000000000000029"
	token := getUserToken(t, "user2", auth_model.AccessTokenScopeWriteUser)
	key := registerAgentKey(t, token, agentPubKey(t, secretKey), string(auth_model.AccessTokenScopeWriteUser))

	const path = "/api/v1/user/emails"
	body := `{"emails":["agent@example.com"]}`
	req := NewRequestWithBody(t, "POST", path, strings.NewReader(body)).
		SetHeader("Content-Type", "application/json").
		SetHeader("Authorization", signNIP98(t, secretKey, "POST", path, body, time.Now(), "non-repo"))
	MakeRequest(t, req, http.StatusCreated)

	req = NewRequest(t, "GET", "/api/v1/agent/audit").AddTokenAuth(token)
	resp := MakeRequest(t, req, http.StatusOK)

	var events []*api.AgentAuditEvent
	DecodeJSON(t, resp, &events)
	require.NotEmpty(t, events, "a signed non-repository request was recorded nowhere readable")
	assert.EqualValues(t, 0, events[0].RepoID, "this is exactly the row the repo endpoint cannot show")
	assert.Equal(t, key.ID, events[0].AgentKeyID)
	assert.Equal(t, http.StatusCreated, events[0].ResponseStatus)
	assert.True(t, strings.HasSuffix(events[0].RequestURL, path), "request url was %q", events[0].RequestURL)

	// The trail is kept *about* the agent, for the human accountable for it: the agent's own
	// signature must not read it, and neither must another user's credential.
	req = NewRequest(t, "GET", "/api/v1/agent/audit").
		SetHeader("Authorization", signNIP98(t, secretKey, "GET", "/api/v1/agent/audit", "", time.Now(), "self-audit"))
	MakeRequest(t, req, http.StatusForbidden)

	otherToken := getUserToken(t, "user4", auth_model.AccessTokenScopeWriteUser)
	req = NewRequest(t, "GET", "/api/v1/agent/audit").AddTokenAuth(otherToken)
	resp = MakeRequest(t, req, http.StatusOK)
	var otherEvents []*api.AgentAuditEvent
	DecodeJSON(t, resp, &otherEvents)
	assert.Empty(t, otherEvents, "the trail must be scoped to the human who vouched")
}

// A request that authenticates and is then refused must still leave a row, saying so. Without
// this the trail would only contain the attempts that succeeded, which is the opposite of what
// an audit trail is for.
func TestAPIAgentAuditRecordsRefusedRequests(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	const secretKey = "000000000000000000000000000000000000000000000000000000000000001d"
	token := getUserToken(t, "user2", auth_model.AccessTokenScopeWriteUser, auth_model.AccessTokenScopeWriteIssue, auth_model.AccessTokenScopeReadRepository)
	registerAgentKey(t, token, agentPubKey(t, secretKey), string(auth_model.AccessTokenScopeWriteIssue))

	// user30/repo51 is public and archived, so this resolves the repository - which is what puts
	// the repo id on the audit row - and is then refused for writing to an archive. The refusal
	// has to come after repoAssignment: a request rejected before it has no repository to be
	// listed under, and this test is about the trail being complete, not about which 4xx it is.
	const path = "/api/v1/repos/user30/repo51/issues"
	body := `{"title":"not allowed here"}`
	req := NewRequestWithBody(t, "POST", path, strings.NewReader(body)).
		SetHeader("Content-Type", "application/json").
		SetHeader("Authorization", signNIP98(t, secretKey, "POST", path, body, time.Now(), "refused"))
	resp := MakeRequest(t, req, NoExpectedStatus)
	require.NotEqual(t, http.StatusCreated, resp.Code)
	require.NotEqual(t, http.StatusUnauthorized, resp.Code, "the credential itself must have been accepted")

	adminToken := getUserToken(t, "user1", auth_model.AccessTokenScopeReadRepository)
	auditReq := NewRequest(t, "GET", "/api/v1/repos/user30/repo51/agent-audit").AddTokenAuth(adminToken)
	auditResp := MakeRequest(t, auditReq, http.StatusOK)

	var events []*api.AgentAuditEvent
	DecodeJSON(t, auditResp, &events)
	require.NotEmpty(t, events, "a refused agent request left no trace")
	assert.Equal(t, resp.Code, events[0].ResponseStatus,
		"the trail must record the refusal, not imply the request was accepted")
}
