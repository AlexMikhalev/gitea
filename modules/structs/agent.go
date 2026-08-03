// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package structs

import "time"

// CreateAgentKeyOption is the option for registering a Nostr key for an agent user.
// swagger:model
type CreateAgentKeyOption struct {
	// The Nostr public key, either as 32-byte hex or as a NIP-19 npub.
	//
	// required: true
	PublicKey string `json:"public_key" binding:"Required"`

	// What the key is permitted to do, in the same vocabulary as an access token's scopes
	// (for example "write:issue"). At least one is required: a key with no scope would be able
	// to do everything its user can, which is more than any personal access token may.
	//
	// required: true
	Scopes []string `json:"scopes" binding:"Required"`

	// The user the signed requests will be authenticated as. Defaults to the authenticated
	// user. Only a site administrator may name a different user.
	AgentUserID int64 `json:"agent_user_id"`
}

// AgentKey is a Nostr key registered to an agent user.
// swagger:model
type AgentKey struct {
	ID int64 `json:"id"`
	// The human who vouches for the agent (the NIP-OA ownership chain).
	OwnerUserID int64 `json:"owner_user_id"`
	// The user that signed requests are authenticated as.
	AgentUserID int64  `json:"agent_user_id"`
	PublicKey   string `json:"public_key"`
	Npub        string `json:"npub"`
	// What the key is permitted to do; the same scopes an access token carries.
	Scopes []string `json:"scopes"`
	// swagger:strfmt date-time
	Created time.Time `json:"created_at"`
	// Null while the key is still valid.
	// swagger:strfmt date-time
	Revoked *time.Time `json:"revoked_at"`
}

// AgentAuditEvent is one NIP-98 signed request that authenticated successfully.
//
// It records authentication, not authorization: the row is written before the request reaches
// its handler so that nothing an agent signs can happen unlogged. Whether the request was then
// permitted, and whether it worked, is in ResponseStatus.
//
// The signed event is returned in full - created_at, kind, nonce, tags, content and the
// signature - so that a client can re-derive EventID and check the signature against PublicKey
// without trusting this server's copy of the summary fields. An entry that cannot be verified
// that way was altered after it was written.
// swagger:model
type AgentAuditEvent struct {
	ID          int64  `json:"id"`
	RepoID      int64  `json:"repo_id"`
	AgentUserID int64  `json:"agent_user_id"`
	OwnerUserID int64  `json:"owner_user_id"`
	AgentKeyID  int64  `json:"agent_key_id"`
	EventID     string `json:"event_id"`
	PublicKey   string `json:"public_key"`
	Method      string `json:"method"`
	RequestURL  string `json:"request_url"`
	// Hex SHA-256 of the request body, empty when the request had no body.
	PayloadHash string `json:"payload_hash"`
	// The event's own created_at, in seconds since the Unix epoch. This is what the signature
	// covers, and is not the same as CreatedAt below, which is when the server stored the row.
	EventCreatedAt int64 `json:"event_created_at"`
	// The NIP-01 kind, 27235 for every NIP-98 authorization event.
	EventKind int `json:"event_kind"`
	// The event's nonce tag, which is what makes two otherwise identical requests distinct events.
	Nonce string `json:"nonce"`
	// The event's full NIP-01 tag list, in the order the signature covers.
	EventTags [][]string `json:"event_tags"`
	// The event's content, empty for a NIP-98 authorization event.
	EventContent string `json:"event_content"`
	// Hex BIP-340 signature over EventID by PublicKey.
	Signature string `json:"signature"`
	// The HTTP status the request finally received, or 0 if it never produced one.
	ResponseStatus int `json:"response_status"`
	// swagger:strfmt date-time
	Created time.Time `json:"created_at"`
}
