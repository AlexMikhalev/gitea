// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	auth_model "code.gitea.io/gitea/models/auth"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/test"
	"code.gitea.io/gitea/modules/web"
	v1 "code.gitea.io/gitea/routers/api/v1"
	"code.gitea.io/gitea/tests"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The containment policy reqHumanAuth() implements is written one route line at a time, and the
// same failure mode surfaced in four consecutive review rounds: an operation reachable at two
// paths, guarded at one of them. PUT /teams/{teamid}/repos/{org}/{reponame} and
// PUT /repos/{owner}/{repo}/teams/{team} are the same call into repo_service.TeamAddRepository;
// only the first was guarded until the round that produced this test. Reading the guard off the
// handler name cannot catch that, because the two routes have different handler names.
//
// So this is a census rather than another patch. It walks the registered v1 route table and
// requires every mutating route to be classified: either it carries reqHumanAuth(), and is listed
// in agentAuthGuardedRoutes, or a review decided it does not need to, and it is listed in
// agentAuthExemptRoutes with the reason. A new route - or a second path to an operation that is
// already guarded elsewhere - belongs to neither set, so it fails here instead of quietly
// widening what an agent signature can reach.
//
// Two things the walk itself deliberately does not do:
//
//   - It does not assert that a guarded route still has the middleware attached. chi bakes
//     per-route middlewares into a closure chain, so the walk cannot see them, and identifying
//     them by code pointer would be reading the implementation rather than the behaviour. That
//     half is behavioural, and it is driven from the same map:
//     TestAPIAgentGuardedRoutesRefuseASignedRequest below signs a real request at every entry of
//     agentAuthGuardedRoutes and requires reqHumanAuth's own 403. The two halves cannot drift,
//     because adding a route to the map is what adds its behavioural test. The hand-written cases
//     in api_agent_auth_test.go stay: they assert the *consequences* a status code cannot - that
//     the refusal wrote nothing, and that the owner's own token still gets through, which is what
//     makes each 403 the guard's doing rather than a permission check's.
//   - It does not enumerate reads. A GET plants nothing, which is the policy stated at
//     reqHumanAuth, and listing ~250 read routes would bury the mutating ones. The one read class
//     that is equivalent to minting - a runner registration token, which is usable by whoever
//     reads it - is asserted separately below.
//
// Adding a route to agentAuthExemptRoutes is a review decision, not a formality: the question to
// answer is the one reqHumanAuth states - does a subsequent request, made with no NIP-98 signature
// at all, get anything out of it? Destructiveness is not the test; a DELETE that wipes a
// repository is exempt because it leaves nothing behind, not because it is harmless.
const (
	exemptRemoves = "removes an object, some content or someone's access; it plants nothing that " +
		"outlives revoking the agent's key"
	exemptRepoWork = "ordinary work inside the repository the agent's scope already covers - content, " +
		"branches, tags, issues, pulls, releases, labels, milestones, wiki, statuses, protections - " +
		"leaving no credential and no standing access for another principal"
	exemptNoGrant = "presentation, preference or link metadata: avatars, badges, topics, blocks, " +
		"follows, stars, watches, notification state, package links. It issues no credential and " +
		"gives no principal access it did not already have"
	exemptRendering = "renders text and returns it; it stores nothing"
	exemptWebhook   = "a webhook, which reqHumanAuth deliberately places outside this policy: it is a " +
		"persistent outbound channel, but neither a credential this instance issued nor standing " +
		"access for a principal. Guarding these would be widening the policy, not filling a gap"
	exemptNewObject = "creates a repository, organization or team that starts out reaching nobody but " +
		"the doer, who already had that access. A team gains reach only when a member or a " +
		"repository is added to it, and both of those writes are guarded"
	exemptRunsExistingCode = "runs code or a job the instance already holds; it adds no new credential " +
		"and an agent's scope covers pushing the same code anyway"
	exemptVisibility = "changes who can see an existing object. That is disclosure rather than a grant " +
		"to a named principal, so it falls outside the policy as stated; a change that decided to " +
		"guard it would be widening the policy"
	exemptRename = "renames an existing account or organization. The logins that reach it are exactly " +
		"the ones that reached it before"
	exemptOutboundChannel = "a push mirror, which pushes outward with a credential the caller supplies " +
		"rather than one this instance issues - the same reasoning that leaves webhooks outside"
	exemptAnswersAnOffer = "answers a transfer some other owner already offered; it creates no grant, " +
		"and the offer itself is made through the guarded POST /repos/{owner}/{repo}/transfer"
	exemptBasicAuthOnly = "already behind reqBasicOrRevProxyAuth(), which a NIP-98 signature cannot " +
		"satisfy - so it is out of reach for the same reason, one layer earlier"
	exemptFederationInbox = "the ActivityPub inbox, which authenticates the *sending instance* by HTTP " +
		"signature rather than acting for the caller"
	exemptPathGroupCatchAll = "an artifact of web.Router.PathGroup, which registers the pattern for every " +
		"HTTP method at the chi level and then dispatches inside RouterPathGroup.ServeHTTP against " +
		"the matchers declared in it. Every matcher under this pattern is a GET or a HEAD, so a " +
		"request arriving here with any other method falls through to the not-found handler and " +
		"never reaches a handler at all. The census sees these because chi does; they are not " +
		"endpoints. That claim is not taken on trust: the subtest below reads the matchers back " +
		"out of the group and fails if one of them accepts a mutating method"
)

// agentAuthGuardedRoutes is every route carrying reqHumanAuth(). The reasoning for each class is
// at reqHumanAuth in routers/api/v1/api.go; the value here names the class, not a fresh argument.
var agentAuthGuardedRoutes = map[string]string{
	// Signing keys and their trail: an agent must not be able to enrol a sibling for itself,
	// revoke the key an operator is containing it with, or read the trail with its own signature.
	"GET /agent/keys":         "agent signing keys",
	"POST /agent/keys":        "agent signing keys",
	"DELETE /agent/keys/{id}": "agent signing keys",
	"GET /agent/audit":        "agent signing keys",

	// Account-level credentials, each of which outlives the key that planted it.
	"GET /user/keys":                        "SSH keys",
	"POST /user/keys":                       "SSH keys",
	"GET /user/keys/{id}":                   "SSH keys",
	"DELETE /user/keys/{id}":                "SSH keys",
	"GET /user/gpg_keys":                    "GPG keys",
	"POST /user/gpg_keys":                   "GPG keys",
	"GET /user/gpg_keys/{id}":               "GPG keys",
	"DELETE /user/gpg_keys/{id}":            "GPG keys",
	"POST /user/gpg_key_verify":             "GPG keys",
	"GET /user/applications/oauth2":         "OAuth2 applications, which mint tokens",
	"POST /user/applications/oauth2":        "OAuth2 applications, which mint tokens",
	"GET /user/applications/oauth2/{id}":    "OAuth2 applications, which mint tokens",
	"PATCH /user/applications/oauth2/{id}":  "OAuth2 applications, which mint tokens",
	"DELETE /user/applications/oauth2/{id}": "OAuth2 applications, which mint tokens",
	"POST /user/emails":                     "an added address is an account-recovery path",

	// Actions artifacts, at all three levels: the same objects, so the same answer.
	"PUT /user/actions/secrets/{secretname}":                               "Actions secrets",
	"DELETE /user/actions/secrets/{secretname}":                            "Actions secrets",
	"POST /user/actions/variables/{variablename}":                          "Actions variables",
	"PUT /user/actions/variables/{variablename}":                           "Actions variables",
	"DELETE /user/actions/variables/{variablename}":                        "Actions variables",
	"GET /user/actions/runners/registration-token":                         "runner registration tokens",
	"POST /user/actions/runners/registration-token":                        "runner registration tokens",
	"PUT /orgs/{org}/actions/secrets/{secretname}":                         "Actions secrets",
	"DELETE /orgs/{org}/actions/secrets/{secretname}":                      "Actions secrets",
	"POST /orgs/{org}/actions/variables/{variablename}":                    "Actions variables",
	"PUT /orgs/{org}/actions/variables/{variablename}":                     "Actions variables",
	"DELETE /orgs/{org}/actions/variables/{variablename}":                  "Actions variables",
	"GET /orgs/{org}/actions/runners/registration-token":                   "runner registration tokens",
	"POST /orgs/{org}/actions/runners/registration-token":                  "runner registration tokens",
	"PUT /repos/{username}/{reponame}/actions/secrets/{secretname}":        "Actions secrets",
	"DELETE /repos/{username}/{reponame}/actions/secrets/{secretname}":     "Actions secrets",
	"POST /repos/{username}/{reponame}/actions/variables/{variablename}":   "Actions variables",
	"PUT /repos/{username}/{reponame}/actions/variables/{variablename}":    "Actions variables",
	"DELETE /repos/{username}/{reponame}/actions/variables/{variablename}": "Actions variables",
	"GET /repos/{username}/{reponame}/actions/runners/registration-token":  "runner registration tokens",
	"POST /repos/{username}/{reponame}/actions/runners/registration-token": "runner registration tokens",
	"POST /admin/actions/runners/registration-token":                       "runner registration tokens",
	"GET /admin/runners/registration-token":                                "runner registration tokens",

	// Repository-level credentials and executable state.
	"GET /repos/{username}/{reponame}/keys":             "deploy keys",
	"POST /repos/{username}/{reponame}/keys":            "deploy keys",
	"GET /repos/{username}/{reponame}/keys/{id}":        "deploy keys",
	"DELETE /repos/{username}/{reponame}/keys/{id}":     "deploy keys",
	"PATCH /repos/{username}/{reponame}/hooks/git/{id}": "a git hook is a script the server runs on every later push",

	// Standing access for a principal that already holds its own credentials. The team-repository
	// grant appears twice on purpose - see the note at the top of this file.
	"PUT /repos/{username}/{reponame}/collaborators/{collaborator}": "grants a different account standing access",
	"PUT /repos/{username}/{reponame}/teams/{team}":                 "grants a team standing access (alias of PUT /teams/{teamid}/repos/{org}/{reponame})",
	"POST /repos/{username}/{reponame}/transfer":                    "hands the whole repository to a different principal",
	"PUT /teams/{teamid}/members/{username}":                        "grants a different account standing access",
	"PUT /teams/{teamid}/repos/{org}/{reponame}":                    "grants a team standing access",
	"PATCH /teams/{teamid}":                                         "widens an existing team over every member and repository at once",
	"PATCH /orgs/{org}":                                             "sets repo_admin_change_team_access, the authorization gate on the team-repository grant guarded above",

	// Whole logins on other accounts.
	"POST /admin/users":                        "creates an independent login",
	"PATCH /admin/users/{username}":            "sets password, primary email, login source, IsAdmin and AllowGitHook on an existing account",
	"POST /admin/users/{username}/keys":        "plants an SSH credential on an arbitrary account",
	"DELETE /admin/users/{username}/keys/{id}": "plants an SSH credential on an arbitrary account",
}

// agentAuthExemptRoutes is every mutating route a review decided does not need
// reqHumanAuth(), with the reason. The constants above carry the reasoning; grouping the
// routes by constant is what makes a misfiled one visible.
var agentAuthExemptRoutes = map[string]string{

	// Removes
	"DELETE /admin/actions/runners/{runner_id}":                                       exemptRemoves,
	"DELETE /admin/hooks/{id}":                                                        exemptRemoves,
	"DELETE /admin/unadopted/{username}/{reponame}":                                   exemptRemoves,
	"DELETE /admin/users/{username}":                                                  exemptRemoves,
	"DELETE /admin/users/{username}/badges":                                           exemptRemoves,
	"DELETE /orgs/{org}":                                                              exemptRemoves,
	"DELETE /orgs/{org}/actions/runners/{runner_id}":                                  exemptRemoves,
	"DELETE /orgs/{org}/avatar":                                                       exemptRemoves,
	"DELETE /orgs/{org}/blocks/{username}":                                            exemptRemoves,
	"DELETE /orgs/{org}/hooks/{id}":                                                   exemptRemoves,
	"DELETE /orgs/{org}/labels/{id}":                                                  exemptRemoves,
	"DELETE /orgs/{org}/members/{username}":                                           exemptRemoves,
	"DELETE /packages/{username}/{type}/{name}/{version}":                             exemptRemoves,
	"DELETE /repos/{username}/{reponame}":                                             exemptRemoves,
	"DELETE /repos/{username}/{reponame}/actions/artifacts/{artifact_id}":             exemptRemoves,
	"DELETE /repos/{username}/{reponame}/actions/runners/{runner_id}":                 exemptRemoves,
	"DELETE /repos/{username}/{reponame}/actions/runs/{run}":                          exemptRemoves,
	"DELETE /repos/{username}/{reponame}/avatar":                                      exemptRemoves,
	"DELETE /repos/{username}/{reponame}/branch_protections/{name}":                   exemptRemoves,
	"DELETE /repos/{username}/{reponame}/branches/*":                                  exemptRemoves,
	"DELETE /repos/{username}/{reponame}/collaborators/{collaborator}":                exemptRemoves,
	"DELETE /repos/{username}/{reponame}/contents/*":                                  exemptRemoves,
	"DELETE /repos/{username}/{reponame}/hooks/git/{id}":                              exemptRemoves,
	"DELETE /repos/{username}/{reponame}/hooks/{id}":                                  exemptRemoves,
	"DELETE /repos/{username}/{reponame}/issues/comments/{id}":                        exemptRemoves,
	"DELETE /repos/{username}/{reponame}/issues/comments/{id}/assets/{attachment_id}": exemptRemoves,
	"DELETE /repos/{username}/{reponame}/issues/comments/{id}/reactions":              exemptRemoves,
	"DELETE /repos/{username}/{reponame}/issues/{index}":                              exemptRemoves,
	"DELETE /repos/{username}/{reponame}/issues/{index}/assets/{attachment_id}":       exemptRemoves,
	"DELETE /repos/{username}/{reponame}/issues/{index}/blocks":                       exemptRemoves,
	"DELETE /repos/{username}/{reponame}/issues/{index}/comments/{id}":                exemptRemoves,
	"DELETE /repos/{username}/{reponame}/issues/{index}/dependencies":                 exemptRemoves,
	"DELETE /repos/{username}/{reponame}/issues/{index}/labels":                       exemptRemoves,
	"DELETE /repos/{username}/{reponame}/issues/{index}/labels/{id}":                  exemptRemoves,
	"DELETE /repos/{username}/{reponame}/issues/{index}/lock":                         exemptRemoves,
	"DELETE /repos/{username}/{reponame}/issues/{index}/pin":                          exemptRemoves,
	"DELETE /repos/{username}/{reponame}/issues/{index}/reactions":                    exemptRemoves,
	"DELETE /repos/{username}/{reponame}/issues/{index}/stopwatch/delete":             exemptRemoves,
	"DELETE /repos/{username}/{reponame}/issues/{index}/subscriptions/{user}":         exemptRemoves,
	"DELETE /repos/{username}/{reponame}/issues/{index}/times":                        exemptRemoves,
	"DELETE /repos/{username}/{reponame}/issues/{index}/times/{id}":                   exemptRemoves,
	"DELETE /repos/{username}/{reponame}/labels/{id}":                                 exemptRemoves,
	"DELETE /repos/{username}/{reponame}/milestones/{id}":                             exemptRemoves,
	"DELETE /repos/{username}/{reponame}/pulls/{index}/merge":                         exemptRemoves,
	"DELETE /repos/{username}/{reponame}/pulls/{index}/requested_reviewers":           exemptRemoves,
	"DELETE /repos/{username}/{reponame}/pulls/{index}/reviews/{id}":                  exemptRemoves,
	"DELETE /repos/{username}/{reponame}/push_mirrors/{name}":                         exemptRemoves,
	"DELETE /repos/{username}/{reponame}/releases/tags/{tag}":                         exemptRemoves,
	"DELETE /repos/{username}/{reponame}/releases/{id}":                               exemptRemoves,
	"DELETE /repos/{username}/{reponame}/releases/{id}/assets/{attachment_id}":        exemptRemoves,
	"DELETE /repos/{username}/{reponame}/subscription":                                exemptRemoves,
	"DELETE /repos/{username}/{reponame}/tag_protections/{id}":                        exemptRemoves,
	"DELETE /repos/{username}/{reponame}/tags/*":                                      exemptRemoves,
	"DELETE /repos/{username}/{reponame}/teams/{team}":                                exemptRemoves,
	"DELETE /repos/{username}/{reponame}/topics/{topic}":                              exemptRemoves,
	"DELETE /repos/{username}/{reponame}/wiki/page/{pageName}":                        exemptRemoves,
	"DELETE /teams/{teamid}":                                                          exemptRemoves,
	"DELETE /teams/{teamid}/members/{username}":                                       exemptRemoves,
	"DELETE /teams/{teamid}/repos/{org}/{reponame}":                                   exemptRemoves,
	"DELETE /user/actions/runners/{runner_id}":                                        exemptRemoves,
	"DELETE /user/avatar":                                                             exemptRemoves,
	"DELETE /user/blocks/{username}":                                                  exemptRemoves,
	"DELETE /user/emails":                                                             exemptRemoves,
	"DELETE /user/following/{username}":                                               exemptRemoves,
	"DELETE /user/hooks/{id}":                                                         exemptRemoves,
	"DELETE /user/starred/{username}/{reponame}":                                      exemptRemoves,

	// RepoWork
	"POST /orgs/{org}/labels":                                                        exemptRepoWork,
	"PATCH /orgs/{org}/labels/{id}":                                                  exemptRepoWork,
	"POST /repos/{username}/{reponame}/branch_protections":                           exemptRepoWork,
	"POST /repos/{username}/{reponame}/branch_protections/priority":                  exemptRepoWork,
	"PATCH /repos/{username}/{reponame}/branch_protections/{name}":                   exemptRepoWork,
	"POST /repos/{username}/{reponame}/branches":                                     exemptRepoWork,
	"PATCH /repos/{username}/{reponame}/branches/*":                                  exemptRepoWork,
	"PUT /repos/{username}/{reponame}/branches/*":                                    exemptRepoWork,
	"POST /repos/{username}/{reponame}/contents":                                     exemptRepoWork,
	"POST /repos/{username}/{reponame}/contents/*":                                   exemptRepoWork,
	"PUT /repos/{username}/{reponame}/contents/*":                                    exemptRepoWork,
	"POST /repos/{username}/{reponame}/diffpatch":                                    exemptRepoWork,
	"POST /repos/{username}/{reponame}/issues":                                       exemptRepoWork,
	"PATCH /repos/{username}/{reponame}/issues/comments/{id}":                        exemptRepoWork,
	"POST /repos/{username}/{reponame}/issues/comments/{id}/assets":                  exemptRepoWork,
	"PATCH /repos/{username}/{reponame}/issues/comments/{id}/assets/{attachment_id}": exemptRepoWork,
	"POST /repos/{username}/{reponame}/issues/comments/{id}/reactions":               exemptRepoWork,
	"PATCH /repos/{username}/{reponame}/issues/{index}":                              exemptRepoWork,
	"POST /repos/{username}/{reponame}/issues/{index}/assets":                        exemptRepoWork,
	"PATCH /repos/{username}/{reponame}/issues/{index}/assets/{attachment_id}":       exemptRepoWork,
	"POST /repos/{username}/{reponame}/issues/{index}/blocks":                        exemptRepoWork,
	"POST /repos/{username}/{reponame}/issues/{index}/comments":                      exemptRepoWork,
	"PATCH /repos/{username}/{reponame}/issues/{index}/comments/{id}":                exemptRepoWork,
	"POST /repos/{username}/{reponame}/issues/{index}/deadline":                      exemptRepoWork,
	"POST /repos/{username}/{reponame}/issues/{index}/dependencies":                  exemptRepoWork,
	"POST /repos/{username}/{reponame}/issues/{index}/labels":                        exemptRepoWork,
	"PUT /repos/{username}/{reponame}/issues/{index}/labels":                         exemptRepoWork,
	"PUT /repos/{username}/{reponame}/issues/{index}/lock":                           exemptRepoWork,
	"POST /repos/{username}/{reponame}/issues/{index}/pin":                           exemptRepoWork,
	"PATCH /repos/{username}/{reponame}/issues/{index}/pin/{position}":               exemptRepoWork,
	"POST /repos/{username}/{reponame}/issues/{index}/reactions":                     exemptRepoWork,
	"POST /repos/{username}/{reponame}/issues/{index}/stopwatch/start":               exemptRepoWork,
	"POST /repos/{username}/{reponame}/issues/{index}/stopwatch/stop":                exemptRepoWork,
	"POST /repos/{username}/{reponame}/issues/{index}/times":                         exemptRepoWork,
	"POST /repos/{username}/{reponame}/labels":                                       exemptRepoWork,
	"PATCH /repos/{username}/{reponame}/labels/{id}":                                 exemptRepoWork,
	"POST /repos/{username}/{reponame}/merge-upstream":                               exemptRepoWork,
	"POST /repos/{username}/{reponame}/milestones":                                   exemptRepoWork,
	"PATCH /repos/{username}/{reponame}/milestones/{id}":                             exemptRepoWork,
	"POST /repos/{username}/{reponame}/pulls":                                        exemptRepoWork,
	"POST /repos/{username}/{reponame}/pulls/comments/{id}/resolve":                  exemptRepoWork,
	"POST /repos/{username}/{reponame}/pulls/comments/{id}/unresolve":                exemptRepoWork,
	"PATCH /repos/{username}/{reponame}/pulls/{index}":                               exemptRepoWork,
	"POST /repos/{username}/{reponame}/pulls/{index}/merge":                          exemptRepoWork,
	"POST /repos/{username}/{reponame}/pulls/{index}/requested_reviewers":            exemptRepoWork,
	"POST /repos/{username}/{reponame}/pulls/{index}/reviews":                        exemptRepoWork,
	"POST /repos/{username}/{reponame}/pulls/{index}/reviews/{id}":                   exemptRepoWork,
	"POST /repos/{username}/{reponame}/pulls/{index}/reviews/{id}/dismissals":        exemptRepoWork,
	"POST /repos/{username}/{reponame}/pulls/{index}/reviews/{id}/undismissals":      exemptRepoWork,
	"POST /repos/{username}/{reponame}/pulls/{index}/update":                         exemptRepoWork,
	"POST /repos/{username}/{reponame}/releases":                                     exemptRepoWork,
	"PATCH /repos/{username}/{reponame}/releases/{id}":                               exemptRepoWork,
	"POST /repos/{username}/{reponame}/releases/{id}/assets":                         exemptRepoWork,
	"PATCH /repos/{username}/{reponame}/releases/{id}/assets/{attachment_id}":        exemptRepoWork,
	"POST /repos/{username}/{reponame}/statuses/{sha}":                               exemptRepoWork,
	"POST /repos/{username}/{reponame}/tag_protections":                              exemptRepoWork,
	"PATCH /repos/{username}/{reponame}/tag_protections/{id}":                        exemptRepoWork,
	"POST /repos/{username}/{reponame}/tags":                                         exemptRepoWork,
	"PUT /repos/{username}/{reponame}/topics":                                        exemptRepoWork,
	"PUT /repos/{username}/{reponame}/topics/{topic}":                                exemptRepoWork,
	"POST /repos/{username}/{reponame}/wiki/new":                                     exemptRepoWork,
	"PATCH /repos/{username}/{reponame}/wiki/page/{pageName}":                        exemptRepoWork,

	// NoGrant
	"POST /admin/users/{username}/badges":                                  exemptNoGrant,
	"PUT /notifications":                                                   exemptNoGrant,
	"PATCH /notifications/threads/{id}":                                    exemptNoGrant,
	"POST /orgs/{org}/avatar":                                              exemptNoGrant,
	"PUT /orgs/{org}/blocks/{username}":                                    exemptNoGrant,
	"POST /packages/{username}/{type}/{name}/-/link/{repo_name}":           exemptNoGrant,
	"POST /packages/{username}/{type}/{name}/-/unlink":                     exemptNoGrant,
	"POST /repos/{username}/{reponame}/avatar":                             exemptNoGrant,
	"PUT /repos/{username}/{reponame}/issues/{index}/subscriptions/{user}": exemptNoGrant,
	"PUT /repos/{username}/{reponame}/notifications":                       exemptNoGrant,
	"PUT /repos/{username}/{reponame}/subscription":                        exemptNoGrant,
	"POST /user/avatar":                                                    exemptNoGrant,
	"PUT /user/blocks/{username}":                                          exemptNoGrant,
	"PUT /user/following/{username}":                                       exemptNoGrant,
	"PATCH /user/settings":                                                 exemptNoGrant,
	"PUT /user/starred/{username}/{reponame}":                              exemptNoGrant,

	// Rendering
	"POST /markdown":     exemptRendering,
	"POST /markdown/raw": exemptRendering,
	"POST /markup":       exemptRendering,
	"POST /repos/{username}/{reponame}/file-contents": exemptRendering,
	"POST /repos/{username}/{reponame}/markdown":      exemptRendering,
	"POST /repos/{username}/{reponame}/markdown/raw":  exemptRendering,
	"POST /repos/{username}/{reponame}/markup":        exemptRendering,

	// Webhook
	"POST /admin/hooks":                                  exemptWebhook,
	"PATCH /admin/hooks/{id}":                            exemptWebhook,
	"POST /orgs/{org}/hooks":                             exemptWebhook,
	"PATCH /orgs/{org}/hooks/{id}":                       exemptWebhook,
	"POST /repos/{username}/{reponame}/hooks":            exemptWebhook,
	"PATCH /repos/{username}/{reponame}/hooks/{id}":      exemptWebhook,
	"POST /repos/{username}/{reponame}/hooks/{id}/tests": exemptWebhook,
	"POST /user/hooks":                                   exemptWebhook,
	"PATCH /user/hooks/{id}":                             exemptWebhook,

	// NewObject
	"POST /admin/unadopted/{username}/{reponame}": exemptNewObject,
	"POST /admin/users/{username}/orgs":           exemptNewObject,
	"POST /admin/users/{username}/repos":          exemptNewObject,
	"POST /org/{org}/repos":                       exemptNewObject,
	"POST /orgs":                                  exemptNewObject,
	"POST /orgs/{org}/repos":                      exemptNewObject,
	"POST /orgs/{org}/teams":                      exemptNewObject,
	"POST /repos/migrate":                         exemptNewObject,
	"POST /repos/{username}/{reponame}/forks":     exemptNewObject,
	"POST /repos/{username}/{reponame}/generate":  exemptNewObject,
	"POST /user/repos":                            exemptNewObject,

	// RunsExistingCode
	"POST /admin/cron/{task}": exemptRunsExistingCode,
	"PUT /repos/{username}/{reponame}/actions/workflows/{workflow_id}/disable":     exemptRunsExistingCode,
	"POST /repos/{username}/{reponame}/actions/workflows/{workflow_id}/dispatches": exemptRunsExistingCode,
	"PUT /repos/{username}/{reponame}/actions/workflows/{workflow_id}/enable":      exemptRunsExistingCode,
	"POST /repos/{username}/{reponame}/mirror-sync":                                exemptRunsExistingCode,
	"POST /repos/{username}/{reponame}/push_mirrors-sync":                          exemptRunsExistingCode,

	// Visibility
	"DELETE /orgs/{org}/public_members/{username}": exemptVisibility,
	"PUT /orgs/{org}/public_members/{username}":    exemptVisibility,
	"PATCH /repos/{username}/{reponame}":           exemptVisibility,

	// Rename
	"POST /admin/users/{username}/rename": exemptRename,
	"POST /orgs/{org}/rename":             exemptRename,

	// OutboundChannel
	"POST /repos/{username}/{reponame}/push_mirrors": exemptOutboundChannel,

	// AnswersAnOffer
	"POST /repos/{username}/{reponame}/transfer/accept": exemptAnswersAnOffer,
	"POST /repos/{username}/{reponame}/transfer/reject": exemptAnswersAnOffer,

	// BasicAuthOnly
	"POST /users/{username}/tokens":        exemptBasicAuthOnly,
	"DELETE /users/{username}/tokens/{id}": exemptBasicAuthOnly,

	// FederationInbox
	"POST /activitypub/user-id/{user-id}/inbox": exemptFederationInbox,
	"POST /activitypub/user/{username}/inbox":   exemptFederationInbox,

	// exemptPathGroupCatchAll
	"CONNECT /repos/{username}/{reponame}/commits/*": exemptPathGroupCatchAll,
	"DELETE /repos/{username}/{reponame}/commits/*":  exemptPathGroupCatchAll,
	"OPTIONS /repos/{username}/{reponame}/commits/*": exemptPathGroupCatchAll,
	"PATCH /repos/{username}/{reponame}/commits/*":   exemptPathGroupCatchAll,
	"POST /repos/{username}/{reponame}/commits/*":    exemptPathGroupCatchAll,
	"PUT /repos/{username}/{reponame}/commits/*":     exemptPathGroupCatchAll,
	"TRACE /repos/{username}/{reponame}/commits/*":   exemptPathGroupCatchAll,
}

// agentAuthConditionalRoutes are registered only under some configurations, so the "every listed
// route still exists" check below skips them. They are still classified above or below - the point
// of listing them is that a route missing because Federation is off must not be mistaken for a
// route that was renamed out from under the census.
var agentAuthConditionalRoutes = map[string]bool{
	"POST /activitypub/user/{username}/inbox":   true,
	"POST /activitypub/user-id/{user-id}/inbox": true,
}

func TestAPIAgentAuthRouteCensus(t *testing.T) {
	// No fixtures and no requests: this reads the route table the server registers.
	registered := map[string]bool{}
	require.NoError(t, v1.Routes().WalkRoutes(func(method, pattern string) error {
		registered[method+" "+pattern] = true
		return nil
	}))
	require.NotEmpty(t, registered, "the v1 route table came back empty; the census would pass vacuously")

	for route := range agentAuthGuardedRoutes {
		assert.NotContains(t, agentAuthExemptRoutes, route, "%s is listed as both guarded and exempt", route)
	}

	// Every mutating route is classified. A failure here is not a formality: decide which set the
	// route belongs in by asking whether a later request carrying no NIP-98 signature gets
	// anything out of it, and if the answer is yes, add reqHumanAuth() rather than an exemption.
	t.Run("every mutating route is classified", func(t *testing.T) {
		for route := range registered {
			method, _, _ := strings.Cut(route, " ")
			if method == http.MethodGet || method == http.MethodHead {
				continue
			}
			_, guarded := agentAuthGuardedRoutes[route]
			_, exempt := agentAuthExemptRoutes[route]
			assert.True(t, guarded || exempt,
				"%s is in neither agentAuthGuardedRoutes nor agentAuthExemptRoutes. If it plants a "+
					"credential or grants a principal standing access - including as a second path to "+
					"an operation guarded elsewhere - give it reqHumanAuth() and list it as guarded; "+
					"otherwise list it as exempt with the reason.", route)
		}
	})

	// Reads are exempt as a class because a GET plants nothing, with one exception: a runner
	// registration token is usable by whoever reads it, so reading is minting. Pinning it here
	// keeps the class exemption honest as new read routes appear.
	t.Run("reads that are equivalent to minting are guarded", func(t *testing.T) {
		for route := range registered {
			method, pattern, _ := strings.Cut(route, " ")
			if method != http.MethodGet || !strings.HasSuffix(pattern, "/registration-token") {
				continue
			}
			assert.Contains(t, agentAuthGuardedRoutes, route,
				"%s hands out a credential to whoever reads it, so it needs reqHumanAuth()", route)
		}
	})

	// The one shape the walk above cannot see. web.Router.PathGroup registers its pattern with
	// r.Any(), so chi's table holds all nine methods for it whatever the group dispatches, and the
	// matchers that decide what actually reaches a handler live in RouterPathGroup and are
	// consulted in its ServeHTTP. That is what exemptPathGroupCatchAll asserts about /commits/*,
	// and until this subtest existed it asserted it in prose: adding one mutating MatchPath inside
	// the group would leave chi's route table byte-identical, so the new endpoint would be born
	// exempt with nothing to notice it. WalkPathGroups reads the matchers back out, so the
	// exemption now stands or falls on what the group dispatches.
	t.Run("nothing mutating hides inside a path group catch-all", func(t *testing.T) {
		catchAllPatterns := map[string]bool{}
		for route, reason := range agentAuthExemptRoutes {
			if reason != exemptPathGroupCatchAll {
				continue
			}
			_, pattern, _ := strings.Cut(route, " ")
			catchAllPatterns[pattern] = true
		}
		require.NotEmpty(t, catchAllPatterns, "no pattern is exempt as a path-group catch-all any more; "+
			"if PathGroup is gone from v1 this subtest and exemptPathGroupCatchAll should go with it")

		walked := map[string]bool{}
		require.NoError(t, v1.Routes().WalkPathGroups(func(pattern string, matchers []web.PathGroupMatcher) error {
			walked[pattern] = true
			if !catchAllPatterns[pattern] {
				// Not exempt as a catch-all, so the walk above already required each of its
				// methods to be classified on its own.
				return nil
			}
			assert.NotEmpty(t, matchers, "%s dispatches no matcher at all", pattern)
			for _, matcher := range matchers {
				for _, method := range matcher.Methods {
					assert.Contains(t, []string{http.MethodGet, http.MethodHead}, method,
						"%s %s%s reaches a handler, but %s is classified as an unreachable "+
							"path-group catch-all. A mutating matcher inside a PathGroup is invisible "+
							"to chi's route table: classify this endpoint - guard it with reqHumanAuth() "+
							"if a later unsigned request would get anything out of it - rather than "+
							"leaving the catch-all exemption to cover it.",
						method, pattern, matcher.Pattern, pattern)
				}
			}
			return nil
		}))

		for pattern := range catchAllPatterns {
			assert.True(t, walked[pattern], "%s is exempt as a path-group catch-all but no path group "+
				"is registered there any more", pattern)
		}
	})

	// The reverse direction: a listed route that no longer exists means a route moved and took its
	// classification with it, which is exactly how an alias goes unnoticed.
	t.Run("no classification is stale", func(t *testing.T) {
		for route := range agentAuthGuardedRoutes {
			assert.True(t, registered[route], "%s is listed as guarded but is not registered any more", route)
		}
		for route := range agentAuthExemptRoutes {
			if agentAuthConditionalRoutes[route] {
				continue
			}
			assert.True(t, registered[route], "%s is listed as exempt but is not registered any more", route)
		}
	})

	// The conditional routes are only skipped above when the config that registers them is off.
	t.Run("conditional routes are checked when their config is on", func(t *testing.T) {
		if !setting.Federation.Enabled {
			t.Skip("federation is disabled in this test environment")
		}
		for route := range agentAuthConditionalRoutes {
			assert.True(t, registered[route], "%s should be registered when federation is enabled", route)
		}
	})
}

// agentAuthRouteParams gives every path placeholder that appears in agentAuthGuardedRoutes a value
// from the fixtures, so that the behavioural test below can turn a route pattern into a request.
//
// The values only have to survive whatever runs *before* reqHumanAuth() - repository, organization
// and user assignment, and the permission checks - because a guarded route never reaches its
// handler in this test. That is why {reponame} is one repository for every route: PUT
// /teams/{teamid}/repos/{org}/{reponame} resolves org3/repo1 inside its handler, which the refusal
// happens before. A placeholder with no value here is a failure rather than a skip; a new guarded
// route that introduced one would otherwise silently lose its behavioural half.
var agentAuthRouteParams = map[string]string{
	"username":     "user2", // a repository owner, an account under /admin/users, a team member
	"reponame":     "repo1", // owned by user2
	"org":          "org3",  // an organization in the fixtures
	"teamid":       "2",     // team1 of org3
	"team":         "team1",
	"collaborator": "user4",
	"id":           "1",
	"secretname":   "CENSUS_SECRET",
	"variablename": "CENSUS_VARIABLE",
}

var agentAuthRoutePlaceholder = regexp.MustCompile(`\{[^}]+\}`)

// agentAuthRoutePath substitutes agentAuthRouteParams into a route pattern.
func agentAuthRoutePath(t *testing.T, pattern string) string {
	t.Helper()

	return agentAuthRoutePlaceholder.ReplaceAllStringFunc(pattern, func(placeholder string) string {
		name, _, _ := strings.Cut(strings.Trim(placeholder, "{}"), ":")
		value, ok := agentAuthRouteParams[name]
		require.True(t, ok, "no fixture value for the %s path parameter of %s; add one to "+
			"agentAuthRouteParams so this route gets a behavioural test", placeholder, pattern)
		return value
	})
}

// The behavioural half of the census: every route listed in agentAuthGuardedRoutes gets a real
// NIP-98-signed request and has to answer reqHumanAuth's 403.
//
// The census next door asks whether a route was classified; it explicitly cannot ask whether the
// middleware is still attached, because chi bakes per-route middlewares into a closure chain. So
// dropping reqHumanAuth() from, say, m.Group("/keys", ..., reqHumanAuth()) used to pass the whole
// suite: the route is still registered, still classified, and nothing signed a request at it.
// Driving this from the map itself is what makes the two halves undriftable - a route added to
// agentAuthGuardedRoutes without a working guard fails here, and one removed from the map fails
// the census.
//
// Two things make each 403 the guard's and not something else's:
//
//   - The agent is user1, the site administrator, holding a key scoped "all". Every check that
//     runs ahead of reqHumanAuth() - reqSiteAdmin, reqAdmin, reqOwner, reqOrgOwnership,
//     reqTeamMembership, reqGitHook and tokenRequiresScopes - is therefore satisfied, so none of
//     them is what answers.
//   - The response body has to carry reqHumanAuth's own message. A 403 from any other check reads
//     differently, so a route that started refusing for a different reason fails here rather than
//     passing quietly.
func TestAPIAgentGuardedRoutesRefuseASignedRequest(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	// PATCH /repos/{username}/{reponame}/hooks/git/{id} sits behind reqGitHook(), which refuses
	// everyone while git hooks are disabled - the default. Without this the 403 for that one route
	// would come from reqGitHook() and the message assertion below would catch it.
	defer test.MockVariableValue(&setting.DisableGitHooks, false)()

	const secretKey = "000000000000000000000000000000000000000000000000000000000000004d"
	// "all" is deliberate: the point is to reach reqHumanAuth() on every route rather than to be
	// stopped by a scope, and a credential may only delegate what it holds, so the token needs it
	// too. user1 is the site administrator in the fixtures.
	token := getUserToken(t, "user1", auth_model.AccessTokenScopeAll)
	registerAgentKey(t, token, agentPubKey(t, secretKey), string(auth_model.AccessTokenScopeAll))

	routes := make([]string, 0, len(agentAuthGuardedRoutes))
	for route := range agentAuthGuardedRoutes {
		routes = append(routes, route)
	}
	sort.Strings(routes)
	require.NotEmpty(t, routes)

	for i, route := range routes {
		t.Run(route, func(t *testing.T) {
			method, pattern, _ := strings.Cut(route, " ")
			path := "/api/v1" + agentAuthRoutePath(t, pattern)

			// The nonce is what keeps the server's replay guard from rejecting the second of two
			// otherwise identical requests; the index makes it distinct per route without
			// depending on the route string's length.
			nonce := "census-" + strconv.Itoa(i)
			req := NewRequestWithBody(t, method, path, strings.NewReader("")).
				SetHeader("Authorization", signNIP98(t, secretKey, method, path, "", time.Now(), nonce))
			resp := MakeRequest(t, req, http.StatusForbidden)

			assert.Contains(t, resp.Body.String(), "an agent signature cannot be used here",
				"%s answered 403, but not with reqHumanAuth's refusal - so the guard is not what "+
					"stopped it, and whether the route is guarded is still untested", route)
		})
	}
}
