// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package robot

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"

	issues_model "code.gitea.io/gitea/models/issues"
	repo_model "code.gitea.io/gitea/models/repo"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/gitrepo"
	"code.gitea.io/gitea/modules/json"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/optional"
	"code.gitea.io/gitea/modules/robotroom"
	"code.gitea.io/gitea/modules/setting"
	api "code.gitea.io/gitea/modules/structs"
	webhook_module "code.gitea.io/gitea/modules/webhook"
	"code.gitea.io/gitea/services/context"
	issue_service "code.gitea.io/gitea/services/issue"
	"code.gitea.io/gitea/services/robot"
)

// featBranchPrefix scopes the whole feature: only feat/* branches get rooms.
const featBranchPrefix = "feat/"

// roomTitlePrefix is the deterministic title prefix of every room issue; it
// doubles as a cheap pre-filter when scanning open issues for a room.
const roomTitlePrefix = "Room: "

// roomIssueTitle renders the deterministic title of a branch's room issue.
func roomIssueTitle(branch string) string {
	return roomTitlePrefix + branch
}

// verifyRoomHookSignature checks the X-Gitea-Signature header against the
// body. Gitea webhook deliveries carry the raw hex HMAC-SHA256 of the payload
// in X-Gitea-Signature (services/webhook/deliver.go) and the prefixed form in
// X-Hub-Signature-256; both are accepted, the empty signature never is.
func verifyRoomHookSignature(secret string, body []byte, giteaSig, hubSig string) bool {
	if secret == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := mac.Sum(nil)

	if giteaSig != "" {
		if got, err := hex.DecodeString(giteaSig); err == nil && hmac.Equal(got, expected) {
			return true
		}
	}
	if got, ok := strings.CutPrefix(hubSig, "sha256="); ok {
		if raw, err := hex.DecodeString(got); err == nil && hmac.Equal(raw, expected) {
			return true
		}
	}
	return false
}

// featBranchRef strips refs/heads/ and reports whether the ref is a feat/*
// branch. An empty branch name (e.g. "refs/heads/feat/") is rejected.
func featBranchRef(ref string) (string, bool) {
	branch, ok := strings.CutPrefix(ref, git.BranchPrefix)
	if !ok || !strings.HasPrefix(branch, featBranchPrefix) || len(branch) == len(featBranchPrefix) {
		return "", false
	}
	return branch, true
}

// isZeroSHA reports whether s is an all-zero git object id, which is what a
// push payload carries in "after" when the push deleted the branch.
func isZeroSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if c != '0' {
			return false
		}
	}
	return true
}

// roomPayloadRepoHint extracts owner/repo from a webhook body for audit
// logging only. The body may be unsigned or badly signed, so the values are
// untrusted hints, never inputs to any decision.
func roomPayloadRepoHint(body []byte) (owner, repo string) {
	var p struct {
		Repository *api.Repository `json:"repository"`
	}
	if err := json.Unmarshal(body, &p); err != nil || p.Repository == nil || p.Repository.Owner == nil {
		return "", ""
	}
	return p.Repository.Owner.UserName, p.Repository.Name
}

// findRoomIssue returns the open room issue for branch in repoID. Idempotency
// of room-open rests entirely on this lookup.
func findRoomIssue(ctx *context.APIContext, repoID int64, branch string) (*issues_model.Issue, bool, error) {
	issues, err := issues_model.Issues(ctx, &issues_model.IssuesOptions{
		RepoIDs:  []int64{repoID},
		IsClosed: optional.Some(false),
		IsPull:   optional.Some(false),
	})
	if err != nil {
		return nil, false, err
	}
	for _, issue := range issues {
		// Cheap pre-filter on the deterministic title before parsing the
		// body, so a delivery does not scan every open issue's content.
		// Trade-off: a room a human renamed no longer matches - renaming a
		// room detaches it from the automation.
		if issue.Title != roomIssueTitle(branch) {
			continue
		}
		if m, ok := robotroom.ParseMarker(issue.Content); ok && m.Branch == branch {
			return issue, true, nil
		}
	}
	return nil, false, nil
}

// openRoom creates the room issue for branch unless one already exists. It
// reports whether a new issue was created.
//
// Idempotency is search-then-create with no unique constraint behind it, so
// two deliveries for the same new branch processed concurrently could both
// pass the search and create duplicate rooms. Webhook deliveries are
// typically serialized per hook, which keeps the window theoretical in
// practice; tightening this would need a DB-level constraint this automation
// deliberately avoids (no schema change).
func openRoom(ctx *context.APIContext, repository *repo_model.Repository, doer *user_model.User, branch, head string) (bool, error) {
	existing, found, err := findRoomIssue(ctx, repository.ID, branch)
	if err != nil {
		return false, err
	}
	if found {
		// A re-push of the same branch refreshes the marker head, so the
		// git-free status fallback (findRoomBranchByHead) tracks the branch's
		// current tip instead of only ever matching the creation-time head.
		if content, changed := robotroom.WithMarkerHead(existing.Content, head); changed {
			existing.Repo = repository
			if err := issue_service.ChangeContent(ctx, existing, doer, content, existing.ContentVersion); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	issue := &issues_model.Issue{
		RepoID:   repository.ID,
		Repo:     repository,
		Title:    roomIssueTitle(branch),
		PosterID: doer.ID,
		Poster:   doer,
		Content:  robotroom.IssueBody(branch, head),
	}
	if err := issue_service.NewIssue(ctx, repository, issue, nil, nil, nil, 0); err != nil {
		return false, err
	}
	return true, nil
}

// closeRoom closes the room issue for branch, if one is open.
func closeRoom(ctx *context.APIContext, repository *repo_model.Repository, doer *user_model.User, branch string) (bool, error) {
	existing, found, err := findRoomIssue(ctx, repository.ID, branch)
	if err != nil || !found {
		return false, err
	}
	existing.Repo = repository
	if err := issue_service.CloseIssue(ctx, existing, doer, ""); err != nil {
		return false, err
	}
	return true, nil
}

// statusComment appends one CI status comment to the room issue for branch.
// The comment shape comes from modules/robotroom, shared with the gitea-robot
// CLI, so hook-posted and CLI-posted comments are byte-identical.
func statusComment(ctx *context.APIContext, repository *repo_model.Repository, doer *user_model.User, branch string, p *api.CommitStatusPayload) (bool, error) {
	existing, found, err := findRoomIssue(ctx, repository.ID, branch)
	if err != nil || !found {
		return false, err
	}
	body := robotroom.StatusComment(p.State, p.Context, p.SHA, p.TargetURL, p.Description)
	if _, err := issue_service.CreateIssueComment(ctx, doer, repository, existing, body, nil); err != nil {
		return false, err
	}
	return true, nil
}

// resolveRoomRepo maps the webhook payload's repository to the local repo.
func resolveRoomRepo(ctx *context.APIContext, payloadRepo *api.Repository) (*repo_model.Repository, error) {
	if payloadRepo == nil || payloadRepo.Owner == nil {
		return nil, errors.New("payload carries no repository owner")
	}
	return repo_model.GetRepositoryByOwnerAndName(ctx, payloadRepo.Owner.UserName, payloadRepo.Name)
}

// resolveRoomDoer attributes room mutations to the webhook sender, falling
// back to the repository owner when the sender is not a local user. For
// organization-owned repos the owner cannot author issues or comments, so a
// site admin acts instead.
func resolveRoomDoer(ctx *context.APIContext, repository *repo_model.Repository, sender *api.User) (*user_model.User, error) {
	if sender != nil && sender.UserName != "" {
		if doer, err := user_model.GetUserByName(ctx, sender.UserName); err == nil {
			return doer, nil
		}
	}
	if err := repository.LoadOwner(ctx); err != nil {
		return nil, err
	}
	if repository.Owner.IsOrganization() {
		return user_model.GetAdminUser(ctx)
	}
	return repository.Owner, nil
}

// featBranchesAtHead returns the feat/* branches whose head is commit sha.
// The CommitStatusPayload carries no branch ref (modules/structs/hook.go), so
// the branch has to be recovered from the commit graph. Only branch heads
// match: a status for an older commit contained in a branch is not that
// branch's CI state, and containment-based resolution would fan one status
// out to every room whose branch happens to contain the commit (e.g. a
// branch cut off another feat/* branch).
func featBranchesAtHead(ctx *context.APIContext, repository *repo_model.Repository, sha string) []string {
	gitRepo, err := gitrepo.OpenRepository(ctx, repository)
	if err != nil {
		log.Warn("room hook: cannot open repo %s/%s: %v", repository.OwnerName, repository.Name, err)
		return nil
	}
	defer gitRepo.Close()

	names, _, err := gitRepo.GetBranchNames(0, 0)
	if err != nil {
		log.Warn("room hook: cannot list branches of %s/%s: %v", repository.OwnerName, repository.Name, err)
		return nil
	}
	var out []string
	for _, name := range names {
		if !strings.HasPrefix(name, featBranchPrefix) {
			continue
		}
		if head, err := gitRepo.GetBranchCommitID(name); err == nil && strings.EqualFold(head, sha) {
			out = append(out, name)
		}
	}
	return out
}

// RoomHook handles POST /api/v1/robot/room/hook, the inbound webhook endpoint
// of branch-as-room automation (issue #56). It accepts Gitea webhook
// deliveries signed with the ROOM_HOOK_SECRET from app.ini; with no secret
// configured the route is disabled and answers 404, and an unsigned or
// badly-signed delivery is always rejected with 401.
func RoomHook(ctx *context.APIContext) {
	secret := setting.IssueGraphSettings.RoomHookSecret
	if secret == "" {
		// Route disabled: indistinguishable from "no such route".
		ctx.APIErrorNotFound()
		return
	}

	body, err := io.ReadAll(ctx.Req.Body)
	if err != nil {
		ctx.APIError(http.StatusBadRequest, "cannot read payload")
		return
	}

	if !verifyRoomHookSignature(secret, body,
		ctx.Req.Header.Get("X-Gitea-Signature"),
		ctx.Req.Header.Get("X-Hub-Signature-256")) {
		// The body is untrusted, but a best-effort owner/repo hint makes the
		// audit record useful; it feeds nothing but the log line.
		owner, repo := roomPayloadRepoHint(body)
		robot.LogRobotAccessQuick(0, "webhook", owner, repo, "/api/v1/robot/room/hook", ctx.RemoteAddr(), false, "bad_signature")
		ctx.APIError(http.StatusUnauthorized, "invalid signature")
		return
	}

	switch webhook_module.HookEventType(ctx.Req.Header.Get("X-Gitea-Event")) {
	case webhook_module.HookEventPush:
		handleRoomPush(ctx, body)
	case webhook_module.HookEventDelete:
		handleRoomDelete(ctx, body)
	case webhook_module.HookEventStatus:
		handleRoomStatus(ctx, body)
	case webhook_module.HookEventPullRequest:
		handleRoomPullRequest(ctx, body)
	default:
		// Signed, but not an event this automation cares about.
		ctx.JSON(http.StatusAccepted, map[string]string{"status": "ignored"})
	}
}

// handleRoomPush opens the room for a pushed feat/* branch, and closes it when
// the push deleted the branch (zero "after" SHA).
func handleRoomPush(ctx *context.APIContext, body []byte) {
	var p api.PushPayload
	if err := json.Unmarshal(body, &p); err != nil {
		ctx.APIError(http.StatusBadRequest, "invalid push payload")
		return
	}
	branch, ok := featBranchRef(p.Ref)
	if !ok {
		ctx.JSON(http.StatusAccepted, map[string]string{"status": "ignored", "reason": "not a feat/* branch"})
		return
	}
	repository, err := resolveRoomRepo(ctx, p.Repo)
	if err != nil {
		ctx.APIErrorNotFound()
		return
	}
	sender := p.Sender
	if sender == nil {
		sender = p.Pusher
	}
	doer, err := resolveRoomDoer(ctx, repository, sender)
	if err != nil {
		ctx.APIError(http.StatusInternalServerError, err)
		return
	}

	if isZeroSHA(p.After) {
		closed, err := closeRoom(ctx, repository, doer, branch)
		if err != nil {
			ctx.APIError(http.StatusInternalServerError, err)
			return
		}
		robot.LogRobotAccessQuick(doer.ID, doer.Name, repository.OwnerName, repository.Name, "/api/v1/robot/room/hook", ctx.RemoteAddr(), true, "")
		ctx.JSON(http.StatusOK, map[string]any{"status": "closed", "branch": branch, "changed": closed})
		return
	}

	created, err := openRoom(ctx, repository, doer, branch, p.After)
	if err != nil {
		ctx.APIError(http.StatusInternalServerError, err)
		return
	}
	robot.LogRobotAccessQuick(doer.ID, doer.Name, repository.OwnerName, repository.Name, "/api/v1/robot/room/hook", ctx.RemoteAddr(), true, "")
	ctx.JSON(http.StatusOK, map[string]any{"status": "open", "branch": branch, "created": created})
}

// deleteTargetsRoom reports whether a delete event closes a room. Only feat/*
// branch deletions do: the payload's ref_type distinguishes branch from tag
// deletions (services/webhook/notifier.go sets it from the git ref), and a
// tag named like a feat branch never carried a room.
func deleteTargetsRoom(refType, ref string) bool {
	return refType == "branch" && strings.HasPrefix(ref, featBranchPrefix)
}

// handleRoomDelete closes the room when a feat/* branch is deleted. Gitea
// fires a dedicated delete event for branch deletion - the push payload has no
// deleted flag - so room-close lives here, with the zero-after push path in
// handleRoomPush as a fallback.
func handleRoomDelete(ctx *context.APIContext, body []byte) {
	var p api.DeletePayload
	if err := json.Unmarshal(body, &p); err != nil {
		ctx.APIError(http.StatusBadRequest, "invalid delete payload")
		return
	}
	if !deleteTargetsRoom(p.RefType, p.Ref) {
		ctx.JSON(http.StatusAccepted, map[string]string{"status": "ignored"})
		return
	}
	repository, err := resolveRoomRepo(ctx, p.Repo)
	if err != nil {
		ctx.APIErrorNotFound()
		return
	}
	doer, err := resolveRoomDoer(ctx, repository, p.Sender)
	if err != nil {
		ctx.APIError(http.StatusInternalServerError, err)
		return
	}
	closed, err := closeRoom(ctx, repository, doer, p.Ref)
	if err != nil {
		ctx.APIError(http.StatusInternalServerError, err)
		return
	}
	robot.LogRobotAccessQuick(doer.ID, doer.Name, repository.OwnerName, repository.Name, "/api/v1/robot/room/hook", ctx.RemoteAddr(), true, "")
	ctx.JSON(http.StatusOK, map[string]any{"status": "closed", "branch": p.Ref, "changed": closed})
}

// mergedFeatBranch returns the merged feat/* head branch of a pull_request
// payload, if the payload represents a merge that should close a room. A PR
// closed without merging leaves the branch (and its room) alone.
func mergedFeatBranch(p *api.PullRequestPayload) (string, bool) {
	if p.Action != api.HookIssueClosed || p.PullRequest == nil || !p.PullRequest.HasMerged || p.PullRequest.Head == nil {
		return "", false
	}
	branch := p.PullRequest.Head.Ref
	if !strings.HasPrefix(branch, featBranchPrefix) || len(branch) == len(featBranchPrefix) {
		return "", false
	}
	return branch, true
}

// handleRoomPullRequest closes the room when a PR from a feat/* branch is
// merged: the "merge" half of "branch merge/delete closes the room issue".
// A merged branch is often kept around without an explicit deletion, which
// would otherwise leak its open room forever.
func handleRoomPullRequest(ctx *context.APIContext, body []byte) {
	var p api.PullRequestPayload
	if err := json.Unmarshal(body, &p); err != nil {
		ctx.APIError(http.StatusBadRequest, "invalid pull_request payload")
		return
	}
	branch, ok := mergedFeatBranch(&p)
	if !ok {
		ctx.JSON(http.StatusAccepted, map[string]string{"status": "ignored"})
		return
	}
	repository, err := resolveRoomRepo(ctx, p.Repository)
	if err != nil {
		ctx.APIErrorNotFound()
		return
	}
	doer, err := resolveRoomDoer(ctx, repository, p.Sender)
	if err != nil {
		ctx.APIError(http.StatusInternalServerError, err)
		return
	}
	closed, err := closeRoom(ctx, repository, doer, branch)
	if err != nil {
		ctx.APIError(http.StatusInternalServerError, err)
		return
	}
	robot.LogRobotAccessQuick(doer.ID, doer.Name, repository.OwnerName, repository.Name, "/api/v1/robot/room/hook", ctx.RemoteAddr(), true, "")
	ctx.JSON(http.StatusOK, map[string]any{"status": "closed", "branch": branch, "changed": closed})
}

// handleRoomStatus appends a CI status comment to the room of the branch whose
// head the status is for. The payload has no branch ref, so branches are
// recovered from the commit graph; as a fallback a room whose marker head
// matches the SHA is used, which keeps working when the git repo is
// unavailable to this process.
func handleRoomStatus(ctx *context.APIContext, body []byte) {
	var p api.CommitStatusPayload
	if err := json.Unmarshal(body, &p); err != nil {
		ctx.APIError(http.StatusBadRequest, "invalid status payload")
		return
	}
	if p.SHA == "" {
		ctx.APIError(http.StatusBadRequest, "status payload carries no sha")
		return
	}
	repository, err := resolveRoomRepo(ctx, p.Repo)
	if err != nil {
		ctx.APIErrorNotFound()
		return
	}
	doer, err := resolveRoomDoer(ctx, repository, p.Sender)
	if err != nil {
		ctx.APIError(http.StatusInternalServerError, err)
		return
	}

	branches := featBranchesAtHead(ctx, repository, p.SHA)
	if len(branches) == 0 {
		// Fallback: match the head recorded in the room marker.
		if branch, ok := findRoomBranchByHead(ctx, repository.ID, p.SHA); ok {
			branches = []string{branch}
		}
	}
	if len(branches) == 0 {
		ctx.JSON(http.StatusAccepted, map[string]string{"status": "ignored", "reason": "sha is not the head of any feat/* branch"})
		return
	}

	commented := make([]string, 0, len(branches))
	for _, branch := range branches {
		ok, err := statusComment(ctx, repository, doer, branch, &p)
		if err != nil {
			ctx.APIError(http.StatusInternalServerError, err)
			return
		}
		if ok {
			commented = append(commented, branch)
		}
	}
	if len(commented) == 0 {
		// The SHA is a feat/* branch head, but no open room exists for it
		// (the room was closed manually, or the branch was pushed while the
		// hook secret was unset): report that distinctly instead of claiming
		// a comment was posted.
		ctx.JSON(http.StatusAccepted, map[string]string{"status": "ignored", "reason": "no_open_room"})
		return
	}
	robot.LogRobotAccessQuick(doer.ID, doer.Name, repository.OwnerName, repository.Name, "/api/v1/robot/room/hook", ctx.RemoteAddr(), true, "")
	ctx.JSON(http.StatusOK, map[string]any{"status": "commented", "branches": commented})
}

// findRoomBranchByHead returns the branch of an open room whose marker head is
// sha - the git-free fallback for status-to-room resolution.
func findRoomBranchByHead(ctx *context.APIContext, repoID int64, sha string) (string, bool) {
	issues, err := issues_model.Issues(ctx, &issues_model.IssuesOptions{
		RepoIDs:  []int64{repoID},
		IsClosed: optional.Some(false),
		IsPull:   optional.Some(false),
	})
	if err != nil {
		return "", false
	}
	for _, issue := range issues {
		// Same title pre-filter as findRoomIssue: a renamed room is detached.
		if !strings.HasPrefix(issue.Title, roomTitlePrefix) {
			continue
		}
		if m, ok := robotroom.ParseMarker(issue.Content); ok && m.Head == sha {
			return m.Branch, true
		}
	}
	return "", false
}
