// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package robot

import (
	"context"
	"sort"

	issues_model "code.gitea.io/gitea/models/issues"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/setting"
)

// Service provides agent-optimized API functionality
type Service struct {
	enabled bool
}

// NewService creates a new robot service
func NewService() *Service {
	return &Service{
		enabled: setting.IssueGraph.Enabled,
	}
}

// IsEnabled returns whether the robot service is enabled
func (s *Service) IsEnabled() bool {
	return s.enabled
}

// TriageResponse represents the response for the triage endpoint
type TriageResponse struct {
	QuickRef         QuickRef           `json:"quick_ref"`
	Recommendations  []Recommendation   `json:"recommendations"`
	BlockersToClear  []BlockerInfo      `json:"blockers_to_clear"`
	ProjectHealth    ProjectHealth      `json:"project_health"`
}

// QuickRef provides at-a-glance counts
type QuickRef struct {
	Total   int64 `json:"total"`
	Open    int64 `json:"open"`
	Blocked int64 `json:"blocked"`
	Ready   int64 `json:"ready"`
}

// Recommendation represents a recommended issue to work on
type Recommendation struct {
	ID          int64    `json:"id"`
	Index       int64    `json:"index"`
	Title       string   `json:"title"`
	PageRank    float64  `json:"pagerank"`
	Centrality  float64  `json:"centrality"`
	Unblocks    []int64  `json:"unblocks"`
	Priority    int      `json:"priority"`
	Status      string   `json:"status"`
	ClaimCommand string  `json:"claim_command"`
}

// BlockerInfo represents an issue that blocks many others
type BlockerInfo struct {
	ID           int64   `json:"id"`
	Index        int64   `json:"index"`
	Title        string  `json:"title"`
	BlocksCount  int     `json:"blocks_count"`
	PageRank     float64 `json:"pagerank"`
}

// ProjectHealth represents overall project health metrics
type ProjectHealth struct {
	CycleDetected bool    `json:"cycle_detected"`
	AvgPageRank   float64 `json:"avg_pagerank"`
	MaxPageRank   float64 `json:"max_pagerank"`
	DepCount      int64   `json:"dependency_count"`
}

// Triage returns prioritized list of issues for agents
func (s *Service) Triage(ctx context.Context, repoID int64) (*TriageResponse, error) {
	if !s.enabled {
		return &TriageResponse{
			QuickRef: QuickRef{},
			Recommendations: []Recommendation{},
			BlockersToClear: []BlockerInfo{},
			ProjectHealth: ProjectHealth{
				CycleDetected: false,
				AvgPageRank:   0,
				MaxPageRank:   0,
				DepCount:      0,
			},
		}, nil
	}

	log.Trace("Generating triage report for repo %d", repoID)

	response := &TriageResponse{}

	// Get quick ref counts
	quickRef, err := s.getQuickRef(ctx, repoID)
	if err != nil {
		return nil, err
	}
	response.QuickRef = *quickRef

	// Get recommendations
	recommendations, err := s.getRecommendations(ctx, repoID)
	if err != nil {
		return nil, err
	}
	response.Recommendations = recommendations

	// Get blockers to clear
	blockers, err := s.getBlockersToClear(ctx, repoID)
	if err != nil {
		return nil, err
	}
	response.BlockersToClear = blockers

	// Get project health
	health, err := s.getProjectHealth(ctx, repoID)
	if err != nil {
		return nil, err
	}
	response.ProjectHealth = *health

	return response, nil
}

// getQuickRef gets at-a-glance counts for the repository
func (s *Service) getQuickRef(ctx context.Context, repoID int64) (*QuickRef, error) {
	// This would need to be implemented with actual issue counts
	// For now, return placeholder
	return &QuickRef{
		Total:   0,
		Open:    0,
		Blocked: 0,
		Ready:   0,
	}, nil
}

// getRecommendations gets prioritized list of issues to work on
func (s *Service) getRecommendations(ctx context.Context, repoID int64) ([]Recommendation, error) {
	// Get ready issues (no open blockers)
	readyIssueIDs, err := issues_model.GetReadyIssues(ctx, repoID)
	if err != nil {
		return nil, err
	}

	if len(readyIssueIDs) == 0 {
		return []Recommendation{}, nil
	}

	// Get PageRank scores
	pageRanks, err := issues_model.GetAllPageRanks(ctx, repoID)
	if err != nil {
		return nil, err
	}

	// Build recommendations
	recommendations := make([]Recommendation, 0, len(readyIssueIDs))
	for _, issueID := range readyIssueIDs {
		// Get issue details
		issue, err := issues_model.GetIssueByID(ctx, issueID)
		if err != nil {
			log.Warn("Failed to get issue %d: %v", issueID, err)
			continue
		}

		// Get what this issue unblocks
		dependents, err := issues_model.GetDependents(ctx, repoID, issueID)
		if err != nil {
			log.Warn("Failed to get dependents for issue %d: %v", issueID, err)
			continue
		}

		unblocks := make([]int64, 0)
		for _, dep := range dependents {
			if dep.DepType == issues_model.DepTypeBlocks {
				unblocks = append(unblocks, dep.IssueID)
			}
		}

		rec := Recommendation{
			ID:           issue.ID,
			Index:        issue.Index,
			Title:        issue.Title,
			PageRank:     pageRanks[issueID],
			Unblocks:     unblocks,
			Priority:     issue.Priority,
			Status:       "open",
			ClaimCommand: s.getClaimCommand(issue.Index),
		}

		recommendations = append(recommendations, rec)
	}

	// Sort by PageRank (descending)
	sort.Slice(recommendations, func(i, j int) bool {
		return recommendations[i].PageRank > recommendations[j].PageRank
	})

	// Limit to top 10
	if len(recommendations) > 10 {
		recommendations = recommendations[:10]
	}

	return recommendations, nil
}

// getBlockersToClear gets issues that block many others
func (s *Service) getBlockersToClear(ctx context.Context, repoID int64) ([]BlockerInfo, error) {
	// Get all dependencies
	deps, err := issues_model.GetDependencyGraph(ctx, repoID)
	if err != nil {
		return nil, err
	}

	// Count how many issues each issue blocks
	blockCounts := make(map[int64]int)
	for _, dep := range deps {
		if dep.DepType == issues_model.DepTypeBlocks {
			blockCounts[dep.DependsOn]++
		}
	}

	// Get PageRank scores
	pageRanks, err := issues_model.GetAllPageRanks(ctx, repoID)
	if err != nil {
		return nil, err
	}

	// Build blocker list
	blockers := make([]BlockerInfo, 0)
	for issueID, count := range blockCounts {
		if count > 0 {
			issue, err := issues_model.GetIssueByID(ctx, issueID)
			if err != nil {
				continue
			}

			blockers = append(blockers, BlockerInfo{
				ID:          issue.ID,
				Index:       issue.Index,
				Title:       issue.Title,
				BlocksCount: count,
				PageRank:    pageRanks[issueID],
			})
		}
	}

	// Sort by blocks count (descending)
	sort.Slice(blockers, func(i, j int) bool {
		return blockers[i].BlocksCount > blockers[j].BlocksCount
	})

	// Limit to top 5
	if len(blockers) > 5 {
		blockers = blockers[:5]
	}

	return blockers, nil
}

// getProjectHealth gets overall project health metrics
func (s *Service) getProjectHealth(ctx context.Context, repoID int64) (*ProjectHealth, error) {
	metrics, err := issues_model.GetGraphMetrics(ctx, repoID)
	if err != nil {
		return nil, err
	}

	return &ProjectHealth{
		CycleDetected: metrics["has_cycle"].(bool),
		AvgPageRank:   metrics["avg_pagerank"].(float64),
		MaxPageRank:   metrics["max_pagerank"].(float64),
		DepCount:      metrics["dependency_count"].(int64),
	}, nil
}

// getClaimCommand returns the command to claim an issue
func (s *Service) getClaimCommand(issueIndex int64) string {
	return "git claim " + string(rune(issueIndex))
}

// ReadyResponse represents the response for the ready endpoint
type ReadyResponse struct {
	Issues []ReadyIssue `json:"issues"`
}

// ReadyIssue represents an issue that is ready to work on
type ReadyIssue struct {
	ID       int64   `json:"id"`
	Index    int64   `json:"index"`
	Title    string  `json:"title"`
	PageRank float64 `json:"pagerank"`
}

// Ready returns issues with no open blockers
func (s *Service) Ready(ctx context.Context, repoID int64) (*ReadyResponse, error) {
	if !s.enabled {
		return &ReadyResponse{Issues: []ReadyIssue{}}, nil
	}

	readyIssueIDs, err := issues_model.GetReadyIssues(ctx, repoID)
	if err != nil {
		return nil, err
	}

	pageRanks, err := issues_model.GetAllPageRanks(ctx, repoID)
	if err != nil {
		return nil, err
	}

	issues := make([]ReadyIssue, 0, len(readyIssueIDs))
	for _, issueID := range readyIssueIDs {
		issue, err := issues_model.GetIssueByID(ctx, issueID)
		if err != nil {
			continue
		}

		issues = append(issues, ReadyIssue{
			ID:       issue.ID,
			Index:    issue.Index,
			Title:    issue.Title,
			PageRank: pageRanks[issueID],
		})
	}

	// Sort by PageRank
	sort.Slice(issues, func(i, j int) bool {
		return issues[i].PageRank > issues[j].PageRank
	})

	return &ReadyResponse{Issues: issues}, nil
}

// GraphResponse represents the dependency graph
type GraphResponse struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

// GraphNode represents a node in the graph
type GraphNode struct {
	ID       int64   `json:"id"`
	Index    int64   `json:"index"`
	Title    string  `json:"title"`
	PageRank float64 `json:"pagerank"`
	Status   string  `json:"status"`
}

// GraphEdge represents an edge in the graph
type GraphEdge struct {
	Source int64  `json:"source"`
	Target int64  `json:"target"`
	Type   string `json:"type"`
}

// Graph returns the dependency graph for visualization
func (s *Service) Graph(ctx context.Context, repoID int64) (*GraphResponse, error) {
	if !s.enabled {
		return &GraphResponse{Nodes: []GraphNode{}, Edges: []GraphEdge{}}, nil
	}

	// Get all dependencies
	deps, err := issues_model.GetDependencyGraph(ctx, repoID)
	if err != nil {
		return nil, err
	}

	// Get PageRank scores
	pageRanks, err := issues_model.GetAllPageRanks(ctx, repoID)
	if err != nil {
		return nil, err
	}

	// Build node list
	nodeMap := make(map[int64]GraphNode)
	for _, dep := range deps {
		if _, ok := nodeMap[dep.IssueID]; !ok {
			issue, err := issues_model.GetIssueByID(ctx, dep.IssueID)
			if err != nil {
				continue
			}
			status := "open"
			if issue.IsClosed {
				status = "closed"
			}
			nodeMap[dep.IssueID] = GraphNode{
				ID:       issue.ID,
				Index:    issue.Index,
				Title:    issue.Title,
				PageRank: pageRanks[dep.IssueID],
				Status:   status,
			}
		}
		if _, ok := nodeMap[dep.DependsOn]; !ok {
			issue, err := issues_model.GetIssueByID(ctx, dep.DependsOn)
			if err != nil {
				continue
			}
			status := "open"
			if issue.IsClosed {
				status = "closed"
			}
			nodeMap[dep.DependsOn] = GraphNode{
				ID:       issue.ID,
				Index:    issue.Index,
				Title:    issue.Title,
				PageRank: pageRanks[dep.DependsOn],
				Status:   status,
			}
		}
	}

	nodes := make([]GraphNode, 0, len(nodeMap))
	for _, node := range nodeMap {
		nodes = append(nodes, node)
	}

	// Build edge list
	edges := make([]GraphEdge, 0, len(deps))
	for _, dep := range deps {
		edges = append(edges, GraphEdge{
			Source: dep.DependsOn,
			Target: dep.IssueID,
			Type:   string(dep.DepType),
		})
	}

	return &GraphResponse{
		Nodes: nodes,
		Edges: edges,
	}, nil
}