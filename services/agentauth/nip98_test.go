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
	event := nostr.Event{
		Kind:      kind,
		CreatedAt: nostr.Timestamp(createdAt.Unix()),
		Tags:      tags,
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
	if err := signed.VerifyPayload(req); err != nil {
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
	require.NoError(t, signed.VerifyPayload(req))
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

// MaxBodySize is declared as a bound; check that it actually binds, both when the caller
// advertises an oversized body and when it lies about the length and streams one anyway.
func TestVerifyPayloadRejectsAnOversizedBody(t *testing.T) {
	header := encodeHeader(t, signedEvent(t, EventKind, frozenNow, authTags(testURL, "POST", "x")))

	t.Run("declared Content-Length over the cap", func(t *testing.T) {
		counting := &countingReader{inner: strings.NewReader("x")}
		req, err := http.NewRequest(http.MethodPost, testURL, counting)
		require.NoError(t, err)
		req.Header.Set("Authorization", header)
		req.ContentLength = MaxBodySize + 1

		signed, err := VerifyCredential(req, Options{ExpectedURL: testURL, Now: frozenNow})
		require.NoError(t, err)
		assert.ErrorIs(t, signed.VerifyPayload(req), ErrBodyTooLarge)
		assert.Zero(t, counting.n, "an over-length body should be refused on the header alone")
	})

	t.Run("undeclared body longer than the cap", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, testURL, io.LimitReader(zeroReader{}, MaxBodySize+10))
		require.NoError(t, err)
		req.Header.Set("Authorization", header)
		req.ContentLength = -1

		signed, err := VerifyCredential(req, Options{ExpectedURL: testURL, Now: frozenNow})
		require.NoError(t, err)
		assert.ErrorIs(t, signed.VerifyPayload(req), ErrBodyTooLarge)
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
