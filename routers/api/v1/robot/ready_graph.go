// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package robot

import (
	"net/http"

	"code.gitea.io/gitea/services/context"
	"code.gitea.io/gitea/modules/setting"
)

// Ready returns unblocked issues
func Ready(ctx *context.APIContext) {
	if !setting.IssueGraph.Enabled {
		ctx.APIErrorNotFound("Issue graph features are disabled")
		return
	}
	ctx.JSON(http.StatusOK, map[string]interface{}{"issues": []interface{}{}})
}

// Graph returns dependency graph
func Graph(ctx *context.APIContext) {
	if !setting.IssueGraph.Enabled {
		ctx.APIErrorNotFound("Issue graph features are disabled")
		return
	}
	ctx.JSON(http.StatusOK, map[string]interface{}{
		"nodes": []interface{}{},
		"edges": []interface{}{},
	})
}