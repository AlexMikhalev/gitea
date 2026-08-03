// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package agentauth verifies NIP-98 (kind 27235) HTTP authorization events.
//
// A NIP-98 event is a bearer credential that is bound to one method, one absolute URL and one
// request body, and expires in seconds - unlike a personal access token, capturing it does not
// let the holder make a different request. This package does the protocol half only: it says
// whether the event is well-formed, fresh and correctly signed. Deciding whether the signing key
// is registered, unrevoked and mapped to a Gitea user is services/auth's job.
package agentauth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"code.gitea.io/gitea/modules/json"
	"code.gitea.io/gitea/modules/nostr"
	"code.gitea.io/gitea/modules/util"
)

const (
	// AuthorizationScheme is the `Authorization:` scheme NIP-98 reserves.
	AuthorizationScheme = "Nostr"

	// EventKind is the NIP-98 "HTTP Auth" event kind.
	EventKind = 27235

	// DefaultClockSkew is how far the event's created_at may be from the server clock in
	// either direction. NIP-98 suggests 60 seconds.
	DefaultClockSkew = 60 * time.Second

	// DefaultMaxBodySize caps the request body this package will hash when the caller names no
	// limit of its own. Only requests that actually carry a NIP-98 header are read here, so this
	// does not constrain any other upload path.
	//
	// It is a default rather than the limit because the buffer is the one cost of a signed
	// request an operator cannot otherwise bound: a signature commits to a hash of the whole
	// body, so the body cannot be streamed, and N concurrent requests from one registered key
	// hold N buffers resident. services/auth passes setting.Agent.MaxRequestBodySize, which is
	// this value unless the operator says otherwise.
	DefaultMaxBodySize = 32 << 20 // 32 MiB
)

// Errors returned by Verify. They are distinguished so that callers can log a precise reason;
// every one of them except ErrNotApplicable must be surfaced to the client as a flat 401.
var (
	// ErrNotApplicable means there is no NIP-98 credential in this request at all. It is not a
	// failure: the caller must fall through to the other authentication methods.
	ErrNotApplicable = errors.New("no NIP-98 authorization header")

	ErrMalformedHeader = errors.New("malformed NIP-98 authorization header")
	ErrInvalidEvent    = errors.New("invalid NIP-98 event")
	ErrMissingNonce    = errors.New("authorization event has no `nonce` tag")
	ErrWrongKind       = errors.New("authorization event is not kind 27235")
	ErrURLMismatch     = errors.New("authorization event was signed for a different URL")
	ErrMethodMismatch  = errors.New("authorization event was signed for a different method")
	ErrMethodUnknown   = errors.New("authorization event was signed for an unrecognised HTTP method")
	ErrPayloadMismatch = errors.New("authorization event was signed for a different body")
	ErrStale           = errors.New("authorization event is outside the accepted time window")
	ErrBadSignature    = errors.New("authorization event signature is invalid")
	ErrBodyTooLarge    = errors.New("request body is too large to authorize with NIP-98")
)

// SignedRequest is the verified content of an accepted NIP-98 event.
type SignedRequest struct {
	// EventID is the NIP-01 event id (hex SHA-256 of the canonical serialization).
	EventID string
	// PubKey is the hex x-only secp256k1 key that signed the event.
	PubKey string
	// Method and RequestURL are the values the signature committed to, already checked
	// against the incoming request.
	Method     string
	RequestURL string
	// PayloadHash is the hex SHA-256 of the request body, empty when the request had none or
	// when VerifyPayload has not run yet.
	PayloadHash string
	CreatedAt   time.Time

	// Nonce is the `nonce` tag. It is what makes EventID unique per request rather than a pure
	// function of (pubkey, second, url, method), and is pulled out of Tags so that a caller
	// storing the event does not have to re-walk them.
	Nonce string

	// Kind, Tags, Content and Sig are the remaining ingredients of the event, kept so that a
	// caller can persist enough to re-derive EventID and re-check the signature later. Without
	// them a stored event id is an opaque string: nothing binds it to PubKey, and nothing binds
	// the recorded method and URL to either. See models/agent.AuditEvent.
	Kind    int
	Tags    nostr.Tags
	Content string
	Sig     string

	// signedPayload is the `payload` tag as it appeared in the signed event, kept so that
	// VerifyPayload can compare against it without re-parsing the credential.
	signedPayload string
}

// Options carries everything VerifyCredential needs that it must not read from a global.
type Options struct {
	// ExpectedURL is the absolute URL the request is considered to have arrived at. It must be
	// derived from server configuration, never from the request's own Host header - otherwise
	// an event signed for one host could be replayed against another.
	ExpectedURL string
	// Now is the reference clock. The zero value means time.Now().
	Now time.Time
	// ClockSkew is the accepted +/- window. The zero value means DefaultClockSkew.
	ClockSkew time.Duration
}

// HasCredential reports whether the request carries a NIP-98 authorization header. It exists so
// that an auth method can bail out before touching the request body.
func HasCredential(req *http.Request) bool {
	_, ok := parseHeader(req.Header.Get("Authorization"))
	return ok
}

// parseHeader returns the base64 payload of an `Authorization: Nostr <event>` header.
func parseHeader(header string) (payload string, ok bool) {
	scheme, rest, found := strings.Cut(strings.TrimSpace(header), " ")
	if !found || !strings.EqualFold(scheme, AuthorizationScheme) {
		return "", false
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", false
	}
	return rest, true
}

// VerifyCredential checks everything about a request's NIP-98 credential that can be decided
// from the `Authorization` header alone: shape, kind, freshness, the `u`, `method` and `nonce`
// tags, and the signature. It returns ErrNotApplicable when there is no such credential, which
// callers must treat as "not my business" rather than as a rejection.
//
// It deliberately does not look at the request body. Verification is split in two because
// buffering the body is the only step whose cost the caller controls, and a signature alone is
// cheap to produce: anyone can mint a keypair. Reading up to MaxBodySize into memory must
// therefore wait until the *key* has been recognised, which only the caller can decide. Once it
// has, the caller must call VerifyPayload before trusting the request - a credential that has
// passed only this half says nothing about the bytes the handler will read.
func VerifyCredential(req *http.Request, opts Options) (*SignedRequest, error) {
	encoded, ok := parseHeader(req.Header.Get("Authorization"))
	if !ok {
		return nil, ErrNotApplicable
	}

	raw, err := decodeBase64(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: not valid base64", ErrMalformedHeader)
	}

	var event nostr.Event
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil, fmt.Errorf("%w: not valid JSON", ErrInvalidEvent)
	}

	if event.Kind != EventKind {
		return nil, fmt.Errorf("%w: got kind %d", ErrWrongKind, event.Kind)
	}

	// created_at freshness. Checked before the signature so that a flood of stale replays costs
	// no elliptic-curve work.
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	skew := opts.ClockSkew
	if skew <= 0 {
		skew = DefaultClockSkew
	}
	createdAt := event.CreatedAt.Time()
	if delta := now.Sub(createdAt); delta > skew || delta < -skew {
		return nil, fmt.Errorf("%w: created_at is %s away from now", ErrStale, delta.Truncate(time.Second))
	}

	// `u` tag: the absolute URL the signature commits to.
	signedURL := tagValue(event.Tags, "u")
	if signedURL == "" {
		return nil, fmt.Errorf("%w: missing `u` tag", ErrInvalidEvent)
	}
	gotURL, err := NormalizeURL(signedURL)
	if err != nil {
		return nil, fmt.Errorf("%w: `u` tag is not an absolute URL", ErrURLMismatch)
	}
	wantURL, err := NormalizeURL(opts.ExpectedURL)
	if err != nil {
		return nil, fmt.Errorf("%w: server could not resolve its own request URL", ErrURLMismatch)
	}
	if gotURL != wantURL {
		return nil, fmt.Errorf("%w: signed for %q", ErrURLMismatch, gotURL)
	}

	// `method` tag.
	signedMethod := strings.ToUpper(tagValue(event.Tags, "method"))
	if signedMethod == "" {
		return nil, fmt.Errorf("%w: missing `method` tag", ErrInvalidEvent)
	}
	// Only the methods RFC 9110 defines are honoured. Authentication runs before routing, so any
	// RFC 7230 token an attacker cares to send arrives here - net/http does not restrict the
	// method - and the accepted value is written verbatim into the audit trail. That column is
	// narrow, so a long method (`VERSION-CONTROL`, `UNSUBSCRIBE`) either fails the insert, turning
	// a request that deserved a 404 into a 500, or is silently truncated, after which the row's
	// `method` column no longer matches the `method` tag its signature covers and an authentic row
	// reads as tampered. Refusing the credential is the fix rather than a wider column, because
	// none of those methods is routable here anyway: the request could only ever have ended in a
	// 404 or 405, and this way it does so without spending a row.
	if !isKnownHTTPMethod(signedMethod) {
		return nil, fmt.Errorf("%w: signed for %q", ErrMethodUnknown, signedMethod)
	}
	if !strings.EqualFold(signedMethod, req.Method) {
		return nil, fmt.Errorf("%w: signed for %s", ErrMethodMismatch, signedMethod)
	}

	// `nonce` tag. NIP-98 calls this one optional; this server requires it, and the requirement
	// is load-bearing rather than pedantic. The event id is a hash over
	// (pubkey, created_at, kind, tags, content) and the caller spends each id exactly once, so
	// for a client that omits the nonce the id is a pure function of the request: two identical
	// requests issued inside the same wall-clock second mint the same id, and the second is
	// refused by the replay guard - intermittently, and with the same opaque 401 a forged
	// signature gets. Refusing the credential up front turns that into a deterministic failure
	// with a reason the server log can name, which is the difference between a client bug that
	// is found in a minute and one that reads like a clock or key problem for an afternoon.
	// It is also symmetric with `u` and `method`, which are already required.
	nonce := tagValue(event.Tags, "nonce")
	if nonce == "" {
		return nil, fmt.Errorf("%w: every signed request must carry a unique nonce", ErrMissingNonce)
	}

	// The hex fields must be in NIP-01's canonical lower case. This is not tidiness: the event id
	// is the hash of a serialization that embeds `pubkey` as its literal characters, so an event
	// whose pubkey were upper case would have an id that a lower-cased copy of the same fields
	// does not reproduce. A caller stores those fields in order to re-derive the id later
	// (models/agent.AuditEvent), so one representation has to be *the* representation, and NIP-01
	// already picked it: "32-bytes lowercase hex-encoded". Cheap, so it goes before the signature.
	if !isLowerHex(event.ID, nostr.KeyHexLength) || !isLowerHex(event.PubKey, nostr.KeyHexLength) {
		return nil, fmt.Errorf("%w: id and pubkey must be %d lower-case hex characters", ErrInvalidEvent, nostr.KeyHexLength)
	}
	if !isLowerHex(event.Sig, nostr.SigHexLength) {
		return nil, fmt.Errorf("%w: sig must be %d lower-case hex characters", ErrInvalidEvent, nostr.SigHexLength)
	}

	// Signature last of the header-only checks: it is the expensive one, so a flood of events
	// that are stale or signed for the wrong URL costs no elliptic-curve work.
	//
	// nostr.Event.Verify re-derives the event id from the serialization and rejects an event
	// whose `id` field disagrees with it, so the EventID returned below is safe to persist as
	// the identity of this request.
	if err := event.Verify(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSignature, err)
	}

	return &SignedRequest{
		EventID:       event.ID,
		PubKey:        event.PubKey,
		Method:        signedMethod,
		RequestURL:    gotURL,
		CreatedAt:     createdAt,
		Nonce:         nonce,
		Kind:          event.Kind,
		Tags:          event.Tags,
		Content:       event.Content,
		Sig:           event.Sig,
		signedPayload: strings.ToLower(tagValue(event.Tags, "payload")),
	}, nil
}

// knownHTTPMethods is the set of request methods RFC 9110 defines, which is every method this
// server routes. The longest is 7 characters, comfortably inside the audit trail's `method`
// column; keeping the set and the column in step is the point of checking against it at all.
var knownHTTPMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodPost:    true,
	http.MethodPut:     true,
	http.MethodPatch:   true,
	http.MethodDelete:  true,
	http.MethodConnect: true,
	http.MethodOptions: true,
	http.MethodTrace:   true,
}

// isKnownHTTPMethod reports whether an already upper-cased method is one of the RFC 9110 set.
func isKnownHTTPMethod(method string) bool {
	return knownHTTPMethods[method]
}

// isLowerHex reports whether s is exactly n lower-case hexadecimal characters.
func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// VerifyPayload reads the request body, checks it against the `payload` tag the signature
// committed to, and records its hash on the SignedRequest.
//
// NIP-98 makes the `payload` tag optional when there is no body, so the rule is symmetric: a tag
// must match the body, and no tag requires an empty body. Without the second half a signed
// GET credential could be replayed as a POST carrying arbitrary content.
//
// maxBodySize bounds what will be held in memory to do that; anything larger is refused with
// ErrBodyTooLarge before it is read. A value of 0 or less means DefaultMaxBodySize - the limit
// is never absent, only chosen.
//
// It consumes req.Body and puts an equivalent reader back, so handlers downstream still see the
// full body.
func (s *SignedRequest) VerifyPayload(req *http.Request, maxBodySize int64) error {
	if maxBodySize <= 0 {
		maxBodySize = DefaultMaxBodySize
	}
	body, err := readAndRestoreBody(req, maxBodySize)
	if err != nil {
		return err
	}

	var payloadHash string
	if len(body) > 0 {
		sum := sha256.Sum256(body)
		payloadHash = hex.EncodeToString(sum[:])
	}

	switch {
	case s.signedPayload == "" && len(body) > 0:
		return fmt.Errorf("%w: request has a body but the event has no `payload` tag", ErrPayloadMismatch)
	case s.signedPayload != "" && s.signedPayload != payloadHash:
		return fmt.Errorf("%w: body hash does not match the `payload` tag", ErrPayloadMismatch)
	}

	s.PayloadHash = payloadHash
	return nil
}

// decodeBase64 accepts both standard and URL-safe base64, padded or not: NIP-98 clients in the
// wild emit all four and the choice carries no security meaning.
func decodeBase64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if raw, err := enc.DecodeString(s); err == nil {
			return raw, nil
		}
	}
	return nil, errors.New("not valid base64")
}

func tagValue(tags nostr.Tags, key string) string {
	tag := tags.Find(key)
	if len(tag) < 2 {
		return ""
	}
	return tag[1]
}

// readAndRestoreBody drains req.Body and replaces it with an equivalent reader.
func readAndRestoreBody(req *http.Request, maxBodySize int64) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	if req.ContentLength > maxBodySize {
		return nil, ErrBodyTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxBodySize+1))
	if err != nil {
		return nil, fmt.Errorf("%w: cannot read request body: %v", ErrInvalidEvent, err)
	}
	if int64(len(body)) > maxBodySize {
		return nil, ErrBodyTooLarge
	}
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return body, nil
}

// NormalizeURL renders an absolute URL in a form that can be compared byte-for-byte: lower-case
// scheme and host, no default port, non-empty path. It deliberately does not touch the query -
// parameter order is part of what the client signed.
//
// The implementation lives in modules/util because models/agent needs the identical answer to
// re-check a stored audit row against its signature; see util.NormalizeAbsoluteURL.
func NormalizeURL(raw string) (string, error) {
	return util.NormalizeAbsoluteURL(raw)
}
