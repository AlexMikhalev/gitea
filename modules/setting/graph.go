// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package setting

import (
	"strconv"

	"code.gitea.io/gitea/modules/log"
)

// IssueGraphSettings holds the configuration for the issue graph and robot API
var IssueGraphSettings = struct {
	// Core PageRank settings
	Enabled       bool
	DampingFactor float64
	Iterations    int

	// Security settings (new)
	PageRankCacheTTL int  // Time-to-live for PageRank cache in seconds (default: 300)
	AuditLog         bool // Enable audit logging for robot API access (default: true)
	StrictMode       bool // Enable strict mode - deny access on any error (default: false)
}{
	Enabled:       true,
	DampingFactor: 0.85,
	Iterations:    100,

	// Security defaults
	PageRankCacheTTL: 300, // 5 minutes
	AuditLog:         true,
	StrictMode:       false,
}

// loadIssueGraphFrom loads issue graph settings from the configuration provider
func loadIssueGraphFrom(rootCfg ConfigProvider) {
	sec := rootCfg.Section("issue_graph")

	// Core settings
	IssueGraphSettings.Enabled = sec.Key("ENABLED").MustBool(true)
	// Parse float manually as ConfigKey doesn't have MustFloat64
	if val, err := strconv.ParseFloat(sec.Key("DAMPING_FACTOR").String(), 64); err == nil {
		IssueGraphSettings.DampingFactor = val
	} else {
		IssueGraphSettings.DampingFactor = 0.85
	}
	IssueGraphSettings.Iterations = sec.Key("ITERATIONS").MustInt(100)

	// Security settings (new)
	IssueGraphSettings.PageRankCacheTTL = sec.Key("PAGERANK_CACHE_TTL").MustInt(300)
	IssueGraphSettings.AuditLog = sec.Key("AUDIT_LOG").MustBool(true)
	IssueGraphSettings.StrictMode = sec.Key("STRICT_MODE").MustBool(false)

	// Validation
	if IssueGraphSettings.PageRankCacheTTL < 0 {
		log.Warn("Invalid PAGERANK_CACHE_TTL (%d), using default of 300 seconds", IssueGraphSettings.PageRankCacheTTL)
		IssueGraphSettings.PageRankCacheTTL = 300
	}

	if IssueGraphSettings.PageRankCacheTTL > 3600 {
		log.Warn("PAGERANK_CACHE_TTL (%d) exceeds 1 hour, consider reducing for fresher data", IssueGraphSettings.PageRankCacheTTL)
	}

	if IssueGraphSettings.DampingFactor <= 0 || IssueGraphSettings.DampingFactor >= 1 {
		log.Warn("Invalid DAMPING_FACTOR (%.2f), using default of 0.85", IssueGraphSettings.DampingFactor)
		IssueGraphSettings.DampingFactor = 0.85
	}

	if IssueGraphSettings.Iterations <= 0 {
		log.Warn("Invalid ITERATIONS (%d), using default of 100", IssueGraphSettings.Iterations)
		IssueGraphSettings.Iterations = 100
	}

	log.Info("Issue Graph Settings: Enabled=%v, DampingFactor=%.2f, Iterations=%d, CacheTTL=%ds, AuditLog=%v, StrictMode=%v",
		IssueGraphSettings.Enabled,
		IssueGraphSettings.DampingFactor,
		IssueGraphSettings.Iterations,
		IssueGraphSettings.PageRankCacheTTL,
		IssueGraphSettings.AuditLog,
		IssueGraphSettings.StrictMode,
	)
}

// IsIssueGraphEnabled returns whether the issue graph feature is enabled
func IsIssueGraphEnabled() bool {
	return IssueGraphSettings.Enabled
}

// GetPageRankCacheTTL returns the PageRank cache TTL in seconds
func GetPageRankCacheTTL() int {
	return IssueGraphSettings.PageRankCacheTTL
}

// IsAuditLogEnabled returns whether audit logging is enabled
func IsAuditLogEnabled() bool {
	return IssueGraphSettings.AuditLog
}

// IsStrictModeEnabled returns whether strict mode is enabled
func IsStrictModeEnabled() bool {
	return IssueGraphSettings.StrictMode
}
