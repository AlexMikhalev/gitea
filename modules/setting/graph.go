// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package setting

import (
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
	IssueGraph.DampingFactor = sec.Key("DAMPING_FACTOR").MustFloat64(0.85)
	IssueGraph.Iterations = sec.Key("ITERATIONS").MustInt(100)

	if IssueGraph.Enabled {
		log.Info("Issue graph features enabled (damping_factor=%.2f, iterations=%d)",
			IssueGraph.DampingFactor, IssueGraph.Iterations)
	}
}