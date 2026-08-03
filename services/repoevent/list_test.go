// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repoevent

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMergeEventsOrdersAcrossSources(t *testing.T) {
	page, next := mergeEvents([][]*Event{
		{ev(KindAction, 3, 300), ev(KindAction, 1, 100)},
		{ev(KindComment, 4, 400), ev(KindComment, 2, 200)},
	}, 10)

	require.Len(t, page, 4)
	assert.Equal(t, []int64{4, 3, 2, 1}, ids(page))
	assert.Nil(t, next, "nothing was truncated and no source filled its limit")
}

func TestMergeEventsIsStableAcrossSources(t *testing.T) {
	// Two sources at the same second: the kind tie-break decides, not the order the adapters ran.
	page, _ := mergeEvents([][]*Event{
		{ev(KindStatus, 1, 100)},
		{ev(KindAction, 2, 100)},
	}, 10)
	assert.Equal(t, []Kind{KindAction, KindStatus}, kinds(page))
}

func TestMergeEventsKeepsGlobalTopN(t *testing.T) {
	// A noisy source must not starve a quiet one: every source offers its newest rows for the
	// same n slots, and the merge keeps whichever are globally newest.
	noisy := []*Event{
		ev(KindAction, 10, 1000), ev(KindAction, 9, 900), ev(KindAction, 8, 800),
	}
	quiet := []*Event{ev(KindReview, 1, 950)}

	page, next := mergeEvents([][]*Event{noisy, quiet}, 3)
	require.Len(t, page, 3)
	assert.Equal(t, []Kind{KindAction, KindReview, KindAction}, kinds(page))
	assert.Equal(t, []int64{10, 1, 9}, ids(page))

	require.NotNil(t, next, "a row was dropped, so the stream continues")
	assert.Equal(t, Cursor{CreatedUnix: 900, Kind: KindAction, SourceID: 9}, *next)
}

func TestMergeEventsCursorWhenSourceFilledItsLimitButLostTheMerge(t *testing.T) {
	// The merged list did not overflow, so an overflow check alone would call the stream
	// finished - while the second source still holds rows nobody has seen.
	full := []*Event{ev(KindComment, 5, 10), ev(KindComment, 4, 9)}
	page, next := mergeEvents([][]*Event{{}, full}, 2)

	require.Len(t, page, 2)
	require.NotNil(t, next)
	assert.Equal(t, Cursor{CreatedUnix: 9, Kind: KindComment, SourceID: 4}, *next)
}

func TestMergeEventsEmptyStream(t *testing.T) {
	page, next := mergeEvents(nil, 10)
	assert.Empty(t, page)
	assert.NotNil(t, page, "an empty stream serialises as [] rather than null")
	assert.Nil(t, next)

	page, next = mergeEvents([][]*Event{{}, {}, {}}, 10)
	assert.Empty(t, page)
	assert.Nil(t, next)
}

func TestMergeEventsSingleSource(t *testing.T) {
	page, next := mergeEvents([][]*Event{{ev(KindStatus, 2, 20), ev(KindStatus, 1, 10)}}, 10)
	assert.Equal(t, []int64{2, 1}, ids(page))
	assert.Nil(t, next)
}

// TestMergeEventsPagesExactlyOnce walks a fixed stream page by page the way a client does, and
// asserts that following next_cursor visits every event once - the property the whole keyset design
// exists to provide.
func TestMergeEventsPagesExactlyOnce(t *testing.T) {
	all := []*Event{
		ev(KindAction, 4, 400), ev(KindAction, 3, 300), ev(KindAction, 1, 100),
		ev(KindComment, 9, 400), ev(KindComment, 8, 250), ev(KindComment, 7, 100),
		ev(KindReview, 2, 300),
	}

	seen := map[string]int{}
	var cursor *Cursor
	for range 10 {
		sources := [][]*Event{
			after(all, KindAction, cursor, 2),
			after(all, KindComment, cursor, 2),
			after(all, KindReview, cursor, 2),
		}
		events, next := mergeEvents(sources, 2)
		for _, e := range events {
			seen[string(e.Kind)+":"+strconv.FormatInt(e.SourceID, 10)]++
		}
		if next == nil {
			break
		}
		cursor = next
	}

	assert.Len(t, seen, len(all), "every event was returned")
	for key, count := range seen {
		assert.Equal(t, 1, count, "%s was returned more than once", key)
	}
}

// after is the in-memory twin of keysetCond: the rows of one kind that sort strictly after the
// cursor, newest first, capped at limit.
func after(all []*Event, kind Kind, cursor *Cursor, limit int) []*Event {
	var out []*Event
	for _, e := range all {
		if e.Kind != kind {
			continue
		}
		if cursor != nil {
			probe := &Event{Kind: cursor.Kind, SourceID: cursor.SourceID, CreatedUnix: cursor.CreatedUnix}
			if compareEvents(e, probe) <= 0 {
				continue
			}
		}
		out = append(out, e)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func ids(events []*Event) []int64 {
	out := make([]int64, len(events))
	for i, e := range events {
		out[i] = e.SourceID
	}
	return out
}

func kinds(events []*Event) []Kind {
	out := make([]Kind, len(events))
	for i, e := range events {
		out[i] = e.Kind
	}
	return out
}
