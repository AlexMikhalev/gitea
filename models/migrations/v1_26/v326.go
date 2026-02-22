// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v1_26

import (
	"code.gitea.io/gitea/modules/timeutil"

	"xorm.io/xorm"
)

// IssueDependency represents a dependency relationship between issues
type IssueDependency struct {
	ID          int64              `xorm:"pk autoincr"`
	RepoID      int64              `xorm:"INDEX NOT NULL"`
	IssueID     int64              `xorm:"INDEX NOT NULL"`
	DependsOn   int64              `xorm:"INDEX NOT NULL"`
	DepType     string             `xorm:"VARCHAR(20) NOT NULL"`
	CreatedUnix timeutil.TimeStamp `xorm:"created"`
}

// GraphCache stores pre-computed PageRank and graph metrics for issues
type GraphCache struct {
	RepoID      int64              `xorm:"pk"`
	IssueID     int64              `xorm:"pk"`
	PageRank    float64            `xorm:"DEFAULT 0"`
	Centrality  float64            `xorm:"DEFAULT 0"`
	UpdatedUnix timeutil.TimeStamp `xorm:"updated"`
}

// AddIssueGraphFeatures adds issue dependency tracking and graph analytics tables
func AddIssueGraphFeatures(x *xorm.Engine) error {
	// Create issue_dependency table
	if err := x.Sync(new(IssueDependency)); err != nil {
		return err
	}

	// Create graph_cache table
	if err := x.Sync(new(GraphCache)); err != nil {
		return err
	}

	// Add unique constraint to prevent duplicate dependencies
	_, err := x.Exec("CREATE UNIQUE INDEX IF NOT EXISTS `UQE_issue_dependency` ON `issue_dependency` (`repo_id`, `issue_id`, `depends_on`)")
	return err
}