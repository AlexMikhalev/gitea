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
	// A review is stamped by updated_unix, the second it was submitted; a row written straight
	// to the table has never been anything else, so the two are the same second here. The rows
	// where they differ are what TestListStampsAReviewWithItsSubmission is about.
	review := &issues_model.Review{
		Type:        issues_model.ReviewTypeApprove,
		ReviewerID:  s.owner.ID,
		IssueID:     2,
		Content:     "published review",
		CreatedUnix: streamBase + 40,
		UpdatedUnix: streamBase + 40,
	}
	draftReview := &issues_model.Review{
		Type:        issues_model.ReviewTypePending,
		ReviewerID:  s.draftReviewer.ID,
		IssueID:     2,
		Content:     "unsubmitted draft",
		CreatedUnix: streamBase + 50,
		UpdatedUnix: streamBase + 50,
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

// Every other row this file writes sits at its own second, which is the one case the cursor does
// not need to exist for: created_unix alone already orders those. The keyset condition decides
// something only when two sources stamp the same second - the common case on a busy repository -
// and what it decides is per kind, so it is only here that each adapter's agreement between the
// Kind it hands keysetCond and the column pair its ORDER BY names becomes observable at all.
//
// Every kind gets a *pair* at that second, because the two halves of a source's sort key fail
// separately: a wrong Kind in keysetCond shows up at the boundary between two kinds, and an
// ORDER BY that disagrees with the keyset on direction shows up only within one kind, when a page
// cuts that kind's rows in half. Paging one event at a time puts a cursor on both.
func TestListPagesEventsSharingOneSecond(t *testing.T) {
	s := prepareStream(t)
	// One second past everything prepareStream wrote, so the collision is a block at the head of
	// the stream and its internal order is entirely the tie-break under test.
	at := streamBase + 100

	newAction := func(message string) *activities_model.Action {
		return &activities_model.Action{
			UserID:      s.owner.ID,
			ActUserID:   s.owner.ID,
			RepoID:      s.repo.ID,
			OpType:      activities_model.ActionCommitRepo,
			RefName:     "main",
			Content:     `{"Len":1,"Commits":[{"Message":"` + message + `"}]}`,
			CreatedUnix: at,
		}
	}
	newComment := func(content string) *issues_model.Comment {
		return &issues_model.Comment{
			Type:        issues_model.CommentTypeComment,
			PosterID:    s.owner.ID,
			IssueID:     1,
			Content:     content,
			CreatedUnix: at,
		}
	}
	// A re-approval by the same reviewer supersedes the earlier one rather than removing it, so
	// two published reviews at one second is a state the stream really reaches.
	newReview := func(content string) *issues_model.Review {
		return &issues_model.Review{
			Type:        issues_model.ReviewTypeApprove,
			ReviewerID:  s.owner.ID,
			IssueID:     2,
			Content:     content,
			CreatedUnix: at,
			UpdatedUnix: at,
		}
	}
	newStatus := func(index int64, context string) *git_model.CommitStatus {
		return &git_model.CommitStatus{
			Index:       index,
			RepoID:      s.repo.ID,
			State:       commitstatus.CommitStatusSuccess,
			SHA:         "1234123412341234123412341234123412341234",
			TargetURL:   "https://example.com/builds/" + strconv.FormatInt(index, 10),
			Description: "build in the shared second",
			Context:     context,
			CreatorID:   s.owner.ID,
			CreatedUnix: at,
		}
	}
	newAudit := func(eventID, path string) *agent_model.AuditEvent {
		return &agent_model.AuditEvent{
			RepoID:      s.repo.ID,
			AgentUserID: s.owner.ID,
			OwnerUserID: s.owner.ID,
			AgentKeyID:  1,
			EventID:     eventID,
			PubKey:      "abababababababababababababababababababababababababababababababab",
			Method:      "GET",
			RequestURL:  "https://example.com/api/v1/repos/user2/repo1/" + path,
			CreatedUnix: at,
		}
	}

	firstAction, secondAction := newAction("first push"), newAction("second push")
	firstComment, secondComment := newComment("first remark"), newComment("second remark")
	firstReview, secondReview := newReview("first approval"), newReview("second approval")
	firstStatus, secondStatus := newStatus(101, "ci/first"), newStatus(102, "ci/second")
	firstAudit := newAudit("e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2", "pulls")
	secondAudit := newAudit("e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3e3", "branches")
	insertAt(t,
		firstAction, secondAction,
		firstComment, secondComment,
		firstReview, secondReview,
		firstStatus, secondStatus,
		firstAudit, secondAudit,
	)

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

	whole, next := listOrFail(t, options(nil, 50))
	require.Nil(t, next)
	require.Len(t, whole, 16, "ten rows sharing a second on top of the six prepareStream wrote")
	// Nothing separates these ten but the tie-break: kind ascending, then source id descending.
	require.Equal(t, []string{
		key(KindAction, secondAction.ID),
		key(KindAction, firstAction.ID),
		key(KindAgentAudit, secondAudit.ID),
		key(KindAgentAudit, firstAudit.ID),
		key(KindComment, secondComment.ID),
		key(KindComment, firstComment.ID),
		key(KindReview, secondReview.ID),
		key(KindReview, firstReview.ID),
		key(KindStatus, secondStatus.ID),
		key(KindStatus, firstStatus.ID),
	}, keysOf(whole)[:10], "AllKinds order, then newest id, decides a shared second")

	var paged []string
	var cursor *Cursor
	// One event per page puts a cursor on every position inside the shared second: the four
	// boundaries between one kind and the next, and the split inside each kind's pair.
	for range 40 {
		events, next := listOrFail(t, options(cursor, 1))
		require.LessOrEqual(t, len(events), 1)
		paged = append(paged, keysOf(events)...)
		if next == nil {
			break
		}
		// A cursor a source refuses to advance past would page forever; the loop bound is what
		// turns that into a failed assertion rather than a hung test.
		cursor = next
	}
	assert.Equal(t, keysOf(whole), paged,
		"paging one at a time visits every event exactly once, in AllKinds order")
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

// One submission is one event. Gitea writes both a `review` row and a CommentTypeReview `comment`
// row for it, with the same content, the same poster and the same second, and the two are read by
// two different adapters - so without the filter the stream reports it twice and a client has no
// field on either event with which to notice.
func TestListReportsASubmittedReviewOnce(t *testing.T) {
	s := prepareStream(t)

	// The row SubmitReview writes beside the review prepareStream already inserted.
	submission := &issues_model.Comment{
		Type:        issues_model.CommentTypeReview,
		PosterID:    s.owner.ID,
		IssueID:     2,
		ReviewID:    s.review,
		Content:     "published review",
		CreatedUnix: streamBase + 40,
	}
	// A CommentTypeReview row whose review is gone still records something that happened, and
	// is the case the "review joined to nothing" branch keeps.
	orphan := &issues_model.Comment{
		Type:        issues_model.CommentTypeReview,
		PosterID:    s.owner.ID,
		IssueID:     2,
		ReviewID:    9_999_999,
		Content:     "a review whose row was deleted",
		CreatedUnix: streamBase + 41,
	}
	insertAt(t, submission, orphan)

	events, _ := listOrFail(t, &ListOptions{
		Repo: s.repo, Doer: s.owner, Permission: repoPermission(t, s.repo, s.owner),
		Since: since(), Limit: 50,
	})
	got := keysOf(events)

	assert.Contains(t, got, key(KindReview, s.review),
		"the review row is the copy that is kept - it carries the review type, official and dismissed")
	assert.NotContains(t, got, key(KindComment, submission.ID),
		"one submission reached the stream as two events")
	assert.Contains(t, got, key(KindComment, orphan.ID),
		"a review comment with no review row left is the only report of it there is")
}

// A review event is stamped with the second the review was *submitted*, and only SubmitReview can
// tell whether it is: it is the one path that leaves created_unix and updated_unix disagreeing.
//
// Gitea writes the `review` row as ReviewTypePending the moment its author types their first draft
// line comment, and SubmitReview flips the type in place hours or days later without touching
// created_unix. Every other fixture in this file inserts a row that was already submitted, so a
// regression to created_unix passes all of them - the row only becomes reportable at a second the
// stream never sees if the two differ, which is what this test manufactures.
func TestListStampsAReviewWithItsSubmission(t *testing.T) {
	s := prepareStream(t)

	// The reviewer is user4 rather than user1, who already has a pending review on issue 2 in
	// the shipped fixtures; GetCurrentReview would find that one instead of this one.
	reviewer := s.outsider
	issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: 2})
	require.NoError(t, issue.LoadRepo(t.Context()))

	// The draft, typed long ago. It has to predate the wall clock rather than streamBase,
	// because SubmitReview stamps updated_unix with the current second and streamBase is a
	// second in 2033 - the whole point is that the submission lands after the draft.
	draftedAt := timeutil.TimeStamp(1_000_000_000)
	insertAt(t, &issues_model.Review{
		Type:        issues_model.ReviewTypePending,
		ReviewerID:  reviewer.ID,
		IssueID:     issue.ID,
		Content:     "",
		CreatedUnix: draftedAt,
		UpdatedUnix: draftedAt,
	})

	// The submission. Called rather than simulated: what is under test is precisely which of
	// the columns SubmitReview writes, and a hand-built row would encode this test's belief
	// about that rather than check it.
	review, _, err := issues_model.SubmitReview(t.Context(), reviewer, issue,
		issues_model.ReviewTypeComment, "submitted long after the draft was started", "", false, nil)
	require.NoError(t, err)

	submitted := unittest.AssertExistsAndLoadBean(t, &issues_model.Review{ID: review.ID})
	require.Equal(t, draftedAt, submitted.CreatedUnix,
		"SubmitReview leaves created_unix at the draft's second - if this ever stops being true the finding this test guards is gone, not the test")
	require.Greater(t, submitted.UpdatedUnix, submitted.CreatedUnix)

	// The client that reads the stream between the two seconds and then polls forward. With
	// created_unix the submission lands behind `since` and is never returned at all.
	after := submitted.CreatedUnix + 1
	events, _ := listOrFail(t, &ListOptions{
		Repo: s.repo, Doer: s.owner, Permission: repoPermission(t, s.repo, s.owner),
		Since: &after, Limit: MaxLimit,
	})

	var stamped *Event
	for _, e := range events {
		if e.Kind == KindReview && e.SourceID == review.ID {
			stamped = e
		}
	}
	require.NotNil(t, stamped, "a review submitted after `since` never reached the stream")
	assert.Equal(t, submitted.UpdatedUnix, stamped.CreatedUnix,
		"the event carries the second the review was submitted, not the second the draft was started")

	// And the comment SubmitReview writes beside it - the copy that did carry the right instant -
	// is still the one dropped, so the submission is one event and not two.
	for _, e := range events {
		if e.Kind == KindComment {
			comment := unittest.AssertExistsAndLoadBean(t, &issues_model.Comment{ID: e.SourceID})
			assert.NotEqual(t, review.ID, comment.ReviewID,
				"one submission reached the stream as two events")
		}
	}
}

// A review that a later one supersedes stays where it was.
//
// Submitting an approve or a reject marks every earlier approve/reject by the same reviewer on the
// same issue as dismissed (models/issues/review.go, CreateReview). That is a per-re-approval event,
// not a rare admin action, and doing it through an xorm bean would restamp updated_unix on every
// row it touched - the column this stream orders, pages and range-filters reviews by. A review
// submitted long ago would then jump to now: a client backfilling newest-first has already walked
// past that position and would never be handed the review at all, and the `?since=`/`?until=`
// window that used to contain it no longer would.
//
// The window here is a real one rather than the 2033 band the rest of this file writes into,
// because the failure is about a row leaving the window it was in.
func TestListKeepsASupersededReviewInPlace(t *testing.T) {
	s := prepareStream(t)

	// user4 rather than user1, who already holds a pending review on issue 2 in the shipped
	// fixtures - SubmitReview would reuse that one instead of taking the CreateReview path
	// this test is about.
	reviewer := s.outsider
	issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: 2})
	require.NoError(t, issue.LoadRepo(t.Context()))

	approvedAt := timeutil.TimeStamp(1_000_000_000)
	first := &issues_model.Review{
		Type:        issues_model.ReviewTypeApprove,
		ReviewerID:  reviewer.ID,
		IssueID:     issue.ID,
		Content:     "approved long ago",
		CreatedUnix: approvedAt,
		UpdatedUnix: approvedAt,
	}
	insertAt(t, first)

	from, to := approvedAt-1, approvedAt+1
	inWindow := func() []string {
		t.Helper()
		events, _ := listOrFail(t, &ListOptions{
			Repo: s.repo, Doer: s.owner, Permission: repoPermission(t, s.repo, s.owner),
			Since: &from, Until: &to, Limit: MaxLimit,
		})
		return keysOf(events)
	}
	require.Contains(t, inWindow(), key(KindReview, first.ID),
		"the review is not in its own window before anything has happened to it")

	// The re-approval. Called rather than simulated: what is under test is which columns the
	// supersede path writes, and a hand-built update would encode this test's belief about that.
	again, _, err := issues_model.SubmitReview(t.Context(), reviewer, issue,
		issues_model.ReviewTypeApprove, "approved again", "", false, nil)
	require.NoError(t, err)
	require.NotEqual(t, first.ID, again.ID, "SubmitReview reused the row instead of superseding it")

	superseded := unittest.AssertExistsAndLoadBean(t, &issues_model.Review{ID: first.ID})
	require.True(t, superseded.Dismissed,
		"the supersede no longer happens at all - this test guards how it is written, not whether")
	assert.Equal(t, approvedAt, superseded.UpdatedUnix,
		"superseding restamped the review, moving its position in the stream")

	assert.Contains(t, inWindow(), key(KindReview, first.ID),
		"a superseded review left the historical window it was submitted in")
}

// The pair is in the shipped fixtures on its own, without this file writing anything: `review` 20
// and `comment` 9 (review_id 20) are one submission at 946684810, and `review` 21 and `comment` 10
// (review_id 21) are another. A window-scoped test cannot catch a regression here, because the rows
// it writes are the ones it also asserts about.
func TestListDeduplicatesFixtureReviewSubmissions(t *testing.T) {
	s := prepareStream(t)
	events, _ := listOrFail(t, &ListOptions{
		Repo: s.repo, Doer: s.owner, Permission: repoPermission(t, s.repo, s.owner),
		Limit: MaxLimit,
	})
	got := keysOf(events)

	for _, pair := range []struct{ review, comment int64 }{{20, 9}, {21, 10}} {
		comment := unittest.AssertExistsAndLoadBean(t, &issues_model.Comment{ID: pair.comment})
		require.Equal(t, issues_model.CommentTypeReview, comment.Type)
		require.Equal(t, pair.review, comment.ReviewID)

		assert.Contains(t, got, key(KindReview, pair.review))
		assert.NotContains(t, got, key(KindComment, pair.comment))
	}
}

// One act is one event across kinds, not only within one. Gitea writes an `action` row beside every
// comment and every submitted review, so without the second half of the dedup the stream reports
// each of them twice under two kinds - and the action row's payload.comment_id names the very
// comment the review/comment dedup had already dropped, a reference no page could ever resolve.
func TestListReportsACommentedActOnce(t *testing.T) {
	s := prepareStream(t)

	// The CommentTypeReview row SubmitReview writes beside a review, and the action row the
	// feed notifier writes beside both - the case where comment_id pointed at nothing.
	submission := &issues_model.Comment{
		Type:        issues_model.CommentTypeReview,
		PosterID:    s.owner.ID,
		IssueID:     2,
		ReviewID:    s.review,
		Content:     "published review",
		CreatedUnix: streamBase + 40,
	}
	// A system comment is not an event of this stream, so the action recorded against it is
	// that act's only report and has to stay.
	closure := &issues_model.Comment{
		Type:        issues_model.CommentTypeClose,
		PosterID:    s.owner.ID,
		IssueID:     1,
		CreatedUnix: streamBase + 25,
	}
	insertAt(t, submission, closure)

	action := func(opType activities_model.ActionType, commentID int64, at timeutil.TimeStamp) *activities_model.Action {
		return &activities_model.Action{
			UserID:      s.owner.ID,
			ActUserID:   s.owner.ID,
			RepoID:      s.repo.ID,
			OpType:      opType,
			CommentID:   commentID,
			Content:     "2|a hedgehog on the issue",
			CreatedUnix: at,
		}
	}
	commented := action(activities_model.ActionCommentIssue, s.issueComment, streamBase+21)
	reviewed := action(activities_model.ActionApprovePullRequest, submission.ID, streamBase+41)
	// An action whose comment was deleted out from under it still records something that
	// happened, and is the case the "comment joined to nothing" branch keeps.
	orphan := action(activities_model.ActionCommentIssue, 9_999_999, streamBase+22)
	closed := action(activities_model.ActionCloseIssue, closure.ID, streamBase+26)
	insertAt(t, commented, reviewed, orphan, closed)

	got := keysOf(mustList(t, s, s.owner, repoPermission(t, s.repo, s.owner)))

	assert.NotContains(t, got, key(KindAction, commented.ID),
		"a comment reached the stream as two events, once as `comment` and once as `action`")
	assert.Contains(t, got, key(KindComment, s.issueComment),
		"the comment is the copy that is kept - it carries the full text, the issue and its index")

	assert.NotContains(t, got, key(KindAction, reviewed.ID),
		"a submitted review reached the stream as two events")
	assert.Contains(t, got, key(KindReview, s.review))
	assert.NotContains(t, got, key(KindComment, submission.ID))

	assert.Contains(t, got, key(KindAction, orphan.ID),
		"an action whose comment is gone is the only report of it there is")
	assert.Contains(t, got, key(KindAction, closed.ID),
		"a close is recorded against a system comment this stream does not report")

	// The action kind is deliberately not filtered by unit. For a reader the comment kind
	// reports nothing to, there is no duplicate to drop - and dropping it anyway would turn the
	// deduplication into a deletion.
	t.Run("a reader who cannot see comments keeps the action row", func(t *testing.T) {
		got := keysOf(mustList(t, s, s.outsider, unitPermission(s.repo.ID, unit.TypeCode)))
		assert.Contains(t, got, key(KindAction, commented.ID))
		assert.NotContains(t, got, key(KindComment, s.issueComment))
	})

	t.Run("a pull-requests-only reader keeps the issue comment's action row", func(t *testing.T) {
		got := keysOf(mustList(t, s, s.outsider, unitPermission(s.repo.ID, unit.TypePullRequests)))
		assert.Contains(t, got, key(KindAction, commented.ID),
			"the issue comment is invisible to this reader, so its action row is the only report")
		assert.NotContains(t, got, key(KindAction, reviewed.ID),
			"the review is visible to this reader, so its action row is still a duplicate")
	})
}

// mustList reads the whole written window for one doer and permission.
func mustList(t *testing.T, s *stream, doer *user_model.User, permission *access_model.Permission) []*Event {
	t.Helper()
	events, _ := listOrFail(t, &ListOptions{
		Repo: s.repo, Doer: doer, Permission: permission, Since: since(), Limit: MaxLimit,
	})
	return events
}

// A line comment is not a duplicate of the review it belongs to - it is a remark of its own, at its
// own place in the diff - so it stays, and carries the review's id so a client can group the two
// rather than having to infer the relation from a shared second.
func TestListCorrelatesCodeCommentsWithTheirReview(t *testing.T) {
	s := prepareStream(t)
	events, _ := listOrFail(t, &ListOptions{
		Repo: s.repo, Doer: s.draftReviewer, Permission: repoPermission(t, s.repo, s.draftReviewer),
		Since: since(), Limit: 50,
	})

	var draft *Event
	for _, e := range events {
		if e.Kind == KindComment && e.SourceID == s.draftComment {
			draft = e
		}
	}
	require.NotNil(t, draft)
	assert.Equal(t, strconv.FormatInt(s.draftReview, 10), draft.Payload["review_id"])

	// A comment that belongs to no review says so by carrying no such key, rather than by
	// carrying a zero a client would have to know to ignore.
	for _, e := range events {
		if e.Kind == KindComment && e.SourceID == s.issueComment {
			assert.NotContains(t, e.Payload, "review_id")
		}
	}
}

// A review request is not a review. AddReviewRequest writes a `review` row of ReviewTypeRequest
// whose reviewer_id is the person who was asked, so surfacing it would report a review by someone
// who has not reviewed - and ?actor= would match them for an act somebody else performed.
func TestListDoesNotReportReviewRequestsAsReviews(t *testing.T) {
	s := prepareStream(t)
	request := &issues_model.Review{
		Type:        issues_model.ReviewTypeRequest,
		ReviewerID:  s.outsider.ID, // the person asked, not the person who asked
		IssueID:     2,
		CreatedUnix: streamBase + 45,
	}
	insertAt(t, request)

	events, _ := listOrFail(t, &ListOptions{
		Repo: s.repo, Doer: s.owner, Permission: repoPermission(t, s.repo, s.owner),
		Since: since(), Limit: 50,
	})
	got := keysOf(events)
	assert.NotContains(t, got, key(KindReview, request.ID))
	assert.Contains(t, got, key(KindReview, s.review), "a submitted review is still reported")

	events, _ = listOrFail(t, &ListOptions{
		Repo: s.repo, Doer: s.owner, Permission: repoPermission(t, s.repo, s.owner),
		Since: since(), ActorID: s.outsider.ID, Limit: 50,
	})
	assert.Empty(t, keysOf(events),
		"the requested reviewer did nothing in this window and must not be credited with the request")
}

// `%` and `_` are LIKE's wildcards, and `q` is a term a repository reader typed rather than a
// pattern they may write. Unescaped, `?q=%` matches every row of every source - a free full scan of
// the two largest tables on the instance from anyone who can read one repository.
func TestListDoesNotTreatQueryWildcardsAsPatterns(t *testing.T) {
	s := prepareStream(t)
	literal := &issues_model.Comment{
		Type:        issues_model.CommentTypeComment,
		PosterID:    s.owner.ID,
		IssueID:     1,
		Content:     "coverage went from 90% to 95%",
		CreatedUnix: streamBase + 90,
	}
	insertAt(t, literal)

	list := func(query string) []string {
		events, _ := listOrFail(t, &ListOptions{
			Repo: s.repo, Doer: s.owner, Permission: repoPermission(t, s.repo, s.owner),
			Since: since(), Query: query, Limit: 50,
		})
		return keysOf(events)
	}

	assert.Equal(t, []string{key(KindComment, literal.ID)}, list("%"),
		"?q=%% is a search for a per cent sign, not for everything")
	// `_` matches any single character, so an unescaped one turns this into the query that
	// finds "a hedgehog on the issue".
	assert.Empty(t, list("a_hedgehog"))
	assert.Equal(t, []string{key(KindComment, s.issueComment)}, list("a hedgehog"),
		"escaping must not stop an ordinary term from matching")
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
