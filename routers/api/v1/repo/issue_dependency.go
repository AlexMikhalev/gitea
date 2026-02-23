// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"net/http"

	issues_model "code.gitea.io/gitea/models/issues"
	"code.gitea.io/gitea/services/context"
	"code.gitea.io/gitea/modules/setting"
)

// IssueDependencyResponse represents a dependency response
type IssueDependencyResponse struct {
	ID           int64  `json:"id"`
	IssueID      int64  `json:"issue_id"`
	DependencyID int64  `json:"dependency_id"`
}

// ListIssueDependencies lists all dependencies for an issue
func ListIssueDependencies(ctx *context.APIContext) {
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

	// Use existing Gitea dependency functions
	deps, err := issues_model.GetIssueDependencies(ctx, issue.ID)
	if err != nil {
		ctx.Error(http.StatusInternalServerError, "GetIssueDependencies", err)
		return
	}

	apiDeps := make([]*IssueDependencyResponse, len(deps))
	for i, dep := range deps {
		apiDeps[i] = &IssueDependencyResponse{
			ID:           dep.ID,
			IssueID:      dep.IssueID,
			DependencyID: dep.DependencyID,
		}
	}

	ctx.JSON(http.StatusOK, apiDeps)
}