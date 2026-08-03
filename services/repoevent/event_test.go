// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repoevent

import (
	"sort"
	"testing"

	activities_model "code.gitea.io/gitea/models/activities"
	issues_model "code.gitea.io/gitea/models/issues"
	"code.gitea.io/gitea/modules/commitstatus"
	"code.gitea.io/gitea/modules/timeutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ev(kind Kind, id, created int64) *Event {
	return &Event{Kind: kind, SourceID: id, CreatedUnix: timeutil.TimeStamp(created)}
}

func TestCompareEventsOrdersNewestFirst(t *testing.T) {
	older := ev(KindAction, 1, 100)
	newer := ev(KindAction, 2, 200)
	assert.Negative(t, compareEvents(newer, older))
	assert.Positive(t, compareEvents(older, newer))
	assert.Zero(t, compareEvents(older, older))
}

func TestCompareEventsBreaksTiesTotally(t *testing.T) {
	// Five sources routinely stamp the same second. If the tie-break were not a total order the
	// keyset cursor built from it could not name a unique position.
	same := []*Event{
		ev(KindStatus, 5, 100),
		ev(KindAction, 9, 100),
		ev(KindComment, 3, 100),
		ev(KindComment, 7, 100),
		ev(KindAgentAudit, 1, 100),
		ev(KindReview, 2, 100),
	}
	sort.SliceStable(same, func(i, j int) bool { return compareEvents(same[i], same[j]) < 0 })

	got := make([]string, 0, len(same))
	for _, e := range same {
		got = append(got, string(e.Kind))
	}
	assert.Equal(t, []string{"action", "agent_audit", "comment", "comment", "review", "status"}, got)
	// Within one kind at one second, higher ids come first, matching created_unix DESC.
	assert.EqualValues(t, 7, same[2].SourceID)
	assert.EqualValues(t, 3, same[3].SourceID)

	// No two distinct events compare equal.
	for i := range same {
		for j := range same {
			if i != j {
				assert.NotZero(t, compareEvents(same[i], same[j]), "%d vs %d", i, j)
			}
		}
	}
}

func TestAllKindsIsInComparisonOrder(t *testing.T) {
	sorted := make([]Kind, len(AllKinds))
	copy(sorted, AllKinds)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	assert.Equal(t, sorted, AllKinds, "AllKinds must be listed in the order Kind values compare")
}

func TestCursorRoundTrip(t *testing.T) {
	e := &Event{Kind: KindReview, SourceID: 42, CreatedUnix: timeutil.TimeStamp(1764547200)}
	c := e.Cursor()

	decoded, err := DecodeCursor(c.Encode())
	require.NoError(t, err)
	require.NotNil(t, decoded)
	assert.Equal(t, c, *decoded)
}

func TestDecodeCursorEmptyIsStartOfStream(t *testing.T) {
	decoded, err := DecodeCursor("")
	require.NoError(t, err)
	assert.Nil(t, decoded)
}

func TestDecodeCursorRejectsGarbage(t *testing.T) {
	for name, raw := range map[string]string{
		"not base64":   "!!!!",
		"not json":     "aGVsbG8",                                  // "hello"
		"unknown kind": Cursor{Kind: "wiki", SourceID: 1}.Encode(), // kind not in AllKinds
		"no source id": Cursor{Kind: KindAction, SourceID: 0}.Encode(),
		"negative id":  Cursor{Kind: KindAction, SourceID: -3}.Encode(),
	} {
		t.Run(name, func(t *testing.T) {
			decoded, err := DecodeCursor(raw)
			assert.Error(t, err)
			assert.Nil(t, decoded)
		})
	}
}

func TestKindFromString(t *testing.T) {
	for _, k := range AllKinds {
		got, ok := KindFromString(string(k))
		assert.True(t, ok)
		assert.Equal(t, k, got)
	}
	_, ok := KindFromString("issue")
	assert.False(t, ok)
	_, ok = KindFromString("")
	assert.False(t, ok)
}

// The adapters project five upstream enums whose numbering they do not control. These assertions
// are here so that a renumbering upstream breaks a test rather than quietly changing which rows a
// repository's stream shows.
func TestUpstreamEnumsHaveNotMoved(t *testing.T) {
	assert.EqualValues(t, 1, activities_model.ActionCreateRepo)
	assert.EqualValues(t, 27, activities_model.ActionAutoMergePullRequest)

	assert.EqualValues(t, 0, issues_model.CommentTypeComment)
	assert.EqualValues(t, 21, issues_model.CommentTypeCode)
	assert.EqualValues(t, 22, issues_model.CommentTypeReview)
	assert.Equal(t, []issues_model.CommentType{
		issues_model.CommentTypeComment,
		issues_model.CommentTypeCode,
		issues_model.CommentTypeReview,
	}, contentBearingCommentTypes)

	// ReviewTypePending is the zero value, which is why fetchReviews excludes it explicitly
	// rather than relying on a non-zero "published" marker.
	assert.EqualValues(t, 0, issues_model.ReviewTypePending)
	assert.EqualValues(t, 1, issues_model.ReviewTypeApprove)
	assert.EqualValues(t, 3, issues_model.ReviewTypeReject)

	// CommitStatusState is a string enum, so a status event's payload carries the state itself.
	assert.Equal(t, "pending", string(commitstatus.CommitStatusPending))
	assert.Equal(t, "success", string(commitstatus.CommitStatusSuccess))
	assert.Equal(t, "failure", string(commitstatus.CommitStatusFailure))
}
