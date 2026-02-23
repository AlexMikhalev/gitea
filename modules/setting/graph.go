// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package setting

import (
	"strconv"

	"code.gitea.io/gitea/modules/log"
)

// IssueGraph settings for issue dependency graph features
var IssueGraph = struct {
	Enabled       bool
	DampingFactor float64
	Iterations    int
}{
	Enabled:       false,
	DampingFactor: 0.85,
	Iterations:    100,
}

func loadIssueGraphFrom(rootCfg ConfigProvider) {
	sec := rootCfg.Section("issue_graph")
	IssueGraph.Enabled = sec.Key("ENABLED").MustBool(false)
	
	// Parse float manually since ConfigKey doesn't have MustFloat64
	dampingStr := sec.Key("DAMPING_FACTOR").String()
	if dampingStr != "" {
		if val, err := strconv.ParseFloat(dampingStr, 64); err == nil {
			IssueGraph.DampingFactor = val
		}
	}
	
	IssueGraph.Iterations = sec.Key("ITERATIONS").MustInt(100)

	if IssueGraph.Enabled {
		log.Info("Issue graph features enabled (damping_factor=%.2f, iterations=%d)",
			IssueGraph.DampingFactor, IssueGraph.Iterations)
	}
}