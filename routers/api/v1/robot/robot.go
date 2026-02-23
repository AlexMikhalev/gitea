// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package robot

import (
	"net/http"

	"code.gitea.io/gitea/services/context"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/services/robot"
)

// Triage returns prioritized list of issues for agents
func Triage(ctx *context.APIContext) {
	if !setting.IssueGraph.Enabled {
		ctx.APIError(http.StatusNotFound, "Issue graph features are disabled")
		return
	}

	svc := robot.NewService()
	response, err := svc.Triage(ctx, ctx.Repo.Repository.ID)
	if err != nil {
		ctx.APIError(http.StatusInternalServerError, err)
		return
	}

	ctx.JSON(http.StatusOK, response)
}