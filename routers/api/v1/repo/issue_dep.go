// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"fmt"
	"net/http"

	issues_model "code.gitea.io/gitea/models/issues"
	access_model "code.gitea.io/gitea/models/perm/access"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/modules/setting"
	api "code.gitea.io/gitea/modules/structs"
	"code.gitea.io/gitea/modules/web"
	"code.gitea.io/gitea/services/context"
	"code.gitea.io/gitea/services/convert"
)

// GetIssueDependencies lists dependencies for an issue
func GetIssueDependencies(ctx *context.APIContext) {
	// swagger:operation GET /repos/{owner}/{repo}/issues/{index}/dependencies issue issueListIssueDependencies
	// ---
	// summary: List an issue's dependencies, i.e all issues that block this issue.
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
	// - name: index
	//   in: path
	//   description: index of the issue
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
	//     "$ref": "#/responses/IssueList"
	//   "404":
	//     "$ref": "#/responses/notFound"

	if !setting.IssueGraphSettings.Enabled {
		ctx.APIErrorNotFound("Issue graph features are disabled")
		return
	}

	issue, err := issues_model.GetIssueByIndex(ctx, ctx.Repo.Repository.ID, ctx.PathParamInt64("index"))
	if err != nil {
		if issues_model.IsErrIssueNotExist(err) {
			ctx.APIErrorNotFound("issue not found")
		} else {
			ctx.APIErrorInternal(err)
		}
		return
	}

	deps, err := issues_model.GetDependenciesByIssueID(ctx, issue.ID)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}

	apiIssues := make([]*api.Issue, 0, len(deps))
	for _, dep := range deps {
		if err := dep.LoadRepo(ctx); err != nil {
			ctx.APIErrorInternal(err)
			return
		}
		apiIssues = append(apiIssues, convert.ToAPIIssue(ctx, ctx.Doer, dep))
	}

	ctx.JSON(http.StatusOK, apiIssues)
}

// CreateIssueDependency creates a dependency
func CreateIssueDependency(ctx *context.APIContext) {
	// swagger:operation POST /repos/{owner}/{repo}/issues/{index}/dependencies issue issueCreateIssueDependencies
	// ---
	// summary: Make the issue in the url depend on the issue in the form.
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
	// - name: index
	//   in: path
	//   description: index of the issue
	//   type: string
	//   required: true
	// - name: body
	//   in: body
	//   schema:
	//     "$ref": "#/definitions/IssueMeta"
	// responses:
	//   "201":
	//     "$ref": "#/responses/empty"
	//   "400":
	//     "$ref": "#/responses/error"
	//   "404":
	//     "$ref": "#/responses/notFound"
	//   "409":
	//     "$ref": "#/responses/conflict"
	//   "423":
	//     "$ref": "#/responses/repoArchivedError"

	if !setting.IssueGraphSettings.Enabled {
		ctx.APIErrorNotFound("Issue graph features are disabled")
		return
	}

	issue, err := issues_model.GetIssueByIndex(ctx, ctx.Repo.Repository.ID, ctx.PathParamInt64("index"))
	if err != nil {
		if issues_model.IsErrIssueNotExist(err) {
			ctx.APIErrorNotFound("issue not found")
		} else {
			ctx.APIErrorInternal(err)
		}
		return
	}

	form := web.GetForm(ctx).(*api.IssueMeta)
	dep, err := resolveDependencyIssue(ctx, form)
	if err != nil {
		return // error already written to ctx
	}

	if dep.ID == issue.ID {
		ctx.APIError(http.StatusBadRequest, "cannot add dependency on itself")
		return
	}

	if err := issues_model.CreateIssueDependency(ctx, ctx.Doer, issue, dep); err != nil {
		if issues_model.IsErrDependencyExists(err) {
			ctx.APIError(http.StatusConflict, "dependency already exists")
			return
		} else if issues_model.IsErrCircularDependency(err) {
			ctx.APIError(http.StatusBadRequest, "would create circular dependency")
			return
		}
		ctx.APIErrorInternal(err)
		return
	}

	ctx.Status(http.StatusCreated)
}

// RemoveIssueDependency removes a dependency
func RemoveIssueDependency(ctx *context.APIContext) {
	// swagger:operation DELETE /repos/{owner}/{repo}/issues/{index}/dependencies issue issueRemoveIssueDependencies
	// ---
	// summary: Remove an issue dependency
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
	// - name: index
	//   in: path
	//   description: index of the issue
	//   type: string
	//   required: true
	// - name: body
	//   in: body
	//   schema:
	//     "$ref": "#/definitions/IssueMeta"
	// responses:
	//   "204":
	//     "$ref": "#/responses/empty"
	//   "404":
	//     "$ref": "#/responses/notFound"
	//   "423":
	//     "$ref": "#/responses/repoArchivedError"

	if !setting.IssueGraphSettings.Enabled {
		ctx.APIErrorNotFound("Issue graph features are disabled")
		return
	}

	issue, err := issues_model.GetIssueByIndex(ctx, ctx.Repo.Repository.ID, ctx.PathParamInt64("index"))
	if err != nil {
		if issues_model.IsErrIssueNotExist(err) {
			ctx.APIErrorNotFound("issue not found")
		} else {
			ctx.APIErrorInternal(err)
		}
		return
	}

	form := web.GetForm(ctx).(*api.IssueMeta)
	dep, err := resolveDependencyIssue(ctx, form)
	if err != nil {
		return
	}

	if err := issues_model.RemoveIssueDependency(ctx, ctx.Doer, issue, dep, issues_model.DependencyTypeBlockedBy); err != nil {
		if issues_model.IsErrDependencyNotExists(err) {
			ctx.APIError(http.StatusNotFound, "dependency not found")
			return
		}
		ctx.APIErrorInternal(err)
		return
	}

	ctx.Status(http.StatusNoContent)
}

// GetIssueBlocks lists issues that this issue blocks
func GetIssueBlocks(ctx *context.APIContext) {
	// swagger:operation GET /repos/{owner}/{repo}/issues/{index}/blocks issue issueListBlocks
	// ---
	// summary: List issues that are blocked by this issue
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
	// - name: index
	//   in: path
	//   description: index of the issue
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
	//     "$ref": "#/responses/IssueList"
	//   "404":
	//     "$ref": "#/responses/notFound"

	if !setting.IssueGraphSettings.Enabled {
		ctx.APIErrorNotFound("Issue graph features are disabled")
		return
	}

	issue, err := issues_model.GetIssueByIndex(ctx, ctx.Repo.Repository.ID, ctx.PathParamInt64("index"))
	if err != nil {
		if issues_model.IsErrIssueNotExist(err) {
			ctx.APIErrorNotFound("issue not found")
		} else {
			ctx.APIErrorInternal(err)
		}
		return
	}

	blocks, err := issues_model.GetBlockedByDependencies(ctx, issue.ID)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}

	apiIssues := make([]*api.Issue, 0, len(blocks))
	for _, blocked := range blocks {
		if err := blocked.LoadRepo(ctx); err != nil {
			ctx.APIErrorInternal(err)
			return
		}
		apiIssues = append(apiIssues, convert.ToAPIIssue(ctx, ctx.Doer, blocked))
	}

	ctx.JSON(http.StatusOK, apiIssues)
}

// CreateIssueBlocking creates a blocking relationship (this issue blocks another)
func CreateIssueBlocking(ctx *context.APIContext) {
	// swagger:operation POST /repos/{owner}/{repo}/issues/{index}/blocks issue issueCreateIssueBlocking
	// ---
	// summary: Block the issue given in the body by the issue in path
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
	// - name: index
	//   in: path
	//   description: index of the issue
	//   type: string
	//   required: true
	// - name: body
	//   in: body
	//   schema:
	//     "$ref": "#/definitions/IssueMeta"
	// responses:
	//   "201":
	//     "$ref": "#/responses/empty"
	//   "400":
	//     "$ref": "#/responses/error"
	//   "404":
	//     "$ref": "#/responses/notFound"
	//   "409":
	//     "$ref": "#/responses/conflict"

	if !setting.IssueGraphSettings.Enabled {
		ctx.APIErrorNotFound("Issue graph features are disabled")
		return
	}

	issue, err := issues_model.GetIssueByIndex(ctx, ctx.Repo.Repository.ID, ctx.PathParamInt64("index"))
	if err != nil {
		if issues_model.IsErrIssueNotExist(err) {
			ctx.APIErrorNotFound("issue not found")
		} else {
			ctx.APIErrorInternal(err)
		}
		return
	}

	form := web.GetForm(ctx).(*api.IssueMeta)
	blocked, err := resolveDependencyIssue(ctx, form)
	if err != nil {
		return
	}

	if blocked.ID == issue.ID {
		ctx.APIError(http.StatusBadRequest, "cannot block itself")
		return
	}

	// Create: blocked depends on issue (issue blocks blocked)
	if err := issues_model.CreateIssueDependency(ctx, ctx.Doer, blocked, issue); err != nil {
		if issues_model.IsErrDependencyExists(err) {
			ctx.APIError(http.StatusConflict, "blocking relationship already exists")
			return
		} else if issues_model.IsErrCircularDependency(err) {
			ctx.APIError(http.StatusBadRequest, "would create circular dependency")
			return
		}
		ctx.APIErrorInternal(err)
		return
	}

	ctx.Status(http.StatusCreated)
}

// RemoveIssueBlocking removes a blocking relationship
func RemoveIssueBlocking(ctx *context.APIContext) {
	// swagger:operation DELETE /repos/{owner}/{repo}/issues/{index}/blocks issue issueRemoveIssueBlocking
	// ---
	// summary: Unblock the issue given in the body by the issue in path
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
	// - name: index
	//   in: path
	//   description: index of the issue
	//   type: string
	//   required: true
	// - name: body
	//   in: body
	//   schema:
	//     "$ref": "#/definitions/IssueMeta"
	// responses:
	//   "204":
	//     "$ref": "#/responses/empty"
	//   "404":
	//     "$ref": "#/responses/notFound"

	if !setting.IssueGraphSettings.Enabled {
		ctx.APIErrorNotFound("Issue graph features are disabled")
		return
	}

	issue, err := issues_model.GetIssueByIndex(ctx, ctx.Repo.Repository.ID, ctx.PathParamInt64("index"))
	if err != nil {
		if issues_model.IsErrIssueNotExist(err) {
			ctx.APIErrorNotFound("issue not found")
		} else {
			ctx.APIErrorInternal(err)
		}
		return
	}

	form := web.GetForm(ctx).(*api.IssueMeta)
	blocked, err := resolveDependencyIssue(ctx, form)
	if err != nil {
		return
	}

	if err := issues_model.RemoveIssueDependency(ctx, ctx.Doer, blocked, issue, issues_model.DependencyTypeBlocking); err != nil {
		if issues_model.IsErrDependencyNotExists(err) {
			ctx.APIError(http.StatusNotFound, "blocking relationship not found")
			return
		}
		ctx.APIErrorInternal(err)
		return
	}

	ctx.Status(http.StatusNoContent)
}

// resolveDependencyIssue resolves an IssueMeta to an Issue, checking permissions.
func resolveDependencyIssue(ctx *context.APIContext, form *api.IssueMeta) (*issues_model.Issue, error) {
	// Determine the repo for the dependency
	depRepo := ctx.Repo.Repository

	if form.Owner != "" && form.Name != "" {
		// Cross-repo dependency
		if !setting.Service.AllowCrossRepositoryDependencies {
			ctx.APIError(http.StatusBadRequest, "cross-repository dependencies are not enabled")
			return nil, fmt.Errorf("cross-repository dependencies disabled")
		}

		var err error
		depRepo, err = repo_model.GetRepositoryByOwnerAndName(ctx, form.Owner, form.Name)
		if err != nil {
			if repo_model.IsErrRepoNotExist(err) {
				ctx.APIErrorNotFound("repository not found")
			} else {
				ctx.APIErrorInternal(err)
			}
			return nil, err
		}

		perm, err := access_model.GetUserRepoPermission(ctx, depRepo, ctx.Doer)
		if err != nil {
			ctx.APIErrorInternal(err)
			return nil, err
		}
		if !perm.CanReadIssuesOrPulls(false) {
			ctx.APIErrorNotFound("repository not found")
			return nil, fmt.Errorf("no permission to read issues")
		}
	}

	dep, err := issues_model.GetIssueByIndex(ctx, depRepo.ID, form.Index)
	if err != nil {
		if issues_model.IsErrIssueNotExist(err) {
			ctx.APIErrorNotFound("dependency issue not found")
		} else {
			ctx.APIErrorInternal(err)
		}
		return nil, err
	}

	return dep, nil
}
