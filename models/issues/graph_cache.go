// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package issues

import (
	"context"
	"math"

	"code.gitea.io/gitea/models/db"
	"code.gitea.io/gitea/modules/timeutil"

	"xorm.io/builder"
)

// GraphCache stores pre-computed PageRank and graph metrics for issues
type GraphCache struct {
	RepoID      int64              `xorm:"pk"`
	IssueID     int64              `xorm:"pk"`
	PageRank    float64            `xorm:"DEFAULT 0"`
	Centrality  float64            `xorm:"DEFAULT 0"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated"`
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

// GetCentrality returns the centrality score for an issue
func GetCentrality(ctx context.Context, repoID, issueID int64) (float64, error) {
	cache := &GraphCache{}
	exists, err := db.GetEngine(ctx).Where("repo_id = ? AND issue_id = ?", repoID, issueID).Get(cache)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	return cache.Centrality, nil
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

// UpdateGraphCache updates both PageRank and centrality for an issue
func UpdateGraphCache(ctx context.Context, repoID, issueID int64, pageRank, centrality float64) error {
	cache := &GraphCache{
		RepoID:     repoID,
		IssueID:    issueID,
		PageRank:   pageRank,
		Centrality: centrality,
	}
	_, err := db.GetEngine(ctx).Upsert(cache)
	return err
}

// GetAllPageRanks returns all PageRank scores for a repository
func GetAllPageRanks(ctx context.Context, repoID int64) (map[int64]float64, error) {
	caches := make([]*GraphCache, 0)
	err := db.GetEngine(ctx).Where("repo_id = ?", repoID).Find(&caches)
	if err != nil {
		return nil, err
	}

	result := make(map[int64]float64)
	for _, cache := range caches {
		result[cache.IssueID] = cache.PageRank
	}
	return result, nil
}

// GetRankedIssues returns issues sorted by PageRank (descending)
func GetRankedIssues(ctx context.Context, repoID int64, limit int) ([]*GraphCache, error) {
	caches := make([]*GraphCache, 0)
	err := db.GetEngine(ctx).Where("repo_id = ?", repoID).
		OrderBy("page_rank DESC").
		Limit(limit).
		Find(&caches)
	return caches, err
}

// InvalidateGraphCache removes all cached graph data for a repository
func InvalidateGraphCache(ctx context.Context, repoID int64) error {
	_, err := db.GetEngine(ctx).Where("repo_id = ?", repoID).Delete(&GraphCache{})
	return err
}

// InvalidateIssueCache removes cached graph data for a specific issue
func InvalidateIssueCache(ctx context.Context, repoID, issueID int64) error {
	_, err := db.GetEngine(ctx).Where("repo_id = ? AND issue_id = ?", repoID, issueID).Delete(&GraphCache{})
	return err
}

// CalculatePageRank computes PageRank for all issues in a repository
// This is an incremental update - it only recalculates for issues that have changed
func CalculatePageRank(ctx context.Context, repoID int64, dampingFactor float64, iterations int) error {
	// Get all dependencies
	deps, err := GetDependencyGraph(ctx, repoID)
	if err != nil {
		return err
	}

	// Build adjacency list (only for "blocks" relationships)
	adj := make(map[int64][]int64)
	allIssues := make(map[int64]bool)

	for _, dep := range deps {
		if dep.DepType == DepTypeBlocks {
			adj[dep.DependsOn] = append(adj[dep.DependsOn], dep.IssueID)
			allIssues[dep.IssueID] = true
			allIssues[dep.DependsOn] = true
		}
	}

	if len(allIssues) == 0 {
		return nil
	}

	// Initialize PageRank scores
	pageRanks := make(map[int64]float64)
	for issueID := range allIssues {
		pageRanks[issueID] = 1.0 / float64(len(allIssues))
	}

	// Power iteration
	for i := 0; i < iterations; i++ {
		newRanks := make(map[int64]float64)

		for issueID := range allIssues {
			newRank := (1.0 - dampingFactor) / float64(len(allIssues))

			// Sum contributions from incoming edges
			for _, dep := range deps {
				if dep.DepType == DepTypeBlocks && dep.IssueID == issueID {
					blockerID := dep.DependsOn
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

// CalculateCentrality computes betweenness centrality for all issues
// This is a simplified version - full calculation is expensive
func CalculateCentrality(ctx context.Context, repoID int64) error {
	// Get all dependencies
	deps, err := GetDependencyGraph(ctx, repoID)
	if err != nil {
		return err
	}

	// Build adjacency list
	adj := make(map[int64][]int64)
	allIssues := make(map[int64]bool)

	for _, dep := range deps {
		if dep.DepType == DepTypeBlocks {
			adj[dep.IssueID] = append(adj[dep.IssueID], dep.DependsOn)
			allIssues[dep.IssueID] = true
			allIssues[dep.DependsOn] = true
		}
	}

	// Calculate degree centrality (simplified)
	for issueID := range allIssues {
		inDegree := 0
		outDegree := len(adj[issueID])

		for _, dep := range deps {
			if dep.DepType == DepTypeBlocks && dep.DependsOn == issueID {
				inDegree++
			}
		}

		// Simple centrality = in-degree + out-degree
		centrality := float64(inDegree + outDegree)

		// Get current PageRank
		pageRank, _ := GetPageRank(ctx, repoID, issueID)

		if err := UpdateGraphCache(ctx, repoID, issueID, pageRank, centrality); err != nil {
			return err
		}
	}

	return nil
}

// GetCriticalPath returns the longest dependency chain starting from an issue
func GetCriticalPath(ctx context.Context, repoID, issueID int64) ([]int64, error) {
	// DFS to find longest path
	visited := make(map[int64]bool)
	path := make([]int64, 0)
	longestPath := make([]int64, 0)

	var dfs func(current int64) error
	dfs = func(current int64) error {
		if visited[current] {
			return nil
		}
		visited[current] = true
		path = append(path, current)

		if len(path) > len(longestPath) {
			longestPath = make([]int64, len(path))
			copy(longestPath, path)
		}

		// Get issues that depend on current
		deps, err := GetDependents(ctx, repoID, current)
		if err != nil {
			return err
		}

		for _, dep := range deps {
			if dep.DepType == DepTypeBlocks {
				if err := dfs(dep.IssueID); err != nil {
					return err
				}
			}
		}

		path = path[:len(path)-1]
		visited[current] = false
		return nil
	}

	if err := dfs(issueID); err != nil {
		return nil, err
	}

	return longestPath, nil
}

// GetReadyIssues returns issues that have no open blockers
func GetReadyIssues(ctx context.Context, repoID int64) ([]int64, error) {
	// Get all open issues
	issues := make([]*Issue, 0)
	err := db.GetEngine(ctx).Where("repo_id = ? AND is_closed = ?", repoID, false).Find(&issues)
	if err != nil {
		return nil, err
	}

	ready := make([]int64, 0)
	for _, issue := range issues {
		blocked, err := IsBlocked(ctx, repoID, issue.ID)
		if err != nil {
			return nil, err
		}
		if !blocked {
			ready = append(ready, issue.ID)
		}
	}

	return ready, nil
}

// GetGraphMetrics returns summary metrics for the dependency graph
func GetGraphMetrics(ctx context.Context, repoID int64) (map[string]interface{}, error) {
	metrics := make(map[string]interface{})

	// Count dependencies
	depCount, err := db.GetEngine(ctx).Where("repo_id = ?", repoID).Count(&IssueDependency{})
	if err != nil {
		return nil, err
	}
	metrics["dependency_count"] = depCount

	// Count cached PageRanks
	cacheCount, err := db.GetEngine(ctx).Where("repo_id = ?", repoID).Count(&GraphCache{})
	if err != nil {
		return nil, err
	}
	metrics["cached_issues"] = cacheCount

	// Check for cycles
	hasCycle, err := DetectCycle(ctx, repoID)
	if err != nil {
		return nil, err
	}
	metrics["has_cycle"] = hasCycle

	// Average PageRank
	caches := make([]*GraphCache, 0)
	err = db.GetEngine(ctx).Where("repo_id = ?", repoID).Find(&caches)
	if err != nil {
		return nil, err
	}

	if len(caches) > 0 {
		total := 0.0
		for _, cache := range caches {
			total += cache.PageRank
		}
		metrics["avg_pagerank"] = total / float64(len(caches))
		metrics["max_pagerank"] = caches[0].PageRank // Already sorted by PageRank
	} else {
		metrics["avg_pagerank"] = 0.0
		metrics["max_pagerank"] = 0.0
	}

	return metrics, nil
}