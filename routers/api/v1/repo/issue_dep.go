// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"net/http"

	"code.gitea.io/gitea/services/context"
	"code.gitea.io/gitea/modules/setting"
)

// GetIssueDependencies lists dependencies for an issue
func GetIssueDependencies(ctx *context.APIContext) {
	if !setting.IssueGraphSettings.Enabled {
		ctx.APIErrorNotFound("Issue graph features are disabled")
		return
	}
	ctx.JSON(http.StatusOK, []interface{}{})
}

// CreateIssueDependency creates a dependency
func CreateIssueDependency(ctx *context.APIContext) {
	if !setting.IssueGraphSettings.Enabled {
		ctx.APIErrorNotFound("Issue graph features are disabled")
		return
	}
	ctx.Status(http.StatusCreated)
}

// RemoveIssueDependency removes a dependency
func RemoveIssueDependency(ctx *context.APIContext) {
	if !setting.IssueGraphSettings.Enabled {
		ctx.APIErrorNotFound("Issue graph features are disabled")
		return
	}
	ctx.Status(http.StatusNoContent)
}

// GetIssueBlocks lists blocking issues
func GetIssueBlocks(ctx *context.APIContext) {
	if !setting.IssueGraphSettings.Enabled {
		ctx.APIErrorNotFound("Issue graph features are disabled")
		return
	}
	ctx.JSON(http.StatusOK, []interface{}{})
}

// CreateIssueBlocking creates a blocking relationship
func CreateIssueBlocking(ctx *context.APIContext) {
	if !setting.IssueGraphSettings.Enabled {
		ctx.APIErrorNotFound("Issue graph features are disabled")
		return
	}
	ctx.Status(http.StatusCreated)
}

// RemoveIssueBlocking removes a blocking relationship
func RemoveIssueBlocking(ctx *context.APIContext) {
	if !setting.IssueGraphSettings.Enabled {
		ctx.APIErrorNotFound("Issue graph features are disabled")
		return
	}
	ctx.Status(http.StatusNoContent)
}