// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package issues

import (
	"context"
	"fmt"

	"code.gitea.io/gitea/models/db"
	"code.gitea.io/gitea/modules/timeutil"
	"code.gitea.io/gitea/modules/util"

	"xorm.io/builder"
)

// DependencyType represents the type of dependency relationship
type DependencyType string

const (
	// DepTypeBlocks means the issue blocks another issue
	DepTypeBlocks DependencyType = "blocks"
	// DepTypeRelatesTo means the issue relates to another issue
	DepTypeRelatesTo DependencyType = "relates_to"
	// DepTypeDuplicates means the issue duplicates another issue
	DepTypeDuplicates DependencyType = "duplicates"
	// DepTypeSupersedes means the issue supersedes another issue
	DepTypeSupersedes DependencyType = "supersedes"
)

// IssueDependency represents a dependency relationship between issues
type IssueDependency struct {
	ID          int64              `xorm:"pk autoincr"`
	RepoID      int64              `xorm:"INDEX NOT NULL"`
	IssueID     int64              `xorm:"INDEX NOT NULL"`
	DependsOn   int64              `xorm:"INDEX NOT NULL"`
	DepType     DependencyType     `xorm:"VARCHAR(20) NOT NULL"`
	CreatedUnix timeutil.TimeStamp `xorm:"created"`
}

func init() {
	db.RegisterModel(new(IssueDependency))
}

// ErrDependencyNotExist represents a "DependencyNotExist" kind of error
type ErrDependencyNotExist struct {
	ID      int64
	IssueID int64
}

// IsErrDependencyNotExist checks if an error is a ErrDependencyNotExist
func IsErrDependencyNotExist(err error) bool {
	_, ok := err.(ErrDependencyNotExist)
	return ok
}

func (err ErrDependencyNotExist) Error() string {
	return fmt.Sprintf("dependency does not exist [id: %d, issue_id: %d]", err.ID, err.IssueID)
}

func (err ErrDependencyNotExist) Unwrap() error {
	return util.ErrNotExist
}

// ErrDependencyAlreadyExists represents a "DependencyAlreadyExists" kind of error
type ErrDependencyAlreadyExists struct {
	IssueID   int64
	DependsOn int64
}

// IsErrDependencyAlreadyExists checks if an error is a ErrDependencyAlreadyExists
func IsErrDependencyAlreadyExists(err error) bool {
	_, ok := err.(ErrDependencyAlreadyExists)
	return ok
}

func (err ErrDependencyAlreadyExists) Error() string {
	return fmt.Sprintf("dependency already exists [issue_id: %d, depends_on: %d]", err.IssueID, err.DependsOn)
}

func (err ErrDependencyAlreadyExists) Unwrap() error {
	return util.ErrAlreadyExist
}

// ErrCircularDependency represents a "CircularDependency" kind of error
type ErrCircularDependency struct {
	IssueID   int64
	DependsOn int64
}

// IsErrCircularDependency checks if an error is a ErrCircularDependency
func IsErrCircularDependency(err error) bool {
	_, ok := err.(ErrCircularDependency)
	return ok
}

func (err ErrCircularDependency) Error() string {
	return fmt.Sprintf("circular dependency detected [issue_id: %d, depends_on: %d]", err.IssueID, err.DependsOn)
}

// AddDependency adds a new dependency relationship between issues
func AddDependency(ctx context.Context, repoID, issueID, dependsOn int64, depType DependencyType) error {
	// Check if dependency already exists
	exists, err := db.GetEngine(ctx).Exist(&IssueDependency{
		RepoID:    repoID,
		IssueID:   issueID,
		DependsOn: dependsOn,
	})
	if err != nil {
		return err
	}
	if exists {
		return ErrDependencyAlreadyExists{IssueID: issueID, DependsOn: dependsOn}
	}

	// Check for circular dependency
	if depType == DepTypeBlocks {
		if err := checkCircularDependency(ctx, repoID, issueID, dependsOn); err != nil {
			return err
		}
	}

	dep := &IssueDependency{
		RepoID:    repoID,
		IssueID:   issueID,
		DependsOn: dependsOn,
		DepType:   depType,
	}
	_, err = db.GetEngine(ctx).Insert(dep)
	return err
}

// RemoveDependency removes a dependency relationship
func RemoveDependency(ctx context.Context, repoID, issueID, dependsOn int64) error {
	_, err := db.GetEngine(ctx).Delete(&IssueDependency{
		RepoID:    repoID,
		IssueID:   issueID,
		DependsOn: dependsOn,
	})
	return err
}

// GetDependencies returns all dependencies for an issue
func GetDependencies(ctx context.Context, repoID, issueID int64) ([]*IssueDependency, error) {
	deps := make([]*IssueDependency, 0)
	err := db.GetEngine(ctx).Where("repo_id = ? AND issue_id = ?", repoID, issueID).Find(&deps)
	return deps, err
}

// GetDependents returns all issues that depend on this issue
func GetDependents(ctx context.Context, repoID, dependsOn int64) ([]*IssueDependency, error) {
	deps := make([]*IssueDependency, 0)
	err := db.GetEngine(ctx).Where("repo_id = ? AND depends_on = ?", repoID, dependsOn).Find(&deps)
	return deps, err
}

// GetBlockedIssues returns issues that block the given issue
func GetBlockedIssues(ctx context.Context, repoID, issueID int64) ([]*IssueDependency, error) {
	deps := make([]*IssueDependency, 0)
	err := db.GetEngine(ctx).Where("repo_id = ? AND issue_id = ? AND dep_type = ?", repoID, issueID, DepTypeBlocks).Find(&deps)
	return deps, err
}

// IsBlocked checks if an issue has any open blockers
func IsBlocked(ctx context.Context, repoID, issueID int64) (bool, error) {
	// Get all blocking dependencies
	blockers, err := GetBlockedIssues(ctx, repoID, issueID)
	if err != nil {
		return false, err
	}

	if len(blockers) == 0 {
		return false, nil
	}

	// Check if any blocker is still open
	blockerIDs := make([]int64, len(blockers))
	for i, b := range blockers {
		blockerIDs[i] = b.DependsOn
	}

	// Count open blockers
	count, err := db.GetEngine(ctx).Where("is_closed = ?", false).In("id", blockerIDs).Count(&Issue{})
	if err != nil {
		return false, err
	}

	return count > 0, nil
}

// checkCircularDependency checks if adding this dependency would create a cycle
func checkCircularDependency(ctx context.Context, repoID, issueID, dependsOn int64) error {
	// Simple DFS to detect cycle
	visited := make(map[int64]bool)
	return dfsCheckCycle(ctx, repoID, dependsOn, issueID, visited)
}

// dfsCheckCycle performs DFS to detect cycles in the dependency graph
func dfsCheckCycle(ctx context.Context, repoID, current, target int64, visited map[int64]bool) error {
	if current == target {
		return ErrCircularDependency{IssueID: target, DependsOn: current}
	}

	if visited[current] {
		return nil
	}
	visited[current] = true

	// Get all issues that current depends on
	deps, err := GetDependencies(ctx, repoID, current)
	if err != nil {
		return err
	}

	for _, dep := range deps {
		if dep.DepType == DepTypeBlocks {
			if err := dfsCheckCycle(ctx, repoID, dep.DependsOn, target, visited); err != nil {
				return err
			}
		}
	}

	return nil
}

// GetDependencyGraph returns the full dependency graph for a repository
func GetDependencyGraph(ctx context.Context, repoID int64) ([]*IssueDependency, error) {
	deps := make([]*IssueDependency, 0)
	err := db.GetEngine(ctx).Where("repo_id = ?", repoID).Find(&deps)
	return deps, err
}

// DeleteAllDependenciesForIssue removes all dependencies for an issue
func DeleteAllDependenciesForIssue(ctx context.Context, repoID, issueID int64) error {
	_, err := db.GetEngine(ctx).Where("repo_id = ? AND (issue_id = ? OR depends_on = ?)", repoID, issueID, issueID).Delete(&IssueDependency{})
	return err
}