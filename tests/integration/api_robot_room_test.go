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

// roomRepoSecret derives one repository's hook secret from the instance-wide
// ROOM_HOOK_SECRET. It is spelled out here rather than imported so the test
// pins the operator-facing formula documented in docs/ROBOT_SECURITY.md: if
// the handler's derivation changes, every configured webhook breaks and this
// test says so.
func roomRepoSecret(master, owner, repo string) string {
	mac := hmac.New(sha256.New, []byte(master))
	fmt.Fprintf(mac, "gitea-robot/room:v1:%s/%s", strings.ToLower(owner), strings.ToLower(repo))
	return hex.EncodeToString(mac.Sum(nil))
}

// signRoomDelivery signs a delivery for user2/repo1, the repository every
// payload in this test names.
func signRoomDelivery(body string) string {
	return signRoomHookBody(roomRepoSecret(roomHookTestSecret, "user2", "repo1"), body)
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

// roomMergePayload builds a pull_request payload for repo1 (id 1, the base).
// headRepoID is the repository the head branch lives in: 1 for a branch of
// repo1 itself, anything else for a fork's branch.
func roomMergePayload(headBranch string, merged bool, headRepoID int64) string {
	return fmt.Sprintf(`{"action":"closed","pull_request":{"merged":%t,`+
		`"head":{"ref":%q,"repo_id":%d},"base":{"ref":"master","repo_id":1}},`+
		`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`,
		merged, headBranch, headRepoID)
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
		makeRoomHookRequest(t, "push", body, signRoomDelivery(body), http.StatusNotFound)
	})

	t.Run("UnsignedRejected", func(t *testing.T) {
		body := roomPushPayload(ref, head)
		// No signature header at all
		makeRoomHookRequest(t, "push", body, "", http.StatusUnauthorized)
		// Wrong signature
		makeRoomHookRequest(t, "push", body, strings.Repeat("0", 64), http.StatusUnauthorized)
		assert.Equal(t, 0, unittest.GetCount(t, &issues_model.Issue{RepoID: 1, Title: title}))
	})

	t.Run("SecretIsScopedToOneRepository", func(t *testing.T) {
		// The instance-wide master secret never verifies a delivery on its
		// own, and a repository's own secret verifies only that repository's
		// deliveries. This is what keeps a repo admin who was handed their
		// own hook secret from writing to anyone else's repository.
		body := roomPushPayload(ref, head)

		makeRoomHookRequest(t, "push", body, signRoomHookBody(roomHookTestSecret, body), http.StatusUnauthorized)
		makeRoomHookRequest(t, "push", body,
			signRoomHookBody(roomRepoSecret(roomHookTestSecret, "user2", "repo2"), body), http.StatusUnauthorized)

		// The same holds in the other direction: repo1's secret does not sign
		// a delivery that names repo2.
		otherRepo := fmt.Sprintf(`{"ref":%q,"after":%q,`+
			`"repository":{"name":"repo2","owner":{"login":"user2"}},"sender":{"login":"user2"}}`, ref, head)
		makeRoomHookRequest(t, "push", otherRepo, signRoomDelivery(otherRepo), http.StatusUnauthorized)

		assert.Equal(t, 0, unittest.GetCount(t, &issues_model.Issue{RepoID: 1, Title: title}))
		assert.Equal(t, 0, unittest.GetCount(t, &issues_model.Issue{RepoID: 2, Title: title}))
	})

	t.Run("SenderWithoutWriteAccessRejected", func(t *testing.T) {
		// A correctly signed delivery still has to name an actor who may write
		// issues in the target repository: the sender is a claim in the body,
		// not a credential. user4 is neither owner nor collaborator of repo1.
		const foreignBranch = "feat/room-inttest-foreign"
		foreignTitle := "Room: " + foreignBranch
		body := fmt.Sprintf(`{"ref":%q,"after":%q,`+
			`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user4"}}`,
			"refs/heads/"+foreignBranch, head)
		makeRoomHookRequest(t, "push", body, signRoomDelivery(body), http.StatusForbidden)
		assert.Equal(t, 0, unittest.GetCount(t, &issues_model.Issue{RepoID: 1, Title: foreignTitle}))
	})

	t.Run("ArchivedRepositoryRefused", func(t *testing.T) {
		// An archived repository is immutable to every other API write path
		// (423 Locked), and IsArchived is not part of a repository permission,
		// so nothing else in the hook would have caught it. repo51 is the
		// archived fixture; user30 owns it.
		const archivedBranch = "feat/room-inttest-archived"
		body := fmt.Sprintf(`{"ref":%q,"after":%q,`+
			`"repository":{"name":"repo51","owner":{"login":"user30"}},"sender":{"login":"user30"}}`,
			"refs/heads/"+archivedBranch, head)
		sig := signRoomHookBody(roomRepoSecret(roomHookTestSecret, "user30", "repo51"), body)
		makeRoomHookRequest(t, "push", body, sig, http.StatusLocked)
		assert.Equal(t, 0, unittest.GetCount(t,
			&issues_model.Issue{RepoID: 51, Title: "Room: " + archivedBranch}))
	})

	t.Run("OversizedBodyRefusedBeforeVerification", func(t *testing.T) {
		// The signature is computed over the body, so the body is read before
		// anything about the caller is known - which is exactly why the read
		// is capped. 4 MiB + 1 of padding inside an otherwise valid payload.
		body := fmt.Sprintf(`{"ref":%q,"after":%q,"padding":%q,`+
			`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`,
			ref, head, strings.Repeat("a", 4<<20))
		require.Greater(t, len(body), 4<<20)
		// Correctly signed, and still refused: the cap is not an authorization
		// decision, it is a bound on what one anonymous request may cost.
		makeRoomHookRequest(t, "push", body, signRoomDelivery(body), http.StatusRequestEntityTooLarge)
	})

	t.Run("PayloadRepositoryClaimIsValidated", func(t *testing.T) {
		// The claim is interpolated into the [ROBOT_AUDIT] line, so an
		// embedded newline would let an anonymous caller append a forged
		// record. The same validator the token-authenticated robot routes run
		// rejects the delivery before it is audited at all.
		body := `{"ref":"refs/heads/feat/x","after":"` + head + `",` +
			`"repository":{"name":"repo1","owner":{"login":"user2\n[ROBOT_AUDIT] status=SUCCESS"}}}`
		makeRoomHookRequest(t, "push", body, signRoomDelivery(body), http.StatusBadRequest)

		// A path-traversal owner is refused by the same check.
		traversal := `{"ref":"refs/heads/feat/x","after":"` + head + `",` +
			`"repository":{"name":"repo1","owner":{"login":"../../etc"}}}`
		makeRoomHookRequest(t, "push", traversal, signRoomDelivery(traversal), http.StatusBadRequest)
	})

	t.Run("UnknownEventIgnored", func(t *testing.T) {
		// A signed delivery of an event this automation does not handle. The
		// payload still has to name the repository whose secret signed it -
		// that claim is what selects the key, and it is validated before the
		// signature is checked.
		body := `{"repository":{"name":"repo1","owner":{"login":"user2"}}}`
		makeRoomHookRequest(t, "issues", body, signRoomDelivery(body), http.StatusAccepted)
	})

	t.Run("PushOpensRoomIdempotently", func(t *testing.T) {
		body := roomPushPayload(ref, head)

		// First push creates the room
		makeRoomHookRequest(t, "push", body, signRoomDelivery(body), http.StatusOK)
		issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: title})
		assert.Contains(t, issue.Content, "gitea-robot/room")
		assert.Contains(t, issue.Content, branch)
		assert.False(t, issue.IsClosed)

		// Re-pushing never creates a duplicate
		makeRoomHookRequest(t, "push", body, signRoomDelivery(body), http.StatusOK)
		assert.Equal(t, 1, unittest.GetCount(t, &issues_model.Issue{RepoID: 1, Title: title}))
	})

	t.Run("NonFeatPushIgnored", func(t *testing.T) {
		body := roomPushPayload("refs/heads/main", head)
		makeRoomHookRequest(t, "push", body, signRoomDelivery(body), http.StatusAccepted)
		assert.Equal(t, 0, unittest.GetCount(t, &issues_model.Issue{RepoID: 1, Title: "Room: main"}))
	})

	t.Run("StatusCommentsOnRoom", func(t *testing.T) {
		issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: title})
		before := unittest.GetCount(t, &issues_model.Comment{IssueID: issue.ID, Type: issues_model.CommentTypeComment})

		// The SHA matches the head recorded in the room marker, so the status
		// resolves to this room even without a feat branch in the git repo.
		body := fmt.Sprintf(`{"sha":%q,"state":"success","context":"ci/test","description":"all green",`+
			`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`, head)
		makeRoomHookRequest(t, "status", body, signRoomDelivery(body), http.StatusOK)

		after := unittest.GetCount(t, &issues_model.Comment{IssueID: issue.ID, Type: issues_model.CommentTypeComment})
		assert.Equal(t, before+1, after)

		// The same delivery again - an operator hitting "Redeliver" after a
		// partial failure, or a replay of a captured signed delivery - writes
		// no second copy: the comment is byte-identical to the room's newest.
		resp := makeRoomHookRequest(t, "status", body, signRoomDelivery(body), http.StatusOK)
		var payload map[string]any
		DecodeJSON(t, resp, &payload)
		assert.Empty(t, payload["branches"])
		assert.Equal(t, []any{branch}, payload["duplicate"])
		assert.Equal(t, after,
			unittest.GetCount(t, &issues_model.Comment{IssueID: issue.ID, Type: issues_model.CommentTypeComment}))

		// A different status on the same SHA is a new event, not a repeat.
		other := fmt.Sprintf(`{"sha":%q,"state":"failure","context":"ci/test","description":"went red",`+
			`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`, head)
		makeRoomHookRequest(t, "status", other, signRoomDelivery(other), http.StatusOK)
		assert.Equal(t, after+1,
			unittest.GetCount(t, &issues_model.Comment{IssueID: issue.ID, Type: issues_model.CommentTypeComment}))
	})

	t.Run("StatusForUnknownSHAIgnored", func(t *testing.T) {
		issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: title})
		before := unittest.GetCount(t, &issues_model.Comment{IssueID: issue.ID, Type: issues_model.CommentTypeComment})

		body := fmt.Sprintf(`{"sha":%q,"state":"failure",`+
			`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`,
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		makeRoomHookRequest(t, "status", body, signRoomDelivery(body), http.StatusAccepted)

		after := unittest.GetCount(t, &issues_model.Comment{IssueID: issue.ID, Type: issues_model.CommentTypeComment})
		assert.Equal(t, before, after)
	})

	t.Run("DeleteClosesRoom", func(t *testing.T) {
		body := fmt.Sprintf(`{"ref":%q,"ref_type":"branch",`+
			`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`, branch)
		makeRoomHookRequest(t, "delete", body, signRoomDelivery(body), http.StatusOK)

		issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: title})
		assert.True(t, issue.IsClosed)

		// Closing an already-closed room is a no-op, not an error
		makeRoomHookRequest(t, "delete", body, signRoomDelivery(body), http.StatusOK)
	})

	t.Run("PushAfterCloseReopensTheSameRoom", func(t *testing.T) {
		// DeleteClosesRoom just closed this branch's room. Pushing the branch
		// again revives that issue instead of opening a second one with an
		// identical title - "exactly one room issue per branch" holds across
		// the whole life of a branch name, not just until its first close.
		issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: title})
		require.True(t, issue.IsClosed)

		body := roomPushPayload(ref, head)
		resp := makeRoomHookRequest(t, "push", body, signRoomDelivery(body), http.StatusOK)
		var payload map[string]any
		DecodeJSON(t, resp, &payload)
		assert.Equal(t, false, payload["created"])
		assert.Equal(t, true, payload["reopened"])

		assert.Equal(t, 1, unittest.GetCount(t, &issues_model.Issue{RepoID: 1, Title: title}))
		reopened := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: title})
		assert.Equal(t, issue.ID, reopened.ID)
		assert.False(t, reopened.IsClosed)

		// Put the room back the way DeleteClosesRoom left it, so the subtests
		// after this one see the state they were written against.
		del := fmt.Sprintf(`{"ref":%q,"ref_type":"branch",`+
			`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`, branch)
		makeRoomHookRequest(t, "delete", del, signRoomDelivery(del), http.StatusOK)
	})

	t.Run("ZeroAfterPushClosesRoom", func(t *testing.T) {
		// A push carrying an all-zero "after" SHA is a branch deletion and
		// closes the room, like the dedicated delete event.
		const zapBranch = "feat/room-inttest-zap"
		zapRef := "refs/heads/" + zapBranch
		zapTitle := "Room: " + zapBranch

		body := roomPushPayload(zapRef, head)
		makeRoomHookRequest(t, "push", body, signRoomDelivery(body), http.StatusOK)
		issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: zapTitle})
		require.False(t, issue.IsClosed)

		delBody := roomPushPayload(zapRef, strings.Repeat("0", 40))
		makeRoomHookRequest(t, "push", delBody, signRoomDelivery(delBody), http.StatusOK)
		issue = unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: zapTitle})
		assert.True(t, issue.IsClosed)
	})

	t.Run("MergedPullRequestClosesRoom", func(t *testing.T) {
		const mergeBranch = "feat/room-inttest-merge"
		mergeTitle := "Room: " + mergeBranch

		body := roomPushPayload("refs/heads/"+mergeBranch, head)
		makeRoomHookRequest(t, "push", body, signRoomDelivery(body), http.StatusOK)
		unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: mergeTitle})

		// A PR closed without a merge leaves the branch (and room) alone.
		unmerged := roomMergePayload(mergeBranch, false, 1)
		makeRoomHookRequest(t, "pull_request", unmerged, signRoomDelivery(unmerged), http.StatusAccepted)
		issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: mergeTitle})
		assert.False(t, issue.IsClosed)

		// A merged PR whose head is a branch of *another* repository (a fork)
		// says nothing about this repository's branch of the same name: the
		// payload's repository is the base repo, so closing on the bare head
		// name would close repo1's own, still-active room.
		fromFork := roomMergePayload(mergeBranch, true, 2)
		makeRoomHookRequest(t, "pull_request", fromFork, signRoomDelivery(fromFork), http.StatusAccepted)
		issue = unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: mergeTitle})
		assert.False(t, issue.IsClosed)

		// A merged PR closes the room even though the branch still exists.
		merged := roomMergePayload(mergeBranch, true, 1)
		makeRoomHookRequest(t, "pull_request", merged, signRoomDelivery(merged), http.StatusOK)
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
		makeRoomHookRequest(t, "push", openBody, signRoomDelivery(openBody), http.StatusOK)
		issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: gitTitle})
		countComments := func() int {
			return unittest.GetCount(t, &issues_model.Comment{IssueID: issue.ID, Type: issues_model.CommentTypeComment})
		}
		before := countComments()

		// A status on the branch head comments via git resolution.
		statusBody := fmt.Sprintf(`{"sha":%q,"state":"success","context":"ci/build",`+
			`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`, headSHA)
		makeRoomHookRequest(t, "status", statusBody, signRoomDelivery(statusBody), http.StatusOK)
		assert.Equal(t, before+1, countComments())

		// A status for a commit merely contained in the branch (its parent)
		// is not the branch's CI state: ignored, no fan-out comment.
		parentBody := fmt.Sprintf(`{"sha":%q,"state":"failure",`+
			`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`, parentID.String())
		makeRoomHookRequest(t, "status", parentBody, signRoomDelivery(parentBody), http.StatusAccepted)
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
		resp := makeRoomHookRequest(t, "status", body, signRoomDelivery(body), http.StatusAccepted)
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
		makeRoomHookRequest(t, "push", body, signRoomDelivery(body), http.StatusOK)
		issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: reTitle})
		require.Contains(t, issue.Content, oldHead)

		body = roomPushPayload("refs/heads/"+reBranch, newHead)
		makeRoomHookRequest(t, "push", body, signRoomDelivery(body), http.StatusOK)
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
		makeRoomHookRequest(t, "status", statusBody, signRoomDelivery(statusBody), http.StatusOK)
		assert.Equal(t, before+1, countComments())

		// ...and the stale creation-time head no longer does.
		staleBody := fmt.Sprintf(`{"sha":%q,"state":"failure",`+
			`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`, oldHead)
		makeRoomHookRequest(t, "status", staleBody, signRoomDelivery(staleBody), http.StatusAccepted)
		assert.Equal(t, before+1, countComments())
	})

	t.Run("TagDeleteIgnored", func(t *testing.T) {
		// A tag deletion never closes a room, even when the tag is named
		// like a feat branch: ref_type distinguishes the two (production
		// payloads set it from the git ref, services/webhook/notifier.go).
		const tagBranch = "feat/room-inttest-tagdel"
		tagTitle := "Room: " + tagBranch

		body := roomPushPayload("refs/heads/"+tagBranch, head)
		makeRoomHookRequest(t, "push", body, signRoomDelivery(body), http.StatusOK)
		unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: tagTitle})

		del := fmt.Sprintf(`{"ref":%q,"ref_type":"tag",`+
			`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`, tagBranch)
		makeRoomHookRequest(t, "delete", del, signRoomDelivery(del), http.StatusAccepted)

		issue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{RepoID: 1, Title: tagTitle})
		assert.False(t, issue.IsClosed)
	})

	t.Run("NonFeatBranchDeleteIgnored", func(t *testing.T) {
		del := `{"ref":"main","ref_type":"branch",` +
			`"repository":{"name":"repo1","owner":{"login":"user2"}},"sender":{"login":"user2"}}`
		makeRoomHookRequest(t, "delete", del, signRoomDelivery(del), http.StatusAccepted)
		assert.Equal(t, 0, unittest.GetCount(t, &issues_model.Issue{RepoID: 1, Title: "Room: main"}))
	})
}
