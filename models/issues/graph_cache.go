// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package issues

import (
	"context"
	"time"

	"code.gitea.io/gitea/models/db"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/optional"
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
	// Try update first, then insert
	affected, err := db.GetEngine(ctx).Where("repo_id = ? AND issue_id = ?", repoID, issueID).Cols("page_rank", "updated_unix").Update(cache)
	if err != nil {
		return err
	}
	if affected == 0 {
		_, err = db.GetEngine(ctx).Insert(cache)
	}
	return err
}

// DependencyWithRepo joins IssueDependency with Issue to get repo information
type DependencyWithRepo struct {
	IssueID      int64 `xorm:"issue_dependency.issue_id"`
	DependencyID int64 `xorm:"issue_dependency.dependency_id"`
	IsClosed     bool  `xorm:"issue.is_closed"`
}

// CalculatePageRank computes PageRank for all issues in a repository
// Uses existing IssueDependency model from Gitea
// Excludes closed issues from the graph (per specification interview)
func CalculatePageRank(ctx context.Context, repoID int64, dampingFactor float64, iterations int) error {
	startTime := time.Now()

	// Get all dependencies for this repo, joined with issue info to filter by repoID and closed status
	// This query filters by repoID and excludes closed issues
	var deps []DependencyWithRepo
	err := db.GetEngine(ctx).
		Table("issue_dependency").
		Join("INNER", "issue", "issue.id = issue_dependency.issue_id AND issue.repo_id = ?", repoID).
		Where("issue.is_closed = ?", false).
		Find(&deps)
	if err != nil {
		return err
	}

	// Also get dependencies where the issue is the dependency (blocked by)
	var deps2 []DependencyWithRepo
	err = db.GetEngine(ctx).
		Table("issue_dependency").
		Join("INNER", "issue", "issue.id = issue_dependency.dependency_id AND issue.repo_id = ?", repoID).
		Where("issue.is_closed = ?", false).
		Find(&deps2)
	if err != nil {
		return err
	}

	// Merge both dependency lists
	deps = append(deps, deps2...)

	if len(deps) == 0 {
		log.Info("PageRank: No dependencies found for repo %d", repoID)
		return nil
	}

	// Build issue set and adjacency list
	// Track which issues actually exist (not orphans)
	validIssues := make(map[int64]bool)
	// adj[depID] = list of issues that depend on it (blocked by it)
	adj := make(map[int64][]int64)

	for _, dep := range deps {
		// Skip orphan references - if issue doesn't exist in deps, it's an orphan
		validIssues[dep.IssueID] = true
		validIssues[dep.DependencyID] = true
		adj[dep.DependencyID] = append(adj[dep.DependencyID], dep.IssueID)
	}

	issueCount := len(validIssues)
	if issueCount == 0 {
		log.Info("PageRank: No valid issues for repo %d", repoID)
		return nil
	}

	// Initialize PageRank scores
	pageRanks := make(map[int64]float64)
	for issueID := range validIssues {
		pageRanks[issueID] = 1.0 / float64(issueCount)
	}

	// Power iteration
	for i := 0; i < iterations; i++ {
		newRanks := make(map[int64]float64)

		for issueID := range validIssues {
			newRank := (1.0 - dampingFactor) / float64(issueCount)

			// Sum contributions from blockers (upstream)
			// Find all issues that block this one
			for _, dep := range deps {
				if dep.IssueID == issueID {
					blockerID := dep.DependencyID
					// Skip if blocker doesn't have valid PageRank
					if currentRank, ok := pageRanks[blockerID]; ok {
						outDegree := len(adj[blockerID])
						if outDegree > 0 {
							newRank += dampingFactor * currentRank / float64(outDegree)
						}
					}
				}
			}

			newRanks[issueID] = newRank
		}
		pageRanks = newRanks
	}

	// Update cache - log errors but continue with remaining issues
	// (per specification interview: partial failure returns partial results)
	successCount := 0
	errorCount := 0
	for issueID, rank := range pageRanks {
		if err := UpdatePageRank(ctx, repoID, issueID, rank); err != nil {
			log.Error("Failed to update PageRank for issue %d in repo %d: %v", issueID, repoID, err)
			errorCount++
		} else {
			successCount++
		}
	}

	elapsed := time.Since(startTime)
	log.Info("PageRank calculated for repo %d: %d issues, %d dependencies, %d successes, %d errors, took %v",
		repoID, issueCount, len(deps), successCount, errorCount, elapsed)

	return nil
}

// EnsureRepoPageRankComputed ensures PageRank is computed for a repository
// Calculates if not already cached, otherwise returns cached data
func EnsureRepoPageRankComputed(ctx context.Context, repoID int64, dampingFactor float64, iterations int) error {
	// Check if we have cached PageRank data for this repo
	hasCache, err := hasPageRankCache(ctx, repoID)
	if err != nil {
		return err
	}

	if !hasCache {
		// Calculate PageRank - lazy calculation per spec interview
		return CalculatePageRank(ctx, repoID, dampingFactor, iterations)
	}

	return nil
}

// hasPageRankCache checks if PageRank cache exists for a repository
func hasPageRankCache(ctx context.Context, repoID int64) (bool, error) {
	count, err := db.GetEngine(ctx).Where("repo_id = ?", repoID).Count(&GraphCache{})
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// GetRankedIssues returns issues sorted by PageRank score
// Hybrid approach: issues with dependencies get calculated PageRank,
// issues without get baseline score (1-damping)
func GetRankedIssues(ctx context.Context, repoID int64, limit int) ([]*Issue, error) {
	// Get all open issues for the repo using the Issues function
	issues, err := Issues(ctx, &IssuesOptions{
		RepoIDs:  []int64{repoID},
		IsClosed: optional.Some(false),
		IsPull:   optional.Some(false),
	})
	if err != nil {
		return nil, err
	}

	if len(issues) == 0 {
		return issues, nil
	}

	// Get PageRank scores from cache
	pageRanks := make(map[int64]float64)
	for _, issue := range issues {
		// Get cached PageRank or use baseline
		pageRank, err := GetPageRank(ctx, repoID, issue.ID)
		if err != nil {
			log.Warn("Failed to get PageRank for issue %d: %v", issue.ID, err)
			continue
		}
		if pageRank > 0 {
			pageRanks[issue.ID] = pageRank
		}
		// If pageRank is 0, it will get baseline score below
	}

	// Baseline score for issues without PageRank
	baseline := 1.0 - 0.85 // (1 - dampingFactor)

	// Sort issues by PageRank (descending)
	// Issues without PageRank get baseline score at the end
	sortByPageRank(issues, pageRanks, baseline)

	if limit > 0 && len(issues) > limit {
		issues = issues[:limit]
	}

	return issues, nil
}

// sortByPageRank sorts issues by PageRank in descending order
func sortByPageRank(issues []*Issue, pageRanks map[int64]float64, baseline float64) {
	for i := 0; i < len(issues)-1; i++ {
		for j := i + 1; j < len(issues); j++ {
			rankI := pageRanks[issues[i].ID]
			if rankI == 0 {
				rankI = baseline
			}
			rankJ := pageRanks[issues[j].ID]
			if rankJ == 0 {
				rankJ = baseline
			}
			if rankI < rankJ {
				issues[i], issues[j] = issues[j], issues[i]
			}
		}
	}
}

// InvalidateCache clears PageRank cache for a repository
func InvalidateCache(ctx context.Context, repoID int64) error {
	_, err := db.GetEngine(ctx).Where("repo_id = ?", repoID).Delete(&GraphCache{})
	if err != nil {
		log.Error("Failed to invalidate PageRank cache for repo %d: %v", repoID, err)
		return err
	}
	log.Info("PageRank cache invalidated for repo %d", repoID)
	return nil
}

// GetPageRanksForRepo returns all PageRank scores for a repository
func GetPageRanksForRepo(ctx context.Context, repoID int64) (map[int64]float64, error) {
	caches := make([]*GraphCache, 0)
	err := db.GetEngine(ctx).Where("repo_id = ?", repoID).Find(&caches)
	if err != nil {
		return nil, err
	}

	ranks := make(map[int64]float64)
	for _, cache := range caches {
		ranks[cache.IssueID] = cache.PageRank
	}
	return ranks, nil
}