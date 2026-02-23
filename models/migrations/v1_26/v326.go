// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package v1_26

import (
	"xorm.io/xorm"
)

// GraphCache stores pre-computed PageRank and graph metrics for issues
type GraphCache struct {
	RepoID      int64   `xorm:"pk"`
	IssueID     int64   `xorm:"pk"`
	PageRank    float64 `xorm:"DEFAULT 0"`
	Centrality  float64 `xorm:"DEFAULT 0"`
	UpdatedUnix int64   `xorm:"updated"`
}

// AddGraphCache adds PageRank cache table for issue graph analytics
func AddGraphCache(x *xorm.Engine) error {
	return x.Sync(new(GraphCache))
}