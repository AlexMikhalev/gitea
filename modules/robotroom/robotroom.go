// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package robotroom holds the shared building blocks of the branch-as-room
// automation (issue #56): the machine-readable room marker, the room issue
// body, and the CI status comment shape. The server-side room hook
// (routers/api/v1/robot) and the gitea-robot CLI (cmd/gitea-robot) both build
// and parse rooms through this package, so the two sides can never drift
// apart.
package robotroom

import (
	"fmt"
	"strings"

	"code.gitea.io/gitea/modules/json"
)

// MarkerType identifies the machine-readable room marker embedded at the top
// of a room issue's body. One open issue carrying this marker exists per
// feat/* branch.
const MarkerType = "gitea-robot/room"

// Marker is the JSON front-matter block at the top of a room issue body. The
// design (docs/plans/design-branch-as-room-2026-08-04.md) requires a
// machine-readable JSON front-matter block "aligning with #39's convention";
// no shipped #39 artifact exists in this repository to confirm against, so
// the fenced ```json block below is the room convention going forward.
type Marker struct {
	Type    string `json:"type"`
	Version int    `json:"version"`
	Branch  string `json:"branch"`
	Head    string `json:"head,omitempty"`
}

// MarkerJSON renders the marker for a branch as compact JSON. The head is the
// branch tip SHA, refreshed on every push, and lets a status event find its
// room even when git branch resolution is unavailable.
func MarkerJSON(branch, head string) string {
	m := Marker{Type: MarkerType, Version: 1, Branch: branch, Head: head}
	data, _ := json.Marshal(m) // cannot fail: all fields are strings/ints
	return string(data)
}

// IssueBody builds the full body of a room issue: marker first, then a short
// human-readable explanation.
func IssueBody(branch, head string) string {
	return "```json\n" + MarkerJSON(branch, head) + "\n```\n" +
		"\nRoom for branch `" + branch + "`. Opened automatically on first push; " +
		"CI status changes on the branch head are posted here as comments; " +
		"the room closes when the branch merges or is deleted.\n"
}

// ParseMarker extracts the room marker from an issue body. It looks for a
// fenced ```json block and validates type and branch; anything else is not a
// room. It is deliberately tolerant about where the block sits, so a body a
// human edited still parses.
func ParseMarker(content string) (*Marker, bool) {
	const fence = "```json"
	_, rest, found := strings.Cut(content, fence)
	if !found {
		return nil, false
	}
	block, _, found := strings.Cut(rest, "```")
	if !found {
		return nil, false
	}
	var m Marker
	if err := json.Unmarshal([]byte(strings.TrimSpace(block)), &m); err != nil {
		return nil, false
	}
	if m.Type != MarkerType || m.Branch == "" {
		return nil, false
	}
	return &m, true
}

// WithMarkerHead returns content with the room marker's head replaced by
// head, reporting whether anything changed. The replacement is surgical -
// only the marker JSON is rewritten - so human edits around it survive. A
// body whose marker block was reformatted by hand is left untouched, because
// guessing at the block's new layout risks clobbering human text.
func WithMarkerHead(content, head string) (string, bool) {
	m, ok := ParseMarker(content)
	if !ok || m.Head == head {
		return content, false
	}
	old := MarkerJSON(m.Branch, m.Head)
	if !strings.Contains(content, old) {
		return content, false
	}
	return strings.Replace(content, old, MarkerJSON(m.Branch, head), 1), true
}

// StatusComment composes one CI status comment body. The hook posts it with
// the commit SHA from the status payload; the CLI may omit the SHA.
func StatusComment(state, context, sha, targetURL, description string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "CI status: **%s**", state)
	if context != "" {
		fmt.Fprintf(&sb, " (`%s`)", context)
	}
	if description != "" {
		fmt.Fprintf(&sb, " — %s", description)
	}
	if sha != "" {
		fmt.Fprintf(&sb, "\n\ncommit: `%s`", sha)
	}
	if targetURL != "" {
		fmt.Fprintf(&sb, " · [details](%s)", targetURL)
	}
	return sb.String()
}
