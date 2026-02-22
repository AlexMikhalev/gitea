// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package issues

import (
	"context"
)

// DetectCycle checks if the dependency graph contains any cycles
func DetectCycle(ctx context.Context, repoID int64) (bool, error) {
	// Get all dependencies
	deps, err := GetDependencyGraph(ctx, repoID)
	if err != nil {
		return false, err
	}

	// Build adjacency list (only for "blocks" relationships)
	adj := make(map[int64][]int64)
	allIssues := make(map[int64]bool)

	for _, dep := range deps {
		if dep.DepType == DepTypeBlocks {
			adj[dep.IssueID] = append(adj[dep.IssueID], dep.DependsOn)
			allIssues[dep.IssueID] = true
			allIssues[dep.DependsOn] = true
		}
	}

	// DFS-based cycle detection
	visited := make(map[int64]bool)
	recStack := make(map[int64]bool)

	for issueID := range allIssues {
		if !visited[issueID] {
			if hasCycle, err := dfsDetectCycle(adj, issueID, visited, recStack); err != nil {
				return false, err
			} else if hasCycle {
				return true, nil
			}
		}
	}

	return false, nil
}

// dfsDetectCycle performs DFS to detect cycles
func dfsDetectCycle(adj map[int64][]int64, node int64, visited, recStack map[int64]bool) (bool, error) {
	visited[node] = true
	recStack[node] = true

	for _, neighbor := range adj[node] {
		if !visited[neighbor] {
			if hasCycle, err := dfsDetectCycle(adj, neighbor, visited, recStack); err != nil {
				return false, err
			} else if hasCycle {
				return true, nil
			}
		} else if recStack[neighbor] {
			return true, nil
		}
	}

	recStack[node] = false
	return false, nil
}

// GetCyclePath returns the nodes involved in a cycle (if any)
func GetCyclePath(ctx context.Context, repoID int64) ([]int64, error) {
	// Get all dependencies
	deps, err := GetDependencyGraph(ctx, repoID)
	if err != nil {
		return nil, err
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

	// Find cycle using DFS with path tracking
	visited := make(map[int64]bool)
	recStack := make(map[int64]bool)
	path := make([]int64, 0)

	for issueID := range allIssues {
		if !visited[issueID] {
			if cycle, err := dfsFindCycle(adj, issueID, visited, recStack, path); err != nil {
				return nil, err
			} else if len(cycle) > 0 {
				return cycle, nil
			}
		}
	}

	return nil, nil
}

// dfsFindCycle performs DFS and returns the cycle path if found
func dfsFindCycle(adj map[int64][]int64, node int64, visited, recStack map[int64]bool, path []int64) ([]int64, error) {
	visited[node] = true
	recStack[node] = true
	path = append(path, node)

	for _, neighbor := range adj[node] {
		if !visited[neighbor] {
			if cycle, err := dfsFindCycle(adj, neighbor, visited, recStack, path); err != nil {
				return nil, err
			} else if len(cycle) > 0 {
				return cycle, nil
			}
		} else if recStack[neighbor] {
			// Found cycle - extract the cycle from path
			cycle := make([]int64, 0)
			for i := len(path) - 1; i >= 0; i-- {
				cycle = append(cycle, path[i])
				if path[i] == neighbor {
					break
				}
			}
			// Reverse to get correct order
			for i, j := 0, len(cycle)-1; i < j; i, j = i+1, j-1 {
				cycle[i], cycle[j] = cycle[j], cycle[i]
			}
			return cycle, nil
		}
	}

	path = path[:len(path)-1]
	recStack[node] = false
	return nil, nil
}