// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package agent stores the Nostr identities that agent users sign their API mutations with
// (NIP-98, see services/agentauth) and the append-only audit trail of accepted signed requests.
package agent

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	auth_model "code.gitea.io/gitea/models/auth"
	"code.gitea.io/gitea/models/db"
	"code.gitea.io/gitea/modules/nostr"
	"code.gitea.io/gitea/modules/timeutil"
	"code.gitea.io/gitea/modules/util"
)

const (
	// PubKeyHexLength is the length of a hex-encoded 32-byte secp256k1 x-only public key.
	PubKeyHexLength = nostr.KeyHexLength

	// NpubPrefix is the NIP-19 bech32 prefix (including its separator) of a bare public key.
	NpubPrefix = nostr.NpubPrefix + "1"
)

// Key binds a Nostr public key to an agent user and to the human who owns that agent.
//
// PubKey is the lookup key because that is what a NIP-98 event carries; Npub is the bech32
// (NIP-19) rendering of the same bytes and exists for display only. A key is never deleted:
// revocation sets RevokedUnix so that audit rows keep referring to a row that still exists.
type Key struct {
	ID int64 `xorm:"pk autoincr"`
	// OwnerUserID is the human who vouches for the agent - the NIP-OA ownership chain.
	OwnerUserID int64 `xorm:"INDEX NOT NULL"`
	// AgentUserID is the Gitea user the signed request is authenticated as.
	AgentUserID int64  `xorm:"INDEX NOT NULL"`
	PubKey      string `xorm:"VARCHAR(64) UNIQUE NOT NULL"`
	Npub        string `xorm:"VARCHAR(80) NOT NULL"`
	// Scope limits what the key may do, using exactly the vocabulary of a personal access
	// token. It is not optional: without it a signed request would be *more* powerful than a
	// PAT for the same user, because routers/api/v1's tokenRequiresScopes only constrains
	// requests that arrive carrying a scope. A key with no scope must never authenticate.
	Scope       auth_model.AccessTokenScope `xorm:"VARCHAR(255) NOT NULL DEFAULT ''"`
	CreatedUnix timeutil.TimeStamp          `xorm:"created NOT NULL"`
	RevokedUnix timeutil.TimeStamp          `xorm:"NOT NULL DEFAULT 0"`
}

// TableName returns the database table name.
func (k *Key) TableName() string {
	return "agent_key"
}

// IsRevoked reports whether the key has been revoked and must no longer authenticate anything.
func (k *Key) IsRevoked() bool {
	return k.RevokedUnix > 0
}

func init() {
	db.RegisterModel(new(Key))
	db.RegisterModel(new(AuditEvent))
	db.RegisterModel(new(UsedEvent))
}

// ErrAgentKeyNotExist is returned when no registered key matches the lookup.
type ErrAgentKeyNotExist struct {
	PubKey string
}

func (err ErrAgentKeyNotExist) Error() string {
	return fmt.Sprintf("agent key does not exist [pubkey: %s]", err.PubKey)
}

func (err ErrAgentKeyNotExist) Unwrap() error {
	return util.ErrNotExist
}

// IsErrAgentKeyNotExist reports whether err is an ErrAgentKeyNotExist.
func IsErrAgentKeyNotExist(err error) bool {
	_, ok := err.(ErrAgentKeyNotExist)
	return ok
}

// NormalizePubKey lower-cases and validates a 32-byte hex public key.
func NormalizePubKey(pubKey string) (string, error) {
	pubKey = strings.ToLower(strings.TrimSpace(pubKey))
	if len(pubKey) != PubKeyHexLength {
		return "", util.NewInvalidArgumentErrorf("nostr public key must be %d hex characters", PubKeyHexLength)
	}
	if _, err := hex.DecodeString(pubKey); err != nil {
		return "", util.NewInvalidArgumentErrorf("nostr public key is not valid hex")
	}
	return pubKey, nil
}

// PubKeyFromInput accepts either a 32-byte hex key or a NIP-19 npub and returns both renderings.
func PubKeyFromInput(input string) (pubKey, npub string, err error) {
	input = strings.TrimSpace(input)
	if strings.HasPrefix(strings.ToLower(input), NpubPrefix) {
		hexKey, decErr := nostr.DecodePublicKey(input)
		if decErr != nil {
			return "", "", util.NewInvalidArgumentErrorf("invalid npub")
		}
		// Re-encode rather than echoing the input: an npub differing from the canonical
		// rendering only by case would otherwise be stored verbatim and never match again.
		npub, err = nostr.EncodePublicKey(hexKey)
		if err != nil {
			return "", "", util.NewInvalidArgumentErrorf("invalid npub")
		}
		return hexKey, npub, nil
	}

	pubKey, err = NormalizePubKey(input)
	if err != nil {
		return "", "", err
	}
	npub, err = nostr.EncodePublicKey(pubKey)
	if err != nil {
		return "", "", util.NewInvalidArgumentErrorf("cannot encode npub for the given public key")
	}
	return pubKey, npub, nil
}

// RegisterKey inserts a new agent key. The caller is responsible for authorizing the binding.
func RegisterKey(ctx context.Context, key *Key) error {
	if key.OwnerUserID <= 0 || key.AgentUserID <= 0 {
		return util.NewInvalidArgumentErrorf("agent key needs both an owner and an agent user")
	}
	scope, err := key.Scope.Normalize()
	if err != nil {
		return util.NewInvalidArgumentErrorf("invalid agent key scope: %v", err)
	}
	if scope == "" {
		return util.NewInvalidArgumentErrorf("an agent key must have a scope")
	}
	key.Scope = scope
	normalized, err := NormalizePubKey(key.PubKey)
	if err != nil {
		return err
	}
	key.PubKey = normalized
	if key.Npub == "" {
		if key.Npub, err = nostr.EncodePublicKey(key.PubKey); err != nil {
			return util.NewInvalidArgumentErrorf("cannot encode npub for the given public key")
		}
	}
	return db.Insert(ctx, key)
}

// GetKeyByPubKey returns the registered key for a hex public key, revoked or not.
// Callers must check IsRevoked; the distinction matters for the audit trail.
func GetKeyByPubKey(ctx context.Context, pubKey string) (*Key, error) {
	normalized, err := NormalizePubKey(pubKey)
	if err != nil {
		return nil, err
	}
	key := new(Key)
	has, err := db.GetEngine(ctx).Where("pub_key = ?", normalized).Get(key)
	if err != nil {
		return nil, err
	}
	if !has {
		return nil, ErrAgentKeyNotExist{PubKey: normalized}
	}
	return key, nil
}

// HasActiveKeyForAgent reports whether an agent user still holds at least one unrevoked key.
//
// It is what decides whether the `user.is_agent` flag is still earned: revoking the last key has
// to take the flag away again, or the account would stay eligible for NIP-98 authentication
// forever and "revoke the key" would not fully undo enrolment.
func HasActiveKeyForAgent(ctx context.Context, agentUserID int64) (bool, error) {
	return db.GetEngine(ctx).Where("agent_user_id = ? AND revoked_unix = 0", agentUserID).Exist(new(Key))
}

// ListKeysByOwner returns every key registered by a given human owner, newest first.
func ListKeysByOwner(ctx context.Context, ownerUserID int64) ([]*Key, error) {
	keys := make([]*Key, 0, 8)
	return keys, db.GetEngine(ctx).Where("owner_user_id = ?", ownerUserID).OrderBy("id DESC").Find(&keys)
}

// GetKeyByID returns a registered key by its row id, revoked or not.
func GetKeyByID(ctx context.Context, id int64) (*Key, error) {
	key := new(Key)
	has, err := db.GetEngine(ctx).ID(id).Get(key)
	if err != nil {
		return nil, err
	}
	if !has {
		return nil, ErrAgentKeyNotExist{PubKey: fmt.Sprintf("id %d", id)}
	}
	return key, nil
}

// RevokeKey marks a key as revoked. Revoking an already-revoked key keeps the original timestamp.
func RevokeKey(ctx context.Context, id int64) error {
	_, err := db.GetEngine(ctx).ID(id).Where("revoked_unix = 0").
		Cols("revoked_unix").Update(&Key{RevokedUnix: timeutil.TimeStampNow()})
	return err
}
