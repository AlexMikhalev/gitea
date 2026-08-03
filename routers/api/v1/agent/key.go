// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package agent exposes the agent identity API: registering the Nostr keys that agents sign
// their requests with, and reading back the audit trail those signed requests leave.
package agent

import (
	gocontext "context"
	"errors"
	"net/http"
	"strings"

	agent_model "code.gitea.io/gitea/models/agent"
	auth_model "code.gitea.io/gitea/models/auth"
	"code.gitea.io/gitea/models/db"
	user_model "code.gitea.io/gitea/models/user"
	api "code.gitea.io/gitea/modules/structs"
	"code.gitea.io/gitea/modules/util"
	"code.gitea.io/gitea/modules/web"
	"code.gitea.io/gitea/routers/api/v1/utils"
	"code.gitea.io/gitea/services/context"
)

// errDuplicateKey is raised inside the registration transaction so that the "already registered"
// refusal can roll the transaction back and still be reported as a 422 rather than a 500.
var errDuplicateKey = errors.New("this public key is already registered")

// CreateKey registers a Nostr public key for an agent user.
func CreateKey(ctx *context.APIContext) {
	// swagger:operation POST /agent/keys agent agentCreateKey
	// ---
	// summary: Register a Nostr public key for an agent user
	// consumes:
	// - application/json
	// produces:
	// - application/json
	// parameters:
	// - name: body
	//   in: body
	//   schema:
	//     "$ref": "#/definitions/CreateAgentKeyOption"
	// responses:
	//   "201":
	//     "$ref": "#/responses/AgentKey"
	//   "403":
	//     "$ref": "#/responses/forbidden"
	//   "422":
	//     "$ref": "#/responses/validationError"

	form := web.GetForm(ctx).(*api.CreateAgentKeyOption)

	pubKey, npub, err := agent_model.PubKeyFromInput(form.PublicKey)
	if err != nil {
		ctx.APIError(http.StatusUnprocessableEntity, err)
		return
	}

	// The scope is mandatory and is the same vocabulary a personal access token uses. Without
	// one, a signed request would pass every tokenRequiresScopes guard in the API untouched -
	// which is to say the key would be strictly more powerful than any PAT its owner could
	// mint. Gitea already refuses to create a scopeless PAT; this matches it.
	scope, err := auth_model.AccessTokenScope(strings.Join(form.Scopes, ",")).Normalize()
	if err != nil {
		ctx.APIError(http.StatusUnprocessableEntity, "invalid agent key scope provided: "+err.Error())
		return
	}
	if scope == "" {
		ctx.APIError(http.StatusUnprocessableEntity, "an agent key must have a scope")
		return
	}

	// The ownership chain always roots at the human making this call, and that human may only
	// bind their own account as the agent. Letting an arbitrary user nominate someone else's
	// account would be a straight privilege escalation: register a key against an admin's user
	// id and every signed request afterwards is an admin request. Site admins are exempt
	// because they can already act as anyone.
	agentUserID := form.AgentUserID
	if agentUserID == 0 {
		agentUserID = ctx.Doer.ID
	}
	if agentUserID != ctx.Doer.ID && !ctx.IsUserSiteAdmin() {
		ctx.APIError(http.StatusForbidden, "only a site administrator may register a key for another user")
		return
	}

	agentUser, err := user_model.GetUserByID(ctx, agentUserID)
	if err != nil {
		if user_model.IsErrUserNotExist(err) {
			ctx.APIError(http.StatusUnprocessableEntity, "the agent user does not exist")
		} else {
			ctx.APIErrorInternal(err)
		}
		return
	}

	// An organization has no credentials of its own and the system users (Ghost, Actions) are
	// not accounts anybody signs in as, so neither can meaningfully be an agent. Nothing below
	// would reject them - a site admin could write is_agent onto a row that is not a login at
	// all - and the only thing standing between that and a working credential would be the
	// IsActive check in services/auth. Refuse the binding instead of corrupting the record.
	if !agentUser.IsTokenAccessAllowed() {
		ctx.APIError(http.StatusUnprocessableEntity, "only an individual or bot user can be an agent")
		return
	}

	key := &agent_model.Key{
		OwnerUserID: ctx.Doer.ID,
		AgentUserID: agentUser.ID,
		PubKey:      pubKey,
		Npub:        npub,
		Scope:       scope,
	}

	// The key row and the flag are one act, not two. Registering the key without the flag would
	// leave a credential that can never authenticate and can never be re-registered either -
	// pub_key is UNIQUE and there is no delete path, so the keypair would be burned for good.
	err = db.WithTx(ctx, func(ctx gocontext.Context) error {
		if _, err := agent_model.GetKeyByPubKey(ctx, pubKey); err == nil {
			return errDuplicateKey
		} else if !agent_model.IsErrAgentKeyNotExist(err) {
			return err
		}

		if err := agent_model.RegisterKey(ctx, key); err != nil {
			return err
		}

		// Registering a key is what makes a user an agent; services/auth refuses to
		// authenticate a signed request for a user without the flag.
		if !agentUser.IsAgent {
			agentUser.IsAgent = true
			if err := user_model.UpdateUserCols(ctx, agentUser, "is_agent"); err != nil {
				return err
			}
		}
		return nil
	})
	switch {
	case errors.Is(err, errDuplicateKey):
		ctx.APIError(http.StatusUnprocessableEntity, err)
		return
	case errors.Is(err, util.ErrInvalidArgument):
		ctx.APIError(http.StatusUnprocessableEntity, err)
		return
	case err != nil:
		ctx.APIErrorInternal(err)
		return
	}

	ctx.JSON(http.StatusCreated, toAPIAgentKey(key))
}

// ListKeys lists the Nostr keys the authenticated user has registered.
func ListKeys(ctx *context.APIContext) {
	// swagger:operation GET /agent/keys agent agentListKeys
	// ---
	// summary: List the Nostr keys the authenticated user has registered
	// produces:
	// - application/json
	// responses:
	//   "200":
	//     "$ref": "#/responses/AgentKeyList"
	//   "403":
	//     "$ref": "#/responses/forbidden"

	keys, err := agent_model.ListKeysByOwner(ctx, ctx.Doer.ID)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}

	result := make([]*api.AgentKey, len(keys))
	for i, key := range keys {
		result[i] = toAPIAgentKey(key)
	}
	ctx.JSON(http.StatusOK, result)
}

// RevokeKey revokes one of the authenticated user's registered Nostr keys.
func RevokeKey(ctx *context.APIContext) {
	// swagger:operation DELETE /agent/keys/{id} agent agentRevokeKey
	// ---
	// summary: Revoke a registered Nostr key
	// produces:
	// - application/json
	// parameters:
	// - name: id
	//   in: path
	//   description: id of the key to revoke
	//   type: integer
	//   format: int64
	//   required: true
	// responses:
	//   "204":
	//     "$ref": "#/responses/empty"
	//   "403":
	//     "$ref": "#/responses/forbidden"
	//   "404":
	//     "$ref": "#/responses/notFound"

	key, err := agent_model.GetKeyByID(ctx, ctx.PathParamInt64("id"))
	if err != nil {
		if agent_model.IsErrAgentKeyNotExist(err) {
			ctx.APIErrorNotFound()
		} else {
			ctx.APIErrorInternal(err)
		}
		return
	}

	// Only the human who vouched for the agent may withdraw that vouching, plus site admins.
	// Note that this is not the *agent* user: an agent whose user account happens to have API
	// access must not be able to unpick its owner's containment.
	if key.OwnerUserID != ctx.Doer.ID && !ctx.IsUserSiteAdmin() {
		ctx.APIError(http.StatusForbidden, "only the owner of a key may revoke it")
		return
	}

	// The key row survives revocation so that audit rows keep pointing at something real.
	//
	// Revoking the agent's last key also takes `is_agent` away again. Without that the flag is
	// one-way: services/auth checks only the flag before looking for a key, so an account that
	// has had every key revoked would stay enrolled as an agent forever and revocation would be
	// only half an undo. Both statements are one act, for the same reason registration is.
	err = db.WithTx(ctx, func(ctx gocontext.Context) error {
		if err := agent_model.RevokeKey(ctx, key.ID); err != nil {
			return err
		}

		stillActive, err := agent_model.HasActiveKeyForAgent(ctx, key.AgentUserID)
		if err != nil || stillActive {
			return err
		}

		agentUser, err := user_model.GetUserByID(ctx, key.AgentUserID)
		if err != nil {
			// The user is gone; there is no flag left to clear and the revocation stands.
			if user_model.IsErrUserNotExist(err) {
				return nil
			}
			return err
		}
		if !agentUser.IsAgent {
			return nil
		}
		agentUser.IsAgent = false
		return user_model.UpdateUserCols(ctx, agentUser, "is_agent")
	})
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	ctx.Status(http.StatusNoContent)
}

// ListRepoAudit lists the signed agent requests recorded against a repository.
func ListRepoAudit(ctx *context.APIContext) {
	// swagger:operation GET /repos/{owner}/{repo}/agent-audit repository repoListAgentAudit
	// ---
	// summary: List the signed agent requests recorded against a repository
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
	// - name: page
	//   in: query
	//   description: page number of results to return (1-based)
	//   type: integer
	// - name: limit
	//   in: query
	//   description: page size of results
	//   type: integer
	// responses:
	//   "200":
	//     "$ref": "#/responses/AgentAuditEventList"
	//   "403":
	//     "$ref": "#/responses/forbidden"
	//   "404":
	//     "$ref": "#/responses/notFound"

	events, total, err := agent_model.FindAuditEvents(ctx, agent_model.FindAuditEventsOptions{
		ListOptions: utils.GetListOptions(ctx),
		RepoID:      ctx.Repo.Repository.ID,
	})
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}

	result := make([]*api.AgentAuditEvent, len(events))
	for i, event := range events {
		result[i] = toAPIAgentAuditEvent(event)
	}

	ctx.SetTotalCountHeader(total)
	ctx.JSON(http.StatusOK, result)
}

// ListOwnedAudit lists every signed request made by the agents the authenticated user vouches for.
//
// The repo-scoped endpoint answers "what did agents do in this repository", which by construction
// cannot show anything that was not repository-scoped: a signed POST /user/keys, an org-scoped
// call, and every request refused before the router resolved a repository all carry repo_id 0.
// Those are precisely the rows an owner most needs to see, so they are reachable here, keyed on
// the ownership chain rather than on a repository.
func ListOwnedAudit(ctx *context.APIContext) {
	// swagger:operation GET /agent/audit agent agentListOwnedAudit
	// ---
	// summary: List the signed requests made by the agents the authenticated user owns
	// produces:
	// - application/json
	// parameters:
	// - name: agent_user_id
	//   in: query
	//   description: only list requests signed on behalf of this agent user
	//   type: integer
	//   format: int64
	// - name: page
	//   in: query
	//   description: page number of results to return (1-based)
	//   type: integer
	// - name: limit
	//   in: query
	//   description: page size of results
	//   type: integer
	// responses:
	//   "200":
	//     "$ref": "#/responses/AgentAuditEventList"
	//   "403":
	//     "$ref": "#/responses/forbidden"

	events, total, err := agent_model.FindAuditEvents(ctx, agent_model.FindAuditEventsOptions{
		ListOptions: utils.GetListOptions(ctx),
		// Scoped to the caller, never to a caller-supplied owner: the trail names who is
		// accountable, and only that human may read it.
		OwnerUserID: ctx.Doer.ID,
		AgentUserID: ctx.FormInt64("agent_user_id"),
	})
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}

	result := make([]*api.AgentAuditEvent, len(events))
	for i, event := range events {
		result[i] = toAPIAgentAuditEvent(event)
	}

	ctx.SetTotalCountHeader(total)
	ctx.JSON(http.StatusOK, result)
}

func toAPIAgentKey(key *agent_model.Key) *api.AgentKey {
	result := &api.AgentKey{
		ID:          key.ID,
		OwnerUserID: key.OwnerUserID,
		AgentUserID: key.AgentUserID,
		PublicKey:   key.PubKey,
		Npub:        key.Npub,
		Scopes:      key.Scope.StringSlice(),
		Created:     key.CreatedUnix.AsTime(),
	}
	if key.IsRevoked() {
		revoked := key.RevokedUnix.AsTime()
		result.Revoked = &revoked
	}
	return result
}

func toAPIAgentAuditEvent(event *agent_model.AuditEvent) *api.AgentAuditEvent {
	return &api.AgentAuditEvent{
		ID:             event.ID,
		RepoID:         event.RepoID,
		AgentUserID:    event.AgentUserID,
		OwnerUserID:    event.OwnerUserID,
		AgentKeyID:     event.AgentKeyID,
		EventID:        event.EventID,
		PublicKey:      event.PubKey,
		Method:         event.Method,
		RequestURL:     event.RequestURL,
		PayloadHash:    event.PayloadHash,
		ResponseStatus: event.ResponseStatus,
		Created:        event.CreatedUnix.AsTime(),
	}
}
