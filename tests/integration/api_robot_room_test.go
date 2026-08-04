// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	issues_model "code.gitea.io/gitea/models/issues"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/unittest"
	"code.gitea.io/gitea/modules/git/gitcmd"
	"code.gitea.io/gitea/modules/gitrepo"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/tests"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const roomHookTestSecret = "room-hook-test-secret"

// createRawGitBranch creates a branch directly in the on-disk git repo,
// bypassing the push machinery (whose pre-receive hook would need a live
// internal API server, i.e. onGiteaRun). The room hook resolves branches
// straight from the git repo, so a raw ref is all it takes.
func createRawGitBranch(t *testing.T, repo *repo_model.Repository, name, sha string) {
	t.Helper()
	_, _, err := gitcmd.NewCommand("branch").AddDynamicArguments(name, sha).
		WithDir(repo.RepoPath()).
		RunStdString(t.Context())
	require.NoError(t, err)
}

// deleteRawGitBranch removes a branch created by createRawGitBranch.
func deleteRawGitBranch(t *testing.T, repo *repo_model.Repository, name string) func() {
	return func() {
		_, _, _ = gitcmd.NewCommand("branch", "-D").AddDynamicArguments(name).
			WithDir(repo.RepoPath()).
			RunStdString(t.Context())
	}
}

// signRoomHookBody computes the X-Gitea-Signature value for a webhook delivery.
func signRoomHookBody(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

// makeRoomHookRequest POSTs a webhook delivery to the room hook endpoint.
func makeRoomHookRequest(t *testing.T, event, body, sig string, expectedStatus int) *httptest.ResponseRecorder {
	t.Helper()
	req := NewRequestWithBody(t, "POST", "/api/v1/robot/room/hook", strings.NewReader(body)).
		SetHeader("X-Gitea-Event", event).
		SetHeader("Content-Type", "application/json")
	if sig != "" {
		req.SetHeader("X-Gitea-Signature", sig)
	}
	return MakeRequest(t, req, expectedStatus)
}

// roomPushPayload builds a push payload for repo1 (owned by user2).
func roomPushPayload(ref, after string) string {
	return fmt.Sprintf(`{"ref":%q,"after":%q,`+
		`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`, ref, after)
}

// TestAPIRobotRoom exercises the full branch-as-room hook flow in-process:
// push opens the room, a duplicate push is a no-op, a status event comments on
// the room, a delete event closes it, and unsigned deliveries are rejected.
func TestAPIRobotRoom(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	oldSecret := setting.IssueGraphSettings.RoomHookSecret
	setting.IssueGraphSettings.RoomHookSecret = roomHookTestSecret
	defer func() { setting.IssueGraphSettings.RoomHookSecret = oldSecret }()

	const (
		branch = "feat/room-inttest"
		ref    = "refs/heads/" + branch
		head   = "0123456789abcdef0123456789abcdef01234567"
		title  = "Room: " + branch
	)

	t.Run("RouteDisabledWithoutSecret", func(t *testing.T) {
		setting.IssueGraphSettings.RoomHookSecret = ""
		defer func() { setting.IssueGraphSettings.RoomHookSecret = roomHookTestSecret }()
		body := roomPushPayload(ref, head)
		makeRoomHookRequest(t, "push", body, signRoomHookBody(roomHookTestSecret, body), http.StatusNotFound)
	})

	t.Run("UnsignedRejected", func(t *testing.T) {
		body := roomPushPayload(ref, head)
		// No signature header at all
		makeRoomHookRequest(t, "push", body, "", http.StatusUnauthorized)
		// Wrong signature
		makeRoomHookRequest(t, "push", body, strings.Repeat("0", 64), http.StatusUnauthorized)
		assert.Equal(t, 0, unittest.GetCount(t, &issues_model.Issue{RepoID: 1, Title: title}))
	})

	t.Run("UnknownEventIgnored", func(t *testing.T) {
		body := `{}`
		makeRoomHookRequest(t, "issues", body, signRoomHookBody(roomHookTestSecret, body), http.StatusAccepted)
	})

	t.Run("PushOpensRoomIdempotently", func(t *testing.T) {
		body := roomPushPayload(ref, head)

		// First push creates the room
		makeRoomHookRequest(t, "push", body, signRoomHookBody(roomHookTestSecret, body), http.StatusOK)
		issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: title})
		assert.Contains(t, issue.Content, "gitea-robot/room")
		assert.Contains(t, issue.Content, branch)
		assert.False(t, issue.IsClosed)

		// Re-pushing never creates a duplicate
		makeRoomHookRequest(t, "push", body, signRoomHookBody(roomHookTestSecret, body), http.StatusOK)
		assert.Equal(t, 1, unittest.GetCount(t, &issues_model.Issue{RepoID: 1, Title: title}))
	})

	t.Run("NonFeatPushIgnored", func(t *testing.T) {
		body := roomPushPayload("refs/heads/main", head)
		makeRoomHookRequest(t, "push", body, signRoomHookBody(roomHookTestSecret, body), http.StatusAccepted)
		assert.Equal(t, 0, unittest.GetCount(t, &issues_model.Issue{RepoID: 1, Title: "Room: main"}))
	})

	t.Run("StatusCommentsOnRoom", func(t *testing.T) {
		issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: title})
		before := unittest.GetCount(t, &issues_model.Comment{IssueID: issue.ID, Type: issues_model.CommentTypeComment})

		// The SHA matches the head recorded in the room marker, so the status
		// resolves to this room even without a feat branch in the git repo.
		body := fmt.Sprintf(`{"sha":%q,"state":"success","context":"ci/test","description":"all green",`+
			`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`, head)
		makeRoomHookRequest(t, "status", body, signRoomHookBody(roomHookTestSecret, body), http.StatusOK)

		after := unittest.GetCount(t, &issues_model.Comment{IssueID: issue.ID, Type: issues_model.CommentTypeComment})
		assert.Equal(t, before+1, after)
	})

	t.Run("StatusForUnknownSHAIgnored", func(t *testing.T) {
		issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: title})
		before := unittest.GetCount(t, &issues_model.Comment{IssueID: issue.ID, Type: issues_model.CommentTypeComment})

		body := fmt.Sprintf(`{"sha":%q,"state":"failure",`+
			`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`,
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		makeRoomHookRequest(t, "status", body, signRoomHookBody(roomHookTestSecret, body), http.StatusAccepted)

		after := unittest.GetCount(t, &issues_model.Comment{IssueID: issue.ID, Type: issues_model.CommentTypeComment})
		assert.Equal(t, before, after)
	})

	t.Run("DeleteClosesRoom", func(t *testing.T) {
		body := fmt.Sprintf(`{"ref":%q,"ref_type":"branch",`+
			`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`, branch)
		makeRoomHookRequest(t, "delete", body, signRoomHookBody(roomHookTestSecret, body), http.StatusOK)

		issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: title})
		assert.True(t, issue.IsClosed)

		// Closing an already-closed room is a no-op, not an error
		makeRoomHookRequest(t, "delete", body, signRoomHookBody(roomHookTestSecret, body), http.StatusOK)
	})

	t.Run("ZeroAfterPushClosesRoom", func(t *testing.T) {
		// A push carrying an all-zero "after" SHA is a branch deletion and
		// closes the room, like the dedicated delete event.
		const zapBranch = "feat/room-inttest-zap"
		zapRef := "refs/heads/" + zapBranch
		zapTitle := "Room: " + zapBranch

		body := roomPushPayload(zapRef, head)
		makeRoomHookRequest(t, "push", body, signRoomHookBody(roomHookTestSecret, body), http.StatusOK)
		issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: zapTitle})
		require.False(t, issue.IsClosed)

		delBody := roomPushPayload(zapRef, strings.Repeat("0", 40))
		makeRoomHookRequest(t, "push", delBody, signRoomHookBody(roomHookTestSecret, delBody), http.StatusOK)
		issue = unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: zapTitle})
		assert.True(t, issue.IsClosed)
	})

	t.Run("MergedPullRequestClosesRoom", func(t *testing.T) {
		const mergeBranch = "feat/room-inttest-merge"
		mergeTitle := "Room: " + mergeBranch

		body := roomPushPayload("refs/heads/"+mergeBranch, head)
		makeRoomHookRequest(t, "push", body, signRoomHookBody(roomHookTestSecret, body), http.StatusOK)
		unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: mergeTitle})

		// A PR closed without a merge leaves the branch (and room) alone.
		unmerged := fmt.Sprintf(`{"action":"closed","pull_request":{"merged":false,"head":{"ref":%q}},`+
			`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`, mergeBranch)
		makeRoomHookRequest(t, "pull_request", unmerged, signRoomHookBody(roomHookTestSecret, unmerged), http.StatusAccepted)
		issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: mergeTitle})
		assert.False(t, issue.IsClosed)

		// A merged PR closes the room even though the branch still exists.
		merged := fmt.Sprintf(`{"action":"closed","pull_request":{"merged":true,"head":{"ref":%q}},`+
			`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`, mergeBranch)
		makeRoomHookRequest(t, "pull_request", merged, signRoomHookBody(roomHookTestSecret, merged), http.StatusOK)
		issue = unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: mergeTitle})
		assert.True(t, issue.IsClosed)
	})

	t.Run("StatusResolvesViaGitBranchHead", func(t *testing.T) {
		// The primary status→room resolution path: a real feat/* branch in
		// the git repo, resolved through the commit graph - not the
		// marker-head fallback. The fixture branch2 is multi-commit
		// (985f030 → 5c050d3 → 65f1bf2), so its head has a parent for the
		// containment negative assertion below (master is a single root
		// commit and would not).
		repo1 := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})

		gitRepo, err := gitrepo.OpenRepository(t.Context(), repo1)
		require.NoError(t, err)
		headSHA, err := gitRepo.GetBranchCommitID("branch2")
		require.NoError(t, err)
		headCommit, err := gitRepo.GetCommit(headSHA)
		require.NoError(t, err)
		gitRepo.Close()
		parentID, err := headCommit.ParentID(0)
		require.NoError(t, err)

		const gitBranch = "feat/room-inttest-git"
		gitTitle := "Room: " + gitBranch
		createRawGitBranch(t, repo1, gitBranch, headSHA)
		defer deleteRawGitBranch(t, repo1, gitBranch)

		// Open the room with a marker head that does NOT match the branch
		// head, so only git resolution can find this room below.
		openBody := roomPushPayload("refs/heads/"+gitBranch, "ffffffffffffffffffffffffffffffffffffffff")
		makeRoomHookRequest(t, "push", openBody, signRoomHookBody(roomHookTestSecret, openBody), http.StatusOK)
		issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: gitTitle})
		countComments := func() int {
			return unittest.GetCount(t, &issues_model.Comment{IssueID: issue.ID, Type: issues_model.CommentTypeComment})
		}
		before := countComments()

		// A status on the branch head comments via git resolution.
		statusBody := fmt.Sprintf(`{"sha":%q,"state":"success","context":"ci/build",`+
			`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`, headSHA)
		makeRoomHookRequest(t, "status", statusBody, signRoomHookBody(roomHookTestSecret, statusBody), http.StatusOK)
		assert.Equal(t, before+1, countComments())

		// A status for a commit merely contained in the branch (its parent)
		// is not the branch's CI state: ignored, no fan-out comment.
		parentBody := fmt.Sprintf(`{"sha":%q,"state":"failure",`+
			`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`, parentID.String())
		makeRoomHookRequest(t, "status", parentBody, signRoomHookBody(roomHookTestSecret, parentBody), http.StatusAccepted)
		assert.Equal(t, before+1, countComments())
	})

	t.Run("StatusWithoutOpenRoomReportsNoOpenRoom", func(t *testing.T) {
		// The SHA is a real feat/* branch head, but no open room exists for
		// the branch (never pushed through the hook): the hook must say so
		// instead of claiming it commented.
		repo1 := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})

		gitRepo, err := gitrepo.OpenRepository(t.Context(), repo1)
		require.NoError(t, err)
		headSHA, err := gitRepo.GetBranchCommitID("branch2")
		require.NoError(t, err)
		headCommit, err := gitRepo.GetCommit(headSHA)
		require.NoError(t, err)
		gitRepo.Close()
		parentID, err := headCommit.ParentID(0)
		require.NoError(t, err)

		const noRoomBranch = "feat/room-inttest-noroom"
		createRawGitBranch(t, repo1, noRoomBranch, parentID.String())
		defer deleteRawGitBranch(t, repo1, noRoomBranch)

		body := fmt.Sprintf(`{"sha":%q,"state":"success",`+
			`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`, parentID.String())
		resp := makeRoomHookRequest(t, "status", body, signRoomHookBody(roomHookTestSecret, body), http.StatusAccepted)
		var payload map[string]any
		DecodeJSON(t, resp, &payload)
		assert.Equal(t, "ignored", payload["status"])
		assert.Equal(t, "no_open_room", payload["reason"])
		assert.Equal(t, 0, unittest.GetCount(t, &issues_model.Issue{RepoID: 1, Title: "Room: " + noRoomBranch}))
	})

	t.Run("RePushRefreshesMarkerHead", func(t *testing.T) {
		// The marker-head fallback must track the branch's current tip: a
		// re-push rewrites the head recorded in the room marker, so a status
		// for the new head resolves and one for the stale head does not.
		const (
			reBranch = "feat/room-inttest-repush"
			reTitle  = "Room: " + reBranch
			oldHead  = "1111111111111111111111111111111111111111"
			newHead  = "2222222222222222222222222222222222222222"
		)

		body := roomPushPayload("refs/heads/"+reBranch, oldHead)
		makeRoomHookRequest(t, "push", body, signRoomHookBody(roomHookTestSecret, body), http.StatusOK)
		issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: reTitle})
		require.Contains(t, issue.Content, oldHead)

		body = roomPushPayload("refs/heads/"+reBranch, newHead)
		makeRoomHookRequest(t, "push", body, signRoomHookBody(roomHookTestSecret, body), http.StatusOK)
		issue = unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: reTitle})
		assert.Contains(t, issue.Content, newHead)
		assert.NotContains(t, issue.Content, oldHead)
		countComments := func() int {
			return unittest.GetCount(t, &issues_model.Comment{IssueID: issue.ID, Type: issues_model.CommentTypeComment})
		}
		before := countComments()

		// Both SHAs are fake, so git resolution cannot find them: only the
		// marker-head fallback is in play. The new head resolves...
		statusBody := fmt.Sprintf(`{"sha":%q,"state":"success",`+
			`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`, newHead)
		makeRoomHookRequest(t, "status", statusBody, signRoomHookBody(roomHookTestSecret, statusBody), http.StatusOK)
		assert.Equal(t, before+1, countComments())

		// ...and the stale creation-time head no longer does.
		staleBody := fmt.Sprintf(`{"sha":%q,"state":"failure",`+
			`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`, oldHead)
		makeRoomHookRequest(t, "status", staleBody, signRoomHookBody(roomHookTestSecret, staleBody), http.StatusAccepted)
		assert.Equal(t, before+1, countComments())
	})

	t.Run("TagDeleteIgnored", func(t *testing.T) {
		// A tag deletion never closes a room, even when the tag is named
		// like a feat branch: ref_type distinguishes the two (production
		// payloads set it from the git ref, services/webhook/notifier.go).
		const tagBranch = "feat/room-inttest-tagdel"
		tagTitle := "Room: " + tagBranch

		body := roomPushPayload("refs/heads/"+tagBranch, head)
		makeRoomHookRequest(t, "push", body, signRoomHookBody(roomHookTestSecret, body), http.StatusOK)
		unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: tagTitle})

		del := fmt.Sprintf(`{"ref":%q,"ref_type":"tag",`+
			`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`, tagBranch)
		makeRoomHookRequest(t, "delete", del, signRoomHookBody(roomHookTestSecret, del), http.StatusAccepted)

		issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: tagTitle})
		assert.False(t, issue.IsClosed)
	})

	t.Run("NonFeatBranchDeleteIgnored", func(t *testing.T) {
		del := `{"ref":"main","ref_type":"branch",` +
			`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`
		makeRoomHookRequest(t, "delete", del, signRoomHookBody(roomHookTestSecret, del), http.StatusAccepted)
		assert.Equal(t, 0, unittest.GetCount(t, &issues_model.Issue{RepoID: 1, Title: "Room: main"}))
	})
}
