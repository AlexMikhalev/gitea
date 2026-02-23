// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"net/http"

	issues_model "code.gitea.io/gitea/models/issues"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/web"
	api "code.gitea.io/gitea/modules/structs"
	"code.gitea.io/gitea/services/context"
	"code.gitea.io/gitea/services/convert"
)

// IssueDependencyRequest represents a request to add a dependency
type IssueDependencyRequest struct {
	DependsOn int64  `json:"depends_on" binding:"Required"`
	DepType   string `json:"dep_type" binding:"Required"`
}

// IssueDependencyResponse represents a dependency response
type IssueDependencyResponse struct {
	ID        int64  `json:"id"`
	IssueID   int64  `json:"issue_id"`
	DependsOn int64  `json:"depends_on"`
	DepType   string `json:"dep_type"`
}

// ListIssueDependencies lists all dependencies for an issue
func ListIssueDependencies(ctx *context.APIContext) {
	// swagger:operation GET /repos/{owner}/{repo}/issues/{index}/dependencies issue ListIssueDependencies
	// ---
	// summary: List an issue's dependencies
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
	//   type: integer
	//   required: true
	// responses:
	//   "200":
	//     "$ref": "#/responses/IssueDependencyList"
	//   "404":
	//     "$ref": "#/responses/notFound"

	if !setting.IssueGraph.Enabled {
		ctx.Error(http.StatusNotFound, "IssueGraphDisabled", "Issue graph features are disabled")
		return
	}

	issue, err := issues_model.GetIssueByIndex(ctx, ctx.Repo.Repository.ID, ctx.ParamsInt64(":index"))
	if err != nil {
		if issues_model.IsErrIssueNotExist(err) {
			ctx.NotFound()
		} else {
			ctx.Error(http.StatusInternalServerError, "GetIssueByIndex", err)
		}
		return
	}

	deps, err := issues_model.GetDependencies(ctx, ctx.Repo.Repository.ID, issue.ID)
	if err != nil {
		ctx.Error(http.StatusInternalServerError, "GetDependencies", err)
		return
	}

	apiDeps := make([]*IssueDependencyResponse, len(deps))
	for i, dep := range deps {
		apiDeps[i] = &IssueDependencyResponse{
			ID:        dep.ID,
			IssueID:   dep.IssueID,
			DependsOn: dep.DependsOn,
			DepType:   string(dep.DepType),
		}
	}

	ctx.JSON(http.StatusOK, apiDeps)
}

// AddIssueDependency adds a dependency to an issue
func AddIssueDependency(ctx *context.APIContext) {
	// swagger:operation POST /repos/{owner}/{repo}/issues/{index}/dependencies issue AddIssueDependency
	// ---
	// summary: Add a dependency to an issue
	// consumes:
	// - application/json
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
	//   type: integer
	//   required: true
	// - name: body
	//   in: body
	//   schema:
	//     "$ref": "#/definitions/IssueDependencyRequest"
	// responses:
	//   "201":
	//     "$ref": "#/responses/IssueDependency"
	//   "400":
	//     "$ref": "#/responses/error"
	//   "404":
	//     "$ref": "#/responses/notFound"

	if !setting.IssueGraph.Enabled {
		ctx.Error(http.StatusNotFound, "IssueGraphDisabled", "Issue graph features are disabled")
		return
	}

	form := web.GetForm(ctx).(*IssueDependencyRequest)

	issue, err := issues_model.GetIssueByIndex(ctx, ctx.Repo.Repository.ID, ctx.ParamsInt64(":index"))
	if err != nil {
		if issues_model.IsErrIssueNotExist(err) {
			ctx.NotFound()
		} else {
			ctx.Error(http.StatusInternalServerError, "GetIssueByIndex", err)
		}
		return
	}

	// Check if target issue exists
	_, err = issues_model.GetIssueByID(ctx, form.DependsOn)
	if err != nil {
		if issues_model.IsErrIssueNotExist(err) {
			ctx.Error(http.StatusNotFound, "DependsOnIssueNotExist", "The issue to depend on does not exist")
		} else {
			ctx.Error(http.StatusInternalServerError, "GetIssueByID", err)
		}
		return
	}

	depType := issues_model.DependencyType(form.DepType)
	if err := issues_model.AddDependency(ctx, ctx.Repo.Repository.ID, issue.ID, form.DependsOn, depType); err != nil {
		if issues_model.IsErrDependencyAlreadyExists(err) {
			ctx.Error(http.StatusConflict, "DependencyAlreadyExists", "This dependency already exists")
		} else if issues_model.IsErrCircularDependency(err) {
			ctx.Error(http.StatusBadRequest, "CircularDependency", "This would create a circular dependency")
		} else {
			ctx.Error(http.StatusInternalServerError, "AddDependency", err)
		}
		return
	}

	ctx.Status(http.StatusCreated)
}

// RemoveIssueDependency removes a dependency from an issue
func RemoveIssueDependency(ctx *context.APIContext) {
	// swagger:operation DELETE /repos/{owner}/{repo}/issues/{index}/dependencies/{dependency_id} issue RemoveIssueDependency
	// ---
	// summary: Remove a dependency from an issue
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
	//   type: integer
	//   required: true
	// - name: dependency_id
	//   in: path
	//   description: id of the dependency to remove
	//   type: integer
	//   required: true
	// responses:
	//   "204":
	//     "$ref": "#/responses/empty"
	//   "404":
	//     "$ref": "#/responses/notFound"

	if !setting.IssueGraph.Enabled {
		ctx.Error(http.StatusNotFound, "IssueGraphDisabled", "Issue graph features are disabled")
		return
	}

	issue, err := issues_model.GetIssueByIndex(ctx, ctx.Repo.Repository.ID, ctx.ParamsInt64(":index"))
	if err != nil {
		if issues_model.IsErrIssueNotExist(err) {
			ctx.NotFound()
		} else {
			ctx.Error(http.StatusInternalServerError, "GetIssueByIndex", err)
		}
		return
	}

	depID := ctx.ParamsInt64(":dependency_id")

	// Get the dependency to find the depends_on issue
	deps, err := issues_model.GetDependencies(ctx, ctx.Repo.Repository.ID, issue.ID)
	if err != nil {
		ctx.Error(http.StatusInternalServerError, "GetDependencies", err)
		return
	}

	var dependsOn int64
	found := false
	for _, dep := range deps {
		if dep.ID == depID {
			dependsOn = dep.DependsOn
			found = true
			break
		}
	}

	if !found {
		ctx.Error(http.StatusNotFound, "DependencyNotFound", "Dependency not found")
		return
	}

	if err := issues_model.RemoveDependency(ctx, ctx.Repo.Repository.ID, issue.ID, dependsOn); err != nil {
		ctx.Error(http.StatusInternalServerError, "RemoveDependency", err)
		return
	}

	ctx.Status(http.StatusNoContent)
}

// GetIssueBlockers returns issues that block this issue
func GetIssueBlockers(ctx *context.APIContext) {
	// swagger:operation GET /repos/{owner}/{repo}/issues/{index}/blockers issue GetIssueBlockers
	// ---
	// summary: Get issues that block this issue
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
	//   type: integer
	//   required: true
	// responses:
	//   "200":
	//     "$ref": "#/responses/IssueList"
	//   "404":
	//     "$ref": "#/responses/notFound"

	if !setting.IssueGraph.Enabled {
		ctx.Error(http.StatusNotFound, "IssueGraphDisabled", "Issue graph features are disabled")
		return
	}

	issue, err := issues_model.GetIssueByIndex(ctx, ctx.Repo.Repository.ID, ctx.ParamsInt64(":index"))
	if err != nil {
		if issues_model.IsErrIssueNotExist(err) {
			ctx.NotFound()
		} else {
			ctx.Error(http.StatusInternalServerError, "GetIssueByIndex", err)
		}
		return
	}

	blockers, err := issues_model.GetBlockedIssues(ctx, ctx.Repo.Repository.ID, issue.ID)
	if err != nil {
		ctx.Error(http.StatusInternalServerError, "GetBlockedIssues", err)
		return
	}

	// Get full issue details for blockers
	apiIssues := make([]*api.Issue, len(blockers))
	for i, blocker := range blockers {
		blockerIssue, err := issues_model.GetIssueByID(ctx, blocker.DependsOn)
		if err != nil {
			continue
		}
		apiIssues[i] = convert.ToAPIIssue(ctx, ctx.Doer, blockerIssue)
	}

	ctx.JSON(http.StatusOK, apiIssues)
}