// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package robot

import (
	"net/http"

	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/services/context"
	"code.gitea.io/gitea/services/robot"
)

// Triage returns prioritized list of issues for agents
func Triage(ctx *context.APIContext) {
	if !setting.IssueGraph.Enabled {
		ctx.APIErrorNotFound("Issue graph features are disabled")
		return
	}

	owner := ctx.FormString("owner")
	repoName := ctx.FormString("repo")

	if owner == "" || repoName == "" {
		ctx.APIError(http.StatusBadRequest, "owner and repo query parameters are required")
		return
	}

	repo, err := repo_model.GetRepositoryByOwnerAndName(ctx, owner, repoName)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}
	if repo == nil {
		ctx.APIErrorNotFound("Repository not found")
		return
	}

	svc := robot.NewService()
	response, err := svc.Triage(ctx, repo.ID)
	if err != nil {
		ctx.APIErrorInternal(err)
		return
	}

	ctx.JSON(http.StatusOK, response)
}