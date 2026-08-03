// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package auth

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	agent_model "code.gitea.io/gitea/models/agent"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/json"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/timeutil"
	"code.gitea.io/gitea/services/agentauth"
)

// Ensure the struct implements the interface.
var (
	_ Method = &Nostr{}
)

const (
	// NostrMethodName is the name recorded in the request store for NIP-98 authenticated
	// requests. routers/api/v1 compares against it to keep credential-minting endpoints out
	// of reach of a signed request.
	NostrMethodName = "nostr"

	// AgentAuditPendingKey is the request-store key under which a successful NIP-98
	// authentication leaves the audit row it has just written, so that the middleware which
	// runs after the handler can attach the outcome to it.
	AgentAuditPendingKey = "AgentAuditPending"
)

// Nostr authenticates API requests that carry a NIP-98 (kind 27235) event in the Authorization
// header, signed by a Nostr key that has been registered to an agent user.
//
// It is strictly additive: a request without an `Authorization: Nostr ...` header returns
// (nil, nil) so that OAuth2, PAT and Basic behave exactly as they did before.
type Nostr struct{}

// Name represents the name of auth method
func (n *Nostr) Name() string {
	return NostrMethodName
}

// Verify checks the NIP-98 credential and returns the agent user it authenticates.
//
// The order of the checks is load-bearing and is, in one line: prove the event is signed, prove
// the signer is known, only then touch the body, then spend the event id. Each step is gated on
// the previous one so that no unauthenticated caller can make the server do unbounded work, and
// so that no rejected request leaves a trace an attacker chose.
//
// Every rejection is deliberately reported as the same opaque message: the caller learns that
// the credential was refused, not which check refused it. The precise reason is logged
// server-side.
func (n *Nostr) Verify(req *http.Request, w http.ResponseWriter, store DataStore, sess SessionStore) (*user_model.User, error) {
	if !agentauth.HasCredential(req) {
		return nil, nil //nolint:nilnil // the auth method is not applicable
	}

	// NIP-98 is an API-only credential, like Basic.
	if detector := newAuthPathDetector(req); !detector.isAPIPath() {
		return nil, nil //nolint:nilnil // the auth method is not applicable
	}

	// Step 1: the header alone. Nothing here reads the request body.
	signed, err := agentauth.VerifyCredential(req, agentauth.Options{ExpectedURL: expectedRequestURL(req)})
	if err != nil {
		if errors.Is(err, agentauth.ErrNotApplicable) {
			return nil, nil //nolint:nilnil // the auth method is not applicable
		}
		log.Debug("Nostr Authorization: rejected NIP-98 event: %v", err)
		return nil, ErrUserAuthMessage("invalid NIP-98 authorization")
	}

	ctx := req.Context()

	// Step 2: is this signer anyone we know? A valid signature proves only that *a* keypair
	// signed, and keypairs are free to mint, so this is the first check that costs an attacker
	// anything.
	key, err := agent_model.GetKeyByPubKey(ctx, signed.PubKey)
	if err != nil {
		if !agent_model.IsErrAgentKeyNotExist(err) {
			log.Error("Nostr Authorization: GetKeyByPubKey: %v", err)
		} else {
			log.Debug("Nostr Authorization: unregistered pubkey %s", signed.PubKey)
		}
		return nil, ErrUserAuthMessage("invalid NIP-98 authorization")
	}
	if key.IsRevoked() {
		log.Debug("Nostr Authorization: pubkey %s was revoked at %d", signed.PubKey, key.RevokedUnix)
		return nil, ErrUserAuthMessage("invalid NIP-98 authorization")
	}
	// A key with no scope would sail past routers/api/v1's tokenRequiresScopes, which only
	// constrains requests that arrive carrying a scope, and would therefore be able to do more
	// than any personal access token of the same user. Registration refuses to create such a
	// key; this refuses to honour one that exists anyway.
	if key.Scope == "" {
		log.Error("Nostr Authorization: agent key %d has no scope and cannot be used", key.ID)
		return nil, ErrUserAuthMessage("invalid NIP-98 authorization")
	}

	u, err := user_model.GetUserByID(ctx, key.AgentUserID)
	if err != nil {
		log.Error("Nostr Authorization: GetUserByID(%d): %v", key.AgentUserID, err)
		return nil, ErrUserAuthMessage("invalid NIP-98 authorization")
	}
	// The key stays registered but stops authenticating if the user is no longer an agent, is
	// deactivated, or is barred from logging in.
	if !u.IsAgent || !u.IsActive || u.ProhibitLogin {
		log.Debug("Nostr Authorization: user %d is not an eligible agent", u.ID)
		return nil, ErrUserAuthMessage("invalid NIP-98 authorization")
	}

	// Step 3: the body. Buffering it is the only step whose cost the caller chooses, so it
	// waits until the request is known to come from a registered, unrevoked, eligible agent -
	// and even then it is bounded by a number the operator set, because a registered key can
	// still have N requests in flight and each one holds its buffer resident.
	if err := signed.VerifyPayload(req, setting.Agent.MaxRequestBodySize); err != nil {
		// A body over the limit is not a rejected credential, and saying so would send the
		// operator hunting for a key or clock problem they do not have. It is the one payload
		// failure whose reason is safe to state: the caller already knows how big their request
		// was, so naming it tells an attacker nothing they did not supply themselves.
		if errors.Is(err, agentauth.ErrBodyTooLarge) {
			log.Debug("Nostr Authorization: body exceeds the %d byte NIP-98 limit", setting.Agent.MaxRequestBodySize)
			return nil, ErrUserAuthStatus{
				Status:  http.StatusRequestEntityTooLarge,
				Message: "request body is too large to authorize with NIP-98",
			}
		}
		log.Debug("Nostr Authorization: rejected NIP-98 payload: %v", err)
		return nil, ErrUserAuthMessage("invalid NIP-98 authorization")
	}

	// Step 4: spend the event id. Binding the event to a method, URL and body stops it being
	// repurposed but not repeated - without this, anyone who captured the header could re-send
	// the identical mutation until created_at went stale.
	expires := timeutil.TimeStamp(signed.CreatedAt.Add(agentauth.DefaultClockSkew).Unix())
	if err := agent_model.ConsumeEvent(ctx, signed.EventID, expires); err != nil {
		if agent_model.IsErrEventReplayed(err) {
			log.Debug("Nostr Authorization: event %s was replayed", signed.EventID)
		} else {
			log.Error("Nostr Authorization: ConsumeEvent: %v", err)
		}
		return nil, ErrUserAuthMessage("invalid NIP-98 authorization")
	}

	// The audit row is written now, not after the handler, so that a signed request can never
	// be performed unlogged: if this insert fails the request is refused. What the request went
	// on to do is attached to the row later by the audit middleware in routers/api/v1.
	//
	// The whole event goes in, not a summary of it. A row holding only the event id records an
	// identifier that nothing can be checked against: the id is a hash of fields the row did not
	// keep, so no reader - not the audit endpoints, not an operator with a SQL prompt - could
	// tell an authentic row from one that had been edited afterwards. With created_at, the kind,
	// the tags, the content and the signature stored alongside, models/agent.VerifyEvent
	// re-derives the id and re-checks the signature against the recorded pubkey, so altering
	// what the trail says an agent did means forging the agent's key.
	tags, err := json.Marshal(signed.Tags)
	if err != nil {
		log.Error("Nostr Authorization: cannot serialize event tags: %v", err)
		return nil, ErrUserAuthMessage("could not record the agent audit event")
	}
	audit := &agent_model.AuditEvent{
		AgentUserID:      u.ID,
		OwnerUserID:      key.OwnerUserID,
		AgentKeyID:       key.ID,
		EventID:          signed.EventID,
		PubKey:           signed.PubKey,
		Method:           signed.Method,
		RequestURL:       signed.RequestURL,
		PayloadHash:      signed.PayloadHash,
		EventCreatedUnix: timeutil.TimeStamp(signed.CreatedAt.Unix()),
		EventKind:        signed.Kind,
		Nonce:            signed.Nonce,
		EventTags:        string(tags),
		EventContent:     signed.Content,
		Sig:              signed.Sig,
	}
	if err := agent_model.InsertAuditEvent(ctx, audit); err != nil {
		log.Error("Nostr Authorization: InsertAuditEvent: %v", err)
		return nil, ErrUserAuthMessage("could not record the agent audit event")
	}

	// Present the key's scope in exactly the shape a personal access token uses, so that every
	// tokenRequiresScopes guard in routers/api/v1 constrains a signed request the same way it
	// constrains a PAT. Without these two, a scope check is a no-op for NIP-98 requests.
	store.GetData()["IsApiToken"] = true
	store.GetData()["ApiTokenScope"] = key.Scope
	store.GetData()["LoginMethod"] = NostrMethodName
	store.GetData()[AgentAuditPendingKey] = audit

	log.Trace("Nostr Authorization: Logged in agent user %-v", u)
	return u, nil
}

// expectedRequestURL builds the absolute URL that the NIP-98 `u` tag must match.
//
// It is derived from setting.AppURL rather than from the request's Host header on purpose: the
// host is attacker-controlled, so trusting it would let an event signed for one deployment be
// replayed against another that happens to share a database. The request-derived fallback only
// applies when AppURL is unset, which in practice means a test binary.
//
// AppURL's *path* is part of the answer, not just its scheme and host. On a sub-path deployment
// (ROOT_URL=https://git.example/gitea/) modules/web/router.go has already stripped AppSubURL from
// req.URL.Path by the time authentication runs, so RequestURI() is the sub-path-less
// "/api/v1/...". The client necessarily signed the URL it posted to, which includes "/gitea";
// re-attaching the prefix from AppURL is what makes the two strings the same one.
//
// The corollary is an operational one, and it is the likeliest cause of an otherwise inexplicable
// 401: a client must sign the URL the *server* believes in. If ROOT_URL says https://git.example
// and the agent posts to http://localhost:3000, the signature is over the wrong string and the
// request is refused. The reason is at log level Debug.
func expectedRequestURL(req *http.Request) string {
	if base, err := url.Parse(setting.AppURL); err == nil && base.Scheme != "" && base.Host != "" {
		// AppURL is normalized to end in "/"; RequestURI() begins with one.
		return strings.TrimSuffix(setting.AppURL, "/") + req.URL.RequestURI()
	}
	scheme := "http"
	if req.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + req.Host + req.URL.RequestURI()
}
