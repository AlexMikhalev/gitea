// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"net/http"
	"strings"
	"time"

	user_model "code.gitea.io/gitea/models/user"
	api "code.gitea.io/gitea/modules/structs"
	"code.gitea.io/gitea/modules/timeutil"
	"code.gitea.io/gitea/modules/util"
	"code.gitea.io/gitea/services/context"
	"code.gitea.io/gitea/services/repoevent"
)

// ListRepoEvents returns one page of a repository's unified event stream.
func ListRepoEvents(ctx *context.APIContext) {
	// swagger:operation GET /repos/{owner}/{repo}/events repository repoListEvents
	// ---
	// summary: List a repository's unified event stream
	// description: |-
	//   Returns activity rows, issue and pull request comments, published reviews, commit
	//   statuses and - for repository admins - the agent audit trail, merged newest first.
	//   Paging is by the opaque next_cursor rather than by page number, so following it visits
	//   every event exactly once even while new events are being written. Events the requesting
	//   user may not see are absent from the stream rather than reported as forbidden.
	//
	//   Free-text payload values are truncated; when a value was cut, the payload also carries
	//   `<key>_truncated: "true"` and the full text must be read from the endpoint that owns
	//   the row.
	//
	//   Visibility is per kind. Comments, reviews and statuses are filtered by the repository
	//   unit they belong to (issues, pull requests, code), and the agent audit trail is
	//   repository admins only. The action kind is filtered the way
	//   `/repos/{owner}/{repo}/activities/feeds` filters it - by access to the repository and by
	//   the actor's own activity-privacy setting - and not by unit, so a reader with access to
	//   only some units still sees this repository's activity rows.
	// produces:
	// - application/json
	// parameters:
	// - name: owner
	//   in: path
	//   description: owner of the repo
	//   type: string
	//   required: true
	// - name: repo
	//   in: path
	//   description: name of the repo
	//   type: string
	//   required: true
	// - name: kinds
	//   in: query
	//   description: comma-separated kinds to include (action, agent_audit, comment, review, status); all when omitted
	//   type: string
	// - name: since
	//   in: query
	//   description: only events created at or after this time (RFC 3339)
	//   type: string
	//   format: date-time
	// - name: until
	//   in: query
	//   description: only events created at or before this time (RFC 3339)
	//   type: string
	//   format: date-time
	// - name: actor
	//   in: query
	//   description: only events whose actor is this username
	//   type: string
	// - name: q
	//   in: query
	//   description: |-
	//     only events whose text matches this search. Matching is against each kind's own text
	//     column, which for the action kind is action.content - serialized JSON for push events
	//     rather than prose, so a search there matches the JSON's text.
	//
	//     How the match is made depends on the instance's database. On PostgreSQL it is a
	//     full-text match, which stems words, ignores case and skips stop words: `running`
	//     matches "runs" and `the` matches nothing. On MySQL and SQLite it is a
	//     case-insensitive substring match: `fix` matches "prefix". Callers that must behave
	//     identically everywhere should not depend on either one's extra matches.
	//   type: string
	// - name: cursor
	//   in: query
	//   description: opaque next_cursor from the previous page
	//   type: string
	// - name: limit
	//   in: query
	//   description: page size of results
	//   type: integer
	// responses:
	//   "200":
	//     "$ref": "#/responses/RepoEventList"
	//   "422":
	//     "$ref": "#/responses/validationError"
	//   "404":
	//     "$ref": "#/responses/notFound"

	opts := &repoevent.ListOptions{
		Repo:       ctx.Repo.Repository,
		Doer:       ctx.Doer,
		Permission: &ctx.Repo.Permission,
		Query:      ctx.FormString("q"),
		Limit:      ctx.FormInt("limit"),
	}

	kinds, err := parseKinds(ctx.FormString("kinds"))
	if err != nil {
		ctx.APIError(http.StatusUnprocessableEntity, err)
		return
	}
	opts.Kinds = kinds

	if opts.Since, err = parseEventTime(ctx.FormString("since")); err != nil {
		ctx.APIError(http.StatusUnprocessableEntity, err)
		return
	}
	if opts.Until, err = parseEventTime(ctx.FormString("until")); err != nil {
		ctx.APIError(http.StatusUnprocessableEntity, err)
		return
	}

	if opts.Cursor, err = repoevent.DecodeCursor(ctx.FormString("cursor")); err != nil {
		ctx.APIError(http.StatusUnprocessableEntity, err)
		return
	}

	if actor := ctx.FormString("actor"); actor != "" {
		user, err := user_model.GetUserByName(ctx, actor)
		if err != nil {
			if user_model.IsErrUserNotExist(err) {
				// An unknown actor is an empty stream, not an error: answering 404
				// would turn this parameter into a way to test whether a username
				// exists on the instance.
				ctx.JSON(http.StatusOK, &api.RepoEventList{Data: []*api.RepoEvent{}})
				return
			}
			ctx.APIErrorInternal(err)
			return
		}
		opts.ActorID = user.ID
	}

	events, next, err := repoevent.List(ctx, opts)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}

	result := &api.RepoEventList{Data: make([]*api.RepoEvent, len(events))}
	for i, event := range events {
		result.Data[i] = &api.RepoEvent{
			Kind:     string(event.Kind),
			SourceID: event.SourceID,
			ActorID:  event.ActorID,
			RepoID:   event.RepoID,
			Title:    event.Title,
			Payload:  event.Payload,
			Created:  event.CreatedUnix.AsTime(),
		}
	}
	if next != nil {
		result.NextCursor = next.Encode()
	}
	ctx.JSON(http.StatusOK, result)
}

// parseKinds reads the `kinds` parameter. An unknown kind is rejected rather than ignored: silently
// dropping it would answer a question the caller did not ask, with a stream wider than the one they
// filtered for.
func parseKinds(raw string) ([]repoevent.Kind, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	kinds := make([]repoevent.Kind, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kind, ok := repoevent.KindFromString(part)
		if !ok {
			return nil, util.NewInvalidArgumentErrorf("unknown event kind %q", part)
		}
		kinds = append(kinds, kind)
	}
	return kinds, nil
}

// parseEventTime reads an RFC 3339 timestamp, the format the rest of the API uses for `since`.
//
// Absent is nil rather than zero, because zero is a timestamp a caller can write:
// `until=1970-01-01T00:00:00Z` asks for the empty stream and must not be read as "no filter".
func parseEventTime(raw string) (*timeutil.TimeStamp, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		//nolint:nilnil // an absent parameter is not a failure, and nil is how the rest of
		// this package says "the caller did not give a bound" - see rangeCond.
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, util.NewInvalidArgumentErrorf("%q is not an RFC 3339 timestamp", raw)
	}
	ts := timeutil.TimeStamp(t.Unix())
	return &ts, nil
}
