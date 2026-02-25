// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package repo

import (
	"net/http"

	"code.gitea.io/gitea/services/context"
	"code.gitea.io/gitea/modules/setting"
)

// ListIssueDependencies lists all dependencies for an issue
func ListIssueDependencies(ctx *context.APIContext) {
	if !setting.IssueGraphSettings.Enabled {
		ctx.APIErrorNotFound("Issue graph features are disabled")
		return
	}

	// TODO: Implement using existing Gitea dependency functions
	ctx.JSON(http.StatusOK, []interface{}{})
}