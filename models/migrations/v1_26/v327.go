// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v1_26

import (
	"code.gitea.io/gitea/modules/timeutil"

	"xorm.io/xorm"
)

// AgentKey binds a Nostr public key to an agent user and its human owner.
type AgentKey struct {
	ID          int64              `xorm:"pk autoincr"`
	OwnerUserID int64              `xorm:"INDEX NOT NULL"`
	AgentUserID int64              `xorm:"INDEX NOT NULL"`
	PubKey      string             `xorm:"VARCHAR(64) UNIQUE NOT NULL"`
	Npub        string             `xorm:"VARCHAR(80) NOT NULL"`
	Scope       string             `xorm:"VARCHAR(255) NOT NULL DEFAULT ''"`
	CreatedUnix timeutil.TimeStamp `xorm:"created NOT NULL"`
	RevokedUnix timeutil.TimeStamp `xorm:"NOT NULL DEFAULT 0"`
}

// TableName returns the database table name.
func (k *AgentKey) TableName() string {
	return "agent_key"
}

// AgentAuditEvent is the record of one NIP-98 signed request that authenticated successfully.
type AgentAuditEvent struct {
	ID             int64              `xorm:"pk autoincr"`
	RepoID         int64              `xorm:"INDEX NOT NULL DEFAULT 0"`
	AgentUserID    int64              `xorm:"INDEX NOT NULL"`
	OwnerUserID    int64              `xorm:"INDEX NOT NULL"`
	AgentKeyID     int64              `xorm:"INDEX NOT NULL"`
	EventID        string             `xorm:"VARCHAR(64) UNIQUE NOT NULL"`
	PubKey         string             `xorm:"VARCHAR(64) NOT NULL"`
	Method         string             `xorm:"VARCHAR(10) NOT NULL"`
	RequestURL     string             `xorm:"TEXT NOT NULL"`
	PayloadHash    string             `xorm:"VARCHAR(64) NOT NULL DEFAULT ''"`
	ResponseStatus int                `xorm:"NOT NULL DEFAULT 0"`
	CreatedUnix    timeutil.TimeStamp `xorm:"created INDEX NOT NULL"`
}

// TableName returns the database table name.
func (e *AgentAuditEvent) TableName() string {
	return "agent_audit_event"
}

// AgentUsedEvent is the replay guard: one row per NIP-98 event id that has been spent.
type AgentUsedEvent struct {
	ID          int64              `xorm:"pk autoincr"`
	EventID     string             `xorm:"VARCHAR(64) UNIQUE NOT NULL"`
	ExpiresUnix timeutil.TimeStamp `xorm:"INDEX NOT NULL"`
}

// TableName returns the database table name.
func (e *AgentUsedEvent) TableName() string {
	return "agent_used_event"
}

// AddAgentIdentity adds the `user.is_agent` flag plus the agent key, audit and replay-guard
// tables. x.Sync is additive and idempotent, so re-running the migration is a no-op.
func AddAgentIdentity(x *xorm.Engine) error {
	type User struct {
		IsAgent bool `xorm:"NOT NULL DEFAULT false"`
	}

	if err := x.Sync(new(User)); err != nil {
		return err
	}
	return x.Sync(new(AgentKey), new(AgentAuditEvent), new(AgentUsedEvent))
}
