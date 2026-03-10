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
		enabled: setting.IssueGraphSettings.Enabled,
	}
}

// IsEnabled returns whether the robot service is enabled
func (s *Service) IsEnabled() bool {
	return s.enabled
}

// TriageResponse represents the response for the triage endpoint
type TriageResponse struct {
	QuickRef        QuickRef         `json:"quick_ref"`
	Recommendations []Recommendation `json:"recommendations"`
	ProjectHealth   ProjectHealth    `json:"project_health"`
}

// QuickRef provides at-a-glance counts
type QuickRef struct {
	Total int64 `json:"total"`
	Open  int64 `json:"open"`
}

// Recommendation represents a recommended issue to work on
type Recommendation struct {
	ID       int64   `json:"id"`
	Index    int64   `json:"index"`
	Title    string  `json:"title"`
	PageRank float64 `json:"pagerank"`
	Status   string  `json:"status"`
}

// ProjectHealth represents overall project health metrics
type ProjectHealth struct {
	AvgPageRank float64 `json:"avg_pagerank"`
	MaxPageRank float64 `json:"max_pagerank"`
}

// Triage returns prioritized list of issues for agents
func (s *Service) Triage(ctx context.Context, repoID int64) (*TriageResponse, error) {
	if !s.enabled {
		return &TriageResponse{
			QuickRef:        QuickRef{},
			Recommendations: []Recommendation{},
			ProjectHealth:   ProjectHealth{},
		}, nil
	}

	log.Trace("Generating triage report for repo %d", repoID)

	// Calculate PageRank first
	if err := issues_model.CalculatePageRank(ctx, repoID, 0.85, 100); err != nil {
		return nil, err
	}

	// Get all issues for repo using IssuesOptions
	opts := &issues_model.IssuesOptions{
		RepoIDs: []int64{repoID},
	}
	issues, err := issues_model.Issues(ctx, opts)
	if err != nil {
		return nil, err
	}

	response := &TriageResponse{}

	// Quick ref
	response.QuickRef.Total = int64(len(issues))
	for _, issue := range issues {
		if !issue.IsClosed {
			response.QuickRef.Open++
		}
	}

	// Build recommendations with PageRank
	recommendations := make([]Recommendation, 0)
	for _, issue := range issues {
		if issue.IsClosed {
			continue
		}

		rank, _ := issues_model.GetPageRank(ctx, repoID, issue.ID)

		recommendations = append(recommendations, Recommendation{
			ID:       issue.ID,
			Index:    issue.Index,
			Title:    issue.Title,
			PageRank: rank,
			Status:   "open",
		})
	}

	// Sort by PageRank (descending)
	sort.Slice(recommendations, func(i, j int) bool {
		return recommendations[i].PageRank > recommendations[j].PageRank
	})

	// Limit to top 10
	if len(recommendations) > 10 {
		recommendations = recommendations[:10]
	}
	response.Recommendations = recommendations

	// Project health
	if len(recommendations) > 0 {
		total := 0.0
		maxRank := 0.0
		for _, rec := range recommendations {
			total += rec.PageRank
			if rec.PageRank > maxRank {
				maxRank = rec.PageRank
			}
		}
		response.ProjectHealth.AvgPageRank = total / float64(len(recommendations))
		response.ProjectHealth.MaxPageRank = maxRank
	}

	return response, nil
}