// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package quarto

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHasFrontmatter(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"no frontmatter", "Hello world", false},
		{"with frontmatter", "---\ntitle: Test\n---\nContent", true},
		{"empty with frontmatter markers", "---", true},
		{"frontmatter without closing", "---\ntitle: Test\nContent", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := HasFrontmatter([]byte(tt.input))
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestStripFrontmatter(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "no frontmatter",
			input:    "Hello world",
			expected: "Hello world",
		},
		{
			name:     "with frontmatter",
			input:    "---\ntitle: Test\n---\nContent here",
			expected: "Content here",
		},
		{
			name:     "frontmatter without closing",
			input:    "---\ntitle: Test\nContent",
			expected: "---\ntitle: Test\nContent",
		},
		{
			name:     "empty frontmatter",
			input:    "---\n---\nContent",
			expected: "Content",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := StripFrontmatter([]byte(tt.input))
			assert.Equal(t, tt.expected, string(result))
		})
	}
}

func TestExtractFrontmatter(t *testing.T) {
	tests := []struct {
		name            string
		input           string
		expectedFront   string
		expectedContent string
	}{
		{
			name:            "no frontmatter",
			input:           "Hello world",
			expectedFront:   "",
			expectedContent: "Hello world",
		},
		{
			name:            "with frontmatter",
			input:           "---\ntitle: Test\n---\nContent here",
			expectedFront:   "\ntitle: Test",
			expectedContent: "Content here",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			front, content := ExtractFrontmatter([]byte(tt.input))
			assert.Equal(t, tt.expectedFront, string(front))
			assert.Equal(t, tt.expectedContent, string(content))
		})
	}
}

func TestStripQuartoOptions(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "no options",
			input:    "```python\nprint('hello')\n```",
			expected: "```python\nprint('hello')\n```",
		},
		{
			name:     "with quarto options",
			input:    "```python\n#| label: fig-1\n#| fig-cap: \"Hello\"\nprint('hello')\n```",
			expected: "```python\nprint('hello')\n```",
		},
		{
			name:     "with multiple options",
			input:    "```r\n#| echo: false\n#| message: false\nx <- 1\n```",
			expected: "```r\nx <- 1\n```",
		},
		{
			name:     "options with spaces",
			input:    "```\n#| key: value\n#| another: test\ncode\n```",
			expected: "```\ncode\n```",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := StripQuartoOptions([]byte(tt.input))
			assert.Equal(t, tt.expected, string(result))
		})
	}
}
