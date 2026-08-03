// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"net/http"
	"strings"
	"time"

	auth_model "code.gitea.io/gitea/models/auth"
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
	//   One act is one event, across kinds. Gitea records a comment or a submitted review in
	//   more than one table - a comment row, for a review also a review row, and an activity row
	//   beside either - and exactly one of them is reported. A submitted review is a single
	//   event of kind `review`; a comment is a single event of kind `comment`; neither is also
	//   an `action`. So a kind filter narrows the stream without splitting an act in two:
	//   `kinds=comment` alone will not show a submitted review, and `kinds=action` alone will
	//   not show comments or reviews. The line comments of a review are separate events, each
	//   carrying that review's id as `review_id` in its payload so that they can be grouped
	//   with it.
	//
	//   An `action` event's `payload.comment_id`, where present, names the system comment the
	//   activity was recorded against - the one written when an issue was closed or a review
	//   dismissed. Those are not events of this stream; read them from the comments API.
	//
	//   A `review` event's `created` is the second the review was submitted, not the second its
	//   author began drafting it - Gitea writes the row when the first draft line comment is
	//   typed and publishes it later, and a stream ordered by the earlier second would place a
	//   review behind a position readers had already passed. Dismissing a review reports it
	//   again, at the dismissal's second, with `dismissed` set. A review superseded by a later
	//   one from the same reviewer is not reported again: it keeps its submission position and
	//   its `dismissed` flag is set where it stands.
	//
	//   Visibility is per kind. Comments, reviews and statuses are filtered by the repository
	//   unit they belong to (issues, pull requests, code), and the agent audit trail is
	//   repository admins only. When the caller is an access token, a kind is also dropped if
	//   the token lacks the scope the endpoint owning that kind's rows requires: `comment`
	//   needs `read:issue`, the scope `/repos/{owner}/{repo}/issues/comments` is behind. The
	//   action kind is filtered the way
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
	//
	//     The term is matched literally either way. `%` and `_` are searched for as themselves
	//     rather than read as wildcards, so there is no pattern syntax here on any database.
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

	// After the 422s rather than before them: a caller whose token cannot reach the kind they
	// asked for still wrote the rest of the request, and answering 200 to a malformed `since`
	// because of their scopes would hide the parameter error behind an empty page.
	if opts.Kinds, err = kindsWithinTokenScope(ctx, opts.Kinds); err != nil {
		ctx.APIError(http.StatusForbidden, "checking scope failed: "+err.Error())
		return
	}
	if len(opts.Kinds) == 0 {
		// Every kind the caller asked for is outside their token's scopes. That is the empty
		// stream, not a refusal, for the same reason the audit trail is absent rather than
		// forbidden: the answer to "what may I see" is the stream itself.
		ctx.JSON(http.StatusOK, &api.RepoEventList{Data: []*api.RepoEvent{}})
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

// kindScopeCategories names, per kind, the token scope category the endpoint that owns those rows
// sits behind - for the kinds where that is more than this route's own gate.
//
// This route is registered inside the group closed by
// tokenRequiresScopes(AccessTokenScopeCategoryRepository) (routers/api/v1/api.go), which is also
// what `/pulls/{index}/reviews`, `/pulls/{index}/reviews/{id}/comments` and the commit status
// endpoints sit behind - so `review` and `status` are already no wider here than at their own
// endpoints, and the audit trail is narrower still (repository admin, enforced in the adapter).
//
// `comment` is the one that is wider. Plain issue and pull request comment bodies are owned by
// `/repos/{owner}/{repo}/issues/comments` and `/issues/{index}/comments`, which are inside the
// group closed by tokenRequiresScopes(AccessTokenScopeCategoryIssue). Without this a token
// deliberately created with `read:repository` and *not* `read:issue` would read every comment body
// in the repository off this endpoint - data that scope has no other route to. tokenRequiresScopes
// is any-of, so naming both categories on the route would not draw this line; only the handler can.
//
// The whole kind goes rather than the issue-owned subset of it. A review's line comments are
// reachable at repository scope, so dropping them too is stricter than the disclosure requires -
// but "this kind is absent" is a rule a client can hold, and a comment kind that silently contains
// some comment types and not others is not. Erring toward the narrower stream is also the direction
// that fails safely.
var kindScopeCategories = map[repoevent.Kind]auth_model.AccessTokenScopeCategory{
	repoevent.KindComment: auth_model.AccessTokenScopeCategoryIssue,
}

// kindsWithinTokenScope drops the kinds the caller's token has no scope for, and expands "all
// kinds" to the explicit list first - an empty ListOptions.Kinds means every kind, so a filter that
// left it empty would widen the stream rather than narrow it.
//
// A caller who is not an access token (a session, basic auth, an Actions task) is unrestricted
// here: scopes are a property of tokens, and there is nothing to check.
func kindsWithinTokenScope(ctx *context.APIContext, kinds []repoevent.Kind) ([]repoevent.Kind, error) {
	scope, ok := ctx.Data["ApiTokenScope"].(auth_model.AccessTokenScope)
	if ctx.Data["IsApiToken"] != true || !ok {
		return kinds, nil
	}

	requested := kinds
	if len(requested) == 0 {
		requested = repoevent.AllKinds
	}
	allowed := make([]repoevent.Kind, 0, len(requested))
	for _, kind := range requested {
		category, restricted := kindScopeCategories[kind]
		if restricted {
			// Read level: this route is GET only, so a write scope is not what is being
			// asked for. GetRequiredScopes is the same mapping tokenRequiresScopes uses,
			// so the two cannot drift apart on what "read:issue" is.
			has, err := scope.HasScope(auth_model.GetRequiredScopes(auth_model.Read, category)...)
			if err != nil {
				return nil, err
			}
			if !has {
				continue
			}
		}
		allowed = append(allowed, kind)
	}
	return allowed, nil
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
