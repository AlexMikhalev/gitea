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
	issues_model "code.gitea.io/gitea/models/issues"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/unittest"
	user_model "code.gitea.io/gitea/models/user"
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
			Type:        issues_model.ReviewTypeApprove,
			ReviewerID:  2,
			IssueID:     2, // repo1, a pull request
			Content:     "published review",
			CreatedUnix: eventStreamBase + 30,
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

// TestAPIRepoEventStream drives the endpoint end to end: the five queries run against a real
// schema, through the router's permission middleware, and their union is paged by the cursor the
// previous response handed back.
func TestAPIRepoEventStream(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	since := writeEventStreamRows(t)
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: repo.OwnerID})
	token := getUserToken(t, owner.Name, auth_model.AccessTokenScopeReadRepository)

	t.Run("one page holds the window, newest first", func(t *testing.T) {
		resp := MakeRequest(t, NewRequest(t, "GET",
			eventStreamURL(t, owner.Name, repo.Name, url.Values{"since": {since}, "limit": {"50"}}),
		).AddTokenAuth(token), http.StatusOK)

		var list api.RepoEventList
		DecodeJSON(t, resp, &list)
		assert.Empty(t, list.NextCursor, "the stream is exhausted, so there is no next page")
		require.Len(t, list.Data, 4)
		assert.Equal(t, []string{"agent_audit", "review", "comment", "action"}, []string{
			list.Data[0].Kind, list.Data[1].Kind, list.Data[2].Kind, list.Data[3].Kind,
		})
		assert.Equal(t, repo.ID, list.Data[0].RepoID)
		assert.Equal(t, "a hedgehog on the issue", list.Data[2].Payload["content"])
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

	t.Run("q searches each kind's own text", func(t *testing.T) {
		resp := MakeRequest(t, NewRequest(t, "GET",
			eventStreamURL(t, owner.Name, repo.Name, url.Values{"since": {since}, "q": {"hedgehog"}}),
		).AddTokenAuth(token), http.StatusOK)

		var list api.RepoEventList
		DecodeJSON(t, resp, &list)
		assert.Equal(t, []string{"comment", "action"}, []string{list.Data[0].Kind, list.Data[1].Kind})
		assert.Len(t, list.Data, 2)
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
		token := getUserToken(t, user.Name, auth_model.AccessTokenScopeReadRepository)
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

// The route is behind reqToken(): an anonymous caller gets 401 rather than an anonymous view of the
// stream, which matters because four of the five sources decide what to show from the doer.
func TestAPIRepoEventStreamRequiresAToken(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
	owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: repo.OwnerID})

	MakeRequest(t, NewRequest(t, "GET",
		eventStreamURL(t, owner.Name, repo.Name, url.Values{})), http.StatusUnauthorized)
}
