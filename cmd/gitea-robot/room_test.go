// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"code.gitea.io/gitea/modules/json"
	"code.gitea.io/gitea/modules/robotroom"
)

// withGiteaURL points the CLI at a test server for the duration of a test.
func withGiteaURL(t *testing.T, url string) {
	t.Helper()
	old := giteaURL
	giteaURL = url
	t.Cleanup(func() { giteaURL = old })
}

// recordedRequest captures one request the CLI made to the fake server.
type recordedRequest struct {
	Method string
	Path   string
	Body   string
}

// TestRoomMarkerRoundTripCLI checks the CLI against the shared room builder
// in modules/robotroom - the same package the server-side hook uses, which is
// what guarantees hook-vs-CLI wire parity.
func TestRoomMarkerRoundTripCLI(t *testing.T) {
	body := robotroom.IssueBody("feat/foo", "0123456789abcdef0123456789abcdef01234567")
	m, ok := robotroom.ParseMarker(body)
	if !ok {
		t.Fatal("ParseMarker() did not find the marker")
	}
	if m.Type != robotroom.MarkerType || m.Branch != "feat/foo" {
		t.Errorf("marker = %+v", m)
	}
	if _, ok := robotroom.ParseMarker("# just an issue"); ok {
		t.Error("ParseMarker() accepted a non-room body")
	}
}

// TestRoomOpenCreatesIssue verifies `room open` searches first and creates the
// room issue with the marker body when none exists.
func TestRoomOpenCreatesIssue(t *testing.T) {
	var requests []recordedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, recordedRequest{r.Method, r.URL.Path, string(body)})
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/o/r/issues":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/o/r/issues":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"number": 7}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	withGiteaURL(t, server.URL)
	withEnv(t, "tok", "")

	out, err := runRoomAction(roomArgs{Action: "open", Owner: "o", Repo: "r", Branch: "feat/foo"})
	if err != nil {
		t.Fatalf("room open failed: %v", err)
	}
	if !strings.Contains(out, "#7") {
		t.Errorf("unexpected output: %q", out)
	}
	if len(requests) != 2 {
		t.Fatalf("expected GET then POST, got %d requests: %+v", len(requests), requests)
	}
	if requests[0].Method != "GET" || requests[1].Method != "POST" {
		t.Errorf("unexpected request sequence: %+v", requests)
	}
	// The created issue carries the room marker for the branch.
	var created struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.Unmarshal([]byte(requests[1].Body), &created); err != nil {
		t.Fatalf("create body is not JSON: %v", err)
	}
	if created.Title != "Room: feat/foo" {
		t.Errorf("title = %q", created.Title)
	}
	m, ok := robotroom.ParseMarker(created.Body)
	if !ok || m.Branch != "feat/foo" {
		t.Errorf("created body has no valid room marker: %q", created.Body)
	}
}

// TestRoomOpenIdempotent verifies a second `room open` does not create a
// duplicate when the marker is already present.
func TestRoomOpenIdempotent(t *testing.T) {
	existing := robotroom.IssueBody("feat/foo", "")
	issueJSON, _ := json.Marshal([]map[string]any{
		{"number": 5, "body": existing, "state": "open"},
	})
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
		}
		_, _ = w.Write(issueJSON)
	}))
	defer server.Close()
	withGiteaURL(t, server.URL)
	withEnv(t, "tok", "")

	out, err := runRoomAction(roomArgs{Action: "open", Owner: "o", Repo: "r", Branch: "feat/foo"})
	if err != nil {
		t.Fatalf("room open failed: %v", err)
	}
	if !strings.Contains(out, "already exists") || !strings.Contains(out, "#5") {
		t.Errorf("unexpected output: %q", out)
	}
	if posts != 0 {
		t.Errorf("room open created a duplicate: %d POSTs", posts)
	}
}

// TestRoomStatusPostsComment verifies `room status` finds the room and posts
// exactly one comment on it.
func TestRoomStatusPostsComment(t *testing.T) {
	existing := robotroom.IssueBody("feat/foo", "")
	var commentPath, commentBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/o/r/issues":
			issueJSON, _ := json.Marshal([]map[string]any{{"number": 5, "body": existing, "state": "open"}})
			_, _ = w.Write(issueJSON)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			commentPath = r.URL.Path
			body, _ := io.ReadAll(r.Body)
			commentBody = string(body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": 1}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	withGiteaURL(t, server.URL)
	withEnv(t, "tok", "")

	out, err := runRoomAction(roomArgs{
		Action: "status", Owner: "o", Repo: "r", Branch: "feat/foo",
		State: "success", Context: "ci/test", SHA: "abc123",
	})
	if err != nil {
		t.Fatalf("room status failed: %v", err)
	}
	if !strings.Contains(out, "#5") {
		t.Errorf("unexpected output: %q", out)
	}
	if commentPath != "/api/v1/repos/o/r/issues/5/comments" {
		t.Errorf("comment went to %q", commentPath)
	}
	var posted struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(commentBody), &posted); err != nil {
		t.Fatalf("comment body is not JSON: %v", err)
	}
	if !strings.Contains(posted.Body, "**success**") || !strings.Contains(posted.Body, "ci/test") {
		t.Errorf("comment body = %q", posted.Body)
	}
}

// TestRoomStatusWithoutRoom verifies `room status` fails cleanly when no room
// exists for the branch.
func TestRoomStatusWithoutRoom(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	withGiteaURL(t, server.URL)
	withEnv(t, "tok", "")

	_, err := runRoomAction(roomArgs{Action: "status", Owner: "o", Repo: "r", Branch: "feat/foo", State: "success"})
	if err == nil || !strings.Contains(err.Error(), "no open room") {
		t.Errorf("expected 'no open room' error, got %v", err)
	}
}

// TestRoomClose verifies `room close` PATCHes the room issue closed.
func TestRoomClose(t *testing.T) {
	existing := robotroom.IssueBody("feat/foo", "")
	var patchPath, patchBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/o/r/issues":
			issueJSON, _ := json.Marshal([]map[string]any{{"number": 5, "body": existing, "state": "open"}})
			_, _ = w.Write(issueJSON)
		case r.Method == http.MethodPatch:
			patchPath = r.URL.Path
			body, _ := io.ReadAll(r.Body)
			patchBody = string(body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"number": 5, "state": "closed"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	withGiteaURL(t, server.URL)
	withEnv(t, "tok", "")

	out, err := runRoomAction(roomArgs{Action: "close", Owner: "o", Repo: "r", Branch: "feat/foo"})
	if err != nil {
		t.Fatalf("room close failed: %v", err)
	}
	if !strings.Contains(out, "#5") {
		t.Errorf("unexpected output: %q", out)
	}
	if patchPath != "/api/v1/repos/o/r/issues/5" {
		t.Errorf("PATCH went to %q", patchPath)
	}
	if !strings.Contains(patchBody, `"closed"`) {
		t.Errorf("PATCH body = %q", patchBody)
	}
}

// TestRoomOpenUnparseableCreateResponse verifies a create response that fails
// to parse surfaces an error instead of a bogus "issue #0" success.
func TestRoomOpenUnparseableCreateResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`not json`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	withGiteaURL(t, server.URL)
	withEnv(t, "tok", "")

	_, err := runRoomAction(roomArgs{Action: "open", Owner: "o", Repo: "r", Branch: "feat/foo"})
	if err == nil || !strings.Contains(err.Error(), "could not be parsed") {
		t.Errorf("expected parse error, got %v", err)
	}
}

// TestRoomArgValidation covers the room subcommand's argument validation:
// required flags, the feat/* restriction, and unknown actions.
func TestRoomArgValidation(t *testing.T) {
	tests := []struct {
		name    string
		args    roomArgs
		wantErr string
	}{
		{"missing owner", roomArgs{Action: "open", Repo: "r", Branch: "feat/foo"}, "required"},
		{"missing repo", roomArgs{Action: "open", Owner: "o", Branch: "feat/foo"}, "required"},
		{"missing branch", roomArgs{Action: "open", Owner: "o", Repo: "r"}, "required"},
		{"non-feat branch", roomArgs{Action: "open", Owner: "o", Repo: "r", Branch: "main"}, "feat/*"},
		{"unknown action", roomArgs{Action: "reopen", Owner: "o", Repo: "r", Branch: "feat/foo"}, "unknown room action"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := runRoomAction(tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("runRoomAction() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestHandleRoomToolValidation covers the MCP room tool's argument handling.
func TestHandleRoomToolValidation(t *testing.T) {
	// Invalid JSON (jsonRawMessagePtr from main_test.go avoids a second
	// encoding/json import here; modules/json has no RawMessage type)
	resp := handleRoomTool(*jsonRawMessagePtr(`{invalid`), jsonRawMessagePtr(`1`))
	errResp, ok := resp.(MCPErrorResponse)
	if !ok {
		t.Fatalf("expected MCPErrorResponse, got %T", resp)
	}
	if errResp.Error.Code != -32602 {
		t.Errorf("error code = %d, want -32602", errResp.Error.Code)
	}

	// Missing branch is invalid params (-32602), not an internal error
	resp = handleRoomTool(*jsonRawMessagePtr(`{"action":"open","owner":"o","repo":"r"}`), jsonRawMessagePtr(`2`))
	errResp, ok = resp.(MCPErrorResponse)
	if !ok {
		t.Fatalf("expected MCPErrorResponse, got %T", resp)
	}
	if errResp.Error.Code != -32602 {
		t.Errorf("error code = %d, want -32602", errResp.Error.Code)
	}
	if !strings.Contains(errResp.Error.Message, "required") {
		t.Errorf("error message = %q", errResp.Error.Message)
	}

	// A non-feat branch is invalid params too
	resp = handleRoomTool(*jsonRawMessagePtr(`{"action":"open","owner":"o","repo":"r","branch":"main"}`), jsonRawMessagePtr(`3`))
	errResp, ok = resp.(MCPErrorResponse)
	if !ok {
		t.Fatalf("expected MCPErrorResponse, got %T", resp)
	}
	if errResp.Error.Code != -32602 {
		t.Errorf("error code = %d, want -32602", errResp.Error.Code)
	}
}

// TestHandleRoomToolInternalError verifies a server-side failure (valid
// arguments, unreachable API) surfaces as -32603, distinct from -32602.
func TestHandleRoomToolInternalError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	server.Close() // immediately unreachable
	withGiteaURL(t, server.URL)
	withEnv(t, "tok", "")

	resp := handleRoomTool(
		*jsonRawMessagePtr(`{"action":"open","owner":"o","repo":"r","branch":"feat/foo"}`),
		jsonRawMessagePtr(`4`))
	errResp, ok := resp.(MCPErrorResponse)
	if !ok {
		t.Fatalf("expected MCPErrorResponse, got %T", resp)
	}
	if errResp.Error.Code != -32603 {
		t.Errorf("error code = %d, want -32603", errResp.Error.Code)
	}
}
