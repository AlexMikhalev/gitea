// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repoevent

import (
	"context"
	"slices"
	"sort"

	access_model "code.gitea.io/gitea/models/perm/access"
	repo_model "code.gitea.io/gitea/models/repo"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/timeutil"
	"code.gitea.io/gitea/modules/util"
)

const (
	// DefaultLimit is the page size when the caller does not ask for one.
	DefaultLimit = 20
	// MaxLimit caps the page size. Each source is read up to Limit rows, so the real ceiling on
	// work per request is MaxLimit times the number of kinds asked for.
	MaxLimit = 100
)

// ListOptions is one request for a page of the stream.
type ListOptions struct {
	Repo       *repo_model.Repository
	Doer       *user_model.User
	Permission *access_model.Permission
	// Kinds narrows the stream to these sources. Empty means all of them.
	Kinds []Kind
	// Since and Until are nil when the caller did not give them; see rangeCond.
	Since   *timeutil.TimeStamp
	Until   *timeutil.TimeStamp
	ActorID int64
	Query   string
	Cursor  *Cursor
	Limit   int
}

func (opts *ListOptions) wants(k Kind) bool {
	if len(opts.Kinds) == 0 {
		return true
	}
	return slices.Contains(opts.Kinds, k)
}

// List returns one page of a repository's event stream and the cursor for the next page, or nil
// when the stream is exhausted.
//
// The five sources are read in sequence rather than concurrently. They share the request's
// database session, and a page of at most MaxLimit rows per source is not where the time goes;
// fanning out would trade a correctness property - one session, one transaction's view of the five
// tables - for latency that is not the bottleneck.
func List(ctx context.Context, opts *ListOptions) ([]*Event, *Cursor, error) {
	if opts.Repo == nil || opts.Permission == nil {
		return nil, nil, util.NewInvalidArgumentErrorf("repository and permission are required")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}

	fetchOpts := &fetchOptions{
		Repo:       opts.Repo,
		Doer:       opts.Doer,
		Permission: opts.Permission,
		Since:      opts.Since,
		Until:      opts.Until,
		ActorID:    opts.ActorID,
		Query:      opts.Query,
		Cursor:     opts.Cursor,
		Limit:      limit,
	}

	var sources [][]*Event
	for _, a := range adapters {
		if !opts.wants(a.kind) {
			continue
		}
		events, err := a.fetch(ctx, fetchOpts)
		if err != nil {
			return nil, nil, err
		}
		sources = append(sources, events)
	}

	page, next := mergeEvents(sources, limit)
	return page, next, nil
}

// mergeEvents merges the per-source pages into one page of at most limit events and returns the
// cursor for the next page, or nil when the stream is exhausted.
//
// It is separate from List because it is the part with no database in it: the ordering, the
// truncation and the decision about whether more events exist are all decidable from the slices
// alone, and are worth testing without fixtures.
//
// "More events exist" has two independent causes and both have to be checked. The merged list
// overflowing limit is the obvious one. The other is a source that filled its own limit: it has
// more to give even when none of its rows survived the merge, and a page that ended the stream
// there would silently drop everything behind it.
func mergeEvents(sources [][]*Event, limit int) ([]*Event, *Cursor) {
	merged := []*Event{}
	sourceExhausted := true
	for _, events := range sources {
		if len(events) >= limit {
			sourceExhausted = false
		}
		merged = append(merged, events...)
	}

	sort.SliceStable(merged, func(i, j int) bool { return compareEvents(merged[i], merged[j]) < 0 })

	overflowed := len(merged) > limit
	if overflowed {
		merged = merged[:limit]
	}

	var next *Cursor
	if len(merged) > 0 && (overflowed || !sourceExhausted) {
		c := merged[len(merged)-1].Cursor()
		next = &c
	}
	return merged, next
}
