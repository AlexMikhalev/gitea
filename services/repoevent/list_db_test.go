// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repoevent

import (
	"strconv"
	"testing"

	activities_model "code.gitea.io/gitea/models/activities"
	agent_model "code.gitea.io/gitea/models/agent"
	"code.gitea.io/gitea/models/db"
	git_model "code.gitea.io/gitea/models/git"
	issues_model "code.gitea.io/gitea/models/issues"
	perm_model "code.gitea.io/gitea/models/perm"
	access_model "code.gitea.io/gitea/models/perm/access"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/unit"
	"code.gitea.io/gitea/models/unittest"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/commitstatus"
	"code.gitea.io/gitea/modules/timeutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file run the five adapters' SQL against a real schema, which the builder-shape
// assertions elsewhere in this package cannot do. A wrong column name, a join that maps the wrong
// table into the wrong struct, a typed-int slice that xorm binds differently than expected or a
// backtick that survives into a Postgres expression are all things that compile, pass every
// string-level assertion, and 500 on the first request. The permission gates are the half that
// matters most: "a pull-requests-only reader does not see issue comments" is a claim about rows,
// and only a query can answer it.
//
// Every fixture row this file writes is stamped in 2033, well past the newest row in
// models/fixtures, so a test can name exactly the events it expects by asking for that window -
// while the unwindowed tests still read the fixture rows through the same queries.

// streamBase is the second the written rows start at.
const streamBase = timeutil.TimeStamp(2_000_000_000)

// stream is the set of rows a test works against, by kind and id, so an assertion can name an event
// rather than an index into a page.
type stream struct {
	action        int64
	issueComment  int64
	pullComment   int64
	review        int64
	draftReview   int64
	draftComment  int64
	commitStatus  int64
	auditEvent    int64
	repo          *repo_model.Repository
	owner         *user_model.User // user2, owns repo1
	draftReviewer *user_model.User // user1, site admin, author of the pending review
	outsider      *user_model.User // user4, a signed-in reader with no privileges here
}

// prepareStream writes one row per source into repo1 and returns their ids.
//
// The pending review and its code comment are the disclosure case: a reviewer's unsubmitted draft
// is written to `comment` the moment it is typed, and must not reach anyone else's stream.
func prepareStream(t *testing.T) *stream {
	t.Helper()
	unittest.PrepareTestEnv(t)

	s := &stream{
		repo:          unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1}),
		owner:         unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2}),
		draftReviewer: unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 1}),
		outsider:      unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 4}),
	}

	action := &activities_model.Action{
		// user_id = act_user_id is what ActivityQueryCondition keeps: `action` holds one row
		// per receiver and this is the original.
		UserID:      s.owner.ID,
		ActUserID:   s.owner.ID,
		RepoID:      s.repo.ID,
		OpType:      activities_model.ActionCommitRepo,
		RefName:     "main",
		IsPrivate:   false,
		Content:     `{"Len":1,"Commits":[{"Message":"hedgehog in the push"}]}`,
		CreatedUnix: streamBase + 10,
	}
	issueComment := &issues_model.Comment{
		Type:        issues_model.CommentTypeComment,
		PosterID:    s.owner.ID,
		IssueID:     1, // repo1, not a pull request
		Content:     "a hedgehog on the issue",
		CreatedUnix: streamBase + 20,
	}
	pullComment := &issues_model.Comment{
		Type:        issues_model.CommentTypeComment,
		PosterID:    s.owner.ID,
		IssueID:     2, // repo1, a pull request
		Content:     "a remark on the pull request",
		CreatedUnix: streamBase + 30,
	}
	review := &issues_model.Review{
		Type:        issues_model.ReviewTypeApprove,
		ReviewerID:  s.owner.ID,
		IssueID:     2,
		Content:     "published review",
		CreatedUnix: streamBase + 40,
	}
	draftReview := &issues_model.Review{
		Type:        issues_model.ReviewTypePending,
		ReviewerID:  s.draftReviewer.ID,
		IssueID:     2,
		Content:     "unsubmitted draft",
		CreatedUnix: streamBase + 50,
	}
	commitStatus := &git_model.CommitStatus{
		Index:       100,
		RepoID:      s.repo.ID,
		State:       commitstatus.CommitStatusSuccess,
		SHA:         "1234123412341234123412341234123412341234",
		TargetURL:   "https://example.com/builds/100",
		Description: "hedgehog build passed",
		Context:     "ci/hedgehog",
		CreatorID:   s.owner.ID,
		CreatedUnix: streamBase + 70,
	}
	auditEvent := &agent_model.AuditEvent{
		RepoID:      s.repo.ID,
		AgentUserID: s.owner.ID,
		OwnerUserID: s.owner.ID,
		AgentKeyID:  1,
		EventID:     "e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1e1",
		PubKey:      "abababababababababababababababababababababababababababababababab",
		Method:      "GET",
		RequestURL:  "https://example.com/api/v1/repos/user2/repo1/issues",
		CreatedUnix: streamBase + 80,
	}
	insertAt(t, action, issueComment, pullComment, review, draftReview, commitStatus, auditEvent)

	// Written after the review it belongs to, because it carries that review's id.
	draftComment := &issues_model.Comment{
		Type:        issues_model.CommentTypeCode,
		PosterID:    s.draftReviewer.ID,
		IssueID:     2,
		ReviewID:    draftReview.ID,
		Content:     "draft line comment",
		TreePath:    "README.md",
		Line:        1,
		CreatedUnix: streamBase + 60,
	}
	insertAt(t, draftComment)

	s.action, s.issueComment, s.pullComment = action.ID, issueComment.ID, pullComment.ID
	s.review, s.draftReview, s.draftComment = review.ID, draftReview.ID, draftComment.ID
	s.commitStatus, s.auditEvent = commitStatus.ID, auditEvent.ID
	return s
}

// insertAt writes rows keeping the CreatedUnix each one carries.
//
// NoAutoTime is the point: xorm stamps every `created` column with the current second on insert,
// and a test that cannot place a row at a chosen second cannot assert anything about an ordering
// across five tables.
func insertAt(t *testing.T, beans ...any) {
	t.Helper()
	for _, bean := range beans {
		_, err := db.GetEngine(t.Context()).NoAutoTime().Insert(bean)
		require.NoError(t, err)
	}
}

// key names one event the way the cursor does: unique only as (kind, source id).
func key(kind Kind, id int64) string {
	return string(kind) + ":" + strconv.FormatInt(id, 10)
}

func keysOf(events []*Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, key(e.Kind, e.SourceID))
	}
	return out
}

func listOrFail(t *testing.T, opts *ListOptions) ([]*Event, *Cursor) {
	t.Helper()
	events, next, err := List(t.Context(), opts)
	require.NoError(t, err)
	return events, next
}

// repoPermission is the real permission the endpoint would compute for this user.
func repoPermission(t *testing.T, repo *repo_model.Repository, doer *user_model.User) *access_model.Permission {
	t.Helper()
	permission, err := access_model.GetUserRepoPermission(t.Context(), repo, doer)
	require.NoError(t, err)
	return &permission
}

// unitPermission is a read-only permission over exactly the named units, for the cases where the
// question is "what does a reader of these units see" rather than "what does this fixture user see".
func unitPermission(repoID int64, units ...unit.Type) *access_model.Permission {
	repoUnits := make([]*repo_model.RepoUnit, 0, len(units))
	for _, u := range units {
		repoUnits = append(repoUnits, &repo_model.RepoUnit{RepoID: repoID, Type: u})
	}
	permission := &access_model.Permission{AccessMode: perm_model.AccessModeRead}
	permission.SetUnitsWithDefaultAccessMode(repoUnits, perm_model.AccessModeRead)
	return permission
}

// since returns a pointer to the window every written row falls into.
func since() *timeutil.TimeStamp {
	from := streamBase
	return &from
}

// TestListReadsEveryKindFromTheDatabase is the query smoke test: every adapter runs its own SQL
// against the real schema, and every kind comes back projected.
func TestListReadsEveryKindFromTheDatabase(t *testing.T) {
	s := prepareStream(t)
	events, next := listOrFail(t, &ListOptions{
		Repo:       s.repo,
		Doer:       s.owner,
		Permission: repoPermission(t, s.repo, s.owner),
		Since:      since(),
		Limit:      50,
	})

	assert.Nil(t, next, "one page holds the whole window")
	assert.Equal(t, []string{
		// Newest first: the audit row is the last one written.
		key(KindAgentAudit, s.auditEvent),
		key(KindStatus, s.commitStatus),
		key(KindReview, s.review),
		key(KindComment, s.pullComment),
		key(KindComment, s.issueComment),
		key(KindAction, s.action),
	}, keysOf(events))

	byKey := map[string]*Event{}
	for _, e := range events {
		byKey[key(e.Kind, e.SourceID)] = e
	}

	// The projection is part of the query's contract: an event that came back with the wrong
	// actor or a payload read out of the wrong column is as broken as one that did not come back.
	action := byKey[key(KindAction, s.action)]
	assert.Equal(t, s.owner.ID, action.ActorID)
	assert.Equal(t, s.repo.ID, action.RepoID)
	assert.Equal(t, "main", action.Payload["ref_name"])

	comment := byKey[key(KindComment, s.issueComment)]
	assert.Equal(t, "a hedgehog on the issue", comment.Payload["content"])
	// Title and index come from the joined issue, so an empty one means the join is wrong.
	assert.Equal(t, "issue1", comment.Title)
	assert.Equal(t, "1", comment.Payload["index"])
	assert.Equal(t, "false", comment.Payload["is_pull"])
	assert.Equal(t, "true", byKey[key(KindComment, s.pullComment)].Payload["is_pull"])

	status := byKey[key(KindStatus, s.commitStatus)]
	assert.Equal(t, "success", status.Payload["state"])
	assert.Equal(t, "ci/hedgehog", status.Payload["context"])
	assert.Equal(t, "hedgehog build passed", status.Title)

	audit := byKey[key(KindAgentAudit, s.auditEvent)]
	assert.Equal(t, "GET", audit.Payload["method"])
	assert.Equal(t, "https://example.com/api/v1/repos/user2/repo1/issues", audit.Payload["request_url"])
}

// An unsubmitted review is visible to nobody but its author, and the code comments written into it
// are the way it leaks: they are ordinary `comment` rows with a review_id pointing at a draft.
func TestListHidesDraftReviewCommentsFromEveryoneButTheirAuthor(t *testing.T) {
	s := prepareStream(t)
	t.Run("another reader does not see the draft", func(t *testing.T) {
		events, _ := listOrFail(t, &ListOptions{
			Repo:       s.repo,
			Doer:       s.owner,
			Permission: repoPermission(t, s.repo, s.owner),
			Since:      since(),
			Limit:      50,
		})
		assert.NotContains(t, keysOf(events), key(KindComment, s.draftComment))
	})

	t.Run("the reviewer sees their own draft comment", func(t *testing.T) {
		events, _ := listOrFail(t, &ListOptions{
			Repo:       s.repo,
			Doer:       s.draftReviewer,
			Permission: repoPermission(t, s.repo, s.draftReviewer),
			Since:      since(),
			Limit:      50,
		})
		assert.Contains(t, keysOf(events), key(KindComment, s.draftComment))
	})

	t.Run("the pending review itself is never an event", func(t *testing.T) {
		for _, doer := range []*user_model.User{s.owner, s.draftReviewer} {
			events, _ := listOrFail(t, &ListOptions{
				Repo:       s.repo,
				Doer:       doer,
				Permission: repoPermission(t, s.repo, doer),
				Since:      since(),
				Limit:      50,
			})
			assert.NotContains(t, keysOf(events), key(KindReview, s.draftReview))
			assert.Contains(t, keysOf(events), key(KindReview, s.review))
		}
	})
}

// The audit trail records every URL an agent touched, which is more than read access to a
// repository's contents implies - so it is repository admins only, and a reader gets a stream
// without that kind rather than a 403.
func TestListGivesTheAuditTrailToAdminsOnly(t *testing.T) {
	s := prepareStream(t)
	admin := repoPermission(t, s.repo, s.owner)
	require.True(t, admin.IsAdmin())
	events, _ := listOrFail(t, &ListOptions{
		Repo: s.repo, Doer: s.owner, Permission: admin, Since: since(), Limit: 50,
	})
	assert.Contains(t, keysOf(events), key(KindAgentAudit, s.auditEvent))

	reader := repoPermission(t, s.repo, s.outsider)
	require.False(t, reader.IsAdmin())
	events, _ = listOrFail(t, &ListOptions{
		Repo: s.repo, Doer: s.outsider, Permission: reader, Since: since(), Limit: 50,
	})
	assert.NotContains(t, keysOf(events), key(KindAgentAudit, s.auditEvent))
	// The rest of the stream is still there: a missing kind is not a refused request.
	assert.Contains(t, keysOf(events), key(KindComment, s.issueComment))
}

// Issues and pull requests are separate units with separate read access, and a reader of one must
// not see the other's comments. The check is worth a query rather than a rendered condition,
// because the condition is joined through `issue` and it is the join that decides.
func TestListFiltersCommentsReviewsAndStatusesByUnit(t *testing.T) {
	s := prepareStream(t)
	list := func(permission *access_model.Permission) []string {
		events, _ := listOrFail(t, &ListOptions{
			Repo:       s.repo,
			Doer:       s.outsider,
			Permission: permission,
			Kinds:      []Kind{KindComment, KindReview, KindStatus, KindAgentAudit},
			Since:      since(),
			Limit:      50,
		})
		return keysOf(events)
	}

	t.Run("issues only", func(t *testing.T) {
		got := list(unitPermission(s.repo.ID, unit.TypeIssues))
		assert.Equal(t, []string{key(KindComment, s.issueComment)}, got)
	})

	t.Run("pull requests only", func(t *testing.T) {
		got := list(unitPermission(s.repo.ID, unit.TypePullRequests))
		assert.Equal(t, []string{
			key(KindReview, s.review),
			key(KindComment, s.pullComment),
		}, got)
	})

	t.Run("code only", func(t *testing.T) {
		got := list(unitPermission(s.repo.ID, unit.TypeCode))
		assert.Equal(t, []string{key(KindStatus, s.commitStatus)}, got)
	})

	t.Run("a wiki-only reader gets none of the four", func(t *testing.T) {
		assert.Empty(t, list(unitPermission(s.repo.ID, unit.TypeWiki)))
	})
}

// Paging is the property the whole cursor design exists for, and it is only meaningful when the
// keyset condition is executed: five tables, a page size that does not divide the result, and rows
// sharing a second across sources.
func TestListPagesEveryEventExactlyOnce(t *testing.T) {
	s := prepareStream(t)
	options := func(cursor *Cursor, limit int) *ListOptions {
		return &ListOptions{
			Repo:       s.repo,
			Doer:       s.owner,
			Permission: repoPermission(t, s.repo, s.owner),
			Since:      since(),
			Cursor:     cursor,
			Limit:      limit,
		}
	}

	whole, _ := listOrFail(t, options(nil, 50))
	require.Len(t, whole, 6)

	for _, limit := range []int{1, 2, 4} {
		t.Run("limit="+strconv.Itoa(limit), func(t *testing.T) {
			var paged []string
			var cursor *Cursor
			for range 20 {
				events, next := listOrFail(t, options(cursor, limit))
				assert.LessOrEqual(t, len(events), limit)
				paged = append(paged, keysOf(events)...)
				if next == nil {
					break
				}
				// A cursor that is handed back unchanged would page forever;
				// the loop bound above is what turns that into a failure.
				cursor = next
			}
			assert.Equal(t, keysOf(whole), paged,
				"paging visits the same events, in the same order, exactly once")
		})
	}
}

// A cursor encoded by one page and decoded by the next is the round trip a client actually makes,
// and the position it names has to survive it.
func TestListResumesFromAnEncodedCursor(t *testing.T) {
	s := prepareStream(t)
	first, next := listOrFail(t, &ListOptions{
		Repo: s.repo, Doer: s.owner, Permission: repoPermission(t, s.repo, s.owner),
		Since: since(), Limit: 2,
	})
	require.NotNil(t, next)

	decoded, err := DecodeCursor(next.Encode())
	require.NoError(t, err)

	second, _ := listOrFail(t, &ListOptions{
		Repo: s.repo, Doer: s.owner, Permission: repoPermission(t, s.repo, s.owner),
		Since: since(), Cursor: decoded, Limit: 2,
	})
	assert.Empty(t, intersect(keysOf(first), keysOf(second)))
	assert.Equal(t, []string{key(KindReview, s.review), key(KindComment, s.pullComment)}, keysOf(second))
}

// ?q= reaches every source that has text, through whichever of the two paths the dialect provides.
func TestListSearchesEachSourcesText(t *testing.T) {
	s := prepareStream(t)
	events, _ := listOrFail(t, &ListOptions{
		Repo: s.repo, Doer: s.owner, Permission: repoPermission(t, s.repo, s.owner),
		Since: since(), Query: "hedgehog", Limit: 50,
	})

	assert.Equal(t, []string{
		key(KindStatus, s.commitStatus),
		key(KindComment, s.issueComment),
		// action.content is serialized JSON, and the search matches that JSON's text.
		key(KindAction, s.action),
	}, keysOf(events))
}

// The audit source searches request_url through LIKE on every dialect, because a URL is not prose
// and to_tsvector would tokenize a path into something a "search for this path" misses.
func TestListSearchesTheAuditTrailByURL(t *testing.T) {
	s := prepareStream(t)
	events, _ := listOrFail(t, &ListOptions{
		Repo: s.repo, Doer: s.owner, Permission: repoPermission(t, s.repo, s.owner),
		Kinds: []Kind{KindAgentAudit}, Since: since(), Query: "/repos/user2/repo1/", Limit: 50,
	})
	assert.Equal(t, []string{key(KindAgentAudit, s.auditEvent)}, keysOf(events))

	events, _ = listOrFail(t, &ListOptions{
		Repo: s.repo, Doer: s.owner, Permission: repoPermission(t, s.repo, s.owner),
		Kinds: []Kind{KindAgentAudit}, Since: since(), Query: "/repos/other/", Limit: 50,
	})
	assert.Empty(t, events)
}

// ?actor= narrows every source at once, so the filter is asserted where the sources disagree: the
// draft's author wrote one row in this window and the owner wrote the rest.
func TestListFiltersByActor(t *testing.T) {
	s := prepareStream(t)
	events, _ := listOrFail(t, &ListOptions{
		Repo: s.repo, Doer: s.draftReviewer, Permission: repoPermission(t, s.repo, s.draftReviewer),
		Since: since(), ActorID: s.draftReviewer.ID, Limit: 50,
	})
	assert.Equal(t, []string{key(KindComment, s.draftComment)}, keysOf(events))

	events, _ = listOrFail(t, &ListOptions{
		Repo: s.repo, Doer: s.owner, Permission: repoPermission(t, s.repo, s.owner),
		Since: since(), ActorID: s.owner.ID, Limit: 50,
	})
	assert.NotContains(t, keysOf(events), key(KindComment, s.draftComment))
	assert.Contains(t, keysOf(events), key(KindAction, s.action))
}

// since and until are inclusive on both ends, against real rows rather than against a rendered
// condition - an off-by-one second here drops or repeats an event at a page boundary.
func TestListRangeIsInclusive(t *testing.T) {
	s := prepareStream(t)
	from, to := streamBase+20, streamBase+40
	events, _ := listOrFail(t, &ListOptions{
		Repo: s.repo, Doer: s.owner, Permission: repoPermission(t, s.repo, s.owner),
		Since: &from, Until: &to, Limit: 50,
	})

	assert.Equal(t, []string{
		key(KindReview, s.review),
		key(KindComment, s.pullComment),
		key(KindComment, s.issueComment),
	}, keysOf(events))
}

// Without a window the same queries run over the fixture rows too, which is the case a hand-built
// window can hide: the fixtures hold comment types this stream deliberately drops (label changes,
// milestone changes - everything the `action` row next to them already reports) and rows belonging
// to other repositories.
func TestListOverFixtureRowsStaysWithinTheRepository(t *testing.T) {
	s := prepareStream(t)
	events, next := listOrFail(t, &ListOptions{
		Repo: s.repo, Doer: s.owner, Permission: repoPermission(t, s.repo, s.owner),
		Limit: MaxLimit,
	})
	require.NotEmpty(t, events)
	assert.Nil(t, next, "repo1's fixtures fit in one page of MaxLimit")

	// The window's rows sort first, because nothing in models/fixtures is stamped in 2033.
	assert.Equal(t, key(KindAgentAudit, s.auditEvent), keysOf(events)[0])

	for _, e := range events {
		assert.Equal(t, s.repo.ID, e.RepoID, "an event of another repository reached the stream")
		if e.Kind == KindComment {
			comment := unittest.AssertExistsAndLoadBean(t, &issues_model.Comment{ID: e.SourceID})
			assert.Contains(t, contentBearingCommentTypes, comment.Type,
				"a system comment reached the stream, duplicating the action row that reports it")
		}
	}
}

func intersect(a, b []string) []string {
	seen := make(map[string]bool, len(a))
	for _, v := range a {
		seen[v] = true
	}
	var both []string
	for _, v := range b {
		if seen[v] {
			both = append(both, v)
		}
	}
	return both
}
