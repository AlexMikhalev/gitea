// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package nostr implements the small slice of the Nostr protocol that Gitea needs to accept
// NIP-98 (kind 27235) HTTP authorization events: the NIP-01 event envelope with its canonical
// serialization and BIP-340 signature, and the NIP-19 bech32 renderings of raw keys.
//
// It is deliberately not a general Nostr client. The reference implementation
// (github.com/nbd-wtf/go-nostr) pulls a relay websocket client, a runtime-assembling JSON
// codec and a SQLite event store into whatever imports it, all of which would end up linked
// into the Gitea server binary for the sake of one signature check. What is actually needed is
// a struct, a deterministic serializer and one call into a BIP-340 library, which is what lives
// here. Interoperability is pinned by test vectors in event_test.go and bech32_test.go rather
// than by sharing code with any particular client.
package nostr

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// KeyHexLength is the length of a hex-encoded 32-byte secp256k1 x-only key, public or secret.
const KeyHexLength = 64

// Errors returned when an event cannot be verified. They are distinguished so callers can log a
// precise reason; NIP-98 callers must collapse them all into one opaque rejection.
var (
	ErrMalformedPubKey    = errors.New("event public key is not 32 bytes of hex")
	ErrMalformedSignature = errors.New("event signature is not 64 bytes of hex")
	ErrIDMismatch         = errors.New("event id does not match the event contents")
	ErrBadSignature       = errors.New("event signature is invalid")
)

// Timestamp is a NIP-01 `created_at`: whole seconds since the Unix epoch.
type Timestamp int64

// Now returns the current time as a NIP-01 timestamp.
func Now() Timestamp { return Timestamp(time.Now().Unix()) }

// Time renders the timestamp as a time.Time in UTC.
func (t Timestamp) Time() time.Time { return time.Unix(int64(t), 0).UTC() }

// Tag is one NIP-01 tag: a non-empty list of strings whose first element is the tag name.
type Tag []string

// Key returns the tag name, or "" for a malformed empty tag.
func (t Tag) Key() string {
	if len(t) < 1 {
		return ""
	}
	return t[0]
}

// Value returns the tag's first value, or "" when the tag carries none.
func (t Tag) Value() string {
	if len(t) < 2 {
		return ""
	}
	return t[1]
}

// Tags is the ordered list of an event's tags.
type Tags []Tag

// Find returns the first tag with the given name, or nil when there is none.
//
// "First" is load-bearing rather than incidental: an attacker who appends a second `u` tag to a
// signed event changes the signature and is rejected, but a verifier that consulted the *last*
// tag would read a different value than the one a signer building the event expects to be
// checked. Both ends therefore have to agree, and NIP-01 orders tags.
func (tags Tags) Find(key string) Tag {
	for _, tag := range tags {
		if tag.Key() == key {
			return tag
		}
	}
	return nil
}

// Event is a NIP-01 event.
type Event struct {
	ID        string    `json:"id"`
	PubKey    string    `json:"pubkey"`
	CreatedAt Timestamp `json:"created_at"`
	Kind      int       `json:"kind"`
	Tags      Tags      `json:"tags"`
	Content   string    `json:"content"`
	Sig       string    `json:"sig"`
}

// Serialize renders the event in the exact form NIP-01 hashes to produce the event id:
//
//	[0,<pubkey>,<created_at>,<kind>,<tags>,<content>]
//
// It is built by hand rather than with encoding/json because NIP-01 fixes the escaping rules
// (only the seven JSON short escapes, everything else verbatim) and encoding/json does not
// follow them - it escapes `<`, `>` and `&` as \u003c etc., which would yield a different id for
// the same event and break interoperability with every other implementation.
func (e *Event) Serialize() []byte {
	dst := make([]byte, 0, 256)
	dst = append(dst, "[0,\""...)
	dst = append(dst, e.PubKey...)
	dst = append(dst, "\","...)
	dst = strconv.AppendInt(dst, int64(e.CreatedAt), 10)
	dst = append(dst, ',')
	dst = strconv.AppendInt(dst, int64(e.Kind), 10)
	dst = append(dst, ',', '[')
	for i, tag := range e.Tags {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, '[')
		for j, item := range tag {
			if j > 0 {
				dst = append(dst, ',')
			}
			dst = escapeString(dst, item)
		}
		dst = append(dst, ']')
	}
	dst = append(dst, ']', ',')
	dst = escapeString(dst, e.Content)
	dst = append(dst, ']')
	return dst
}

// ComputeID returns the hex SHA-256 of the canonical serialization, i.e. the NIP-01 event id.
func (e *Event) ComputeID() string {
	sum := sha256.Sum256(e.Serialize())
	return hex.EncodeToString(sum[:])
}

// Sign computes the event id and signs it with a hex-encoded 32-byte secret key, filling in
// PubKey, ID and Sig.
func (e *Event) Sign(secretKeyHex string) error {
	raw, err := hex.DecodeString(secretKeyHex)
	if err != nil || len(raw) != 32 {
		return fmt.Errorf("secret key must be %d hex characters", KeyHexLength)
	}
	sk, pk := btcec.PrivKeyFromBytes(raw)
	e.PubKey = hex.EncodeToString(schnorr.SerializePubKey(pk))

	id, err := hex.DecodeString(e.ComputeID())
	if err != nil { // unreachable: ComputeID always returns hex
		return err
	}
	sig, err := schnorr.Sign(sk, id)
	if err != nil {
		return err
	}
	e.ID = hex.EncodeToString(id)
	e.Sig = hex.EncodeToString(sig.Serialize())
	return nil
}

// Verify checks that the event's id really is the hash of its contents and that its signature is
// a valid BIP-340 signature over that id by the event's own public key.
//
// Checking the id as well as the signature is what lets a caller store event.ID as a durable
// identifier: the signature alone only proves the *contents* are authentic, and an event whose
// `id` field had been rewritten would otherwise pass verification while being recorded under an
// attacker-chosen identifier.
func (e *Event) Verify() error {
	pubKeyBytes, err := hex.DecodeString(e.PubKey)
	if err != nil || len(pubKeyBytes) != 32 {
		return ErrMalformedPubKey
	}
	pubKey, err := schnorr.ParsePubKey(pubKeyBytes)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrMalformedPubKey, err)
	}

	sigBytes, err := hex.DecodeString(e.Sig)
	if err != nil || len(sigBytes) != schnorr.SignatureSize {
		return ErrMalformedSignature
	}
	sig, err := schnorr.ParseSignature(sigBytes)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrMalformedSignature, err)
	}

	computed := e.ComputeID()
	if e.ID != computed {
		return ErrIDMismatch
	}
	id, err := hex.DecodeString(computed)
	if err != nil { // unreachable: ComputeID always returns hex
		return err
	}

	if !sig.Verify(id, pubKey) {
		return ErrBadSignature
	}
	return nil
}

// PubKeyFromSecretKey derives the hex x-only public key of a hex secret key.
func PubKeyFromSecretKey(secretKeyHex string) (string, error) {
	raw, err := hex.DecodeString(secretKeyHex)
	if err != nil || len(raw) != 32 {
		return "", fmt.Errorf("secret key must be %d hex characters", KeyHexLength)
	}
	_, pk := btcec.PrivKeyFromBytes(raw)
	return hex.EncodeToString(schnorr.SerializePubKey(pk)), nil
}

// escapeString appends s to dst as a JSON string using exactly the escapes NIP-01 permits.
//
// Control characters below 0x20 that have no short escape still have to be escaped or the result
// is not JSON at all; \u00XX is what every implementation emits for those.
func escapeString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			dst = append(dst, '\\', '"')
		case c == '\\':
			dst = append(dst, '\\', '\\')
		case c >= 0x20:
			dst = append(dst, c)
		case c == 0x08:
			dst = append(dst, '\\', 'b')
		case c == 0x09:
			dst = append(dst, '\\', 't')
		case c == 0x0a:
			dst = append(dst, '\\', 'n')
		case c == 0x0c:
			dst = append(dst, '\\', 'f')
		case c == 0x0d:
			dst = append(dst, '\\', 'r')
		default:
			const hexDigits = "0123456789abcdef"
			dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0x0f])
		}
	}
	return append(dst, '"')
}
