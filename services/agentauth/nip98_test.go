// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agentauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"code.gitea.io/gitea/modules/json"
	"code.gitea.io/gitea/modules/nostr"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A fixed keypair, so the tests neither generate randomness nor depend on the network.
// This secret key is a test vector and must never be used for anything real.
const (
	testSecretKey = "0000000000000000000000000000000000000000000000000000000000000003"
	testURL       = "https://gitea.example.com/api/v1/repos/octo/hello/issues"
)

// frozenNow is the reference clock for every test in this file.
var frozenNow = time.Unix(1_800_000_000, 0).UTC()

// signedEvent builds and signs a kind-27235 event. Every field a test wants to corrupt is a
// parameter, so no test has to reach into the signing internals.
func signedEvent(t *testing.T, kind int, createdAt time.Time, tags nostr.Tags) nostr.Event {
	t.Helper()
	return signedEventWithContent(t, kind, createdAt, tags, "")
}

// signedEventWithContent is signedEvent plus the `content` field. It is separate because content
// is empty in a conformant NIP-98 event and only the size tests care about it: it is the largest
// client-controlled part of the event, and the part MaxCredentialLength exists to bound.
func signedEventWithContent(t *testing.T, kind int, createdAt time.Time, tags nostr.Tags, content string) nostr.Event {
	t.Helper()
	event := nostr.Event{
		Kind:      kind,
		CreatedAt: nostr.Timestamp(createdAt.Unix()),
		Tags:      tags,
		Content:   content,
	}
	require.NoError(t, event.Sign(testSecretKey))
	return event
}

// authTags builds the tag set a conformant client sends. The nonce is fixed rather than random:
// this package never spends an event id, so uniqueness buys nothing here, and a stable tag set
// keeps the signatures in these tests reproducible.
func authTags(u, method, body string) nostr.Tags {
	tags := nostr.Tags{
		nostr.Tag{"u", u},
		nostr.Tag{"method", method},
		nostr.Tag{"nonce", "0123456789abcdef"},
	}
	if body != "" {
		sum := sha256.Sum256([]byte(body))
		tags = append(tags, nostr.Tag{"payload", hex.EncodeToString(sum[:])})
	}
	return tags
}

func encodeHeader(t *testing.T, event nostr.Event) string {
	t.Helper()
	raw, err := json.Marshal(event)
	require.NoError(t, err)
	return "Nostr " + base64.StdEncoding.EncodeToString(raw)
}

// headerOfEncodedLength builds a valid, correctly signed credential whose base64 payload is
// exactly want characters long, by padding the event's `content` with the shortfall. Every other
// field of a signed event has a fixed width - the id, pubkey and sig are hex of a known length -
// and each padding character costs exactly one byte of JSON, so the fit is exact rather than
// approximate. That is what lets the limit be tested as a limit and not as an order of magnitude.
func headerOfEncodedLength(t *testing.T, want int) string {
	t.Helper()
	// base64 of n bytes is 4*ceil(n/3) characters, so an exact target must be a multiple of 4 and
	// is reached from exactly want/4*3 raw bytes.
	require.Zero(t, want%4, "an exactly-sized base64 payload has a length divisible by 4")

	tags := authTags(testURL, "GET", "")
	raw, err := json.Marshal(signedEventWithContent(t, EventKind, frozenNow, tags, ""))
	require.NoError(t, err)
	pad := want/4*3 - len(raw)
	require.Positive(t, pad, "target is smaller than an empty event")

	header := encodeHeader(t, signedEventWithContent(t, EventKind, frozenNow, tags, strings.Repeat("a", pad)))
	require.Len(t, strings.TrimPrefix(header, "Nostr "), want)
	return header
}

func newRequest(t *testing.T, method, rawURL, body, authHeader string) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, reader)
	require.NoError(t, err)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	return req
}

// verifyWholeRequest runs both halves of verification in the order a caller must. Production
// code interleaves a key lookup between them (see services/auth/nostr.go); tests that are not
// about that ordering want the whole thing.
func verifyWholeRequest(req *http.Request, opts Options) (*SignedRequest, error) {
	signed, err := VerifyCredential(req, opts)
	if err != nil {
		return nil, err
	}
	if err := signed.VerifyPayload(req, DefaultMaxBodySize); err != nil {
		return nil, err
	}
	return signed, nil
}

func TestVerify(t *testing.T) {
	const body = `{"title":"hello"}`

	cases := []struct {
		name string
		// build the request under test
		method string
		url    string
		body   string
		header func(t *testing.T) string
		// expectations
		wantErr error
	}{
		{
			name:   "happy path with a body",
			method: "POST",
			url:    testURL,
			body:   body,
			header: func(t *testing.T) string {
				return encodeHeader(t, signedEvent(t, EventKind, frozenNow, authTags(testURL, "POST", body)))
			},
		},
		{
			name:   "happy path without a body omits the payload tag",
			method: "GET",
			url:    testURL,
			header: func(t *testing.T) string {
				return encodeHeader(t, signedEvent(t, EventKind, frozenNow, authTags(testURL, "GET", "")))
			},
		},
		{
			name:   "url differing only by default port and case still matches",
			method: "GET",
			url:    testURL,
			header: func(t *testing.T) string {
				variant := "https://GITEA.example.com:443/api/v1/repos/octo/hello/issues"
				return encodeHeader(t, signedEvent(t, EventKind, frozenNow, authTags(variant, "GET", "")))
			},
		},
		{
			name:   "no authorization header at all",
			method: "GET",
			url:    testURL,
			header: func(t *testing.T) string { return "" },

			wantErr: ErrNotApplicable,
		},
		{
			name:   "a foreign authorization scheme is not ours",
			method: "GET",
			url:    testURL,
			header: func(t *testing.T) string { return "token 0123456789abcdef" },

			wantErr: ErrNotApplicable,
		},
		{
			name:   "bad signature",
			method: "POST",
			url:    testURL,
			body:   body,
			header: func(t *testing.T) string {
				event := signedEvent(t, EventKind, frozenNow, authTags(testURL, "POST", body))
				// Flip one hex digit of the signature.
				event.Sig = strings.Repeat("0", 8) + event.Sig[8:]
				return encodeHeader(t, event)
			},
			wantErr: ErrBadSignature,
		},
		{
			name:   "event signed for a different url",
			method: "POST",
			url:    testURL,
			body:   body,
			header: func(t *testing.T) string {
				other := "https://gitea.example.com/api/v1/repos/octo/other/issues"
				return encodeHeader(t, signedEvent(t, EventKind, frozenNow, authTags(other, "POST", body)))
			},
			wantErr: ErrURLMismatch,
		},
		{
			name:   "event signed for a different method",
			method: "POST",
			url:    testURL,
			body:   body,
			header: func(t *testing.T) string {
				return encodeHeader(t, signedEvent(t, EventKind, frozenNow, authTags(testURL, "DELETE", body)))
			},
			wantErr: ErrMethodMismatch,
		},
		// The audit trail stores the accepted method verbatim in a narrow column, so a method
		// outside the RFC 9110 set is refused here rather than recorded. `VERSION-CONTROL` is a
		// real RFC 3253 method and a legal RFC 9110 token, so net/http carries it happily; it is
		// also 15 characters, which is what makes it the case worth pinning.
		{
			name:   "a method the server does not route is refused",
			method: "VERSION-CONTROL",
			url:    testURL,
			header: func(t *testing.T) string {
				return encodeHeader(t, signedEvent(t, EventKind, frozenNow, authTags(testURL, "VERSION-CONTROL", "")))
			},
			wantErr: ErrMethodUnknown,
		},
		{
			name:   "stale created_at",
			method: "GET",
			url:    testURL,
			header: func(t *testing.T) string {
				old := frozenNow.Add(-DefaultClockSkew - time.Second)
				return encodeHeader(t, signedEvent(t, EventKind, old, authTags(testURL, "GET", "")))
			},
			wantErr: ErrStale,
		},
		{
			name:   "created_at too far in the future",
			method: "GET",
			url:    testURL,
			header: func(t *testing.T) string {
				future := frozenNow.Add(DefaultClockSkew + time.Second)
				return encodeHeader(t, signedEvent(t, EventKind, future, authTags(testURL, "GET", "")))
			},
			wantErr: ErrStale,
		},
		{
			name:   "body hash mismatch",
			method: "POST",
			url:    testURL,
			body:   body,
			header: func(t *testing.T) string {
				return encodeHeader(t, signedEvent(t, EventKind, frozenNow, authTags(testURL, "POST", `{"title":"other"}`)))
			},
			wantErr: ErrPayloadMismatch,
		},
		{
			name:   "body present but the event carries no payload tag",
			method: "POST",
			url:    testURL,
			body:   body,
			header: func(t *testing.T) string {
				return encodeHeader(t, signedEvent(t, EventKind, frozenNow, authTags(testURL, "POST", "")))
			},
			wantErr: ErrPayloadMismatch,
		},
		{
			name:   "wrong kind",
			method: "GET",
			url:    testURL,
			header: func(t *testing.T) string {
				return encodeHeader(t, signedEvent(t, 1, frozenNow, authTags(testURL, "GET", "")))
			},
			wantErr: ErrWrongKind,
		},
		{
			name:    "header is not base64",
			method:  "GET",
			url:     testURL,
			header:  func(t *testing.T) string { return "Nostr !!!not-base64!!!" },
			wantErr: ErrMalformedHeader,
		},
		{
			name:   "header is base64 but not JSON",
			method: "GET",
			url:    testURL,
			header: func(t *testing.T) string {
				return "Nostr " + base64.StdEncoding.EncodeToString([]byte("not json"))
			},
			wantErr: ErrInvalidEvent,
		},
		{
			name:   "missing u tag",
			method: "GET",
			url:    testURL,
			header: func(t *testing.T) string {
				return encodeHeader(t, signedEvent(t, EventKind, frozenNow, nostr.Tags{nostr.Tag{"method", "GET"}}))
			},
			wantErr: ErrInvalidEvent,
		},
		{
			name:   "missing method tag",
			method: "GET",
			url:    testURL,
			header: func(t *testing.T) string {
				return encodeHeader(t, signedEvent(t, EventKind, frozenNow, nostr.Tags{nostr.Tag{"u", testURL}}))
			},
			wantErr: ErrInvalidEvent,
		},
		// NIP-98 makes the nonce optional; this server does not. Without it the event id is a
		// pure function of the request, so a client repeating one request inside a second mints
		// the same id twice and the replay guard refuses the second - a 401 indistinguishable
		// from a forged signature, arriving only sometimes. Refusing here makes it deterministic
		// and gives the log a reason to name.
		{
			name:   "missing nonce tag",
			method: "GET",
			url:    testURL,
			header: func(t *testing.T) string {
				return encodeHeader(t, signedEvent(t, EventKind, frozenNow, nostr.Tags{
					nostr.Tag{"u", testURL},
					nostr.Tag{"method", "GET"},
				}))
			},
			wantErr: ErrMissingNonce,
		},
		{
			name:   "empty nonce tag",
			method: "GET",
			url:    testURL,
			header: func(t *testing.T) string {
				return encodeHeader(t, signedEvent(t, EventKind, frozenNow, nostr.Tags{
					nostr.Tag{"u", testURL},
					nostr.Tag{"method", "GET"},
					nostr.Tag{"nonce", ""},
				}))
			},
			wantErr: ErrMissingNonce,
		},
		// The nonce is bounded for the same reason the method is checked against a fixed set:
		// it is client-controlled and is written verbatim into a VARCHAR(255) audit column. A
		// nonce that does not fit either fails the insert - a 401 for a request that deserved
		// its ordinary response, after the event id has already been spent - or is truncated,
		// leaving a row whose `nonce` column disagrees with the tag its signature covers while
		// still verifying cleanly, since VerifyEvent cross-checks `method`, `u` and `payload`
		// but not the nonce.
		{
			name:   "nonce longer than the audit column",
			method: "GET",
			url:    testURL,
			header: func(t *testing.T) string {
				return encodeHeader(t, signedEvent(t, EventKind, frozenNow, nostr.Tags{
					nostr.Tag{"u", testURL},
					nostr.Tag{"method", "GET"},
					nostr.Tag{"nonce", strings.Repeat("a", MaxNonceLength+1)},
				}))
			},
			wantErr: ErrNonceTooLong,
		},
		// The bound is a bound and not a fencepost off it: a nonce that exactly fills the
		// column is a nonce the row can hold, so it is accepted.
		{
			name:   "nonce exactly at the limit",
			method: "GET",
			url:    testURL,
			header: func(t *testing.T) string {
				return encodeHeader(t, signedEvent(t, EventKind, frozenNow, nostr.Tags{
					nostr.Tag{"u", testURL},
					nostr.Tag{"method", "GET"},
					nostr.Tag{"nonce", strings.Repeat("a", MaxNonceLength)},
				}))
			},
		},
		// The same coupling again, for the whole credential rather than one tag. `content`, the
		// tag list and the `u` tag are client-controlled and are recorded verbatim in TEXT audit
		// columns - 65,535 bytes on MySQL - which are the ones AuditEvent.VerifyEvent re-derives
		// the event id from. Unbounded, the only ceiling is net/http's 1 MiB header limit, so a
		// registered key could sign an otherwise ordinary event carrying a ~768 KiB `content`:
		// the insert then fails on a strict database, after the event id has already been spent,
		// or truncates on a lax one and the authentic row reads as tampered forever after.
		{
			name:   "credential longer than the audit columns can hold",
			method: "GET",
			url:    testURL,
			header: func(t *testing.T) string {
				return headerOfEncodedLength(t, MaxCredentialLength+4)
			},
			wantErr: ErrCredentialTooLong,
		},
		// The bound is a bound and not a fencepost off it, exactly as for the nonce.
		{
			name:   "credential exactly at the limit",
			method: "GET",
			url:    testURL,
			header: func(t *testing.T) string {
				return headerOfEncodedLength(t, MaxCredentialLength)
			},
		},
		// The length is checked before the payload is decoded, which is the other half of why the
		// bound exists: everything in VerifyCredential runs before the signing key has been looked
		// up, so an unauthenticated caller must not be able to buy four base64 decode attempts and
		// a JSON unmarshal of ~768 KiB with one request. This payload is not valid base64, so a
		// check placed after the decode would report ErrMalformedHeader instead.
		{
			name:   "oversized credential is refused before it is decoded",
			method: "GET",
			url:    testURL,
			header: func(t *testing.T) string {
				return "Nostr " + strings.Repeat("!", MaxCredentialLength+1)
			},
			wantErr: ErrCredentialTooLong,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := newRequest(t, tc.method, tc.url, tc.body, tc.header(t))
			got, err := verifyWholeRequest(req, Options{ExpectedURL: testURL, Now: frozenNow})

			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, strings.ToUpper(tc.method), got.Method)
			assert.Len(t, got.PubKey, 64)
			assert.Len(t, got.EventID, 64)
			assert.Equal(t, frozenNow.Unix(), got.CreatedAt.Unix())
			if tc.body != "" {
				sum := sha256.Sum256([]byte(tc.body))
				assert.Equal(t, hex.EncodeToString(sum[:]), got.PayloadHash)
			} else {
				assert.Empty(t, got.PayloadHash)
			}
		})
	}
}

// The point of checking the method against a fixed set is that models/agent.AuditEvent stores it
// in a VARCHAR(10), and a row is written for every accepted credential. If the two ever drift, the
// symptom is not a rejected request: it is a 500 on a strict database, or a truncated column whose
// value no longer matches the `method` tag the signature covers - an authentic audit row that
// reads as forged. So assert the coupling here rather than trusting that nobody widens the set.
func TestAcceptedMethodsFitTheAuditColumn(t *testing.T) {
	// models/agent.AuditEvent.Method and models/migrations/v1_26/v327.go.
	const auditMethodColumnWidth = 10

	require.NotEmpty(t, knownHTTPMethods)
	for method := range knownHTTPMethods {
		assert.LessOrEqual(t, len(method), auditMethodColumnWidth,
			"method %q does not fit agent_audit_event.method; widen the column in a migration or drop the method", method)
		assert.Equal(t, strings.ToUpper(method), method, "the set is matched against an upper-cased tag")
	}
}

// The same coupling one column over: the accepted nonce has to fit agent_audit_event.nonce, for
// the same reason and with the same symptoms - a 500 on a strict database, or a truncated column
// that no longer matches the tag the signature covers. The bound and the column are declared in
// two packages, so assert they agree rather than trusting that a later widening of either
// remembers the other.
func TestAcceptedNoncesFitTheAuditColumn(t *testing.T) {
	// models/agent.AuditEvent.Nonce and models/migrations/v1_26/v327.go.
	const auditNonceColumnWidth = 255

	assert.LessOrEqual(t, MaxNonceLength, auditNonceColumnWidth,
		"a nonce this package accepts does not fit agent_audit_event.nonce; widen the column in a migration or lower the bound")
}

// And once more for the credential as a whole. agent_audit_event.request_url, .event_tags and
// .event_content are TEXT, which is 65,535 *bytes* on MySQL/MariaDB - the narrowest of the
// supported backends. A credential this package accepts must fit there whole, because in the worst
// case a single one of those columns carries nearly the entire event.
func TestAcceptedCredentialsFitTheAuditColumns(t *testing.T) {
	// models/agent.AuditEvent.RequestURL, .EventTags, .EventContent, all `xorm:"TEXT"`.
	const mysqlTextBytes = 65535

	// A base64 payload of n characters decodes to at most 3n/4 bytes of event.
	assert.LessOrEqual(t, MaxCredentialLength/4*3, mysqlTextBytes,
		"an event this package accepts does not fit the TEXT audit columns; widen them in a migration or lower the bound")

	// The other direction: a bound that no conformant client can meet would be just as much of a
	// bug, and a less visible one, so assert the headroom rather than only the ceiling.
	header := encodeHeader(t, signedEvent(t, EventKind, frozenNow, authTags(testURL, "POST", `{"title":"hello"}`)))
	assert.Less(t, len(strings.TrimPrefix(header, "Nostr ")), MaxCredentialLength/4,
		"an ordinary credential is close to the limit; the bound is too tight")
}

// The verifier has to read the body to hash it; a handler downstream must still see all of it.
// The body here is deliberately not JSON: NIP-98 commits to the exact bytes, so nothing on this
// path may re-encode them.
func TestVerifyRestoresBody(t *testing.T) {
	const body = "raw request bytes, byte-for-byte"
	header := encodeHeader(t, signedEvent(t, EventKind, frozenNow, authTags(testURL, "POST", body)))
	req := newRequest(t, "POST", testURL, body, header)

	_, err := verifyWholeRequest(req, Options{ExpectedURL: testURL, Now: frozenNow})
	require.NoError(t, err)

	got, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	assert.Equal(t, body, string(got))
}

// The whole reason verification is split in two is that buffering the body is the one step whose
// cost an unauthenticated caller chooses. Pin it: VerifyCredential must not read a single byte of
// the body, no matter how large the caller claims it is.
func TestVerifyCredentialDoesNotTouchTheBody(t *testing.T) {
	const body = "the verifier must not read this yet"
	header := encodeHeader(t, signedEvent(t, EventKind, frozenNow, authTags(testURL, "POST", body)))

	counting := &countingReader{inner: strings.NewReader(body)}
	req, err := http.NewRequest(http.MethodPost, testURL, counting)
	require.NoError(t, err)
	req.Header.Set("Authorization", header)
	req.ContentLength = int64(len(body))

	signed, err := VerifyCredential(req, Options{ExpectedURL: testURL, Now: frozenNow})
	require.NoError(t, err)
	assert.Zero(t, counting.n, "VerifyCredential read %d bytes of the request body", counting.n)
	assert.Empty(t, signed.PayloadHash, "the payload hash cannot be known before the body is read")

	// And the second half really does read it, so the first half is not silently doing nothing.
	require.NoError(t, signed.VerifyPayload(req, DefaultMaxBodySize))
	assert.NotZero(t, counting.n)
	assert.NotEmpty(t, signed.PayloadHash)
}

// A rejected signature must never reach the body either, even when the caller runs both halves.
func TestVerifyCredentialRejectsBeforeTheBodyOnABadSignature(t *testing.T) {
	const body = "never read"
	event := signedEvent(t, EventKind, frozenNow, authTags(testURL, "POST", body))
	event.Sig = strings.Repeat("0", 8) + event.Sig[8:]

	counting := &countingReader{inner: strings.NewReader(body)}
	req, err := http.NewRequest(http.MethodPost, testURL, counting)
	require.NoError(t, err)
	req.Header.Set("Authorization", encodeHeader(t, event))

	_, err = VerifyCredential(req, Options{ExpectedURL: testURL, Now: frozenNow})
	assert.ErrorIs(t, err, ErrBadSignature)
	assert.Zero(t, counting.n, "a request with an invalid signature was buffered anyway")
}

// The body cap is declared as a bound; check that it actually binds, both when the caller
// advertises an oversized body and when it lies about the length and streams one anyway.
func TestVerifyPayloadRejectsAnOversizedBody(t *testing.T) {
	header := encodeHeader(t, signedEvent(t, EventKind, frozenNow, authTags(testURL, "POST", "x")))

	t.Run("declared Content-Length over the cap", func(t *testing.T) {
		counting := &countingReader{inner: strings.NewReader("x")}
		req, err := http.NewRequest(http.MethodPost, testURL, counting)
		require.NoError(t, err)
		req.Header.Set("Authorization", header)
		req.ContentLength = DefaultMaxBodySize + 1

		signed, err := VerifyCredential(req, Options{ExpectedURL: testURL, Now: frozenNow})
		require.NoError(t, err)
		assert.ErrorIs(t, signed.VerifyPayload(req, DefaultMaxBodySize), ErrBodyTooLarge)
		assert.Zero(t, counting.n, "an over-length body should be refused on the header alone")
	})

	t.Run("undeclared body longer than the cap", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, testURL, io.LimitReader(zeroReader{}, DefaultMaxBodySize+10))
		require.NoError(t, err)
		req.Header.Set("Authorization", header)
		req.ContentLength = -1

		signed, err := VerifyCredential(req, Options{ExpectedURL: testURL, Now: frozenNow})
		require.NoError(t, err)
		assert.ErrorIs(t, signed.VerifyPayload(req, DefaultMaxBodySize), ErrBodyTooLarge)
	})
}

// countingReader records how many bytes anyone pulled from a request body.
type countingReader struct {
	inner io.Reader
	n     int
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	r.n += n
	return n, err
}

// zeroReader is an endless source of NUL bytes, for exercising the size cap without allocating
// a test fixture of that size up front.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// The signature commits to the whole request URI, query string included.
func TestVerifyRejectsQueryStringTampering(t *testing.T) {
	signedURL := testURL + "?state=open"
	header := encodeHeader(t, signedEvent(t, EventKind, frozenNow, authTags(signedURL, "GET", "")))
	req := newRequest(t, "GET", testURL+"?state=closed", "", header)

	_, err := verifyWholeRequest(req, Options{ExpectedURL: testURL + "?state=closed", Now: frozenNow})
	assert.ErrorIs(t, err, ErrURLMismatch)
}

func TestHasCredential(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{"token abcdef", false},
		{"Basic dXNlcjpwYXNz", false},
		{"Nostr", false},
		{"Nostr ", false},
		{"Nostr eyJ9", true},
		{"nostr eyJ9", true}, // RFC 9110 says the scheme is case-insensitive
	}
	for _, tc := range cases {
		req := newRequest(t, "GET", testURL, "", tc.header)
		assert.Equal(t, tc.want, HasCredential(req), "header %q", tc.header)
	}
}

func TestNormalizeURL(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "https://Example.COM/a/b", want: "https://example.com/a/b"},
		{in: "https://example.com:443/a", want: "https://example.com/a"},
		{in: "http://example.com:80/a", want: "http://example.com/a"},
		{in: "https://example.com:8443/a", want: "https://example.com:8443/a"},
		{in: "https://example.com", want: "https://example.com/"},
		{in: "https://example.com/a?z=1&a=2", want: "https://example.com/a?z=1&a=2"},
		{in: "/api/v1/version", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tc := range cases {
		got, err := NormalizeURL(tc.in)
		if tc.wantErr {
			assert.Error(t, err, "input %q", tc.in)
			continue
		}
		require.NoError(t, err, "input %q", tc.in)
		assert.Equal(t, tc.want, got)
	}
}

func TestDecodeBase64AcceptsEveryCommonAlphabet(t *testing.T) {
	// A payload whose standard encoding contains both `+` and `/`, so the URL-safe variants
	// really are different strings.
	raw := []byte{0xfb, 0xff, 0xbe, 0x00, 0x11}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		got, err := decodeBase64(enc.EncodeToString(raw))
		require.NoError(t, err)
		assert.Equal(t, raw, got)
	}
}
