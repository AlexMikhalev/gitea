// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package robotroom

import (
	"strings"
	"testing"
)

// TestMarkerRoundTrip checks the marker survives being embedded in a room
// issue body and parsed back, including after a human edited the body.
func TestMarkerRoundTrip(t *testing.T) {
	body := IssueBody("feat/foo", "0123456789abcdef0123456789abcdef01234567")

	m, ok := ParseMarker(body)
	if !ok {
		t.Fatal("ParseMarker() did not find the marker in a room body")
	}
	if m.Type != MarkerType {
		t.Errorf("marker type = %q, want %q", m.Type, MarkerType)
	}
	if m.Branch != "feat/foo" {
		t.Errorf("marker branch = %q, want feat/foo", m.Branch)
	}
	if m.Head != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("marker head = %q", m.Head)
	}

	// Human text appended after the marker must not break parsing.
	edited := body + "\nSome notes a human added.\n"
	if _, ok := ParseMarker(edited); !ok {
		t.Error("ParseMarker() failed on an edited room body")
	}

	// Text before the fence (an edited body) must not break parsing either.
	prefixed := "pinned note\n\n" + body
	if _, ok := ParseMarker(prefixed); !ok {
		t.Error("ParseMarker() failed on a body with text before the marker")
	}
}

// TestParseMarkerNonRoom verifies ordinary issue bodies are not rooms.
func TestParseMarkerNonRoom(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"empty", ""},
		{"plain markdown", "# Title\n\nsome text"},
		{"json block of different type", "```json\n{\"type\":\"other\",\"branch\":\"feat/x\"}\n```"},
		{"json block without branch", "```json\n{\"type\":\"gitea-robot/room\",\"version\":1}\n```"},
		{"invalid json in fence", "```json\n{not json}\n```"},
		{"unclosed fence", "```json\n{\"type\":\"gitea-robot/room\",\"branch\":\"feat/x\"}"},
		{"non-json fence", "```\n{\"type\":\"gitea-robot/room\",\"branch\":\"feat/x\"}\n```"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := ParseMarker(tt.content); ok {
				t.Errorf("ParseMarker() accepted non-room body %q", tt.content)
			}
		})
	}
}

// TestMarkerIdempotencyKey verifies that two rooms for different branches
// produce distinct markers and that the marker is stable for the same branch,
// which is what makes the search-then-create idempotency check work.
func TestMarkerIdempotencyKey(t *testing.T) {
	a := MarkerJSON("feat/a", "")
	a2 := MarkerJSON("feat/a", "")
	b := MarkerJSON("feat/b", "")
	if a != a2 {
		t.Error("marker is not stable for the same branch")
	}
	if a == b {
		t.Error("markers for different branches collide")
	}
	if !strings.Contains(a, `"branch":"feat/a"`) {
		t.Errorf("marker %s does not carry the branch name", a)
	}
}

// TestWithMarkerHead covers the head refresh a re-push performs: the marker
// head is rewritten in place, human text around it survives, and a body
// without a replaceable marker is left untouched.
func TestWithMarkerHead(t *testing.T) {
	const oldHead = "0123456789abcdef0123456789abcdef01234567"
	const newHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	body := IssueBody("feat/foo", oldHead) + "\nhuman notes stay\n"
	updated, changed := WithMarkerHead(body, newHead)
	if !changed {
		t.Fatal("WithMarkerHead() reported no change")
	}
	m, ok := ParseMarker(updated)
	if !ok || m.Head != newHead {
		t.Errorf("updated marker = %+v, ok = %v, want head %q", m, ok, newHead)
	}
	if !strings.Contains(updated, "human notes stay") {
		t.Error("WithMarkerHead() clobbered human text around the marker")
	}
	if strings.Contains(updated, oldHead) {
		t.Error("WithMarkerHead() left the old head behind")
	}

	// Same head: no rewrite.
	if _, changed := WithMarkerHead(updated, newHead); changed {
		t.Error("WithMarkerHead() rewrote an already-current head")
	}
	// Non-room body: no rewrite.
	if _, changed := WithMarkerHead("# just an issue", newHead); changed {
		t.Error("WithMarkerHead() rewrote a non-room body")
	}
	// A hand-reformatted marker block (pretty-printed JSON) parses but is
	// not replaced: guessing the new layout risks clobbering human text.
	pretty := "```json\n{\n  \"type\": \"gitea-robot/room\",\n  \"version\": 1,\n  \"branch\": \"feat/foo\",\n  \"head\": \"" + oldHead + "\"\n}\n```\n"
	if _, changed := WithMarkerHead(pretty, newHead); changed {
		t.Error("WithMarkerHead() rewrote a hand-reformatted marker block")
	}
}

// TestStatusComment pins the exact comment shape. The hook and the CLI both
// compose status comments through this one function, so pinning the output
// here pins hook-vs-CLI parity for identical inputs.
func TestStatusComment(t *testing.T) {
	tests := []struct {
		name        string
		state       string
		context     string
		sha         string
		targetURL   string
		description string
		want        string
	}{
		{
			"all fields",
			"success", "ci/test", "abc123", "https://ci.example/1", "all green",
			"CI status: **success** (`ci/test`) — all green\n\ncommit: `abc123` · [details](https://ci.example/1)",
		},
		{
			"state and sha only",
			"failure", "", "def456", "", "",
			"CI status: **failure**\n\ncommit: `def456`",
		},
		{
			"no sha (CLI may omit it)",
			"pending", "ci/lint", "", "", "",
			"CI status: **pending** (`ci/lint`)",
		},
		{
			"no context",
			"success", "", "abc123", "", "deployed",
			"CI status: **success** — deployed\n\ncommit: `abc123`",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StatusComment(tt.state, tt.context, tt.sha, tt.targetURL, tt.description); got != tt.want {
				t.Errorf("StatusComment() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestIssueTitle pins the room title, half of a room's identity and the field
// both sides match on.
func TestIssueTitle(t *testing.T) {
	if got := IssueTitle("feat/foo"); got != "Room: feat/foo" {
		t.Errorf("IssueTitle() = %q, want %q", got, "Room: feat/foo")
	}
	if !strings.HasPrefix(IssueTitle("feat/foo"), TitlePrefix) {
		t.Error("IssueTitle() does not start with TitlePrefix")
	}
}

// TestIsRoomFor covers the shared match predicate: the room hook and the
// gitea-robot CLI both decide "is this issue the room for that branch?"
// through it, so it is the one place the rule is written down.
func TestIsRoomFor(t *testing.T) {
	body := IssueBody("feat/foo", "abc123")

	tests := []struct {
		name   string
		title  string
		body   string
		branch string
		want   bool
	}{
		{"exact title and marker", IssueTitle("feat/foo"), body, "feat/foo", true},
		{"renamed title", "Room: feat/foo (WIP)", body, "feat/foo", false},
		{"empty title", "", body, "feat/foo", false},
		{"title of another branch", IssueTitle("feat/bar"), body, "feat/foo", false},
		{"marker of another branch", IssueTitle("feat/foo"), IssueBody("feat/bar", ""), "feat/foo", false},
		{"no marker at all", IssueTitle("feat/foo"), "just an issue", "feat/foo", false},
		{"marker but wrong type", IssueTitle("feat/foo"), "```json\n{\"type\":\"other\",\"branch\":\"feat/foo\"}\n```", "feat/foo", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRoomFor(tt.title, tt.body, tt.branch); got != tt.want {
				t.Errorf("IsRoomFor() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestMarkerHeadFragment checks the fragment really is a substring of the
// marker JSON: the room hook uses it as a database pre-filter, so a drift
// between the two would silently stop the marker-head fallback from matching.
func TestMarkerHeadFragment(t *testing.T) {
	const head = "0123456789abcdef0123456789abcdef01234567"
	if frag := MarkerHeadFragment(head); !strings.Contains(MarkerJSON("feat/foo", head), frag) {
		t.Errorf("MarkerHeadFragment(%q) = %q, not a substring of %q", head, frag, MarkerJSON("feat/foo", head))
	}
	if strings.Contains(MarkerJSON("feat/foo", "other"), MarkerHeadFragment(head)) {
		t.Error("MarkerHeadFragment matched a marker with a different head")
	}
}
