// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package issues

import (
	"context"

	"code.gitea.io/gitea/models/db"
)

// GraphCache stores pre-computed PageRank and graph metrics for issues
type GraphCache struct {
	RepoID      int64   `xorm:"pk"`
	IssueID     int64   `xorm:"pk"`
	PageRank    float64 `xorm:"DEFAULT 0"`
	Centrality  float64 `xorm:"DEFAULT 0"`
	UpdatedUnix int64   `xorm:"updated"`
}

func init() {
	db.RegisterModel(new(GraphCache))
}

// GetPageRank returns the PageRank score for an issue
func GetPageRank(ctx context.Context, repoID, issueID int64) (float64, error) {
	cache := &GraphCache{}
	exists, err := db.GetEngine(ctx).Where("repo_id = ? AND issue_id = ?", repoID, issueID).Get(cache)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	return cache.PageRank, nil
}

// UpdatePageRank updates the PageRank score for an issue
func UpdatePageRank(ctx context.Context, repoID, issueID int64, pageRank float64) error {
	cache := &GraphCache{
		RepoID:   repoID,
		IssueID:  issueID,
		PageRank: pageRank,
	}
	_, err := db.GetEngine(ctx).Upsert(cache)
	return err
}

// CalculatePageRank computes PageRank for all issues in a repository
// Uses existing IssueDependency model from Gitea
func CalculatePageRank(ctx context.Context, repoID int64, dampingFactor float64, iterations int) error {
	// Get all dependencies for this repo
	deps := make([]*IssueDependency, 0)
	err := db.GetEngine(ctx).Find(&deps)
	if err != nil {
		return err
	}

	// Build issue set and adjacency list
	allIssues := make(map[int64]bool)
	// adj[depID] = list of issues that depend on it (blocked by it)
	adj := make(map[int64][]int64)

	for _, dep := range deps {
		allIssues[dep.IssueID] = true
		allIssues[dep.DependencyID] = true
		adj[dep.DependencyID] = append(adj[dep.DependencyID], dep.IssueID)
	}

	if len(allIssues) == 0 {
		return nil
	}

	// Initialize PageRank scores
	pageRanks := make(map[int64]float64)
	n := len(allIssues)
	for issueID := range allIssues {
		pageRanks[issueID] = 1.0 / float64(n)
	}

	// Power iteration
	for i := 0; i < iterations; i++ {
		newRanks := make(map[int64]float64)

		for issueID := range allIssues {
			newRank := (1.0 - dampingFactor) / float64(n)

			// Sum contributions from blockers (upstream)
			// Find all issues that block this one
			for _, dep := range deps {
				if dep.IssueID == issueID {
					blockerID := dep.DependencyID
					outDegree := len(adj[blockerID])
					if outDegree > 0 {
						newRank += dampingFactor * pageRanks[blockerID] / float64(outDegree)
					}
				}
			}

			newRanks[issueID] = newRank
		}
		pageRanks = newRanks
	}

	// Update cache
	for issueID, rank := range pageRanks {
		if err := UpdatePageRank(ctx, repoID, issueID, rank); err != nil {
			return err
		}
	}

	return nil
}