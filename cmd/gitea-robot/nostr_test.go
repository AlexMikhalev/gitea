// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"code.gitea.io/gitea/modules/json"
	"code.gitea.io/gitea/modules/nostr"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A fixed keypair; a test vector, never to be used for anything real.
const (
	robotTestSecretHex  = "0000000000000000000000000000000000000000000000000000000000000005"
	robotTestSecretNsec = "nsec1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqzs9ypk7n"
)

// withEnv swaps the package-level credentials for the duration of one test. They are read from
// the environment at init time, so a test that wants a different pair has to set them directly.
func withEnv(t *testing.T, token, nostrKey string) {
	t.Helper()
	oldToken, oldKey := giteaToken, giteaNostrKey
	giteaToken, giteaNostrKey = token, nostrKey
	t.Cleanup(func() { giteaToken, giteaNostrKey = oldToken, oldKey })
}

// decodeAuthHeader parses an `Authorization: Nostr ...` value back into an event.
func decodeAuthHeader(t *testing.T, header string) nostr.Event {
	t.Helper()
	require.True(t, strings.HasPrefix(header, "Nostr "), "header was %q", header)
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, "Nostr "))
	require.NoError(t, err)
	var event nostr.Event
	require.NoError(t, json.Unmarshal(raw, &event))
	return event
}

// TestSetRequestAuthWithoutNostrKeyIsUnchanged pins the compatibility guarantee: with
// GITEA_NOSTR_KEY unset the robot must send exactly the bearer header it always sent.
func TestSetRequestAuthWithoutNostrKeyIsUnchanged(t *testing.T) {
	withEnv(t, "s3cr3t-token", "")

	req, err := http.NewRequest(http.MethodPost, "http://localhost:3000/api/v1/repos/o/r/issues", strings.NewReader(`{}`))
	require.NoError(t, err)
	require.NoError(t, setRequestAuth(req, `{}`))

	assert.Equal(t, "token s3cr3t-token", req.Header.Get("Authorization"))
}

func TestSetRequestAuthSignsWithNostrKey(t *testing.T) {
	withEnv(t, "s3cr3t-token", robotTestSecretHex)

	const url = "http://localhost:3000/api/v1/repos/o/r/issues"
	const body = `{"title":"hello"}`

	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	require.NoError(t, setRequestAuth(req, body))

	event := decodeAuthHeader(t, req.Header.Get("Authorization"))
	assert.Equal(t, nip98Kind, event.Kind)
	assert.Equal(t, url, event.Tags.Find("u").Value())
	assert.Equal(t, "POST", event.Tags.Find("method").Value())

	sum := sha256.Sum256([]byte(body))
	assert.Equal(t, hex.EncodeToString(sum[:]), event.Tags.Find("payload").Value())

	require.NoError(t, event.Verify())

	// The bearer token must not ride along: the whole point is that the signed event replaces it.
	assert.NotContains(t, req.Header.Get("Authorization"), "s3cr3t-token")
}

func TestNostrAuthHeaderOmitsPayloadTagWithoutBody(t *testing.T) {
	header, err := nostrAuthHeader(robotTestSecretHex, "GET", "http://localhost:3000/api/v1/version", "")
	require.NoError(t, err)

	event := decodeAuthHeader(t, header)
	assert.Empty(t, event.Tags.Find("payload"), "NIP-98 makes the payload tag optional when there is no body")
}

// Reads are signed too. If they were not, the PAT would still have to exist on the box and be
// sent on every GET, so an attacker with access to either would still hold an unscoped
// credential and signing the mutations would have bought nothing.
func TestApiGetSendsSignedHeader(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	url := server.URL + "/api/v1/repos/o/r/issues"

	t.Run("signed", func(t *testing.T) {
		// Deliberately no token at all: with a Nostr key configured the robot must never need
		// one, which is the whole point of configuring one.
		withEnv(t, "", robotTestSecretHex)

		assert.JSONEq(t, `{"ok":true}`, apiGet(url))

		event := decodeAuthHeader(t, gotAuth)
		assert.Equal(t, url, event.Tags.Find("u").Value())
		assert.Equal(t, "GET", event.Tags.Find("method").Value())
		assert.Empty(t, event.Tags.Find("payload"), "a GET has no body to commit to")
		require.NoError(t, event.Verify())
	})

	t.Run("unsigned", func(t *testing.T) {
		withEnv(t, "s3cr3t-token", "")

		assert.JSONEq(t, `{"ok":true}`, apiGet(url))
		assert.Equal(t, "token s3cr3t-token", gotAuth)
	})
}

// TestNostrAuthHeaderIsUniquePerCall pins the fix for the one failure mode a deterministic
// signature guarantees: every other input to the event is a pure function of the request and the
// current whole second, so without a nonce the same request repeated inside one second would
// produce a byte-identical event id and the server would refuse the second as a replay. Nothing
// in the CLI serialises calls, so this is the ordinary case for any batch script.
func TestNostrAuthHeaderIsUniquePerCall(t *testing.T) {
	const (
		url  = "http://localhost:3000/api/v1/repos/o/r/issues"
		body = `{"title":"exactly the same request"}`
	)

	seen := make(map[string]bool, 32)
	for range 32 {
		header, err := nostrAuthHeader(robotTestSecretHex, "POST", url, body)
		require.NoError(t, err)

		event := decodeAuthHeader(t, header)
		require.NoError(t, event.Verify())
		assert.Len(t, event.Tags.Find("nonce").Value(), 32, "the nonce must be 16 bytes of hex")

		// The signature must still commit to the request itself; the nonce is additive.
		assert.Equal(t, url, event.Tags.Find("u").Value())
		assert.Equal(t, "POST", event.Tags.Find("method").Value())
		sum := sha256.Sum256([]byte(body))
		assert.Equal(t, hex.EncodeToString(sum[:]), event.Tags.Find("payload").Value())

		assert.False(t, seen[event.ID], "event id %s was produced twice", event.ID)
		seen[event.ID] = true
	}
}

func TestNostrSecretKey(t *testing.T) {
	t.Run("hex passes through, lower-cased", func(t *testing.T) {
		got, err := nostrSecretKey(strings.ToUpper(robotTestSecretHex))
		require.NoError(t, err)
		assert.Equal(t, robotTestSecretHex, got)
	})

	t.Run("nsec is decoded to hex", func(t *testing.T) {
		got, err := nostrSecretKey(robotTestSecretNsec)
		require.NoError(t, err)
		assert.Equal(t, robotTestSecretHex, got)
	})

	for _, bad := range []string{"", "deadbeef", "nsec1notavalidnsecatall", strings.Repeat("z", 64)} {
		t.Run("rejects "+bad, func(t *testing.T) {
			_, err := nostrSecretKey(bad)
			assert.Error(t, err)
		})
	}
}

// TestApiPostSafeSendsSignedHeader exercises the header all the way through the real request
// path, against a server that checks what actually arrived on the wire.
func TestApiPostSafeSendsSignedHeader(t *testing.T) {
	// Deliberately not JSON: the `payload` tag commits to the exact bytes, so nothing on this
	// path may re-encode them and the assertions below have to be byte comparisons.
	const body = "raw robot payload, byte-for-byte"

	var gotAuth, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	url := server.URL + "/api/v1/repos/o/r/issues/1/dependencies"

	t.Run("signed", func(t *testing.T) {
		withEnv(t, "s3cr3t-token", robotTestSecretHex)

		_, err := apiPostSafe(url, body)
		require.NoError(t, err)
		assert.Equal(t, body, gotBody, "signing must not consume the body")

		event := decodeAuthHeader(t, gotAuth)
		assert.Equal(t, url, event.Tags.Find("u").Value())
		assert.Equal(t, "POST", event.Tags.Find("method").Value())
		sum := sha256.Sum256([]byte(body))
		assert.Equal(t, hex.EncodeToString(sum[:]), event.Tags.Find("payload").Value())
		require.NoError(t, event.Verify())
	})

	t.Run("unsigned", func(t *testing.T) {
		withEnv(t, "s3cr3t-token", "")

		_, err := apiPostSafe(url, body)
		require.NoError(t, err)
		assert.Equal(t, "token s3cr3t-token", gotAuth)
		assert.Equal(t, body, gotBody)
	})
}
