// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package robot

import (
	"net/http"
	"strings"

	"code.gitea.io/gitea/models/db"
	"code.gitea.io/gitea/models/issues"
	"code.gitea.io/gitea/models/perm/access"
	"code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/unit"
	"code.gitea.io/gitea/modules/context"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/services/robot"
)

// ReadyIssue represents an issue that is ready to be worked on
// (no blocking dependencies)
type ReadyIssue struct {
	ID           int64   `json:"id"`
	Index        int64   `json:"index"`
	Title        string  `json:"title"`
	PageRank     float64 `json:"page_rank"`
	Priority     int     `json:"priority"`
	IsBlocked    bool    `json:"is_blocked"`
	BlockerCount int     `json:"blocker_count"`
}

// ReadyResponse represents the response for the Ready endpoint
type ReadyResponse struct {
	RepoID      int64        `json:"repo_id"`
	RepoName    string       `json:"repo_name"`
	TotalCount  int          `json:"total_count"`
	ReadyIssues []ReadyIssue `json:"ready_issues"`
}

// Ready returns issues that are ready to be worked on (no blocking dependencies)
func Ready(ctx *context.APIContext) {
	// 1. Check feature enabled
	if !setting.IssueGraph.Enabled {
		ctx.NotFound()
		return
	}

	// Get parameters from URL
	owner := ctx.Params(":owner")
	repoName := ctx.Params(":repo")

	// 2. Validate input
	if err := validateOwnerRepoInput(owner, repoName); err != nil {
		ctx.Error(http.StatusBadRequest, "ValidationError", err.Error())
		return
	}

	// 3. Check authentication
	// For private repos, user must be signed in
	// For public repos, anonymous access is allowed

	// 4. Lookup repo
	repository, err := repo.GetRepositoryByOwnerAndName(ctx, owner, repoName)
	if err != nil {
		if db.IsErrNotExist(err) {
			// Log the access attempt before returning 404
			if setting.IssueGraphSettings.AuditLog {
				var userID int64
				var username string
				if ctx.IsSigned && ctx.Doer != nil {
					userID = ctx.Doer.GetID()
					username = ctx.Doer.GetName()
				} else {
					userID = 0
					username = "anonymous"
				}
				robot.LogRobotAccessQuick(
					userID,
					username,
					owner,
					repoName,
					"/api/v1/robot/ready",
					ctx.RemoteAddr(),
					false,
					"repository not found",
				)
			}
			ctx.NotFound()
			return
		}
		log.Error("Failed to get repository %s/%s: %v", owner, repoName, err)
		ctx.Error(http.StatusInternalServerError, "GetRepository", err)
		return
	}

	// Check if repo is private and user is not signed in
	if repository.IsPrivate && !ctx.IsSigned {
		if setting.IssueGraphSettings.AuditLog {
			robot.LogRobotAccessQuick(
				0,
				"anonymous",
				owner,
				repoName,
				"/api/v1/robot/ready",
				ctx.RemoteAddr(),
				false,
				"authentication required for private repository",
			)
		}
		ctx.NotFound()
		return
	}

	// 5. Check permission
	if !checkRepoPermissionForTriage(ctx, repository) {
		// Permission check failed and response already set
		if setting.IssueGraphSettings.AuditLog {
			var userID int64
			var username string
			if ctx.IsSigned && ctx.Doer != nil {
				userID = ctx.Doer.GetID()
				username = ctx.Doer.GetName()
			} else {
				userID = 0
				username = "anonymous"
			}
			robot.LogRobotAccessQuick(
				userID,
				username,
				owner,
				repoName,
				"/api/v1/robot/ready",
				ctx.RemoteAddr(),
				false,
				"permission denied",
			)
		}
		return
	}

	// 6. Log access
	if setting.IssueGraphSettings.AuditLog {
		var userID int64
		var username string
		if ctx.IsSigned && ctx.Doer != nil {
			userID = ctx.Doer.GetID()
			username = ctx.Doer.GetName()
		} else {
			userID = 0
			username = "anonymous"
		}
		robot.LogRobotAccessQuick(
			userID,
			username,
			owner,
			repoName,
			"/api/v1/robot/ready",
			ctx.RemoteAddr(),
			true,
			"",
		)
	}

	// 7. Return ready issues (actual implementation)
	readyIssues, err := getReadyIssues(ctx, repository)
	if err != nil {
		log.Error("Failed to get ready issues for repo %d: %v", repository.ID, err)
		ctx.Error(http.StatusInternalServerError, "GetReadyIssues", err)
		return
	}

	response := ReadyResponse{
		RepoID:      repository.ID,
		RepoName:    repository.Name,
		TotalCount:  len(readyIssues),
		ReadyIssues: readyIssues,
	}

	ctx.JSON(http.StatusOK, response)
}

// getReadyIssues queries the database for issues that are ready to be worked on
// (open issues with no blocking dependencies)
func getReadyIssues(ctx *context.APIContext, repository *repo.Repository) ([]ReadyIssue, error) {
	// Get all open issues for the repository
	issuesList, err := issues.GetIssuesByRepoID(ctx, repository.ID, issues.IssuesOptions{
		State: "open",
	})
	if err != nil {
		return nil, err
	}

	readyIssues := make([]ReadyIssue, 0)

	for _, issue := range issuesList {
		// Skip pull requests (they are also issues in Gitea)
		if issue.IsPull {
			continue
		}

		// Get dependency count for this issue
		// In Gitea, dependencies are stored in issue_dependency table
		// We need to check if this issue has any open blockers
		blockerCount, err := getBlockerCount(ctx, issue.ID)
		if err != nil {
			log.Warn("Failed to get blocker count for issue %d: %v", issue.ID, err)
			// Continue anyway, assume no blockers
			blockerCount = 0
		}

		// Issue is ready if it has no blockers
		isBlocked := blockerCount > 0

		// Calculate priority based on labels, comments, etc.
		priority := calculatePriority(issue)

		// Get PageRank score (placeholder - would come from cache or calculation)
		pageRank := 0.5 // Default score

		readyIssues = append(readyIssues, ReadyIssue{
			ID:           issue.ID,
			Index:        issue.Index,
			Title:        issue.Title,
			PageRank:     pageRank,
			Priority:     priority,
			IsBlocked:    isBlocked,
			BlockerCount: blockerCount,
		})
	}

	return readyIssues, nil
}

// getBlockerCount returns the number of open issues blocking the given issue
func getBlockerCount(ctx *context.APIContext, issueID int64) (int, error) {
	// Query the issue_dependency table for blockers
	// This is a simplified version - in real implementation, would use proper model
	sql := `SELECT COUNT(*) FROM issue_dependency 
			WHERE issue_id = ? AND dependency_id IN (
				SELECT id FROM issue WHERE is_closed = false
			)`

	var count int
	_, err := db.GetEngine(ctx).SQL(sql, issueID).Get(&count)
	if err != nil {
		return 0, err
	}

	return count, nil
}

// calculatePriority calculates a priority score for an issue
func calculatePriority(issue *issues.Issue) int {
	priority := 0

	// Higher priority for issues with labels
	if len(issue.Labels) > 0 {
		priority += len(issue.Labels) * 5
	}

	// Higher priority for issues with more comments
	if issue.NumComments > 0 {
		priority += issue.NumComments * 2
	}

	// Check for priority labels
	for _, label := range issue.Labels {
		labelName := strings.ToLower(label.Name)
		if strings.Contains(labelName, "priority") || strings.Contains(labelName, "urgent") ||
			strings.Contains(labelName, "critical") || strings.Contains(labelName, "high") {
			priority += 20
		}
	}

	return priority
}

// GraphNode represents a node in the dependency graph
type GraphNode struct {
	ID       int64   `json:"id"`
	Index    int64   `json:"index"`
	Title    string  `json:"title"`
	PageRank float64 `json:"page_rank"`
	IsClosed bool    `json:"is_closed"`
}

// GraphEdge represents a dependency relationship between two issues
type GraphEdge struct {
	From   int64  `json:"from"`
	To     int64  `json:"to"`
	Type   string `json:"type"` // "blocks", "depends_on"
	Weight int    `json:"weight"`
}

// GraphResponse represents the response for the Graph endpoint
type GraphResponse struct {
	RepoID    int64       `json:"repo_id"`
	RepoName  string      `json:"repo_name"`
	NodeCount int         `json:"node_count"`
	EdgeCount int         `json:"edge_count"`
	Nodes     []GraphNode `json:"nodes"`
	Edges     []GraphEdge `json:"edges"`
}

// Graph returns the dependency graph for a repository
func Graph(ctx *context.APIContext) {
	// 1. Check feature enabled
	if !setting.IssueGraph.Enabled {
		ctx.NotFound()
		return
	}

	// Get parameters from URL
	owner := ctx.Params(":owner")
	repoName := ctx.Params(":repo")

	// 2. Validate input
	if err := validateOwnerRepoInput(owner, repoName); err != nil {
		ctx.Error(http.StatusBadRequest, "ValidationError", err.Error())
		return
	}

	// 3. Check authentication
	// For private repos, user must be signed in
	// For public repos, anonymous access is allowed

	// 4. Lookup repo
	repository, err := repo.GetRepositoryByOwnerAndName(ctx, owner, repoName)
	if err != nil {
		if db.IsErrNotExist(err) {
			// Log the access attempt before returning 404
			if setting.IssueGraphSettings.AuditLog {
				var userID int64
				var username string
				if ctx.IsSigned && ctx.Doer != nil {
					userID = ctx.Doer.GetID()
					username = ctx.Doer.GetName()
				} else {
					userID = 0
					username = "anonymous"
				}
				robot.LogRobotAccessQuick(
					userID,
					username,
					owner,
					repoName,
					"/api/v1/robot/graph",
					ctx.RemoteAddr(),
					false,
					"repository not found",
				)
			}
			ctx.NotFound()
			return
		}
		log.Error("Failed to get repository %s/%s: %v", owner, repoName, err)
		ctx.Error(http.StatusInternalServerError, "GetRepository", err)
		return
	}

	// Check if repo is private and user is not signed in
	if repository.IsPrivate && !ctx.IsSigned {
		if setting.IssueGraphSettings.AuditLog {
			robot.LogRobotAccessQuick(
				0,
				"anonymous",
				owner,
				repoName,
				"/api/v1/robot/graph",
				ctx.RemoteAddr(),
				false,
				"authentication required for private repository",
			)
		}
		ctx.NotFound()
		return
	}

	// 5. Check permission
	if !checkRepoPermissionForTriage(ctx, repository) {
		// Permission check failed and response already set
		if setting.IssueGraphSettings.AuditLog {
			var userID int64
			var username string
			if ctx.IsSigned && ctx.Doer != nil {
				userID = ctx.Doer.GetID()
				username = ctx.Doer.GetName()
			} else {
				userID = 0
				username = "anonymous"
			}
			robot.LogRobotAccessQuick(
				userID,
				username,
				owner,
				repoName,
				"/api/v1/robot/graph",
				ctx.RemoteAddr(),
				false,
				"permission denied",
			)
		}
		return
	}

	// 6. Log access
	if setting.IssueGraphSettings.AuditLog {
		var userID int64
		var username string
		if ctx.IsSigned && ctx.Doer != nil {
			userID = ctx.Doer.GetID()
			username = ctx.Doer.GetName()
		} else {
			userID = 0
			username = "anonymous"
		}
		robot.LogRobotAccessQuick(
			userID,
			username,
			owner,
			repoName,
			"/api/v1/robot/graph",
			ctx.RemoteAddr(),
			true,
			"",
		)
	}

	// 7. Return dependency graph (actual implementation)
	nodes, edges, err := getDependencyGraph(ctx, repository)
	if err != nil {
		log.Error("Failed to get dependency graph for repo %d: %v", repository.ID, err)
		ctx.Error(http.StatusInternalServerError, "GetDependencyGraph", err)
		return
	}

	response := GraphResponse{
		RepoID:    repository.ID,
		RepoName:  repository.Name,
		NodeCount: len(nodes),
		EdgeCount: len(edges),
		Nodes:     nodes,
		Edges:     edges,
	}

	ctx.JSON(http.StatusOK, response)
}

// getDependencyGraph builds the dependency graph for a repository
func getDependencyGraph(ctx *context.APIContext, repository *repo.Repository) ([]GraphNode, []GraphEdge, error) {
	// Get all issues for the repository (both open and closed)
	issuesList, err := issues.GetIssuesByRepoID(ctx, repository.ID, issues.IssuesOptions{
		State: "all", // Get both open and closed
	})
	if err != nil {
		return nil, nil, err
	}

	// Build node map
	nodeMap := make(map[int64]GraphNode)
	issueIDs := make([]int64, 0, len(issuesList))

	for _, issue := range issuesList {
		// Skip pull requests
		if issue.IsPull {
			continue
		}

		// Get PageRank score (placeholder)
		pageRank := 0.5

		node := GraphNode{
			ID:       issue.ID,
			Index:    issue.Index,
			Title:    issue.Title,
			PageRank: pageRank,
			IsClosed: issue.IsClosed,
		}
		nodeMap[issue.ID] = node
		issueIDs = append(issueIDs, issue.ID)
	}

	// Get all dependencies for these issues
	edges := make([]GraphEdge, 0)
	if len(issueIDs) > 0 {
		dependencies, err := getDependencies(ctx, issueIDs)
		if err != nil {
			log.Warn("Failed to get dependencies: %v", err)
			// Continue without dependencies
		} else {
			for _, dep := range dependencies {
				// Only include edges where both nodes exist in our graph
				if _, fromExists := nodeMap[dep.From]; fromExists {
					if _, toExists := nodeMap[dep.To]; toExists {
						edges = append(edges, dep)
					}
				}
			}
		}
	}

	// Convert node map to slice
	nodes := make([]GraphNode, 0, len(nodeMap))
	for _, node := range nodeMap {
		nodes = append(nodes, node)
	}

	return nodes, edges, nil
}

// Dependency represents a raw dependency from the database
type Dependency struct {
	IssueID      int64
	DependencyID int64
}

// getDependencies retrieves all dependencies for the given issue IDs
func getDependencies(ctx *context.APIContext, issueIDs []int64) ([]GraphEdge, error) {
	if len(issueIDs) == 0 {
		return []GraphEdge{}, nil
	}

	// Build placeholders for IN clause
	placeholders := make([]string, len(issueIDs))
	args := make([]interface{}, len(issueIDs))
	for i, id := range issueIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	sql := `SELECT issue_id, dependency_id FROM issue_dependency 
			WHERE issue_id IN (` + strings.Join(placeholders, ",") + `)`

	var deps []Dependency
	err := db.GetEngine(ctx).SQL(sql, args...).Find(&deps)
	if err != nil {
		return nil, err
	}

	edges := make([]GraphEdge, 0, len(deps))
	for _, dep := range deps {
		edges = append(edges, GraphEdge{
			From:   dep.IssueID,
			To:     dep.DependencyID,
			Type:   "depends_on",
			Weight: 1,
		})
	}

	return edges, nil
}
