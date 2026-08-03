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
	"net/url"
	"strings"
	"time"

	"code.gitea.io/gitea/modules/json"
	"code.gitea.io/gitea/modules/nostr"
)

const (
	// AuthorizationScheme is the `Authorization:` scheme NIP-98 reserves.
	AuthorizationScheme = "Nostr"

	// EventKind is the NIP-98 "HTTP Auth" event kind.
	EventKind = 27235

	// DefaultClockSkew is how far the event's created_at may be from the server clock in
	// either direction. NIP-98 suggests 60 seconds.
	DefaultClockSkew = 60 * time.Second

	// MaxBodySize caps the request body this package will hash. Only requests that actually
	// carry a NIP-98 header are read here, so this does not constrain any other upload path.
	MaxBodySize = 32 << 20 // 32 MiB
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
	if tagValue(event.Tags, "nonce") == "" {
		return nil, fmt.Errorf("%w: every signed request must carry a unique nonce", ErrMissingNonce)
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
		EventID:       strings.ToLower(event.ID),
		PubKey:        strings.ToLower(event.PubKey),
		Method:        signedMethod,
		RequestURL:    gotURL,
		CreatedAt:     createdAt,
		signedPayload: strings.ToLower(tagValue(event.Tags, "payload")),
	}, nil
}

// VerifyPayload reads the request body, checks it against the `payload` tag the signature
// committed to, and records its hash on the SignedRequest.
//
// NIP-98 makes the `payload` tag optional when there is no body, so the rule is symmetric: a tag
// must match the body, and no tag requires an empty body. Without the second half a signed
// GET credential could be replayed as a POST carrying arbitrary content.
//
// It consumes req.Body and puts an equivalent reader back, so handlers downstream still see the
// full body.
func (s *SignedRequest) VerifyPayload(req *http.Request) error {
	body, err := readAndRestoreBody(req)
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
func readAndRestoreBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	if req.ContentLength > MaxBodySize {
		return nil, ErrBodyTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, MaxBodySize+1))
	if err != nil {
		return nil, fmt.Errorf("%w: cannot read request body: %v", ErrInvalidEvent, err)
	}
	if len(body) > MaxBodySize {
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
func NormalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Host)
	if scheme == "" || host == "" {
		return "", errors.New("url is not absolute")
	}
	switch {
	case scheme == "http" && strings.HasSuffix(host, ":80"):
		host = strings.TrimSuffix(host, ":80")
	case scheme == "https" && strings.HasSuffix(host, ":443"):
		host = strings.TrimSuffix(host, ":443")
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	out := scheme + "://" + host + path
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out, nil
}
