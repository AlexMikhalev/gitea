// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package cron

import (
	"context"
	"time"

	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/modules/graceful"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/services/graph"
)

// IssueGraphTask updates PageRank for all repositories
func IssueGraphTask(timeout time.Duration, gracefulCtx graceful.Context) error {
	if !setting.IssueGraph.Enabled {
		return nil
	}

	ctx, cancel := context.WithTimeout(gracefulCtx, timeout)
	defer cancel()

	log.Trace("Starting PageRank update task")

	// Get all repositories
	repos, err := repo_model.GetRepositoriesMap(ctx)
	if err != nil {
		return err
	}

	svc := graph.NewService()
	if !svc.IsEnabled() {
		return nil
	}

	for _, repo := range repos {
		select {
		case <-ctx.Done():
			log.Trace("PageRank update task cancelled")
			return ctx.Err()
		default:
		}

		log.Trace("Updating PageRank for repo %d", repo.ID)
		if err := svc.CalculatePageRank(ctx, repo.ID); err != nil {
			log.Error("Failed to update PageRank for repo %d: %v", repo.ID, err)
			continue
		}
	}

	log.Trace("Finished PageRank update task")
	return nil
}