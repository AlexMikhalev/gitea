// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	activities_model "code.gitea.io/gitea/models/activities"
	agent_model "code.gitea.io/gitea/models/agent"
	auth_model "code.gitea.io/gitea/models/auth"
	"code.gitea.io/gitea/models/db"
	git_model "code.gitea.io/gitea/models/git"
	issues_model "code.gitea.io/gitea/models/issues"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/unittest"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/commitstatus"
	api "code.gitea.io/gitea/modules/structs"
	"code.gitea.io/gitea/modules/timeutil"
	"code.gitea.io/gitea/tests"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// eventStreamBase is the second the rows these tests write start at - past every row in
// models/fixtures, so `since` names exactly this test's events and nothing else.
const eventStreamBase = timeutil.TimeStamp(2_000_000_000)

// writeEventStreamRows puts one row of each kind into repo1 and returns the `since` value that
// selects exactly them.
//
// NoAutoTime is what keeps the timestamps: xorm stamps every `created` column with the current
// second on insert, and these tests are about an ordering.
func writeEventStreamRows(t *testing.T) string {
	t.Helper()

	rows := []any{
		&activities_model.Action{
			UserID:      2,
			ActUserID:   2,
			RepoID:      1,
			OpType:      activities_model.ActionCommitRepo,
			RefName:     "main",
			Content:     `{"Len":1,"Commits":[{"Message":"hedgehog in the push"}]}`,
			CreatedUnix: eventStreamBase + 10,
		},
		&issues_model.Comment{
			Type:        issues_model.CommentTypeComment,
			PosterID:    2,
			IssueID:     1, // repo1, not a pull request
			Content:     "a hedgehog on the issue",
			CreatedUnix: eventStreamBase + 20,
		},
		&issues_model.Review{
			Type:       issues_model.ReviewTypeApprove,
			ReviewerID: 2,
			IssueID:    2, // repo1, a pull request
			Content:    "a hedgehog reviewed this",
			// A review event is stamped by updated_unix - the second it was submitted,
			// not the second its author started drafting it.
			CreatedUnix: eventStreamBase + 30,
			UpdatedUnix: eventStreamBase + 30,
		},
		&agent_model.AuditEvent{
			RepoID:      1,
			AgentUserID: 2,
			OwnerUserID: 2,
			AgentKeyID:  1,
			EventID:     "f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1",
			PubKey:      "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd",
			Method:      "GET",
			RequestURL:  "https://example.com/api/v1/repos/user2/repo1/issues",
			CreatedUnix: eventStreamBase + 40,
		},
		// The commit status is here for ?q=. Its searchable text is split across two columns
		// and the Postgres branch matches them through a single two-column to_tsvector
		// expression, which is the most fragile of the four and the only one no other test
		// makes match a row.
		&git_model.CommitStatus{
			Index:       200,
			RepoID:      1,
			State:       commitstatus.CommitStatusSuccess,
			SHA:         "1234123412341234123412341234123412341234",
			TargetURL:   "https://example.com/builds/200",
			Description: "the hedgehog build passed",
			Context:     "ci/hedgehog",
			CreatorID:   2,
			CreatedUnix: eventStreamBase + 50,
		},
	}
	for _, row := range rows {
		_, err := db.GetEngine(t.Context()).NoAutoTime().Insert(row)
		require.NoError(t, err)
	}
	return time.Unix(int64(eventStreamBase), 0).UTC().Format(time.RFC3339)
}

func eventStreamURL(t *testing.T, owner, repo string, params url.Values) string {
	t.Helper()
	link, err := url.Parse(fmt.Sprintf("/api/v1/repos/%s/%s/events", owner, repo))
	require.NoError(t, err)
	link.RawQuery = params.Encode()
	return link.String()
}

func eventKeys(list *api.RepoEventList) []string {
	keys := make([]string, 0, len(list.Data))
	for _, event := range list.Data {
		keys = append(keys, fmt.Sprintf("%s:%d", event.Kind, event.SourceID))
	}
	return keys
}

func eventKinds(list *api.RepoEventList) []string {
	kinds := make([]string, 0, len(list.Data))
	for _, event := range list.Data {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

// TestAPIRepoEventStream drives the endpoint end to end: the five queries run against a real
// schema, through the router's permission middleware, and their union is paged by the cursor the
// previous response handed back.
func TestAPIRepoEventStream(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	since := writeEventStreamRows(t)
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: repo.OwnerID})
	// Both scopes: the route is gated on read:repository, but the comment kind is served only to
	// a token that also holds the scope the comment endpoints themselves sit behind - see
	// TestAPIRepoEventStreamWithholdsCommentsFromARepositoryScopedToken.
	token := getUserToken(t, owner.Name,
		auth_model.AccessTokenScopeReadRepository, auth_model.AccessTokenScopeReadIssue)

	t.Run("one page holds the window, newest first", func(t *testing.T) {
		resp := MakeRequest(t, NewRequest(t, "GET",
			eventStreamURL(t, owner.Name, repo.Name, url.Values{"since": {since}, "limit": {"50"}}),
		).AddTokenAuth(token), http.StatusOK)

		var list api.RepoEventList
		DecodeJSON(t, resp, &list)
		assert.Empty(t, list.NextCursor, "the stream is exhausted, so there is no next page")
		require.Len(t, list.Data, 5)
		assert.Equal(t, []string{"status", "agent_audit", "review", "comment", "action"},
			eventKinds(&list))
		assert.Equal(t, repo.ID, list.Data[0].RepoID)
		assert.Equal(t, "a hedgehog on the issue", list.Data[3].Payload["content"])
	})

	t.Run("the cursor pages every event exactly once", func(t *testing.T) {
		resp := MakeRequest(t, NewRequest(t, "GET",
			eventStreamURL(t, owner.Name, repo.Name, url.Values{"since": {since}, "limit": {"50"}}),
		).AddTokenAuth(token), http.StatusOK)
		var whole api.RepoEventList
		DecodeJSON(t, resp, &whole)

		var paged []string
		cursor := ""
		for range 10 {
			params := url.Values{"since": {since}, "limit": {"1"}}
			if cursor != "" {
				params.Set("cursor", cursor)
			}
			resp := MakeRequest(t, NewRequest(t, "GET",
				eventStreamURL(t, owner.Name, repo.Name, params),
			).AddTokenAuth(token), http.StatusOK)

			var list api.RepoEventList
			DecodeJSON(t, resp, &list)
			assert.LessOrEqual(t, len(list.Data), 1)
			paged = append(paged, eventKeys(&list)...)
			if list.NextCursor == "" {
				break
			}
			cursor = list.NextCursor
		}
		assert.Equal(t, eventKeys(&whole), paged)
	})

	t.Run("kinds narrows the stream", func(t *testing.T) {
		resp := MakeRequest(t, NewRequest(t, "GET",
			eventStreamURL(t, owner.Name, repo.Name, url.Values{"since": {since}, "kinds": {"action,review"}}),
		).AddTokenAuth(token), http.StatusOK)

		var list api.RepoEventList
		DecodeJSON(t, resp, &list)
		require.Len(t, list.Data, 2)
		assert.Equal(t, "review", list.Data[0].Kind)
		assert.Equal(t, "action", list.Data[1].Kind)
	})

	// This is the only suite that runs against PostgreSQL with the full-text indexes built -
	// repoevent.Init runs here (tests/test_utils.go, InitWebInstalled) and flips ?q= onto the
	// to_tsvector branch, where the builder-level assertions in services/repoevent cannot follow
	// it. So every source that has an index is made to match a row rather than merely to have
	// its expression sent: an expression that is a type error, that Postgres refuses as
	// non-immutable, or that simply matches nothing is only distinguishable from here.
	//
	// `hedgehog` is a whole word, not a stop word and not a substring of anything else in the
	// window, which is what makes the expected set the same on both search paths - the two do
	// not otherwise match the same rows, and the endpoint documents that.
	t.Run("q searches each kind's own text", func(t *testing.T) {
		resp := MakeRequest(t, NewRequest(t, "GET",
			eventStreamURL(t, owner.Name, repo.Name, url.Values{"since": {since}, "q": {"hedgehog"}}),
		).AddTokenAuth(token), http.StatusOK)

		var list api.RepoEventList
		DecodeJSON(t, resp, &list)
		assert.Equal(t, []string{"status", "review", "comment", "action"}, eventKinds(&list))
		assert.Len(t, list.Data, 4)
	})

	// The audit trail has no index and takes the LIKE path on every dialect, so a search that
	// only it can answer must return it on Postgres too rather than falling into the full-text
	// branch of a source that has one.
	t.Run("q reaches the sources that have no index", func(t *testing.T) {
		resp := MakeRequest(t, NewRequest(t, "GET",
			eventStreamURL(t, owner.Name, repo.Name,
				url.Values{"since": {since}, "q": {"/repos/user2/repo1/"}}),
		).AddTokenAuth(token), http.StatusOK)

		var list api.RepoEventList
		DecodeJSON(t, resp, &list)
		assert.Equal(t, []string{"agent_audit"}, eventKinds(&list))
	})

	t.Run("an unknown actor is an empty stream, not a 404", func(t *testing.T) {
		// Answering 404 would turn this parameter into a way to test whether a username
		// exists on the instance.
		resp := MakeRequest(t, NewRequest(t, "GET",
			eventStreamURL(t, owner.Name, repo.Name, url.Values{"actor": {"nosuchuser"}}),
		).AddTokenAuth(token), http.StatusOK)

		var list api.RepoEventList
		DecodeJSON(t, resp, &list)
		assert.Empty(t, list.Data)
		assert.Empty(t, list.NextCursor)
	})

	t.Run("bad parameters are rejected rather than ignored", func(t *testing.T) {
		for name, params := range map[string]url.Values{
			"unknown kind":     {"kinds": {"action,nonsense"}},
			"unparsable since": {"since": {"yesterday"}},
			"unparsable until": {"until": {"yesterday"}},
			"corrupt cursor":   {"cursor": {"!!!not-base64!!!"}},
			"foreign cursor":   {"cursor": {"eyJjIjoxLCJrIjoibm9wZSIsInMiOjF9"}}, // kind "nope"
		} {
			t.Run(name, func(t *testing.T) {
				MakeRequest(t, NewRequest(t, "GET",
					eventStreamURL(t, owner.Name, repo.Name, params),
				).AddTokenAuth(token), http.StatusUnprocessableEntity)
			})
		}
	})
}

// The audit trail is repository admins only - a plain reader gets a stream without that kind rather
// than a refusal, because the answer to "what may I see" is the stream itself.
func TestAPIRepoEventStreamHidesTheAuditTrailFromReaders(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	since := writeEventStreamRows(t)
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: repo.OwnerID})
	reader := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 4})

	kindsFor := func(user *user_model.User) []string {
		token := getUserToken(t, user.Name,
			auth_model.AccessTokenScopeReadRepository, auth_model.AccessTokenScopeReadIssue)
		resp := MakeRequest(t, NewRequest(t, "GET",
			eventStreamURL(t, owner.Name, repo.Name, url.Values{"since": {since}, "limit": {"50"}}),
		).AddTokenAuth(token), http.StatusOK)

		var list api.RepoEventList
		DecodeJSON(t, resp, &list)
		kinds := make([]string, 0, len(list.Data))
		for _, event := range list.Data {
			kinds = append(kinds, event.Kind)
		}
		return kinds
	}

	assert.Contains(t, kindsFor(owner), "agent_audit")

	readerKinds := kindsFor(reader)
	assert.NotContains(t, readerKinds, "agent_audit")
	assert.Contains(t, readerKinds, "comment", "the rest of the stream is still served")
}

// The route sits behind tokenRequiresScopes(AccessTokenScopeCategoryRepository), but the endpoints
// that own plain issue and pull request comment bodies - /issues/comments, /issues/{index}/comments -
// sit behind AccessTokenScopeCategoryIssue. A token deliberately created with read:repository and
// not read:issue must therefore not read comment bodies off this endpoint: they are data that scope
// has no other route to, and /activities/feeds (the same scope) leaks only a 200-character excerpt.
func TestAPIRepoEventStreamWithholdsCommentsFromARepositoryScopedToken(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	since := writeEventStreamRows(t)
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: repo.OwnerID})

	listWith := func(t *testing.T, params url.Values, scopes ...auth_model.AccessTokenScope) api.RepoEventList {
		t.Helper()
		token := getUserToken(t, owner.Name, scopes...)
		resp := MakeRequest(t, NewRequest(t, "GET",
			eventStreamURL(t, owner.Name, repo.Name, params),
		).AddTokenAuth(token), http.StatusOK)
		var list api.RepoEventList
		DecodeJSON(t, resp, &list)
		return list
	}

	window := url.Values{"since": {since}, "limit": {"50"}}

	repoOnly := listWith(t, window, auth_model.AccessTokenScopeReadRepository)
	assert.NotContains(t, eventKinds(&repoOnly), "comment",
		"a read:repository token was handed issue comment bodies")
	for _, event := range repoOnly.Data {
		assert.NotEqual(t, "a hedgehog on the issue", event.Payload["content"])
	}
	// The rest of the stream is still served rather than the request refused: review, status
	// and the audit trail are owned by endpoints inside the same repository-scoped group.
	assert.Equal(t, []string{"status", "agent_audit", "review", "action"}, eventKinds(&repoOnly))

	withIssue := listWith(t, window,
		auth_model.AccessTokenScopeReadRepository, auth_model.AccessTokenScopeReadIssue)
	assert.Contains(t, eventKinds(&withIssue), "comment",
		"read:issue is what the comment kind is withheld for, so it has to bring it back")

	// Asking for only the withheld kind is the empty stream, not the whole stream: an empty
	// kinds list means *every* kind one layer down, so this is where that would go wrong.
	onlyComments := listWith(t, url.Values{"since": {since}, "kinds": {"comment"}},
		auth_model.AccessTokenScopeReadRepository)
	assert.Empty(t, onlyComments.Data)
	assert.Empty(t, onlyComments.NextCursor)
}

// The scope filter is a property of access tokens, so every other way of authenticating this route
// has to fall through it untouched. It is the default that is at risk: an absent ?kinds= is the
// empty slice, which one layer down means *every* kind but which the handler also reads as "the
// scope filter dropped everything". A caller with no scopes to check must not be handed that value.
//
// Basic auth with a password is the case every other test here misses - they all sign with a PAT,
// which does carry a scope. services/auth/basic.go takes the UserSignIn path for a password and
// sets LoginMethod alone, leaving both ApiTokenScope and IsApiToken absent; an Actions task token
// (basic.go, same file) and HTTP signature auth (services/auth/httpsign.go) reach the handler with
// the same pair unset, so this one case stands for all three. A signed-in browser session is not
// among them: buildAuthGroup carries no Session method, so a cookie is 401 at reqToken() and never
// arrives here - see TestAPIRepoEventStreamRequiresAToken.
func TestAPIRepoEventStreamServesACallerWithoutTokenScopes(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	since := writeEventStreamRows(t)
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: repo.OwnerID})

	listWith := func(t *testing.T, params url.Values) api.RepoEventList {
		t.Helper()
		resp := MakeRequest(t, NewRequest(t, "GET",
			eventStreamURL(t, owner.Name, repo.Name, params),
		).AddBasicAuth(owner.Name), http.StatusOK)
		var list api.RepoEventList
		DecodeJSON(t, resp, &list)
		return list
	}

	// No ?kinds=: the whole stream, not the empty one.
	all := listWith(t, url.Values{"since": {since}, "limit": {"50"}})
	assert.Equal(t, []string{"status", "agent_audit", "review", "comment", "action"}, eventKinds(&all))

	// And an explicit filter from the same caller still narrows rather than widens - the two
	// halves of the bug looked different from each other, which is what hid it.
	filtered := listWith(t, url.Values{"since": {since}, "kinds": {"comment,review"}, "limit": {"50"}})
	assert.Equal(t, []string{"review", "comment"}, eventKinds(&filtered))
}

// The route is behind reqToken(): an anonymous caller gets 401 rather than an anonymous view of the
// stream, which matters because four of the five sources decide what to show from the doer.
func TestAPIRepoEventStreamRequiresAToken(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: repo.OwnerID})

	MakeRequest(t, NewRequest(t, "GET",
		eventStreamURL(t, owner.Name, repo.Name, url.Values{})), http.StatusUnauthorized)
}
