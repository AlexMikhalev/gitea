// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package repoevent reads a repository's activity out of the five tables it is actually stored in
// - `action`, issue comments, reviews, commit statuses and the agent audit trail - and presents it
// as one stream ordered by time.
//
// It is read-side only. Nothing here writes an event, and there is no unified event table: the
// union is computed per request from the rows the five sources already hold, so an event is never
// stored twice and can never drift from the row it describes.
package repoevent

import (
	"encoding/base64"
	"fmt"
	"strings"

	"code.gitea.io/gitea/modules/json"
	"code.gitea.io/gitea/modules/timeutil"
	"code.gitea.io/gitea/modules/util"
)

// Kind names the source table an event was read from. It is part of the sort key and part of the
// cursor, so these strings are a wire format: renaming one invalidates every cursor in flight.
type Kind string

const (
	// KindAction is a row of `action`, the feed Gitea already keeps for the dashboard.
	KindAction Kind = "action"
	// KindAgentAudit is a row of `agent_audit_event`, one NIP-98 signed request (see models/agent).
	KindAgentAudit Kind = "agent_audit"
	// KindComment is a content-bearing issue or pull request comment.
	KindComment Kind = "comment"
	// KindReview is a published pull request review.
	KindReview Kind = "review"
	// KindStatus is a commit status reported by a CI system.
	KindStatus Kind = "status"
)

// AllKinds lists every kind, in the order Kind values compare - the tie-break the merge uses when
// two events share a timestamp. Sorted here so that order is a property of the list, not of the
// reader's memory of it.
var AllKinds = []Kind{KindAction, KindAgentAudit, KindComment, KindReview, KindStatus}

// KindFromString maps a `kinds=` query value to a Kind. The bool is false for anything that is not
// one of AllKinds, so an unknown kind can be rejected rather than quietly returning nothing.
func KindFromString(s string) (Kind, bool) {
	for _, k := range AllKinds {
		if string(k) == s {
			return k, true
		}
	}
	return "", false
}

// Event is one thing that happened in a repository, in the shape every source is projected into.
//
// SourceID is the primary key *within* Kind and is only unique together with it: comment 7 and
// review 7 are different events. The pair is what identifies an event, and it is what the cursor
// carries.
type Event struct {
	Kind        Kind
	SourceID    int64
	ActorID     int64
	CreatedUnix timeutil.TimeStamp
	RepoID      int64
	Title       string
	Payload     map[string]string
}

// Cursor is a position in the stream: the sort key of the last event already returned.
//
// It is the sort key rather than an offset because the stream is a five-way union. An OFFSET would
// make every source re-read and re-merge each skipped row on every page, and it would drift the
// moment any of the five inserted a row between two pages - the reader would see one event twice
// or miss one entirely. A key names a position that stays put no matter what is inserted.
type Cursor struct {
	CreatedUnix timeutil.TimeStamp `json:"c"`
	Kind        Kind               `json:"k"`
	SourceID    int64              `json:"s"`
}

// Cursor returns the position of this event in the stream.
func (e *Event) Cursor() Cursor {
	return Cursor{CreatedUnix: e.CreatedUnix, Kind: e.Kind, SourceID: e.SourceID}
}

// Encode renders the cursor as the opaque string handed to the client. Opaque is the contract: it
// is base64 of JSON only so that this package can change what a position is made of without every
// client that stored one having to agree.
func (c Cursor) Encode() string {
	raw, err := json.Marshal(c)
	if err != nil {
		// Cursor is three scalars; Marshal cannot fail on it. Returning an empty cursor here
		// would silently restart paging from the top, so fail loudly instead of quietly.
		panic(fmt.Sprintf("repoevent: cannot encode cursor: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// DecodeCursor parses a cursor produced by Encode. An empty string is not an error - it is the
// start of the stream - and yields a nil Cursor.
func DecodeCursor(s string) (*Cursor, error) {
	if s == "" {
		//nolint:nilnil // no cursor is the first page, not a failure: every caller passes the
		// result straight into ListOptions.Cursor, where nil already means "from the top".
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, util.NewInvalidArgumentErrorf("cursor is not valid base64")
	}
	var c Cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, util.NewInvalidArgumentErrorf("cursor is malformed")
	}
	if _, ok := KindFromString(string(c.Kind)); !ok {
		return nil, util.NewInvalidArgumentErrorf("cursor names an unknown kind %q", c.Kind)
	}
	if c.SourceID <= 0 {
		return nil, util.NewInvalidArgumentErrorf("cursor names an invalid source id")
	}
	return &c, nil
}

// compareEvents orders two events as the stream does: newest first, then by kind, then by source
// id descending. It returns a negative number when a comes first.
//
// The two tie-breaks are not cosmetic. Ordering by created_unix alone is not a total order - five
// sources routinely stamp the same second - and a keyset cursor over a non-total order either
// repeats rows or skips them. (kind, source_id) makes the key unique, because source_id is a
// primary key within its kind.
func compareEvents(a, b *Event) int {
	switch {
	case a.CreatedUnix > b.CreatedUnix:
		return -1
	case a.CreatedUnix < b.CreatedUnix:
		return 1
	}
	if c := strings.Compare(string(a.Kind), string(b.Kind)); c != 0 {
		return c
	}
	switch {
	case a.SourceID > b.SourceID:
		return -1
	case a.SourceID < b.SourceID:
		return 1
	}
	return 0
}
