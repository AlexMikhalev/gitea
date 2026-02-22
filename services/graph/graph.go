// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package graph

import (
	"context"

	issues_model "code.gitea.io/gitea/models/issues"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/setting"
)

// Service provides graph analytics functionality
type Service struct {
	dampingFactor float64
	iterations    int
	enabled       bool
}

// NewService creates a new graph service
func NewService() *Service {
	return &Service{
		dampingFactor: setting.IssueGraph.DampingFactor,
		iterations:    setting.IssueGraph.Iterations,
		enabled:       setting.IssueGraph.Enabled,
	}
}

// IsEnabled returns whether the graph service is enabled
func (s *Service) IsEnabled() bool {
	return s.enabled
}

// CalculatePageRank calculates PageRank for all issues in a repository
func (s *Service) CalculatePageRank(ctx context.Context, repoID int64) error {
	if !s.enabled {
		return nil
	}

	log.Trace("Calculating PageRank for repo %d", repoID)

	if err := issues_model.CalculatePageRank(ctx, repoID, s.dampingFactor, s.iterations); err != nil {
		log.Error("Failed to calculate PageRank for repo %d: %v", repoID, err)
		return err
	}

	// Also calculate centrality
	if err := issues_model.CalculateCentrality(ctx, repoID); err != nil {
		log.Error("Failed to calculate centrality for repo %d: %v", repoID, err)
		return err
	}

	log.Trace("Finished calculating PageRank for repo %d", repoID)
	return nil
}

// InvalidateCache invalidates the graph cache for a repository
func (s *Service) InvalidateCache(ctx context.Context, repoID int64) error {
	if !s.enabled {
		return nil
	}

	log.Trace("Invalidating graph cache for repo %d", repoID)
	return issues_model.InvalidateGraphCache(ctx, repoID)
}

// InvalidateIssueCache invalidates the graph cache for a specific issue
func (s *Service) InvalidateIssueCache(ctx context.Context, repoID, issueID int64) error {
	if !s.enabled {
		return nil
	}

	log.Trace("Invalidating graph cache for issue %d in repo %d", issueID, repoID)
	return issues_model.InvalidateIssueCache(ctx, repoID, issueID)
}

// DetectCycle checks if the dependency graph has any cycles
func (s *Service) DetectCycle(ctx context.Context, repoID int64) (bool, error) {
	if !s.enabled {
		return false, nil
	}

	return issues_model.DetectCycle(ctx, repoID)
}

// GetMetrics returns graph metrics for a repository
func (s *Service) GetMetrics(ctx context.Context, repoID int64) (map[string]interface{}, error) {
	if !s.enabled {
		return map[string]interface{}{
			"enabled": false,
		}, nil
	}

	return issues_model.GetGraphMetrics(ctx, repoID)
}