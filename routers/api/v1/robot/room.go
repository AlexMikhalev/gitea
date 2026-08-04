// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package robot

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"code.gitea.io/gitea/models/db"
	issues_model "code.gitea.io/gitea/models/issues"
	access_model "code.gitea.io/gitea/models/perm/access"
	repo_model "code.gitea.io/gitea/models/repo"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/gitrepo"
	"code.gitea.io/gitea/modules/json"
	"code.gitea.io/gitea/modules/log"
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

// roomHookEndpoint is the audit-log identity of this route.
const roomHookEndpoint = "/api/v1/robot/room/hook"

// roomLookupLimit bounds every room lookup. A repository has at most one room
// per branch, so a handful of rows is already far more than a correct
// instance can produce; the limit exists so a webhook delivery can never turn
// into a full scan of the repository's open issues.
const roomLookupLimit = 20

// featBranch reports whether branch is a non-empty feat/* branch name. All
// three entry points (push refs, delete refs, merged PR head refs) agree on
// this definition, so a bare "feat/" is a branch to none of them.
func featBranch(branch string) bool {
	return strings.HasPrefix(branch, featBranchPrefix) && len(branch) > len(featBranchPrefix)
}

// roomHookSecretForRepo derives the webhook secret of one repository from the
// instance-wide ROOM_HOOK_SECRET.
//
// This is what keeps the endpoint's blast radius inside a single repository.
// A delivery is verified with the secret of the repository it names, so the
// repo admin who is handed their own repository's secret - which they must be,
// to configure the webhook - still cannot produce a valid signature for any
// other repository: that would take the instance-wide master secret, which
// never leaves app.ini.
//
// Owner and repo are lower-cased because Gitea repository lookup is
// case-insensitive; without it, "Org/Repo" and "org/repo" would resolve to the
// same repository through two different secrets.
func roomHookSecretForRepo(master, owner, repo string) string {
	mac := hmac.New(sha256.New, []byte(master))
	fmt.Fprintf(mac, "gitea-robot/room:v1:%s/%s", strings.ToLower(owner), strings.ToLower(repo))
	return hex.EncodeToString(mac.Sum(nil))
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
	if !ok || !featBranch(branch) {
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

// roomRepoClaim is the repository a delivery claims to be about. Every
// webhook payload this hook handles carries it under the same "repository"
// key, so it can be read once, before the payload's concrete type is known.
//
// The claim selects the signing key (roomHookSecretForRepo) and, once the
// signature verifies against that key, it is also what the repository is
// resolved from - one value for both, so the repository whose secret signed
// the delivery is necessarily the repository that gets written to.
type roomRepoClaim struct {
	Owner string
	Name  string
}

// roomPayloadRepoClaim extracts the claimed repository from a webhook body.
// It runs before signature verification, on a body that may be unsigned: the
// claim is not trusted, it only decides which secret the signature has to
// match.
func roomPayloadRepoClaim(body []byte) roomRepoClaim {
	var p struct {
		Repository *api.Repository `json:"repository"`
	}
	if err := json.Unmarshal(body, &p); err != nil || p.Repository == nil || p.Repository.Owner == nil {
		return roomRepoClaim{}
	}
	return roomRepoClaim{Owner: p.Repository.Owner.UserName, Name: p.Repository.Name}
}

// roomIssuesByTitle returns at most roomLookupLimit issues of repoID with the
// exact room title for branch, in the requested open/closed state.
//
// The title match is pushed into the query on purpose: it is what makes a
// delivery cost one bounded, indexable lookup instead of loading and
// hydrating every open issue in the repository.
func roomIssuesByTitle(ctx *context.APIContext, repoID int64, branch string, isClosed bool) ([]*issues_model.Issue, error) {
	order := "`issue`.id ASC"
	if isClosed {
		// Among closed rooms the newest is the one a re-push should revive.
		order = "`issue`.id DESC"
	}
	issues := make([]*issues_model.Issue, 0, 4)
	if err := db.GetEngine(ctx).
		Where("`issue`.repo_id = ?", repoID).
		And("`issue`.is_pull = ?", false).
		And("`issue`.is_closed = ?", isClosed).
		And("`issue`.name = ?", robotroom.IssueTitle(branch)).
		OrderBy(order).
		Limit(roomLookupLimit).
		Find(&issues); err != nil {
		return nil, err
	}
	return issues, nil
}

// findRoomIssueInState returns the room issue for branch in the given
// open/closed state, fully loaded and ready to be mutated.
func findRoomIssueInState(ctx *context.APIContext, repoID int64, branch string, isClosed bool) (*issues_model.Issue, bool, error) {
	issues, err := roomIssuesByTitle(ctx, repoID, branch, isClosed)
	if err != nil {
		return nil, false, err
	}
	for _, issue := range issues {
		if !robotroom.IsRoomFor(issue.Title, issue.Content, branch) {
			continue
		}
		// Only the one matched issue is hydrated - the mutation services and
		// their notifications need the attributes, the lookup does not.
		if err := issue.LoadAttributes(ctx); err != nil {
			return nil, false, err
		}
		return issue, true, nil
	}
	return nil, false, nil
}

// findRoomIssue returns the open room issue for branch in repoID. Idempotency
// of room-open rests entirely on this lookup.
func findRoomIssue(ctx *context.APIContext, repoID int64, branch string) (*issues_model.Issue, bool, error) {
	return findRoomIssueInState(ctx, repoID, branch, false)
}

// roomOpenResult reports what an openRoom call did to the branch's room.
type roomOpenResult struct {
	Created  bool
	Reopened bool
}

// refreshRoomHead rewrites the marker head of an existing room, so the
// git-free status fallback (findRoomBranchByHead) tracks the branch's current
// tip instead of only ever matching the creation-time head.
func refreshRoomHead(ctx *context.APIContext, issue *issues_model.Issue, doer *user_model.User, head string) error {
	content, changed := robotroom.WithMarkerHead(issue.Content, head)
	if !changed {
		return nil
	}
	return issue_service.ChangeContent(ctx, issue, doer, content, issue.ContentVersion)
}

// openRoom makes the room for branch current: it refreshes an open room,
// reopens a closed one, and creates a new issue only when the branch has never
// had a room. It reports which of the three happened.
//
// Reopening rather than creating is what keeps the acceptance criterion
// ("push to feat/foo creates exactly one room issue") true across a branch's
// whole life: a branch that is merged or deleted and later pushed again gets
// its room back instead of a second issue with an identical title.
//
// Idempotency is search-then-create with no unique constraint behind it, so
// two deliveries for the same new branch processed concurrently could both
// pass the search and create duplicate rooms. Webhook deliveries are
// typically serialized per hook, which keeps the window theoretical in
// practice; tightening this would need a DB-level constraint this automation
// deliberately avoids (no schema change).
func openRoom(ctx *context.APIContext, repository *repo_model.Repository, doer *user_model.User, branch, head string) (roomOpenResult, error) {
	existing, found, err := findRoomIssue(ctx, repository.ID, branch)
	if err != nil {
		return roomOpenResult{}, err
	}
	if found {
		existing.Repo = repository
		return roomOpenResult{}, refreshRoomHead(ctx, existing, doer, head)
	}

	closed, found, err := findRoomIssueInState(ctx, repository.ID, branch, true)
	if err != nil {
		return roomOpenResult{}, err
	}
	if found {
		closed.Repo = repository
		if err := issue_service.ReopenIssue(ctx, closed, doer, ""); err != nil {
			return roomOpenResult{}, err
		}
		return roomOpenResult{Reopened: true}, refreshRoomHead(ctx, closed, doer, head)
	}

	issue := &issues_model.Issue{
		RepoID:   repository.ID,
		Repo:     repository,
		Title:    robotroom.IssueTitle(branch),
		PosterID: doer.ID,
		Poster:   doer,
		Content:  robotroom.IssueBody(branch, head),
	}
	if err := issue_service.NewIssue(ctx, repository, issue, nil, nil, nil, 0); err != nil {
		return roomOpenResult{}, err
	}
	return roomOpenResult{Created: true}, nil
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

// errRoomActorUnknown means the delivery named no user who could act on the
// repository, and the repository's own owner cannot act either (an
// organization cannot author issues).
var errRoomActorUnknown = errors.New("delivery names no eligible room actor for the target repository")

// errRoomActorDenied means the delivery named a real user who has no
// issue-write access to the repository the delivery is for.
var errRoomActorDenied = errors.New("room actor has no issue-write access to the target repository")

// resolveRoomRepo maps the claimed repository of a verified delivery to the
// local repo.
func resolveRoomRepo(ctx *context.APIContext, claim roomRepoClaim) (*repo_model.Repository, error) {
	if claim.Owner == "" || claim.Name == "" {
		return nil, errors.New("payload carries no repository owner")
	}
	return repo_model.GetRepositoryByOwnerAndName(ctx, claim.Owner, claim.Name)
}

// resolveRoomDoer picks the user a room mutation is attributed to and checks
// that they may actually perform it.
//
// The webhook sender is a *claim*, not a credential: the delivery is
// authenticated as coming from the repository (its derived hook secret signed
// it), never as coming from a particular user. So the named user is only an
// attribution preference, and it is authorized against the target repository
// like any other actor before a single write happens. A sender who is not a
// local user falls back to the repository owner - and there is deliberately no
// site-admin fallback for organization-owned repositories: attributing
// automated content to whichever admin happens to have the lowest id both
// misrepresents who acted and hands the hook far more power than it needs.
func resolveRoomDoer(ctx *context.APIContext, repository *repo_model.Repository, sender *api.User) (*user_model.User, error) {
	var doer *user_model.User
	if sender != nil && sender.UserName != "" {
		u, err := user_model.GetUserByName(ctx, sender.UserName)
		switch {
		case err == nil:
			doer = u
		case !user_model.IsErrUserNotExist(err):
			return nil, err
		}
	}
	if doer == nil {
		if err := repository.LoadOwner(ctx); err != nil {
			return nil, err
		}
		if repository.Owner.IsOrganization() {
			return nil, errRoomActorUnknown
		}
		doer = repository.Owner
	}
	// An organization is not an author, and neither is a user who cannot log
	// in; both would otherwise pass the permission check on their own repos.
	if doer.IsOrganization() || doer.ProhibitLogin || !doer.IsActive {
		return nil, errRoomActorUnknown
	}

	perm, err := access_model.GetUserRepoPermission(ctx, repository, doer)
	if err != nil {
		return nil, err
	}
	if !perm.CanWriteIssuesOrPulls(false) {
		return nil, errRoomActorDenied
	}
	return doer, nil
}

// roomAudit records one outcome of the room hook. Failures are logged as well
// as successes: a caller probing for repository names, or one whose actor is
// refused, is exactly what an operator needs to see.
func roomAudit(ctx *context.APIContext, doer *user_model.User, claim roomRepoClaim, success bool, reason string) {
	var (
		doerID   int64
		doerName = "webhook"
	)
	if doer != nil {
		doerID, doerName = doer.ID, doer.Name
	}
	robot.LogRobotAccessQuick(doerID, doerName, claim.Owner, claim.Name, roomHookEndpoint, ctx.RemoteAddr(), success, reason)
}

// roomTarget resolves the repository and the acting user of a verified
// delivery, writing the API error response and the audit record itself when
// either cannot be established. A false second return means the response is
// already sent.
func roomTarget(ctx *context.APIContext, claim roomRepoClaim, sender *api.User) (*repo_model.Repository, *user_model.User, bool) {
	repository, err := resolveRoomRepo(ctx, claim)
	if err != nil {
		roomAudit(ctx, nil, claim, false, "repo_not_found")
		ctx.APIErrorNotFound()
		return nil, nil, false
	}
	doer, err := resolveRoomDoer(ctx, repository, sender)
	if err != nil {
		if errors.Is(err, errRoomActorUnknown) || errors.Is(err, errRoomActorDenied) {
			roomAudit(ctx, nil, claim, false, "actor_denied")
			ctx.APIError(http.StatusForbidden, err)
			return nil, nil, false
		}
		roomAudit(ctx, nil, claim, false, "doer_error")
		ctx.APIError(http.StatusInternalServerError, err)
		return nil, nil, false
	}
	return repository, doer, true
}

// roomMutationError reports a failed room mutation, auditing it before the
// response goes out.
func roomMutationError(ctx *context.APIContext, doer *user_model.User, claim roomRepoClaim, err error) {
	roomAudit(ctx, doer, claim, false, "room_error")
	ctx.APIError(http.StatusInternalServerError, err)
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
// deliveries signed with the repository's secret, derived from the
// instance-wide ROOM_HOOK_SECRET in app.ini (see roomHookSecretForRepo); with
// no secret configured the route is disabled and answers 404, and an unsigned
// or badly-signed delivery is always rejected with 401.
func RoomHook(ctx *context.APIContext) {
	master := setting.IssueGraphSettings.RoomHookSecret
	if master == "" {
		// Route disabled: indistinguishable from "no such route".
		ctx.APIErrorNotFound()
		return
	}

	body, err := io.ReadAll(ctx.Req.Body)
	if err != nil {
		ctx.APIError(http.StatusBadRequest, "cannot read payload")
		return
	}

	// The claimed repository selects the key the signature must match. It is
	// read from an as-yet-unverified body, which is safe precisely because it
	// only narrows what the delivery can be accepted as: naming another
	// repository means having to sign with that repository's secret.
	claim := roomPayloadRepoClaim(body)
	if !verifyRoomHookSignature(roomHookSecretForRepo(master, claim.Owner, claim.Name), body,
		ctx.Req.Header.Get("X-Gitea-Signature"),
		ctx.Req.Header.Get("X-Hub-Signature-256")) {
		roomAudit(ctx, nil, claim, false, "bad_signature")
		ctx.APIError(http.StatusUnauthorized, "invalid signature")
		return
	}

	switch webhook_module.HookEventType(ctx.Req.Header.Get("X-Gitea-Event")) {
	case webhook_module.HookEventPush:
		handleRoomPush(ctx, body, claim)
	case webhook_module.HookEventDelete:
		handleRoomDelete(ctx, body, claim)
	case webhook_module.HookEventStatus:
		handleRoomStatus(ctx, body, claim)
	case webhook_module.HookEventPullRequest:
		handleRoomPullRequest(ctx, body, claim)
	default:
		// Signed, but not an event this automation cares about.
		ctx.JSON(http.StatusAccepted, map[string]string{"status": "ignored"})
	}
}

// handleRoomPush opens the room for a pushed feat/* branch, and closes it when
// the push deleted the branch (zero "after" SHA).
func handleRoomPush(ctx *context.APIContext, body []byte, claim roomRepoClaim) {
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
	sender := p.Sender
	if sender == nil {
		sender = p.Pusher
	}
	repository, doer, ok := roomTarget(ctx, claim, sender)
	if !ok {
		return
	}

	if isZeroSHA(p.After) {
		closed, err := closeRoom(ctx, repository, doer, branch)
		if err != nil {
			roomMutationError(ctx, doer, claim, err)
			return
		}
		roomAudit(ctx, doer, claim, true, "")
		ctx.JSON(http.StatusOK, map[string]any{"status": "closed", "branch": branch, "changed": closed})
		return
	}

	result, err := openRoom(ctx, repository, doer, branch, p.After)
	if err != nil {
		roomMutationError(ctx, doer, claim, err)
		return
	}
	roomAudit(ctx, doer, claim, true, "")
	ctx.JSON(http.StatusOK, map[string]any{
		"status": "open", "branch": branch, "created": result.Created, "reopened": result.Reopened,
	})
}

// deleteTargetsRoom reports whether a delete event closes a room. Only feat/*
// branch deletions do: the payload's ref_type distinguishes branch from tag
// deletions (services/webhook/notifier.go sets it from the git ref), and a
// tag named like a feat branch never carried a room. The bare "feat/" ref is
// not a branch here either, matching featBranchRef and mergedFeatBranch.
func deleteTargetsRoom(refType, ref string) bool {
	return refType == "branch" && featBranch(ref)
}

// handleRoomDelete closes the room when a feat/* branch is deleted. Gitea
// fires a dedicated delete event for branch deletion - the push payload has no
// deleted flag - so room-close lives here, with the zero-after push path in
// handleRoomPush as a fallback.
func handleRoomDelete(ctx *context.APIContext, body []byte, claim roomRepoClaim) {
	var p api.DeletePayload
	if err := json.Unmarshal(body, &p); err != nil {
		ctx.APIError(http.StatusBadRequest, "invalid delete payload")
		return
	}
	if !deleteTargetsRoom(p.RefType, p.Ref) {
		ctx.JSON(http.StatusAccepted, map[string]string{"status": "ignored"})
		return
	}
	repository, doer, ok := roomTarget(ctx, claim, p.Sender)
	if !ok {
		return
	}
	closed, err := closeRoom(ctx, repository, doer, p.Ref)
	if err != nil {
		roomMutationError(ctx, doer, claim, err)
		return
	}
	roomAudit(ctx, doer, claim, true, "")
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
	if !featBranch(branch) {
		return "", false
	}
	return branch, true
}

// handleRoomPullRequest closes the room when a PR from a feat/* branch is
// merged: the "merge" half of "branch merge/delete closes the room issue".
// A merged branch is often kept around without an explicit deletion, which
// would otherwise leak its open room forever.
func handleRoomPullRequest(ctx *context.APIContext, body []byte, claim roomRepoClaim) {
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
	repository, doer, ok := roomTarget(ctx, claim, p.Sender)
	if !ok {
		return
	}
	closed, err := closeRoom(ctx, repository, doer, branch)
	if err != nil {
		roomMutationError(ctx, doer, claim, err)
		return
	}
	roomAudit(ctx, doer, claim, true, "")
	ctx.JSON(http.StatusOK, map[string]any{"status": "closed", "branch": branch, "changed": closed})
}

// handleRoomStatus appends a CI status comment to the room of the branch whose
// head the status is for. The payload has no branch ref, so branches are
// recovered from the commit graph; as a fallback a room whose marker head
// matches the SHA is used, which keeps working when the git repo is
// unavailable to this process.
func handleRoomStatus(ctx *context.APIContext, body []byte, claim roomRepoClaim) {
	var p api.CommitStatusPayload
	if err := json.Unmarshal(body, &p); err != nil {
		ctx.APIError(http.StatusBadRequest, "invalid status payload")
		return
	}
	if p.SHA == "" {
		ctx.APIError(http.StatusBadRequest, "status payload carries no sha")
		return
	}
	repository, doer, ok := roomTarget(ctx, claim, p.Sender)
	if !ok {
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
		posted, err := statusComment(ctx, repository, doer, branch, &p)
		if err != nil {
			roomMutationError(ctx, doer, claim, err)
			return
		}
		if posted {
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
	roomAudit(ctx, doer, claim, true, "")
	ctx.JSON(http.StatusOK, map[string]any{"status": "commented", "branches": commented})
}

// isHexSHA reports whether s is a non-empty hex string. It gates the marker
// lookup below: a SHA that reached the LIKE pattern with wildcards in it would
// turn a bounded lookup into a scan.
func isHexSHA(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// findRoomBranchByHead returns the branch of an open room whose marker head is
// sha - the git-free fallback for status-to-room resolution. Both filters (the
// room title prefix and the head recorded in the marker) run in the database,
// so the fallback costs a bounded lookup rather than a pass over every open
// issue's body.
func findRoomBranchByHead(ctx *context.APIContext, repoID int64, sha string) (string, bool) {
	if !isHexSHA(sha) {
		return "", false
	}
	issues := make([]*issues_model.Issue, 0, 4)
	if err := db.GetEngine(ctx).
		Where("`issue`.repo_id = ?", repoID).
		And("`issue`.is_pull = ?", false).
		And("`issue`.is_closed = ?", false).
		And("`issue`.name LIKE ?", robotroom.TitlePrefix+"%").
		And("`issue`.content LIKE ?", "%"+robotroom.MarkerHeadFragment(sha)+"%").
		Limit(roomLookupLimit).
		Find(&issues); err != nil {
		log.Error("room hook: cannot look up room by marker head in repo %d: %v", repoID, err)
		return "", false
	}
	for _, issue := range issues {
		// The SQL filters narrow; the marker parse decides. A renamed room is
		// detached from the automation, same rule as robotroom.IsRoomFor.
		if m, ok := robotroom.ParseMarker(issue.Content); ok && m.Head == sha && issue.Title == robotroom.IssueTitle(m.Branch) {
			return m.Branch, true
		}
	}
	return "", false
}
