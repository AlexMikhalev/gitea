// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package robot

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"code.gitea.io/gitea/modules/json"
	api "code.gitea.io/gitea/modules/structs"
)

func signBody(t *testing.T, secret string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// TestVerifyRoomHookSignature pins the signature contract: Gitea deliveries
// carry raw hex HMAC-SHA256 in X-Gitea-Signature and the prefixed form in
// X-Hub-Signature-256; an absent or wrong signature never verifies, and an
// unconfigured secret verifies nothing.
func TestVerifyRoomHookSignature(t *testing.T) {
	secret := "test-hook-secret"
	body := []byte(`{"ref":"refs/heads/feat/foo"}`)
	sig := signBody(t, secret, body)

	tests := []struct {
		name     string
		secret   string
		body     []byte
		giteaSig string
		hubSig   string
		want     bool
	}{
		{"good X-Gitea-Signature", secret, body, sig, "", true},
		{"good X-Hub-Signature-256", secret, body, "", "sha256=" + sig, true},
		{"both headers, gitea wins", secret, body, sig, "sha256=deadbeef", true},
		{"bad signature", secret, body, strings.Repeat("0", 64), "", false},
		{"absent signature", secret, body, "", "", false},
		{"signature for different body", secret, []byte(`{"ref":"refs/heads/feat/bar"}`), sig, "", false},
		{"wrong secret", "other-secret", body, sig, "", false},
		{"empty secret rejects everything", "", body, sig, "", false},
		{"malformed hex", secret, body, "not-hex", "", false},
		{"hub header without sha256 prefix", secret, body, "", sig, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := verifyRoomHookSignature(tt.secret, tt.body, tt.giteaSig, tt.hubSig); got != tt.want {
				t.Errorf("verifyRoomHookSignature() = %v, want %v", got, tt.want)
			}
		})
	}
}

// The room marker itself (build, parse, head refresh) is tested in
// modules/robotroom, the package both this hook and the gitea-robot CLI use.

// TestDeleteTargetsRoom covers the delete-event filter: only feat/* branch
// deletions close a room; tag deletions (ref_type "tag", see
// services/webhook/notifier.go) and other branches are ignored.
func TestDeleteTargetsRoom(t *testing.T) {
	tests := []struct {
		refType string
		ref     string
		want    bool
	}{
		{"branch", "feat/foo", true},
		{"branch", "feat/56-branch-as-room", true},
		{"tag", "feat/foo", false}, // a tag named like a feat branch never carried a room
		{"branch", "main", false},
		{"branch", "feature/foo", false},
		{"tag", "v1.0.0", false},
		{"", "feat/foo", false},
	}
	for _, tt := range tests {
		t.Run(tt.refType+"/"+tt.ref, func(t *testing.T) {
			if got := deleteTargetsRoom(tt.refType, tt.ref); got != tt.want {
				t.Errorf("deleteTargetsRoom(%q, %q) = %v, want %v", tt.refType, tt.ref, got, tt.want)
			}
		})
	}
}

// TestFeatBranchRef covers the feat/* ref filter for push events.
func TestFeatBranchRef(t *testing.T) {
	tests := []struct {
		ref        string
		wantBranch string
		wantOK     bool
	}{
		{"refs/heads/feat/foo", "feat/foo", true},
		{"refs/heads/feat/56-branch-as-room", "feat/56-branch-as-room", true},
		{"refs/heads/main", "", false},
		{"refs/heads/feature/foo", "", false},
		{"refs/heads/feat", "", false},  // no slash: not under feat/
		{"refs/heads/feat/", "", false}, // empty branch name
		{"refs/tags/feat/foo", "", false},
		{"feat/foo", "", false}, // short name is not a push ref
		{"", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			branch, ok := featBranchRef(tt.ref)
			if ok != tt.wantOK || branch != tt.wantBranch {
				t.Errorf("featBranchRef(%q) = (%q, %v), want (%q, %v)",
					tt.ref, branch, ok, tt.wantBranch, tt.wantOK)
			}
		})
	}
}

// TestIsZeroSHA covers the delete-vs-push distinction: a push that deletes a
// branch carries an all-zero "after" SHA.
func TestIsZeroSHA(t *testing.T) {
	tests := []struct {
		sha  string
		want bool
	}{
		{strings.Repeat("0", 40), true},
		{strings.Repeat("0", 64), true},
		{"0123456789abcdef0123456789abcdef01234567", false},
		{"", false},
		{"0", false},
		{strings.Repeat("0", 39), false},
	}
	for _, tt := range tests {
		if got := isZeroSHA(tt.sha); got != tt.want {
			t.Errorf("isZeroSHA(%q) = %v, want %v", tt.sha, got, tt.want)
		}
	}
}

// TestRoomEventPayloadParsing verifies the webhook payload shapes the hook
// depends on: push carries ref/after, delete carries ref/ref_type (no deleted
// flag exists on push - ground truth from modules/structs/hook.go), and status
// carries sha/state with no branch ref.
func TestRoomEventPayloadParsing(t *testing.T) {
	pushJSON := `{"ref":"refs/heads/feat/foo","after":"0123456789abcdef0123456789abcdef01234567",` +
		`"repository":{"name":"repo","owner":{"login":"owner"}},"sender":{"login":"user2"}}`
	var push api.PushPayload
	if err := json.Unmarshal([]byte(pushJSON), &push); err != nil {
		t.Fatalf("push payload did not parse: %v", err)
	}
	if branch, ok := featBranchRef(push.Ref); !ok || branch != "feat/foo" {
		t.Errorf("push ref did not resolve to feat/foo, got %q", branch)
	}
	if push.Repo.Owner.UserName != "owner" || push.Repo.Name != "repo" {
		t.Errorf("push repo resolved to %v/%v", push.Repo.Owner.UserName, push.Repo.Name)
	}

	deleteJSON := `{"ref":"feat/foo","ref_type":"branch","repository":{"name":"repo","owner":{"login":"owner"}}}`
	var del api.DeletePayload
	if err := json.Unmarshal([]byte(deleteJSON), &del); err != nil {
		t.Fatalf("delete payload did not parse: %v", err)
	}
	if del.RefType != "branch" || del.Ref != "feat/foo" {
		t.Errorf("delete payload resolved to %q (%q)", del.Ref, del.RefType)
	}

	statusJSON := `{"sha":"0123456789abcdef0123456789abcdef01234567","state":"success","context":"ci/test",` +
		`"repository":{"name":"repo","owner":{"login":"owner"}}}`
	var status api.CommitStatusPayload
	if err := json.Unmarshal([]byte(statusJSON), &status); err != nil {
		t.Fatalf("status payload did not parse: %v", err)
	}
	if status.SHA == "" || status.State != "success" {
		t.Errorf("status payload resolved to sha=%q state=%q", status.SHA, status.State)
	}
}

// TestMergedFeatBranch covers the pull_request filter: only a closed+merged
// PR from a feat/* branch closes a room; anything else is ignored.
func TestMergedFeatBranch(t *testing.T) {
	prPayload := func(action string, merged bool, headRef string) *api.PullRequestPayload {
		return &api.PullRequestPayload{
			Action: api.HookIssueAction(action),
			PullRequest: &api.PullRequest{
				HasMerged: merged,
				Head:      &api.PRBranchInfo{Ref: headRef},
			},
		}
	}
	tests := []struct {
		name       string
		payload    *api.PullRequestPayload
		wantBranch string
		wantOK     bool
	}{
		{"merged feat PR", prPayload("closed", true, "feat/foo"), "feat/foo", true},
		{"closed but not merged", prPayload("closed", false, "feat/foo"), "", false},
		{"merged non-feat PR", prPayload("closed", true, "main"), "", false},
		{"merged empty feat ref", prPayload("closed", true, "feat/"), "", false},
		{"opened feat PR", prPayload("opened", false, "feat/foo"), "", false},
		{"no pull request", &api.PullRequestPayload{Action: api.HookIssueClosed}, "", false},
		{"no head", &api.PullRequestPayload{Action: api.HookIssueClosed, PullRequest: &api.PullRequest{HasMerged: true}}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			branch, ok := mergedFeatBranch(tt.payload)
			if ok != tt.wantOK || branch != tt.wantBranch {
				t.Errorf("mergedFeatBranch() = (%q, %v), want (%q, %v)",
					branch, ok, tt.wantBranch, tt.wantOK)
			}
		})
	}

	// The wire shape: PRBranchInfo carries the branch name in "ref".
	prJSON := `{"action":"closed","pull_request":{"merged":true,"head":{"ref":"feat/foo","label":"o:feat/foo"}},` +
		`"repository":{"name":"repo","owner":{"login":"owner"}},"sender":{"login":"user2"}}`
	var pr api.PullRequestPayload
	if err := json.Unmarshal([]byte(prJSON), &pr); err != nil {
		t.Fatalf("pull_request payload did not parse: %v", err)
	}
	if branch, ok := mergedFeatBranch(&pr); !ok || branch != "feat/foo" {
		t.Errorf("wire payload resolved to (%q, %v), want (feat/foo, true)", branch, ok)
	}
}

// TestRoomPayloadRepoHint covers the untrusted owner/repo hint used to make
// bad-signature audit records useful.
func TestRoomPayloadRepoHint(t *testing.T) {
	owner, repo := roomPayloadRepoHint([]byte(`{"repository":{"name":"repo1","owner":{"login":"user2"}}}`))
	if owner != "user2" || repo != "repo1" {
		t.Errorf("hint = (%q, %q), want (user2, repo1)", owner, repo)
	}
	for _, body := range []string{`{invalid`, `{}`, `{"repository":{"name":"r"}}`, `{"repository":null}`} {
		if owner, repo := roomPayloadRepoHint([]byte(body)); owner != "" || repo != "" {
			t.Errorf("hint(%s) = (%q, %q), want empty", body, owner, repo)
		}
	}
}
