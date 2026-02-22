// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package robot

import (
	"net/http"

	"code.gitea.io/gitea/modules/context"
	"code.gitea.io/gitea/modules/setting"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/services/robot"
)

// Triage returns prioritized list of issues for agents
func Triage(ctx *context.APIContext) {
	// swagger:operation GET /robot/triage robot Triage
	// ---
	// summary: Get prioritized list of issues for agents
	// description: Returns a triage report with recommended issues to work on,
	//              ranked by PageRank and dependency analysis.
	// produces:
	// - application/json
	// parameters:
	// - name: owner
	//   in: query
	//   description: owner of the repo
	//   type: string
	//   required: true
	// - name: repo
	//   in: query
	//   description: name of the repo
	//   type: string
	//   required: true
	// responses:
	//   "200":
	//     description: Triage report
	//     schema:
	//       type: object
	//       properties:
	//         quick_ref:
	//           type: object
	//         recommendations:
	//           type: array
	//         blockers_to_clear:
	//           type: array
	//         project_health:
	//           type: object
	//   "404":
	//     "$ref": "#/responses/notFound"

	if !setting.IssueGraph.Enabled {
		ctx.Error(http.StatusNotFound, "IssueGraphDisabled", "Issue graph features are disabled")
		return
	}

	owner := ctx.FormString("owner")
	repoName := ctx.FormString("repo")

	if owner == "" || repoName == "" {
		ctx.Error(http.StatusBadRequest, "MissingParams", "owner and repo are required")
		return
	}

	repo, err := repo_model.GetRepositoryByOwnerAndName(ctx, owner, repoName)
	if err != nil {
		ctx.Error(http.StatusInternalServerError, "GetRepository", err)
		return
	}
	if repo == nil {
		ctx.NotFound()
		return
	}

	svc := robot.NewService()
	response, err := svc.Triage(ctx, repo.ID)
	if err != nil {
		ctx.Error(http.StatusInternalServerError, "Triage", err)
		return
	}

	ctx.JSON(http.StatusOK, response)
}

// Ready returns issues with no open blockers
func Ready(ctx *context.APIContext) {
	// swagger:operation GET /robot/ready robot Ready
	// ---
	// summary: Get issues that are ready to work on
	// description: Returns issues with no open blockers, sorted by PageRank.
	// produces:
	// - application/json
	// parameters:
	// - name: owner
	//   in: query
	//   description: owner of the repo
	//   type: string
	//   required: true
	// - name: repo
	//   in: query
	//   description: name of the repo
	//   type: string
	//   required: true
	// responses:
	//   "200":
	//     description: List of ready issues
	//     schema:
	//       type: object
	//       properties:
	//         issues:
	//           type: array
	//   "404":
	//     "$ref": "#/responses/notFound"

	if !setting.IssueGraph.Enabled {
		ctx.Error(http.StatusNotFound, "IssueGraphDisabled", "Issue graph features are disabled")
		return
	}

	owner := ctx.FormString("owner")
	repoName := ctx.FormString("repo")

	if owner == "" || repoName == "" {
		ctx.Error(http.StatusBadRequest, "MissingParams", "owner and repo are required")
		return
	}

	repo, err := repo_model.GetRepositoryByOwnerAndName(ctx, owner, repoName)
	if err != nil {
		ctx.Error(http.StatusInternalServerError, "GetRepository", err)
		return
	}
	if repo == nil {
		ctx.NotFound()
		return
	}

	svc := robot.NewService()
	response, err := svc.Ready(ctx, repo.ID)
	if err != nil {
		ctx.Error(http.StatusInternalServerError, "Ready", err)
		return
	}

	ctx.JSON(http.StatusOK, response)
}

// Graph returns the dependency graph for visualization
func Graph(ctx *context.APIContext) {
	// swagger:operation GET /robot/graph robot Graph
	// ---
	// summary: Get the dependency graph
	// description: Returns the full dependency graph with nodes and edges.
	// produces:
	// - application/json
	// parameters:
	// - name: owner
	//   in: query
	//   description: owner of the repo
	//   type: string
	//   required: true
	// - name: repo
	//   in: query
	//   description: name of the repo
	//   type: string
	//   required: true
	// responses:
	//   "200":
	//     description: Dependency graph
	//     schema:
	//       type: object
	//       properties:
	//         nodes:
	//           type: array
	//         edges:
	//           type: array
	//   "404":
	//     "$ref": "#/responses/notFound"

	if !setting.IssueGraph.Enabled {
		ctx.Error(http.StatusNotFound, "IssueGraphDisabled", "Issue graph features are disabled")
		return
	}

	owner := ctx.FormString("owner")
	repoName := ctx.FormString("repo")

	if owner == "" || repoName == "" {
		ctx.Error(http.StatusBadRequest, "MissingParams", "owner and repo are required")
		return
	}

	repo, err := repo_model.GetRepositoryByOwnerAndName(ctx, owner, repoName)
	if err != nil {
		ctx.Error(http.StatusInternalServerError, "GetRepository", err)
		return
	}
	if repo == nil {
		ctx.NotFound()
		return
	}

	svc := robot.NewService()
	response, err := svc.Graph(ctx, repo.ID)
	if err != nil {
		ctx.Error(http.StatusInternalServerError, "Graph", err)
		return
	}

	ctx.JSON(http.StatusOK, response)
}