// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package nostr

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// NIP-19 human-readable prefixes for the two bare-key entities Gitea deals with.
const (
	NpubPrefix = "npub"
	NsecPrefix = "nsec"
)

// Errors returned by the NIP-19 helpers.
var (
	ErrInvalidBech32 = errors.New("not a valid bech32 string")
	ErrWrongPrefix   = errors.New("bech32 string has the wrong prefix")
)

// bech32Charset is the BIP-173 data alphabet. It excludes `1`, `b`, `i` and `o` so that the
// characters most often confused when a key is copied by hand cannot appear in the data part.
const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

// maxBech32Length caps what the decoder will look at. BIP-173 specifies 90 characters; NIP-19
// entities routinely exceed that (an npub is 63, but nprofile with relay hints is far longer),
// so the limit here exists only to bound work on hostile input, not to enforce the BIP.
const maxBech32Length = 2048

// EncodePublicKey renders a hex x-only public key as a NIP-19 npub.
func EncodePublicKey(pubKeyHex string) (string, error) {
	return encodeKey(NpubPrefix, pubKeyHex)
}

// EncodeSecretKey renders a hex secret key as a NIP-19 nsec.
func EncodeSecretKey(secretKeyHex string) (string, error) {
	return encodeKey(NsecPrefix, secretKeyHex)
}

// DecodePublicKey decodes a NIP-19 npub into a lower-case hex x-only public key.
func DecodePublicKey(npub string) (string, error) {
	return decodeKey(NpubPrefix, npub)
}

// DecodeSecretKey decodes a NIP-19 nsec into a lower-case hex secret key.
func DecodeSecretKey(nsec string) (string, error) {
	return decodeKey(NsecPrefix, nsec)
}

func encodeKey(prefix, keyHex string) (string, error) {
	raw, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(keyHex)))
	if err != nil || len(raw) != 32 {
		return "", fmt.Errorf("%s key must be %d hex characters", prefix, KeyHexLength)
	}
	return bech32Encode(prefix, raw)
}

func decodeKey(prefix, encoded string) (string, error) {
	gotPrefix, raw, err := bech32Decode(encoded)
	if err != nil {
		return "", err
	}
	if gotPrefix != prefix {
		return "", fmt.Errorf("%w: wanted %q, got %q", ErrWrongPrefix, prefix, gotPrefix)
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("%w: %s must carry 32 bytes, got %d", ErrInvalidBech32, prefix, len(raw))
	}
	return hex.EncodeToString(raw), nil
}

// bech32Encode encodes 8-bit data under a human-readable prefix, per BIP-173.
func bech32Encode(hrp string, data []byte) (string, error) {
	converted, err := convertBits(data, 8, 5, true)
	if err != nil {
		return "", err
	}
	checksum := bech32Checksum(hrp, converted)

	var sb strings.Builder
	sb.Grow(len(hrp) + 1 + len(converted) + len(checksum))
	sb.WriteString(hrp)
	sb.WriteByte('1')
	for _, b := range append(converted, checksum...) {
		sb.WriteByte(bech32Charset[b])
	}
	return sb.String(), nil
}

// bech32Decode decodes a bech32 string into its prefix and 8-bit data, per BIP-173.
func bech32Decode(encoded string) (hrp string, data []byte, err error) {
	encoded = strings.TrimSpace(encoded)
	if len(encoded) < 8 || len(encoded) > maxBech32Length {
		return "", nil, fmt.Errorf("%w: implausible length %d", ErrInvalidBech32, len(encoded))
	}
	// BIP-173 forbids mixed case precisely so that the checksum cannot be made to pass by
	// re-casing a string; lower-casing first would silently accept what the BIP rejects.
	if strings.ToLower(encoded) != encoded && strings.ToUpper(encoded) != encoded {
		return "", nil, fmt.Errorf("%w: mixed case", ErrInvalidBech32)
	}
	encoded = strings.ToLower(encoded)

	sep := strings.LastIndexByte(encoded, '1')
	if sep < 1 || sep+7 > len(encoded) {
		return "", nil, fmt.Errorf("%w: no separator", ErrInvalidBech32)
	}
	hrp = encoded[:sep]
	for i := 0; i < len(hrp); i++ {
		if hrp[i] < 33 || hrp[i] > 126 {
			return "", nil, fmt.Errorf("%w: prefix has an out-of-range character", ErrInvalidBech32)
		}
	}

	values := make([]byte, 0, len(encoded)-sep-1)
	for i := sep + 1; i < len(encoded); i++ {
		idx := strings.IndexByte(bech32Charset, encoded[i])
		if idx < 0 {
			return "", nil, fmt.Errorf("%w: %q is not in the bech32 alphabet", ErrInvalidBech32, encoded[i])
		}
		values = append(values, byte(idx))
	}

	if bech32Polymod(append(bech32HRPExpand(hrp), values...)) != 1 {
		return "", nil, fmt.Errorf("%w: checksum does not match", ErrInvalidBech32)
	}

	// The last six data characters are the checksum, not payload.
	converted, err := convertBits(values[:len(values)-6], 5, 8, false)
	if err != nil {
		return "", nil, err
	}
	return hrp, converted, nil
}

func bech32Checksum(hrp string, data []byte) []byte {
	values := append(bech32HRPExpand(hrp), data...)
	values = append(values, 0, 0, 0, 0, 0, 0)
	polymod := bech32Polymod(values) ^ 1
	checksum := make([]byte, 6)
	for i := range checksum {
		checksum[i] = byte((polymod >> uint(5*(5-i))) & 31)
	}
	return checksum
}

func bech32HRPExpand(hrp string) []byte {
	out := make([]byte, 0, len(hrp)*2+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]>>5)
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]&31)
	}
	return out
}

func bech32Polymod(values []byte) uint32 {
	generator := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := range 5 {
			if (top>>uint(i))&1 == 1 {
				chk ^= generator[i]
			}
		}
	}
	return chk
}

// convertBits regroups a byte slice from `from`-bit groups into `to`-bit groups.
func convertBits(data []byte, from, to uint8, pad bool) ([]byte, error) {
	var acc uint32
	var bits uint8
	maxV := uint32(1)<<to - 1
	out := make([]byte, 0, len(data)*int(from)/int(to)+1)

	for _, value := range data {
		if uint32(value)>>from != 0 {
			return nil, fmt.Errorf("%w: value %d overflows %d bits", ErrInvalidBech32, value, from)
		}
		acc = acc<<from | uint32(value)
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte(acc>>bits&maxV))
		}
	}

	if pad {
		if bits > 0 {
			out = append(out, byte(acc<<(to-bits)&maxV))
		}
	} else if bits >= from || acc<<(to-bits)&maxV != 0 {
		// Left-over bits must be fewer than one input group and must be zero, or the encoder
		// smuggled data into the padding.
		return nil, fmt.Errorf("%w: invalid padding", ErrInvalidBech32)
	}
	return out, nil
}
