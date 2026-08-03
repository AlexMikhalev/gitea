// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"

	"code.gitea.io/gitea/models/db"
	"code.gitea.io/gitea/modules/timeutil"
	"code.gitea.io/gitea/modules/util"

	"xorm.io/builder"
)

// AuditEvent is one NIP-98 signed request that authenticated successfully.
//
// It records *authentication*, not authorization and not success. The row is written before the
// request reaches its handler, because a signed request that could not be recorded is refused
// rather than performed unlogged; the alternative - recording afterwards - would leave an agent
// action unaccounted for whenever the insert failed. What happened next is therefore not implied
// by the row's existence, and lives in ResponseStatus, which the audit middleware fills in once
// the handler has returned. A row with ResponseStatus 403 is an attempt that was refused; one
// with 0 is a request whose handler never completed.
//
// Every method is recorded, reads included: "which of this repository's data did the agent
// look at" is as much a part of an audit trail as "what did it change".
//
// The identifying columns are append-only by construction - this package exposes an insert, an
// outcome update that touches nothing else, and two reads. There is no delete path and no way to
// alter who did what.
type AuditEvent struct {
	ID int64 `xorm:"pk autoincr"`
	// RepoID is 0 for requests that are not repository-scoped, and also for requests that
	// failed before the router resolved a repository. The repo audit endpoint only ever
	// selects rows with a matching non-zero RepoID.
	RepoID      int64 `xorm:"INDEX NOT NULL DEFAULT 0"`
	AgentUserID int64 `xorm:"INDEX NOT NULL"`
	OwnerUserID int64 `xorm:"INDEX NOT NULL"`
	AgentKeyID  int64 `xorm:"INDEX NOT NULL"`
	// EventID is the NIP-01 id of the kind-27235 event, i.e. the SHA-256 of its serialization.
	// It is unique because agent_used_event refuses to spend the same id twice, so two rows
	// carrying one id would mean the replay guard had been bypassed.
	EventID string `xorm:"VARCHAR(64) UNIQUE NOT NULL"`
	PubKey  string `xorm:"VARCHAR(64) NOT NULL"`
	Method  string `xorm:"VARCHAR(10) NOT NULL"`
	// RequestURL is the absolute URL the signature committed to (the event's `u` tag).
	RequestURL string `xorm:"TEXT NOT NULL"`
	// PayloadHash is the hex SHA-256 of the request body, empty when the request had no body.
	PayloadHash string `xorm:"VARCHAR(64) NOT NULL DEFAULT ''"`
	// ResponseStatus is the HTTP status the request finally received, or 0 while it is still
	// in flight or if it never produced one.
	ResponseStatus int                `xorm:"NOT NULL DEFAULT 0"`
	CreatedUnix    timeutil.TimeStamp `xorm:"created INDEX NOT NULL"`
}

// TableName returns the database table name.
func (e *AuditEvent) TableName() string {
	return "agent_audit_event"
}

// InsertAuditEvent appends one row to the audit trail, filling in event.ID.
func InsertAuditEvent(ctx context.Context, event *AuditEvent) error {
	if event.AgentUserID <= 0 || event.AgentKeyID <= 0 {
		return util.NewInvalidArgumentErrorf("audit event needs an agent user and key")
	}
	if event.EventID == "" {
		return util.NewInvalidArgumentErrorf("audit event needs the nostr event id it came from")
	}
	return db.Insert(ctx, event)
}

// RecordAuditOutcome attaches the response status, and the repository the router resolved, to a
// row that InsertAuditEvent has already written.
//
// It updates those two columns and nothing else, and only while ResponseStatus is still 0, so
// the record of who signed what cannot be altered through this path and an outcome cannot be
// overwritten once observed.
func RecordAuditOutcome(ctx context.Context, id, repoID int64, responseStatus int) error {
	if id <= 0 {
		return util.NewInvalidArgumentErrorf("audit outcome needs a row id")
	}
	_, err := db.GetEngine(ctx).ID(id).Where("response_status = 0").
		Cols("repo_id", "response_status").
		Update(&AuditEvent{RepoID: repoID, ResponseStatus: responseStatus})
	return err
}

// FindAuditEventsOptions selects audit rows.
//
// Every field is an optional narrowing, so it is the caller's job to set at least one: the repo
// endpoint always sets RepoID, the owner endpoint always sets OwnerUserID. RepoID in particular
// only ever matches repository-scoped rows - a request that was not about a repository, or that
// was refused before the router resolved one, is stored with repo_id 0 and is reachable through
// OwnerUserID instead.
type FindAuditEventsOptions struct {
	db.ListOptions
	RepoID      int64
	AgentUserID int64
	OwnerUserID int64
}

// ToConds implements db.FindOptions.
func (opts FindAuditEventsOptions) ToConds() builder.Cond {
	cond := builder.NewCond()
	if opts.RepoID > 0 {
		cond = cond.And(builder.Eq{"repo_id": opts.RepoID})
	}
	if opts.AgentUserID > 0 {
		cond = cond.And(builder.Eq{"agent_user_id": opts.AgentUserID})
	}
	if opts.OwnerUserID > 0 {
		cond = cond.And(builder.Eq{"owner_user_id": opts.OwnerUserID})
	}
	return cond
}

// ToOrders implements db.FindOptionsOrder: newest first.
func (opts FindAuditEventsOptions) ToOrders() string {
	return "`agent_audit_event`.`id` DESC"
}

// FindAuditEvents returns a page of audit rows plus the total count.
func FindAuditEvents(ctx context.Context, opts FindAuditEventsOptions) ([]*AuditEvent, int64, error) {
	return db.FindAndCount[AuditEvent](ctx, opts)
}
