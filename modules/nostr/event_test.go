// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package nostr

import (
	"testing"
	"time"

	"code.gitea.io/gitea/modules/json"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A fixed keypair; a test vector, never to be used for anything real.
const (
	testSecretKey = "0000000000000000000000000000000000000000000000000000000000000003"
	testPubKey    = "f9308a019258c31049344f85f89d5229b531c845836f99b08601f113bce036f9"
)

// referenceEvent is the worked example from the NIP-01 text. It is one of this package's two
// interoperability anchors: the serialization and id below were produced by the reference
// implementation (github.com/nbd-wtf/go-nostr v0.52.3), not by the code under test, so if
// Serialize or ComputeID ever drift from NIP-01 these assertions fail. Recomputing the
// expectation from our own code would make the test vacuous.
var referenceEvent = Event{
	ID:        "752864cde483da01722ee1cd405be10c222a1c996c436ee1f3c156ab5592968b",
	PubKey:    "3bf0c63fcb93463407af97a5e5ee64fa883d107ef9e558472c4eb9aaaefa459d",
	CreatedAt: 1673347337,
	Kind:      1,
	Tags:      Tags{Tag{"e", "3da979448d9ba263864c4d6f14984c423a3838364ec255f03c7904b1ae77f206"}, Tag{"p", "bf2376e17ba4ec269d10fcc996a4746b451152be9031fa48e74553dde5526bce"}},
	Content:   "Walled gardens became prisons, and nostr is the first step towards tearing down the prison walls.",
}

// The canonical serialization of referenceEvent, as emitted by the reference implementation.
const referenceSerialization = `[0,"3bf0c63fcb93463407af97a5e5ee64fa883d107ef9e558472c4eb9aaaefa459d",1673347337,1,[["e","3da979448d9ba263864c4d6f14984c423a3838364ec255f03c7904b1ae77f206"],["p","bf2376e17ba4ec269d10fcc996a4746b451152be9031fa48e74553dde5526bce"]],"Walled gardens became prisons, and nostr is the first step towards tearing down the prison walls."]`

// referenceNIP98Event is the second anchor: a kind-27235 event signed by the reference
// implementation with testSecretKey. Our Verify must accept it byte-for-byte, which is what
// proves the id derivation and the BIP-340 verification agree with the rest of the ecosystem.
const referenceNIP98Event = `{"kind":27235,"id":"447415a65e58f7a416e6f8a0576484e34abebaec7513409a9e131d43937f8a2d","pubkey":"f9308a019258c31049344f85f89d5229b531c845836f99b08601f113bce036f9","created_at":1800000000,"tags":[["u","https://gitea.example.com/api/v1/version"],["method","GET"]],"content":"","sig":"a17b08ef24ebb5bf904e617b18ed773e61ac3e6e3a06c319db88343f7cea82cebb37f1c9cf1b6dfab68998f99ce600dc6e22357c2b1ca95c37e8763149966aac"}`

func TestSerializeMatchesTheNIP01Example(t *testing.T) {
	assert.Equal(t, referenceSerialization, string(referenceEvent.Serialize()))
	assert.Equal(t, referenceEvent.ID, referenceEvent.ComputeID())
}

// An event signed by a different implementation must verify here, and our own signer must
// reproduce it bit-for-bit from the same inputs.
func TestVerifyAcceptsAReferenceImplementationSignature(t *testing.T) {
	var event Event
	require.NoError(t, json.Unmarshal([]byte(referenceNIP98Event), &event))
	require.NoError(t, event.Verify())

	ours := Event{Kind: event.Kind, CreatedAt: event.CreatedAt, Tags: event.Tags, Content: event.Content}
	require.NoError(t, ours.Sign(testSecretKey))
	assert.Equal(t, event.PubKey, ours.PubKey)
	assert.Equal(t, event.ID, ours.ID)
	assert.Equal(t, event.Sig, ours.Sig)
}

// The NIP-01 escaping rules differ from encoding/json's defaults, and the difference changes the
// event id. Pin every case the spec enumerates plus the HTML characters encoding/json would
// mangle.
func TestSerializeEscapingFollowsNIP01(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{name: "html characters stay verbatim", content: `a<b>c&d`, want: `"a<b>c&d"`},
		{name: "double quote", content: `he said "hi"`, want: `"he said \"hi\""`},
		{name: "backslash", content: `a\b`, want: `"a\\b"`},
		{name: "line break", content: "a\nb", want: `"a\nb"`},
		{name: "carriage return", content: "a\rb", want: `"a\rb"`},
		{name: "tab", content: "a\tb", want: `"a\tb"`},
		{name: "backspace", content: "a\bb", want: `"a\bb"`},
		{name: "form feed", content: "a\fb", want: `"a\fb"`},
		{name: "other control characters use \\u00xx", content: "a\x01\x1fb", want: `"a\u0001\u001fb"`},
		{name: "utf-8 is not escaped", content: "héllo ☺", want: `"héllo ☺"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, string(escapeString(nil, tc.content)))
			// Whatever we emit still has to be JSON that round-trips to the input.
			var got string
			require.NoError(t, json.Unmarshal(escapeString(nil, tc.content), &got))
			assert.Equal(t, tc.content, got)
		})
	}
}

func TestSignThenVerifyRoundTrips(t *testing.T) {
	event := Event{
		Kind:      27235,
		CreatedAt: 1800000000,
		Tags:      Tags{Tag{"u", "https://gitea.example.com/api/v1/version"}, Tag{"method", "GET"}},
	}
	require.NoError(t, event.Sign(testSecretKey))

	assert.Equal(t, testPubKey, event.PubKey)
	assert.Len(t, event.ID, 64)
	assert.Len(t, event.Sig, 128)
	assert.Equal(t, event.ComputeID(), event.ID)
	assert.NoError(t, event.Verify())
}

// Signing is deterministic (BIP-340 with an RFC6979-derived nonce), which is what makes the
// signature a function of the request alone and lets a test pin an exact value.
func TestSignIsDeterministic(t *testing.T) {
	build := func() Event {
		return Event{Kind: 27235, CreatedAt: 1800000000, Tags: Tags{Tag{"method", "GET"}}}
	}
	first, second := build(), build()
	require.NoError(t, first.Sign(testSecretKey))
	require.NoError(t, second.Sign(testSecretKey))
	assert.Equal(t, first.Sig, second.Sig)
	assert.Equal(t, first.ID, second.ID)
}

func TestPubKeyFromSecretKey(t *testing.T) {
	got, err := PubKeyFromSecretKey(testSecretKey)
	require.NoError(t, err)
	assert.Equal(t, testPubKey, got)

	for _, bad := range []string{"", "abcd", "zz" + testSecretKey[2:]} {
		_, err := PubKeyFromSecretKey(bad)
		assert.Error(t, err, "input %q", bad)
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	signed := func(t *testing.T) Event {
		t.Helper()
		event := Event{
			Kind:      27235,
			CreatedAt: 1800000000,
			Tags:      Tags{Tag{"u", "https://gitea.example.com/api/v1/version"}, Tag{"method", "GET"}},
		}
		require.NoError(t, event.Sign(testSecretKey))
		return event
	}

	t.Run("a rewritten tag invalidates the signature", func(t *testing.T) {
		event := signed(t)
		event.Tags[0] = Tag{"u", "https://evil.example.net/api/v1/version"}
		event.ID = event.ComputeID() // recompute so the id check is not what rejects it
		assert.ErrorIs(t, event.Verify(), ErrBadSignature)
	})

	t.Run("a rewritten id is rejected even though the contents are authentic", func(t *testing.T) {
		event := signed(t)
		event.ID = "00000000000000000000000000000000000000000000000000000000deadbeef"
		assert.ErrorIs(t, event.Verify(), ErrIDMismatch)
	})

	t.Run("a rewritten created_at invalidates the signature", func(t *testing.T) {
		event := signed(t)
		event.CreatedAt = 1800000001
		event.ID = event.ComputeID()
		assert.ErrorIs(t, event.Verify(), ErrBadSignature)
	})

	t.Run("a foreign public key does not verify", func(t *testing.T) {
		event := signed(t)
		other, err := PubKeyFromSecretKey("0000000000000000000000000000000000000000000000000000000000000009")
		require.NoError(t, err)
		event.PubKey = other
		event.ID = event.ComputeID()
		assert.ErrorIs(t, event.Verify(), ErrBadSignature)
	})

	t.Run("malformed public key", func(t *testing.T) {
		event := signed(t)
		event.PubKey = "not hex"
		assert.ErrorIs(t, event.Verify(), ErrMalformedPubKey)
	})

	t.Run("malformed signature", func(t *testing.T) {
		event := signed(t)
		event.Sig = "abcd"
		assert.ErrorIs(t, event.Verify(), ErrMalformedSignature)
	})

	t.Run("empty event", func(t *testing.T) {
		assert.ErrorIs(t, (&Event{}).Verify(), ErrMalformedPubKey)
	})
}

func TestTagsFindReturnsTheFirstMatch(t *testing.T) {
	tags := Tags{Tag{"method", "GET"}, Tag{"u", "first"}, Tag{"u", "second"}}
	assert.Equal(t, "first", tags.Find("u").Value())
	assert.Equal(t, "GET", tags.Find("method").Value())
	assert.Nil(t, tags.Find("payload"))
	assert.Empty(t, tags.Find("payload").Value())
	// A malformed tag must not panic or be mistaken for a value.
	assert.Empty(t, Tags{Tag{}, Tag{"u"}}.Find("u").Value())
}

func TestTimestampRoundTrip(t *testing.T) {
	assert.Equal(t, int64(1800000000), Timestamp(1800000000).Time().Unix())
	// Now() has to track the wall clock, not merely agree with itself: a Timestamp that was
	// consistently wrong would sail through the freshness window on one side and fail it on
	// the other.
	assert.InDelta(t, time.Now().Unix(), Now().Time().Unix(), 1)
}

// The wire format is plain JSON with NIP-01's field names; a decoder elsewhere in the tree must
// be able to round-trip an event through it.
func TestEventJSONRoundTrip(t *testing.T) {
	event := Event{Kind: 27235, CreatedAt: 1800000000, Tags: Tags{Tag{"method", "POST"}}}
	require.NoError(t, event.Sign(testSecretKey))

	raw, err := json.Marshal(event)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"created_at":1800000000`)
	assert.Contains(t, string(raw), `"pubkey":"`)

	var got Event
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, event, got)
	assert.NoError(t, got.Verify())
}
