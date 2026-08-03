// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"fmt"
	"strings"

	"code.gitea.io/gitea/models/db"
	"code.gitea.io/gitea/modules/json"
	"code.gitea.io/gitea/modules/nostr"
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
//
// "By construction" is a statement about this package, not about the database, and anything with
// DB write access is outside it. So the row also carries the whole signed event: EventCreatedUnix,
// EventKind, Nonce, EventTags, EventContent and Sig, which together with PubKey are exactly the
// inputs of the NIP-01 serialization. That makes the row checkable against itself - VerifyEvent
// re-derives EventID from those columns and re-checks Sig against PubKey - rather than leaving
// EventID an opaque string that nothing binds to the key or to the recorded method and URL. An
// UPDATE that rewrites request_url or method now contradicts the signature it sits next to, and
// producing a row that does not is as hard as forging the agent's key. Method and RequestURL stay
// as their own columns because they are what the trail is read and filtered by; they are a
// projection *of* EventTags, and VerifyEvent is what says the projection is honest.
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
	// EventCreatedUnix is the event's own `created_at`, which is a signed value and not the same
	// thing as CreatedUnix below - that one is when this server wrote the row.
	EventCreatedUnix timeutil.TimeStamp `xorm:"NOT NULL DEFAULT 0"`
	// EventKind is the NIP-01 kind, 27235 for every row NIP-98 produces. Stored rather than
	// assumed so the row does not depend on a constant living in another package to be checked.
	EventKind int `xorm:"NOT NULL DEFAULT 0"`
	// Nonce is the event's `nonce` tag, pulled out of EventTags because it is the one tag an
	// operator reading the trail is likely to want to see or match on.
	Nonce string `xorm:"VARCHAR(255) NOT NULL DEFAULT ''"`
	// EventTags is the event's full tag list as NIP-01 JSON (`[["u","..."],["method","POST"],...]`).
	// The whole list, not a selection: the serialization hashes every tag, so anything dropped
	// here is an id that can no longer be re-derived.
	EventTags string `xorm:"TEXT NOT NULL DEFAULT ''"`
	// EventContent is the event's `content`, empty for every NIP-98 event seen in practice but
	// hashed all the same, so it is stored rather than assumed.
	EventContent string `xorm:"TEXT NOT NULL DEFAULT ''"`
	// Sig is the hex BIP-340 signature over EventID by PubKey. It is what makes the row evidence
	// rather than a claim: without it the trail records an id nobody can attribute.
	Sig string `xorm:"VARCHAR(128) NOT NULL DEFAULT ''"`
	// ResponseStatus is the HTTP status the request finally received, or 0 while it is still
	// in flight or if it never produced one.
	ResponseStatus int                `xorm:"NOT NULL DEFAULT 0"`
	CreatedUnix    timeutil.TimeStamp `xorm:"created INDEX NOT NULL"`
}

// TableName returns the database table name.
func (e *AuditEvent) TableName() string {
	return "agent_audit_event"
}

// EventTagsList decodes the stored tag list. An empty EventTags decodes to an empty list rather
// than to an error, so that a row from before the column existed is still readable - it simply
// cannot be verified.
func (e *AuditEvent) EventTagsList() ([][]string, error) {
	if e.EventTags == "" {
		return [][]string{}, nil
	}
	var tags [][]string
	if err := json.Unmarshal([]byte(e.EventTags), &tags); err != nil {
		return [][]string{}, fmt.Errorf("audit row %d has unreadable tags: %w", e.ID, err)
	}
	return tags, nil
}

// SignedEvent rebuilds the NIP-01 event this row was written from.
//
// It is the inverse of the projection InsertAuditEvent stores, and it is only meaningful because
// that projection is lossless: every field the serialization hashes has a column.
func (e *AuditEvent) SignedEvent() (*nostr.Event, error) {
	raw, err := e.EventTagsList()
	if err != nil {
		return nil, err
	}
	tags := make(nostr.Tags, len(raw))
	for i, tag := range raw {
		tags[i] = nostr.Tag(tag)
	}
	return &nostr.Event{
		ID:        e.EventID,
		PubKey:    e.PubKey,
		CreatedAt: nostr.Timestamp(e.EventCreatedUnix),
		Kind:      e.EventKind,
		Tags:      tags,
		Content:   e.EventContent,
		Sig:       e.Sig,
	}, nil
}

// VerifyEvent re-derives the event id from the row's own columns and checks the stored signature
// against the stored public key.
//
// This is what an auditor runs. A row that passes was produced by whoever holds the agent's
// secret key; a row that fails has been altered since it was written, whatever wrote the change.
// It deliberately does not consult agent_key: the question is whether the row is internally
// authentic, and answering it out of a table that the same actor could also have edited would
// give the answer away.
//
// Method and RequestURL are checked against the tags the signature actually covers, because those
// two columns are the ones a reader trusts and the ones an editor would go for; a row whose
// `method` column disagreed with its `method` tag would otherwise verify cleanly while lying.
func (e *AuditEvent) VerifyEvent() error {
	event, err := e.SignedEvent()
	if err != nil {
		return err
	}
	if err := event.Verify(); err != nil {
		return fmt.Errorf("audit row %d does not match its signed event: %w", e.ID, err)
	}
	if got := event.Tags.Find("method").Value(); !strings.EqualFold(got, e.Method) {
		return fmt.Errorf("audit row %d records method %q but the signature covers %q", e.ID, e.Method, got)
	}
	if got := event.Tags.Find("u").Value(); got != e.RequestURL {
		// The stored URL is the normalized form, so compare on that footing rather than
		// byte-for-byte against whatever casing the client sent.
		normalized, nErr := util.NormalizeAbsoluteURL(got)
		if nErr != nil || normalized != e.RequestURL {
			return fmt.Errorf("audit row %d records url %q but the signature covers %q", e.ID, e.RequestURL, got)
		}
	}
	return nil
}

// InsertAuditEvent appends one row to the audit trail, filling in event.ID.
//
// It refuses a row whose stored event fields do not re-derive EventID. That check is the only
// thing that keeps the trail's central promise from decaying quietly: if a future change to the
// caller dropped a tag or normalized a field on its way in, every row would still look complete
// and every row would fail verification, and nobody would find out until an auditor needed the
// answer. Re-deriving the id costs one SHA-256 over a few hundred bytes; the signature itself was
// already checked in services/agentauth and is not re-checked here.
func InsertAuditEvent(ctx context.Context, event *AuditEvent) error {
	if event.AgentUserID <= 0 || event.AgentKeyID <= 0 {
		return util.NewInvalidArgumentErrorf("audit event needs an agent user and key")
	}
	if event.EventID == "" {
		return util.NewInvalidArgumentErrorf("audit event needs the nostr event id it came from")
	}
	if event.Sig == "" {
		return util.NewInvalidArgumentErrorf("audit event needs the signature that authenticated it")
	}
	signed, err := event.SignedEvent()
	if err != nil {
		return util.NewInvalidArgumentErrorf("%v", err)
	}
	if computed := signed.ComputeID(); computed != event.EventID {
		return util.NewInvalidArgumentErrorf("audit event fields do not reproduce event id %s (got %s)", event.EventID, computed)
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
