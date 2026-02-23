// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package setting

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"gopkg.in/ini.v1"
)

// mockConfigProvider creates a mock config provider for testing
type mockConfigProvider struct {
	data map[string]map[string]string
}

func (m *mockConfigProvider) Section(name string) *ini.Section {
	// Create a temporary ini file to parse
	cfg := ini.Empty()
	sec, _ := cfg.NewSection(name)

	if sectionData, ok := m.data[name]; ok {
		for key, value := range sectionData {
			sec.Key(key).SetValue(value)
		}
	}
	return sec
}

func (m *mockConfigProvider) MustValue(key string) string {
	return ""
}

func TestLoadIssueGraphFrom_Defaults(t *testing.T) {
	// Reset settings to defaults before test
	IssueGraphSettings.PageRankCacheTTL = 300
	IssueGraphSettings.AuditLog = true
	IssueGraphSettings.StrictMode = false

	// Create empty config (should use defaults)
	cfg := &mockConfigProvider{
		data: map[string]map[string]string{
			"issue_graph": {},
		},
	}

	loadIssueGraphFrom(cfg)

	assert.Equal(t, 300, IssueGraphSettings.PageRankCacheTTL, "Default PAGERANK_CACHE_TTL should be 300")
	assert.Equal(t, true, IssueGraphSettings.AuditLog, "Default AUDIT_LOG should be true")
	assert.Equal(t, false, IssueGraphSettings.StrictMode, "Default STRICT_MODE should be false")
	assert.Equal(t, 0.85, IssueGraphSettings.DampingFactor, "Default DAMPING_FACTOR should be 0.85")
	assert.Equal(t, 100, IssueGraphSettings.Iterations, "Default ITERATIONS should be 100")
	assert.Equal(t, true, IssueGraphSettings.Enabled, "Default ENABLED should be true")
}

func TestLoadIssueGraphFrom_CustomValues(t *testing.T) {
	cfg := &mockConfigProvider{
		data: map[string]map[string]string{
			"issue_graph": {
				"PAGERANK_CACHE_TTL": "600",
				"AUDIT_LOG":          "false",
				"STRICT_MODE":        "true",
				"DAMPING_FACTOR":     "0.90",
				"ITERATIONS":         "200",
				"ENABLED":            "false",
			},
		},
	}

	loadIssueGraphFrom(cfg)

	assert.Equal(t, 600, IssueGraphSettings.PageRankCacheTTL, "Custom PAGERANK_CACHE_TTL should be 600")
	assert.Equal(t, false, IssueGraphSettings.AuditLog, "Custom AUDIT_LOG should be false")
	assert.Equal(t, true, IssueGraphSettings.StrictMode, "Custom STRICT_MODE should be true")
	assert.Equal(t, 0.90, IssueGraphSettings.DampingFactor, "Custom DAMPING_FACTOR should be 0.90")
	assert.Equal(t, 200, IssueGraphSettings.Iterations, "Custom ITERATIONS should be 200")
	assert.Equal(t, false, IssueGraphSettings.Enabled, "Custom ENABLED should be false")
}

func TestLoadIssueGraphFrom_InvalidCacheTTL(t *testing.T) {
	// Reset to known state
	IssueGraphSettings.PageRankCacheTTL = 300

	cfg := &mockConfigProvider{
		data: map[string]map[string]string{
			"issue_graph": {
				"PAGERANK_CACHE_TTL": "-1",
			},
		},
	}

	loadIssueGraphFrom(cfg)

	// Should fall back to default when negative
	assert.Equal(t, 300, IssueGraphSettings.PageRankCacheTTL, "Negative PAGERANK_CACHE_TTL should use default")
}

func TestLoadIssueGraphFrom_ExcessiveCacheTTL(t *testing.T) {
	cfg := &mockConfigProvider{
		data: map[string]map[string]string{
			"issue_graph": {
				"PAGERANK_CACHE_TTL": "7200", // 2 hours
			},
		},
	}

	loadIssueGraphFrom(cfg)

	// Should accept the value but log a warning
	assert.Equal(t, 7200, IssueGraphSettings.PageRankCacheTTL, "Excessive PAGERANK_CACHE_TTL should be accepted with warning")
}

func TestIsIssueGraphEnabled(t *testing.T) {
	IssueGraphSettings.Enabled = true
	assert.True(t, IsIssueGraphEnabled())

	IssueGraphSettings.Enabled = false
	assert.False(t, IsIssueGraphEnabled())
}

func TestGetPageRankCacheTTL(t *testing.T) {
	IssueGraphSettings.PageRankCacheTTL = 300
	assert.Equal(t, 300, GetPageRankCacheTTL())

	IssueGraphSettings.PageRankCacheTTL = 600
	assert.Equal(t, 600, GetPageRankCacheTTL())
}

func TestIsAuditLogEnabled(t *testing.T) {
	IssueGraphSettings.AuditLog = true
	assert.True(t, IsAuditLogEnabled())

	IssueGraphSettings.AuditLog = false
	assert.False(t, IsAuditLogEnabled())
}

func TestIsStrictModeEnabled(t *testing.T) {
	IssueGraphSettings.StrictMode = true
	assert.True(t, IsStrictModeEnabled())

	IssueGraphSettings.StrictMode = false
	assert.False(t, IsStrictModeEnabled())
}
