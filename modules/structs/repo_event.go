// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package structs

import "time"

// RepoEvent is one entry of a repository's unified event stream.
//
// The stream is a union of five tables with five shapes, so the fields here are only the ones all
// five have: what kind of thing it was, which row of that kind, who did it and when. Everything
// that is specific to one kind lives in Payload rather than as a field that is empty for the other
// four.
type RepoEvent struct {
	// Kind is the source the event was read from: action, agent_audit, comment, review or status.
	Kind string `json:"kind"`
	// SourceID is the row's primary key within Kind, and is only unique together with it.
	SourceID int64  `json:"source_id"`
	ActorID  int64  `json:"actor_id"`
	RepoID   int64  `json:"repo_id"`
	Title    string `json:"title"`
	// Payload carries the fields specific to this kind. Its keys depend on Kind and are not a
	// fixed set.
	Payload map[string]string `json:"payload,omitempty"`
	// swagger:strfmt date-time
	Created time.Time `json:"created"`
}

// RepoEventList is a page of a repository's event stream.
//
// NextCursor is opaque and empty at the end of the stream. It is a position in the stream rather
// than a page number, so a client that keeps following it sees every event exactly once even while
// new ones are being written.
type RepoEventList struct {
	Data       []*RepoEvent `json:"data"`
	NextCursor string       `json:"next_cursor"`
}
