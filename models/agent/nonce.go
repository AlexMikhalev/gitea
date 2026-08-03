// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package agent

import (
	"context"
	"fmt"

	"code.gitea.io/gitea/models/db"
	"code.gitea.io/gitea/modules/timeutil"
	"code.gitea.io/gitea/modules/util"
)

// UsedEvent records that one NIP-98 event id has already been spent.
//
// Binding an event to a method, a URL and a body stops it being *repurposed*, but on its own it
// does not stop it being *repeated*: anyone who observes the header (a proxy log, a crash dump,
// a shared CI runner) can send the identical request again until created_at falls outside the
// clock-skew window, duplicating whatever mutation it carried. NIP-98 therefore expects the
// server to remember event ids, which is what this table is.
//
// Rows are worthless once the event they describe could no longer be accepted for freshness
// reasons, so ExpiresUnix carries that moment and PruneUsedEvents deletes what is past it. This
// is the one table in this package that is meant to be emptied.
type UsedEvent struct {
	ID int64 `xorm:"pk autoincr"`
	// EventID is the NIP-01 event id. The unique index is the entire mechanism: two concurrent
	// replays both attempt this insert and the database picks exactly one winner, which a
	// check-then-insert could not guarantee.
	EventID     string             `xorm:"VARCHAR(64) UNIQUE NOT NULL"`
	ExpiresUnix timeutil.TimeStamp `xorm:"INDEX NOT NULL"`
}

// TableName returns the database table name.
func (e *UsedEvent) TableName() string {
	return "agent_used_event"
}

// ErrEventReplayed is returned when an event id has already been spent.
type ErrEventReplayed struct {
	EventID string
}

func (err ErrEventReplayed) Error() string {
	return fmt.Sprintf("NIP-98 event has already been used [event_id: %s]", err.EventID)
}

func (err ErrEventReplayed) Unwrap() error {
	return util.ErrAlreadyExist
}

// IsErrEventReplayed reports whether err is an ErrEventReplayed.
func IsErrEventReplayed(err error) bool {
	_, ok := err.(ErrEventReplayed)
	return ok
}

// ConsumeEvent spends an event id, returning ErrEventReplayed if it has been spent before.
//
// It must be called outside a transaction the caller still needs: on PostgreSQL a failed insert
// aborts the surrounding transaction, and this function deliberately runs a query *after* a
// failed insert to tell a duplicate apart from a genuine database error without matching on
// driver-specific error strings.
func ConsumeEvent(ctx context.Context, eventID string, expires timeutil.TimeStamp) error {
	if eventID == "" {
		return util.NewInvalidArgumentErrorf("cannot consume an empty event id")
	}

	insertErr := db.Insert(ctx, &UsedEvent{EventID: eventID, ExpiresUnix: expires})
	if insertErr == nil {
		return nil
	}

	exists, err := db.GetEngine(ctx).Where("event_id = ?", eventID).Exist(&UsedEvent{})
	if err != nil {
		// The lookup failed too, so we cannot say why the insert failed. Report the insert
		// error: the caller rejects the request either way, and this is the closer cause.
		return insertErr
	}
	if exists {
		return ErrEventReplayed{EventID: eventID}
	}
	return insertErr
}

// PruneUsedEvents deletes spent event ids that can no longer be replayed anyway, and returns how
// many rows it removed.
func PruneUsedEvents(ctx context.Context, before timeutil.TimeStamp) (int64, error) {
	return db.GetEngine(ctx).Where("expires_unix < ?", before).Delete(&UsedEvent{})
}
