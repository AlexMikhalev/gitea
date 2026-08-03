// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package nostr

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Vectors produced by the reference implementation (github.com/nbd-wtf/go-nostr/nip19 v0.52.3),
// not by the code under test: an npub is what a human copies between Gitea and some other Nostr
// client, so a divergence here is a divergence from every other tool.
var nip19Vectors = []struct {
	hexKey string
	npub   string
	nsec   string
}{
	{
		hexKey: "f9308a019258c31049344f85f89d5229b531c845836f99b08601f113bce036f9",
		npub:   "npub1lycg5qvjtrp3qjf5f7zl382j9x6nrjz9sdhenvyxq8c3808qxmus6gq266",
		nsec:   "nsec1lycg5qvjtrp3qjf5f7zl382j9x6nrjz9sdhenvyxq8c3808qxmusk7ttu0",
	},
	{
		hexKey: "3bf0c63fcb93463407af97a5e5ee64fa883d107ef9e558472c4eb9aaaefa459d",
		npub:   "npub180cvv07tjdrrgpa0j7j7tmnyl2yr6yr7l8j4s3evf6u64th6gkwsyjh6w6",
		nsec:   "nsec180cvv07tjdrrgpa0j7j7tmnyl2yr6yr7l8j4s3evf6u64th6gkwsgyumg0",
	},
	{
		// A key with many leading zero bytes, which is where bit-regrouping bugs surface.
		hexKey: "0000000000000000000000000000000000000000000000000000000000000005",
		npub:   "npub1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqzsfj2hcx",
		nsec:   "nsec1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqzs9ypk7n",
	},
}

func TestNIP19MatchesTheReferenceImplementation(t *testing.T) {
	for _, v := range nip19Vectors {
		t.Run(v.hexKey[:8], func(t *testing.T) {
			npub, err := EncodePublicKey(v.hexKey)
			require.NoError(t, err)
			assert.Equal(t, v.npub, npub)

			nsec, err := EncodeSecretKey(v.hexKey)
			require.NoError(t, err)
			assert.Equal(t, v.nsec, nsec)

			gotPub, err := DecodePublicKey(v.npub)
			require.NoError(t, err)
			assert.Equal(t, v.hexKey, gotPub)

			gotSec, err := DecodeSecretKey(v.nsec)
			require.NoError(t, err)
			assert.Equal(t, v.hexKey, gotSec)
		})
	}
}

// npub and nsec share an encoding and differ only in their prefix, so decoding one as the other
// has to fail loudly - otherwise a pasted secret key would be silently registered as a public one.
func TestNIP19PrefixesAreNotInterchangeable(t *testing.T) {
	v := nip19Vectors[0]

	_, err := DecodePublicKey(v.nsec)
	assert.ErrorIs(t, err, ErrWrongPrefix)

	_, err = DecodeSecretKey(v.npub)
	assert.ErrorIs(t, err, ErrWrongPrefix)
}

func TestBech32DecodeRejectsBadInput(t *testing.T) {
	valid := nip19Vectors[0].npub

	cases := []struct {
		name string
		in   string
	}{
		{name: "empty", in: ""},
		{name: "no separator", in: strings.ReplaceAll(valid, "1", "q")},
		{name: "separator first", in: "1" + valid},
		{name: "corrupted checksum", in: valid[:len(valid)-1] + "q"},
		{name: "corrupted payload", in: "npub1qycg5qvjtrp3qjf5f7zl382j9x6nrjz9sdhenvyxq8c3808qxmus6gq266"},
		{name: "character outside the alphabet", in: valid[:len(valid)-1] + "b"},
		{name: "mixed case", in: "npub1LYCG5QVJTRP3QJF5F7ZL382J9X6NRJZ9SDHENVYXQ8C3808QXMUS6GQ266"},
		{name: "truncated", in: valid[:20]},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := bech32Decode(tc.in)
			assert.Error(t, err)
		})
	}
}

// BIP-173 makes the encoding case-insensitive as long as the case is uniform.
func TestBech32DecodeAcceptsUppercase(t *testing.T) {
	v := nip19Vectors[0]
	got, err := DecodePublicKey(strings.ToUpper(v.npub))
	require.NoError(t, err)
	assert.Equal(t, v.hexKey, got)
}

func TestEncodeRejectsKeysThatAreNot32Bytes(t *testing.T) {
	for _, bad := range []string{"", "abcd", "zz" + strings.Repeat("0", 62), strings.Repeat("0", 66)} {
		_, err := EncodePublicKey(bad)
		assert.Error(t, err, "input %q", bad)
		_, err = EncodeSecretKey(bad)
		assert.Error(t, err, "input %q", bad)
	}
}

// A well-formed bech32 string carrying the right prefix but the wrong number of bytes must not
// be mistaken for a key.
func TestDecodeRejectsWrongPayloadLength(t *testing.T) {
	short, err := bech32Encode(NpubPrefix, make([]byte, 16))
	require.NoError(t, err)

	_, err = DecodePublicKey(short)
	assert.ErrorIs(t, err, ErrInvalidBech32)
}

func TestConvertBitsRoundTrips(t *testing.T) {
	for _, size := range []int{1, 2, 31, 32, 65} {
		raw := make([]byte, size)
		for i := range raw {
			raw[i] = byte(i * 7)
		}

		five, err := convertBits(raw, 8, 5, true)
		require.NoError(t, err)
		eight, err := convertBits(five, 5, 8, false)
		require.NoError(t, err)
		assert.Equal(t, raw, eight, "size %d", size)
	}
}
