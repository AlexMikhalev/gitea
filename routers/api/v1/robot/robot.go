// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package robot

import (
	"errors"
	"net/http"
	"strings"

	"code.gitea.io/gitea/models/db"
	access_model "code.gitea.io/gitea/models/perm/access"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/models/unit"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/services/context"
	"code.gitea.io/gitea/services/robot"
)

// validateOwnerRepo validates owner and repo parameters
// Returns error if parameters are invalid
func validateOwnerRepoInput(owner, repo string) error {
	// Check for empty values
	if owner == "" {
		return errors.New("owner parameter is required")
	}
	if repo == "" {
		return errors.New("repo parameter is required")
	}

	// Check length constraints
	// GitHub uses 39 chars for usernames, Gitea uses 40
	if len(owner) > 40 {
		return errors.New("owner name too long (max 40 characters)")
	}
	// Repo names can be up to 100 characters
	if len(repo) > 100 {
		return errors.New("repo name too long (max 100 characters)")
	}

	// Check for path traversal attempts
	if strings.Contains(owner, "..") || strings.Contains(repo, "..") {
		return errors.New("invalid characters in owner or repo name")
	}

	// Check for other potentially dangerous characters
	if strings.ContainsAny(owner, "/\\<>:|?*\x00") || strings.ContainsAny(repo, "/\\<>:|?*\x00") {
		return errors.New("invalid characters in owner or repo name")
	}

	return nil
}

// checkRepoPermission checks if the current user has permission to read issues in the repository
// Returns true if permission is granted, false otherwise
// Always returns 404 to avoid leaking repository existence
func checkRepoPermissionForTriage(ctx *context.APIContext, repository *repo_model.Repository) bool {
	// Get user permissions for this repository
	perm, err := access_model.GetUserRepoPermission(ctx, repository, ctx.Doer)
	if err != nil {
		log.Error("Failed to get user repo permission: %v", err)
		ctx.NotFound()
		return false
	}

	// Check if user can read issues
	if !perm.CanRead(unit.TypeIssues) {
		// Return 404 to avoid leaking repository existence
		ctx.NotFound()
		return false
	}

	return true
}

// Triage handles the /api/v1/robot/triage endpoint
// Returns prioritized issues using PageRank algorithm
func Triage(ctx *context.APIContext) {
	// 1. Check feature enabled
	if !setting.IssueGraphEnabled() {
		ctx.NotFound()
		return
	}

	// Get parameters from query string
	owner := ctx.FormString("owner")
	repoName := ctx.FormString("repo")

	// 2. Validate input
	if err := validateOwnerRepoInput(owner, repoName); err != nil {
		ctx.Error(http.StatusBadRequest, "ValidationError", err.Error())
		return
	}

	// 3. Check authentication for private repos
	// For public repos, anonymous access is allowed
	// The actual permission check happens after repo lookup

	// 4. Lookup repository
	repository, err := repo_model.GetRepositoryByOwnerAndName(ctx, owner, repoName)
	if err != nil {
		if errors.Is(err, db.ErrNotExist) {
			// Return 404 to avoid leaking repository existence
			ctx.NotFound()
			return
		}
		ctx.Error(http.StatusInternalServerError, "RepoLookupError", err.Error())
		return
	}

	// Check if repository is private and user is not signed in
	if repository.IsPrivate && !ctx.IsSigned {
		ctx.NotFound()
		return
	}

	// 5. Check permission
	if !checkRepoPermissionForTriage(ctx, repository) {
		// checkRepoPermission already set the response
		// Log the denied access
		robot.LogRobotAccessQuick(
			ctx.Doer.GetID(),
			ctx.Doer.GetName(),
			owner,
			repoName,
			"/api/v1/robot/triage",
			ctx.RemoteAddr,
			false,
			"insufficient_permissions",
		)
		return
	}

	// 6. Log access
	robot.LogRobotAccessQuick(
		ctx.Doer.GetID(),
		ctx.Doer.GetName(),
		owner,
		repoName,
		"/api/v1/robot/triage",
		ctx.RemoteAddr,
		true,
		"",
	)

	// 7. Call service
	service := robot.NewService()
	response, err := service.Triage(ctx, repository)
	if err != nil {
		log.Error("Robot triage service error: %v", err)
		ctx.Error(http.StatusInternalServerError, "ServiceError", err.Error())
		return
	}

	ctx.JSON(http.StatusOK, response)
}
