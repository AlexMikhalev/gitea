// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// gitea-robot CLI - thin wrapper for Gitea Robot API
// Usage: go run cmd/gitea-robot/main.go [command] [flags]

package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"code.gitea.io/gitea/modules/nostr"
	"code.gitea.io/gitea/modules/robotroom"
)

var (
	giteaURL   = os.Getenv("GITEA_URL")
	giteaToken = os.Getenv("GITEA_TOKEN")
	// giteaNostrKey is the agent's Nostr secret key, as an nsec or as 32-byte hex. When it is
	// set, mutations are signed with a NIP-98 event instead of carrying the bearer token.
	// When it is unset every request takes exactly the path it took before this existed.
	giteaNostrKey = os.Getenv("GITEA_NOSTR_KEY")
)

// nip98Kind is the NIP-98 "HTTP Auth" event kind.
const nip98Kind = 27235

// setRequestAuth sets the Authorization header for one API request.
//
// With GITEA_NOSTR_KEY set the request is authorized by a signature over this exact method, URL
// and body, so capturing the header does not let anyone make a different request. Without it,
// the bearer token is sent as before.
//
// It is applied to reads as well as writes on purpose. Signing only the mutations would leave
// the PAT in the environment, in every process listing that inherits it and on the wire on every
// GET - so an attacker who could read any of those would still hold an unscoped credential, and
// the guarantee above would be a property of the header rather than of the deployment. With a
// Nostr key configured the token is never read, and main() stops requiring one.
func setRequestAuth(req *http.Request, body string) error {
	if giteaNostrKey == "" {
		req.Header.Set("Authorization", "token "+giteaToken)
		return nil
	}
	header, err := nostrAuthHeader(giteaNostrKey, req.Method, req.URL.String(), body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", header)
	return nil
}

// nostrAuthHeader builds one NIP-98 `Authorization` header value.
func nostrAuthHeader(secretKey, method, rawURL, body string) (string, error) {
	sk, err := nostrSecretKey(secretKey)
	if err != nil {
		return "", err
	}

	nonce, err := nostrNonce()
	if err != nil {
		return "", err
	}

	tags := nostr.Tags{
		nostr.Tag{"u", rawURL},
		nostr.Tag{"method", strings.ToUpper(method)},
		// Every other input to this event is a pure function of the request and the current
		// whole second, and signing is deterministic - so without a nonce two identical
		// requests made inside one second would produce the same event id, and the server,
		// which spends each id exactly once, would refuse the second as a replay. The caller
		// would see an opaque 401 indistinguishable from a bad key.
		nostr.Tag{"nonce", nonce},
	}
	if body != "" {
		sum := sha256.Sum256([]byte(body))
		tags = append(tags, nostr.Tag{"payload", hex.EncodeToString(sum[:])})
	}

	event := nostr.Event{
		Kind:      nip98Kind,
		CreatedAt: nostr.Now(),
		Tags:      tags,
	}
	if err := event.Sign(sk); err != nil {
		return "", fmt.Errorf("cannot sign NIP-98 event: %w", err)
	}

	raw, err := json.Marshal(event)
	if err != nil {
		return "", fmt.Errorf("cannot serialize NIP-98 event: %w", err)
	}
	return "Nostr " + base64.StdEncoding.EncodeToString(raw), nil
}

// nostrNonce returns 16 bytes of hex from the system CSPRNG, used to make each signed event
// unique. A failure here is fatal rather than silently degrading to a predictable value: sending
// an event whose id an observer could have guessed would let them burn it before we do.
func nostrNonce() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("cannot generate a NIP-98 nonce: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// nostrSecretKey accepts either a NIP-19 nsec or a raw 32-byte hex secret key.
func nostrSecretKey(input string) (string, error) {
	input = strings.TrimSpace(input)
	if strings.HasPrefix(strings.ToLower(input), "nsec1") {
		hexKey, err := nostr.DecodeSecretKey(input)
		if err != nil {
			return "", errors.New("GITEA_NOSTR_KEY is not a valid nsec")
		}
		return hexKey, nil
	}
	if len(input) != nostr.KeyHexLength {
		return "", errors.New("GITEA_NOSTR_KEY must be an nsec or 64 hex characters")
	}
	if _, err := hex.DecodeString(input); err != nil {
		return "", errors.New("GITEA_NOSTR_KEY is not valid hex")
	}
	return strings.ToLower(input), nil
}

func main() {
	// Set default URL
	if giteaURL == "" {
		giteaURL = "http://localhost:3000"
	}

	// Handle help flags before checking for GITEA_TOKEN
	if len(os.Args) < 2 || os.Args[1] == "help" || os.Args[1] == "--help" || os.Args[1] == "-h" {
		printUsage()
		os.Exit(0)
	}

	// A credential is required, but either kind will do. Demanding GITEA_TOKEN even when a
	// Nostr key is configured would defeat the point of configuring one: the PAT would still
	// have to exist on the box for the process to start.
	if giteaToken == "" && giteaNostrKey == "" {
		fmt.Fprintln(os.Stderr, "Error: GITEA_TOKEN or GITEA_NOSTR_KEY environment variable required")
		os.Exit(1)
	}
	if giteaNostrKey != "" {
		// Fail here rather than on the first request, with the reason, instead of an opaque
		// 401 from the server after the key has silently not been used.
		if _, err := nostrSecretKey(giteaNostrKey); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	}

	command := os.Args[1]
	os.Args = os.Args[1:] // Remove command from args

	run, ok := commands[command]
	if !ok {
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", command)
		printUsage()
		os.Exit(1)
	}
	run()
}

// commands is the dispatch table. It is a map rather than a switch so that one test can
// assert every verb another component shells out to actually exists here - in particular
// the gitea-automations bridge's write leg (crates/bridge/src/robot.rs), whose own tests
// can only check that the bridge agrees with itself about the argv it builds.
var commands = map[string]func(){
	"triage":      triageCmd,
	"ready":       readyCmd,
	"graph":       graphCmd,
	"add-dep":     addDepCmd,
	"room":        roomCmd,
	"comment":     commentCmd,
	"edit-issue":  editIssueCmd,
	"create-pull": createPullCmd,
	"mcp-server":  mcpServerCmd,
}

func printUsage() {
	fmt.Println(`gitea-robot - CLI for Gitea Robot API

Usage:
  gitea-robot [command] [flags]

Commands:
  triage       Get prioritized task list
  ready        Get unblocked (ready) tasks
  graph        Get dependency graph
  add-dep      Add dependency between issues
  room         Manage a branch room issue: open|status|close
  comment      Post an issue comment
  edit-issue   Edit an issue: --add-labels adds labels, keeping the existing ones
  create-pull  Open a pull request (a no-op if one already exists for base/head)
  mcp-server   Start MCP server exposing gitea-robot functionality

Environment:
  GITEA_URL        Gitea instance URL (default: http://localhost:3000)
  GITEA_TOKEN      API token for authentication
  GITEA_NOSTR_KEY  Nostr secret key (nsec... or 64 hex characters) of a registered
                   agent. When set, every request is signed as a NIP-98 event
                   instead of carrying GITEA_TOKEN, and GITEA_TOKEN is not needed.
                   Register the matching public key first with
                   POST /api/v1/agent/keys.

                   The signature commits to the absolute URL, so GITEA_URL must
                   match the server's ROOT_URL exactly - scheme, host and port. If
                   they differ the server signs off on a different string than the
                   one it computes and rejects the request with a deliberately
                   opaque 401; the real reason is only in the server log at Debug
                   level. This is the most common cause of "invalid NIP-98
                   authorization". On a sub-path install, GITEA_URL includes the
                   sub-path (https://git.example/gitea).

                   A signed request body may not exceed 32 MiB, because the
                   signature covers a hash of the whole body and the server has to
                   buffer it to check that. This ceiling is the auth layer's own
                   and is independent of the instance's attachment and release
                   size limits; over it, the server answers 413 rather than 401.
                   Send anything larger with GITEA_TOKEN instead.

Examples:
  # Get triage report
  gitea-robot triage --owner terraphim --repo gitea

  # Get ready issues
  gitea-robot ready --owner terraphim --repo gitea

  # Add dependency: issue 2 blocked by issue 1
  gitea-robot add-dep --owner terraphim --repo gitea --issue 2 --blocks 1

  # Open the room for a feat branch (idempotent: it reopens a closed room
  # rather than creating a second one), post a CI status, close it
  gitea-robot room open --owner terraphim --repo gitea --branch feat/foo
  gitea-robot room status --owner terraphim --repo gitea --branch feat/foo --state success
  gitea-robot room close --owner terraphim --repo gitea --branch feat/foo

  # Write back to an issue: comment, add a label, open a pull request. These are the
  # verbs the gitea-automations bridge shells out to, so that its writes carry the
  # agent's NIP-98 identity rather than a bearer token.
  gitea-robot comment --owner terraphim --repo gitea --issue 57 --body "done"
  gitea-robot edit-issue --owner terraphim --repo gitea --issue 57 --add-labels status/blocked
  gitea-robot create-pull --owner terraphim --repo gitea --title "issue #57: daemon" \
      --head task/57-daemon --base main --body "Refs #57"

  # Start MCP server
  gitea-robot mcp-server`)
}

func triageCmd() {
	fs := flag.NewFlagSet("triage", flag.ExitOnError)
	owner := fs.String("owner", "", "Repository owner")
	repo := fs.String("repo", "", "Repository name")
	format := fs.String("format", "json", "Output format: json or markdown")
	fs.Parse(os.Args[1:])

	if *owner == "" || *repo == "" {
		fmt.Fprintln(os.Stderr, "Error: --owner and --repo required")
		fs.Usage()
		os.Exit(1)
	}

	url := fmt.Sprintf("%s/api/v1/robot/triage?owner=%s&repo=%s", giteaURL, *owner, *repo)
	data := apiGet(url)

	if *format == "json" {
		fmt.Println(data)
	} else {
		// Pretty print as markdown
		var result map[string]any
		json.Unmarshal([]byte(data), &result)
		printTriageMarkdown(result)
	}
}

func readyCmd() {
	fs := flag.NewFlagSet("ready", flag.ExitOnError)
	owner := fs.String("owner", "", "Repository owner")
	repo := fs.String("repo", "", "Repository name")
	fs.Parse(os.Args[1:])

	if *owner == "" || *repo == "" {
		fmt.Fprintln(os.Stderr, "Error: --owner and --repo required")
		fs.Usage()
		os.Exit(1)
	}

	url := fmt.Sprintf("%s/api/v1/robot/ready?owner=%s&repo=%s", giteaURL, *owner, *repo)
	data := apiGet(url)
	fmt.Println(data)
}

func graphCmd() {
	fs := flag.NewFlagSet("graph", flag.ExitOnError)
	owner := fs.String("owner", "", "Repository owner")
	repo := fs.String("repo", "", "Repository name")
	fs.Parse(os.Args[1:])

	if *owner == "" || *repo == "" {
		fmt.Fprintln(os.Stderr, "Error: --owner and --repo required")
		fs.Usage()
		os.Exit(1)
	}

	url := fmt.Sprintf("%s/api/v1/robot/graph?owner=%s&repo=%s", giteaURL, *owner, *repo)
	data := apiGet(url)
	fmt.Println(data)
}

func addDepCmd() {
	fs := flag.NewFlagSet("add-dep", flag.ExitOnError)
	owner := fs.String("owner", "", "Repository owner")
	repo := fs.String("repo", "", "Repository name")
	issue := fs.Int64("issue", 0, "Issue ID (the one being blocked)")
	blocks := fs.Int64("blocks", 0, "Issue ID that blocks this issue")
	relatesTo := fs.Int64("relates-to", 0, "Issue ID that relates to this issue")
	fs.Parse(os.Args[1:])

	if *owner == "" || *repo == "" || *issue == 0 {
		fmt.Fprintln(os.Stderr, "Error: --owner, --repo, and --issue required")
		fs.Usage()
		os.Exit(1)
	}

	depType := "blocks"
	dependsOn := *blocks
	if *relatesTo > 0 {
		depType = "relates_to"
		dependsOn = *relatesTo
	}
	if dependsOn == 0 {
		fmt.Fprintln(os.Stderr, "Error: --blocks or --relates-to required")
		os.Exit(1)
	}

	url := fmt.Sprintf("%s/api/v1/repos/%s/%s/issues/%d/dependencies", giteaURL, *owner, *repo, *issue)
	body := fmt.Sprintf(`{"depends_on": %d, "dep_type": "%s"}`, dependsOn, depType)

	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	if err := setRequestAuth(req, body); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusCreated {
		fmt.Println("✓ Dependency added successfully")
	} else {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "Error: %s\n%s\n", resp.Status, string(body))
		os.Exit(1)
	}
}

func apiGet(url string) string {
	data, err := apiGetSafe(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	return data
}

// apiGetSafe performs a GET request and returns error instead of calling os.Exit
func apiGetSafe(url string) (string, error) {
	return apiSendSafe("GET", url, "")
}

// apiSendSafe performs an HTTP request with an optional JSON body and returns
// error instead of calling os.Exit. GET requests send no body.
func apiSendSafe(method, url, body string) (string, error) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return "", fmt.Errorf("error creating request: %v", err)
	}

	if err := setRequestAuth(req, body); err != nil {
		return "", err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("error making request: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error reading response: %v", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("error: %s\n%s%s", resp.Status, string(respBody), authFailureHint(resp.StatusCode))
	}

	return string(respBody), nil
}

// authFailureHint explains what a signed request's 401 most likely means. The server refuses
// every NIP-98 failure with the same opaque message on purpose, so the only place this can be
// said is here, where we know a key was configured at all.
func authFailureHint(statusCode int) string {
	if statusCode != http.StatusUnauthorized || giteaNostrKey == "" {
		return ""
	}
	return fmt.Sprintf("\nThe request was signed with GITEA_NOSTR_KEY. Check that:\n"+
		"  - the public key is registered and not revoked (GET /api/v1/agent/keys)\n"+
		"  - GITEA_URL (%s) matches the server's ROOT_URL exactly - the signature\n"+
		"    commits to the absolute URL, so a mismatched scheme, host or port is refused\n"+
		"  - this machine's clock is within 60 seconds of the server's\n", giteaURL)
}

func printTriageMarkdown(result map[string]any) {
	fmt.Println("## Triage Report")
	fmt.Println()

	if quickRef, ok := result["quick_ref"].(map[string]any); ok {
		fmt.Printf("**Stats:** Total: %.0f, Open: %.0f, Blocked: %.0f, Ready: %.0f\n\n",
			quickRef["total"], quickRef["open"], quickRef["blocked"], quickRef["ready"])
	}

	if recs, ok := result["recommendations"].([]any); ok {
		fmt.Println("### Top Recommendations")
		for i, r := range recs {
			if i >= 5 {
				break
			}
			rec := r.(map[string]any)
			fmt.Printf("%d. **#%.0f: %s** (PageRank: %.4f)\n",
				i+1, rec["index"], rec["title"], rec["pagerank"])
		}
	}
}

// captureStdout captures the stdout of the given function and returns it as a string.
// It temporarily redirects os.Stdout to a temporary file, executes the function,
// restores os.Stdout, reads the temporary file, and returns its contents.
// Note: stderr is not captured and will go to the actual stderr of the process.
func captureStdout(fn func()) (string, error) {
	tmpfile, err := os.CreateTemp("", "mcp-tool-*.out")
	if err != nil {
		return "", err
	}
	// Ensure the temporary file is removed when done.
	defer os.Remove(tmpfile.Name())

	old := os.Stdout
	os.Stdout = tmpfile
	fn()
	os.Stdout = old

	// Close the file to flush content.
	if err := tmpfile.Close(); err != nil {
		return "", err
	}

	data, err := os.ReadFile(tmpfile.Name())
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// mcpServerCmd implements the MCP server functionality
func mcpServerCmd() {
	// Create buffered reader and writer for stdio communication
	reader := bufio.NewReader(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)

	// Process MCP messages in a loop
	for {
		// Read a line from stdin (MCP messages are newline-delimited JSON-RPC 2.0)
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				// End of input, exit gracefully
				return
			}
			fmt.Fprintf(os.Stderr, "Error reading from stdin: %v\n", err)
			os.Exit(1)
		}

		// Trim whitespace (including newline)
		line = strings.TrimSpace(line)
		if line == "" {
			// Skip empty lines
			continue
		}

		// Parse the JSON-RPC 2.0 request
		var req MCPRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			// Send parse error response
			resp := MCPErrorResponse{
				JSONRPC: "2.0",
				ID:      nil,
				Error: &MCPError{
					Code:    -32703, // Parse error
					Message: "Failed to parse JSON: " + err.Error(),
				},
			}
			sendResponse(writer, resp)
			continue
		}

		// Handle the request based on method
		var resp any
		switch req.Method {
		case "initialize":
			resp = handleInitialize(req)
		case "notifications/initialized":
			// This is a notification, no response needed
			continue
		case "tools/list":
			resp = handleToolsList(req)
		case "tools/call":
			resp = handleToolsCall(req)
		case "ping":
			resp = handlePing(req)
		default:
			// Method not found
			resp = MCPErrorResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error: &MCPError{
					Code:    -32601, // Method not found
					Message: "Method not found: " + req.Method,
				},
			}
		}

		// Send the response
		sendResponse(writer, resp)
	}
}

// sendResponse writes a JSON-RPC response to the writer
func sendResponse(writer *bufio.Writer, resp any) {
	data, err := json.Marshal(resp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshaling response: %v\n", err)
		os.Exit(1)
	}
	_, err = writer.Write(append(data, '\n'))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error writing response: %v\n", err)
		os.Exit(1)
	}
	err = writer.Flush()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error flushing writer: %v\n", err)
		os.Exit(1)
	}
}

// MCPRequest represents a JSON-RPC 2.0 request
type MCPRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"` // Can be string, number, or null
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

// MCPResponse represents a successful JSON-RPC 2.0 response
type MCPResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Result  any              `json:"result,omitempty"`
}

// MCPErrorResponse represents an error JSON-RPC 2.0 response
type MCPErrorResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Error   *MCPError        `json:"error,omitempty"`
}

// MCPError represents an error in JSON-RPC 2.0
type MCPError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// handleInitialize handles the initialize request
func handleInitialize(req MCPRequest) any {
	// Parse the protocol version from the request
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	protocolVersion := "2024-11-05"
	if err := json.Unmarshal(req.Params, &params); err == nil && params.ProtocolVersion != "" {
		protocolVersion = params.ProtocolVersion
	}

	return MCPResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]string{
				"name":    "gitea-robot",
				"version": "1.0.0",
			},
		},
	}
}

// handleToolsList returns the list of available tools
func handleToolsList(req MCPRequest) any {
	return MCPResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]any{
			"tools": []map[string]any{
				{
					"name":        "triage",
					"description": "Get prioritized task list with PageRank scores",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"owner": map[string]any{
								"type":        "string",
								"description": "Repository owner",
							},
							"repo": map[string]any{
								"type":        "string",
								"description": "Repository name",
							},
							"format": map[string]any{
								"type":        "string",
								"description": "Output format: json or markdown",
								"default":     "json",
							},
						},
						"required": []string{"owner", "repo"},
					},
				},
				{
					"name":        "ready",
					"description": "Get unblocked (ready) tasks",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"owner": map[string]any{
								"type":        "string",
								"description": "Repository owner",
							},
							"repo": map[string]any{
								"type":        "string",
								"description": "Repository name",
							},
						},
						"required": []string{"owner", "repo"},
					},
				},
				{
					"name":        "graph",
					"description": "Get dependency graph",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"owner": map[string]any{
								"type":        "string",
								"description": "Repository owner",
							},
							"repo": map[string]any{
								"type":        "string",
								"description": "Repository name",
							},
						},
						"required": []string{"owner", "repo"},
					},
				},
				{
					"name":        "add_dep",
					"description": "Add dependency between issues",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"owner": map[string]any{
								"type":        "string",
								"description": "Repository owner",
							},
							"repo": map[string]any{
								"type":        "string",
								"description": "Repository name",
							},
							"issue": map[string]any{
								"type":        "integer",
								"description": "Issue ID (the one being blocked)",
							},
							"blocks": map[string]any{
								"type":        "integer",
								"description": "Issue ID that blocks this issue",
							},
							"relates_to": map[string]any{
								"type":        "integer",
								"description": "Issue ID that relates to this issue",
							},
						},
						"required": []string{"owner", "repo", "issue"},
					},
				},
				{
					"name":        "room",
					"description": "Manage a branch room issue (open|status|close) for a feat/* branch",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"action": map[string]any{
								"type":        "string",
								"description": "Room action: open (creates the room, or reopens a closed one), status, or close",
								"enum":        []string{"open", "status", "close"},
							},
							"owner": map[string]any{
								"type":        "string",
								"description": "Repository owner",
							},
							"repo": map[string]any{
								"type":        "string",
								"description": "Repository name",
							},
							"branch": map[string]any{
								"type":        "string",
								"description": "Branch name (feat/* only)",
							},
							"head": map[string]any{
								"type":        "string",
								"description": "Branch head SHA recorded in the room marker (open only)",
							},
							"state": map[string]any{
								"type":        "string",
								"description": "CI state: pending, success, error, or failure (status only)",
							},
							"context": map[string]any{
								"type":        "string",
								"description": "CI context name (status only)",
							},
							"sha": map[string]any{
								"type":        "string",
								"description": "Commit SHA the status is for (status only)",
							},
							"target_url": map[string]any{
								"type":        "string",
								"description": "Link to CI details (status only)",
							},
							"description": map[string]any{
								"type":        "string",
								"description": "Short status description (status only)",
							},
						},
						"required": []string{"action", "owner", "repo", "branch"},
					},
				},
			},
		},
	}
}

// handleToolsCall handles tool execution requests
func handleToolsCall(req MCPRequest) any {
	// Parse the params to get tool name and arguments
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return MCPErrorResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &MCPError{
				Code:    -32602, // Invalid params
				Message: "Invalid arguments: " + err.Error(),
			},
		}
	}

	// Handle the tool call based on name
	switch params.Name {
	case "triage":
		return handleTriageTool(params.Arguments, req.ID)
	case "ready":
		return handleReadyTool(params.Arguments, req.ID)
	case "graph":
		return handleGraphTool(params.Arguments, req.ID)
	case "add_dep":
		return handleAddDepTool(params.Arguments, req.ID)
	case "room":
		return handleRoomTool(params.Arguments, req.ID)
	default:
		return MCPErrorResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &MCPError{
				Code:    -32601, // Method not found
				Message: "Tool not found: " + params.Name,
			},
		}
	}
}

// mcpInternalError turns a failed API call into a JSON-RPC internal error.
// Inside mcp-server the alternative is os.Exit (apiGet), which would take the
// whole server process down on a transient 500 or a network blip instead of
// failing the one tool call the client made.
func mcpInternalError(id *json.RawMessage, tool string, err error) MCPErrorResponse {
	return MCPErrorResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &MCPError{
			Code:    -32603, // Internal error
			Message: tool + " failed: " + err.Error(),
		},
	}
}

// handleTriageTool executes the triage tool
func handleTriageTool(args json.RawMessage, id *json.RawMessage) any {
	// Parse arguments
	var argsStruct struct {
		Owner  *string `json:"owner,omitempty"`
		Repo   *string `json:"repo,omitempty"`
		Format *string `json:"format,omitempty"`
	}
	if err := json.Unmarshal(args, &argsStruct); err != nil {
		return MCPErrorResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error: &MCPError{
				Code:    -32602, // Invalid params
				Message: "Invalid arguments for triage: " + err.Error(),
			},
		}
	}

	// Validate required arguments
	if argsStruct.Owner == nil || *argsStruct.Owner == "" {
		return MCPErrorResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error: &MCPError{
				Code:    -32602, // Invalid params
				Message: "Missing required argument: owner",
			},
		}
	}
	if argsStruct.Repo == nil || *argsStruct.Repo == "" {
		return MCPErrorResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error: &MCPError{
				Code:    -32602, // Invalid params
				Message: "Missing required argument: repo",
			},
		}
	}

	// Set format with default
	format := "json"
	if argsStruct.Format != nil && *argsStruct.Format != "" {
		format = *argsStruct.Format
	}

	// Call API directly instead of using triageCmd to avoid os.Exit()
	url := fmt.Sprintf("%s/api/v1/robot/triage?owner=%s&repo=%s", giteaURL, *argsStruct.Owner, *argsStruct.Repo)
	output, err := apiGetSafe(url)
	if err != nil {
		return mcpInternalError(id, "triage", err)
	}

	// For markdown format, we would need to parse and format the JSON
	// For now, return JSON regardless of format parameter
	_ = format

	// Return the output as the result
	return MCPResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  output,
	}
}

// handleReadyTool executes the ready tool
func handleReadyTool(args json.RawMessage, id *json.RawMessage) any {
	// Parse arguments
	var argsStruct struct {
		Owner *string `json:"owner,omitempty"`
		Repo  *string `json:"repo,omitempty"`
	}
	if err := json.Unmarshal(args, &argsStruct); err != nil {
		return MCPErrorResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error: &MCPError{
				Code:    -32602, // Invalid params
				Message: "Invalid arguments for ready: " + err.Error(),
			},
		}
	}

	// Validate required arguments
	if argsStruct.Owner == nil || *argsStruct.Owner == "" {
		return MCPErrorResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error: &MCPError{
				Code:    -32602, // Invalid params
				Message: "Missing required argument: owner",
			},
		}
	}
	if argsStruct.Repo == nil || *argsStruct.Repo == "" {
		return MCPErrorResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error: &MCPError{
				Code:    -32602, // Invalid params
				Message: "Missing required argument: repo",
			},
		}
	}

	// Call API directly instead of using readyCmd to avoid os.Exit()
	url := fmt.Sprintf("%s/api/v1/robot/ready?owner=%s&repo=%s", giteaURL, *argsStruct.Owner, *argsStruct.Repo)
	output, err := apiGetSafe(url)
	if err != nil {
		return mcpInternalError(id, "ready", err)
	}

	// Return the output as the result
	return MCPResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  output,
	}
}

// handleGraphTool executes the graph tool
func handleGraphTool(args json.RawMessage, id *json.RawMessage) any {
	// Parse arguments
	var argsStruct struct {
		Owner *string `json:"owner,omitempty"`
		Repo  *string `json:"repo,omitempty"`
	}
	if err := json.Unmarshal(args, &argsStruct); err != nil {
		return MCPErrorResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error: &MCPError{
				Code:    -32602, // Invalid params
				Message: "Invalid arguments for graph: " + err.Error(),
			},
		}
	}

	// Validate required arguments
	if argsStruct.Owner == nil || *argsStruct.Owner == "" {
		return MCPErrorResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error: &MCPError{
				Code:    -32602, // Invalid params
				Message: "Missing required argument: owner",
			},
		}
	}
	if argsStruct.Repo == nil || *argsStruct.Repo == "" {
		return MCPErrorResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error: &MCPError{
				Code:    -32602, // Invalid params
				Message: "Missing required argument: repo",
			},
		}
	}

	// Call API directly instead of using graphCmd to avoid os.Exit()
	url := fmt.Sprintf("%s/api/v1/robot/graph?owner=%s&repo=%s", giteaURL, *argsStruct.Owner, *argsStruct.Repo)
	output, err := apiGetSafe(url)
	if err != nil {
		return mcpInternalError(id, "graph", err)
	}

	// Return the output as the result
	return MCPResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  output,
	}
}

// handleAddDepTool executes the add-dep tool
func handleAddDepTool(args json.RawMessage, id *json.RawMessage) any {
	// Parse arguments
	var argsStruct struct {
		Owner     *string `json:"owner,omitempty"`
		Repo      *string `json:"repo,omitempty"`
		Issue     *int64  `json:"issue,omitempty"`
		Blocks    *int64  `json:"blocks,omitempty"`
		RelatesTo *int64  `json:"relates_to,omitempty"`
	}
	if err := json.Unmarshal(args, &argsStruct); err != nil {
		return MCPErrorResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error: &MCPError{
				Code:    -32602, // Invalid params
				Message: "Invalid arguments for add_dep: " + err.Error(),
			},
		}
	}

	// Validate required arguments
	if argsStruct.Owner == nil || *argsStruct.Owner == "" {
		return MCPErrorResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error: &MCPError{
				Code:    -32602, // Invalid params
				Message: "Missing required argument: owner",
			},
		}
	}
	if argsStruct.Repo == nil || *argsStruct.Repo == "" {
		return MCPErrorResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error: &MCPError{
				Code:    -32602, // Invalid params
				Message: "Missing required argument: repo",
			},
		}
	}
	if argsStruct.Issue == nil || *argsStruct.Issue == 0 {
		return MCPErrorResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error: &MCPError{
				Code:    -32602, // Invalid params
				Message: "Missing required argument: issue",
			},
		}
	}

	// Validate that either blocks or relates_to is provided
	if argsStruct.Blocks == nil && argsStruct.RelatesTo == nil {
		return MCPErrorResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error: &MCPError{
				Code:    -32602, // Invalid params
				Message: "Missing required argument: either blocks or relates_to must be provided",
			},
		}
	}

	// Call API directly using safe version that doesn't call os.Exit
	depType := "blocks"
	dependsOn := int64(0)
	if argsStruct.Blocks != nil {
		dependsOn = *argsStruct.Blocks
	} else if argsStruct.RelatesTo != nil {
		depType = "relates_to"
		dependsOn = *argsStruct.RelatesTo
	}

	url := fmt.Sprintf("%s/api/v1/repos/%s/%s/issues/%d/dependencies", giteaURL, *argsStruct.Owner, *argsStruct.Repo, *argsStruct.Issue)
	body := fmt.Sprintf(`{"depends_on": %d, "dep_type": "%s"}`, dependsOn, depType)

	output, err := apiPostSafe(url, body)
	if err != nil {
		return MCPErrorResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error: &MCPError{
				Code:    -32603, // Internal error
				Message: err.Error(),
			},
		}
	}

	// Return the output as the result
	return MCPResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  output,
	}
}

// apiPostSafe performs a POST request and returns error instead of calling os.Exit
func apiPostSafe(url, body string) (string, error) {
	return apiSendSafe("POST", url, body)
}

// handlePing handles ping requests
func handlePing(req MCPRequest) any {
	return MCPResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  map[string]string{},
	}
}

// The room marker, room issue body and status comment shape come from
// modules/robotroom, shared with the server-side room hook
// (routers/api/v1/robot): the hook and this CLI operate on the same rooms,
// and the shared builders keep the two sides byte-identical.

// roomIssueListEntry is the subset of the API issue shape the room commands need.
type roomIssueListEntry struct {
	Index int64  `json:"number"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

const (
	// roomIssuePageSize is the page size *requested* when listing issues. The
	// server clamps it to MAX_RESPONSE_ITEMS (default 50, operator-tunable),
	// so a short page says nothing about whether more pages follow.
	roomIssuePageSize = 50
	// roomIssuePageLimit stops the paging loop from running forever against a
	// server that never returns an empty page. Reaching it is an error, not a
	// silent "no room found": a wrong answer here duplicates rooms.
	roomIssuePageLimit = 200
)

// roomFindIssue locates the open room issue for branch, paging through the
// repo's open issues. It returns the issue index and whether a room exists.
func roomFindIssue(owner, repo, branch string) (int64, bool, error) {
	return roomFindIssueInState(owner, repo, branch, "open")
}

// roomFindClosedIssue locates the closed room issue for branch. The issue list
// is sorted newest-first by the API (SortByCreatedDesc, routers/api/v1/repo),
// so the first match is the newest closed room - the one a re-open should
// revive, same rule as the hook's findRoomIssueInState.
func roomFindClosedIssue(owner, repo, branch string) (int64, bool, error) {
	return roomFindIssueInState(owner, repo, branch, "closed")
}

// roomFindIssueInState locates the room issue for branch among the repo's
// issues in the given state, paging through the list. It returns the issue
// index and whether such a room exists.
func roomFindIssueInState(owner, repo, branch, state string) (int64, bool, error) {
	for page := 1; page <= roomIssuePageLimit; page++ {
		url := fmt.Sprintf("%s/api/v1/repos/%s/%s/issues?state=%s&type=issues&limit=%d&page=%d",
			giteaURL, owner, repo, state, roomIssuePageSize, page)
		data, err := apiGetSafe(url)
		if err != nil {
			return 0, false, err
		}
		var issues []roomIssueListEntry
		if err := json.Unmarshal([]byte(data), &issues); err != nil {
			return 0, false, fmt.Errorf("error parsing issue list: %v", err)
		}
		for _, issue := range issues {
			// The same predicate the server-side hook matches rooms with, so
			// the two sides can never operate on different issues for one
			// branch.
			if robotroom.IsRoomFor(issue.Title, issue.Body, branch) {
				return issue.Index, true, nil
			}
		}
		// Only an empty page ends the walk. A page shorter than requested is
		// what a server with a lower MAX_RESPONSE_ITEMS returns for every
		// page, so treating it as the last one would stop after the first.
		if len(issues) == 0 {
			return 0, false, nil
		}
	}
	return 0, false, fmt.Errorf("scanned %d pages of %s issues in %s/%s without reaching the end of the list; refusing to report 'no room' for %s",
		roomIssuePageLimit, state, owner, repo, branch)
}

// roomOpenOutcome reports what `room open` did to the branch's room. It
// mirrors the hook's roomOpenResult (routers/api/v1/robot/room.go).
type roomOpenOutcome int

const (
	// roomOpenExisting: the branch already had an open room.
	roomOpenExisting roomOpenOutcome = iota
	// roomOpenCreated: the branch had never had a room.
	roomOpenCreated
	// roomOpenReopened: the branch's closed room was revived.
	roomOpenReopened
)

// roomOpen makes the branch's room current: it leaves an open room alone,
// reopens a closed one, and creates an issue only when the branch has never
// had a room. It reports which of the three happened and the room's index.
//
// Reopening rather than creating is what keeps "exactly one room issue per
// branch" true across a branch's whole life, on this side exactly as on the
// hook's (openRoom, routers/api/v1/robot/room.go). Without it, every branch
// whose room the hook had closed - on merge, on delete - would get a second,
// byte-identically titled room the next time an operator ran `room open`, and
// the two sides would no longer agree on which issue is the branch's room.
// The room body is left as it is, including its recorded marker head: the CLI
// never rewrites a room it did not create, for a reopened room as for an
// already-open one.
//
// Like the server side, idempotency is search-then-create with no unique
// constraint behind it; two concurrent opens for the same branch can create
// duplicates. The hook's deliveries are serialized per webhook, which keeps
// the window theoretical in practice.
func roomOpen(owner, repo, branch, head string) (roomOpenOutcome, int64, error) {
	if index, found, err := roomFindIssue(owner, repo, branch); err != nil || found {
		return roomOpenExisting, index, err
	}
	index, found, err := roomFindClosedIssue(owner, repo, branch)
	if err != nil {
		return roomOpenExisting, 0, err
	}
	if found {
		if err := roomReopen(owner, repo, index); err != nil {
			return roomOpenExisting, 0, err
		}
		return roomOpenReopened, index, nil
	}

	url := fmt.Sprintf("%s/api/v1/repos/%s/%s/issues", giteaURL, owner, repo)
	body := fmt.Sprintf(`{"title": %s, "body": %s}`,
		jsonString(robotroom.IssueTitle(branch)), jsonString(robotroom.IssueBody(branch, head)))
	data, err := apiPostSafe(url, body)
	if err != nil {
		return roomOpenExisting, 0, err
	}
	var created roomIssueListEntry
	if err := json.Unmarshal([]byte(data), &created); err != nil {
		// The room was created server-side but its index is unknown; surface
		// the failure instead of reporting a bogus "issue #0". A retry is
		// safe: roomFindIssue finds the room just created.
		return roomOpenCreated, 0, fmt.Errorf("room issue created but its response could not be parsed: %v", err)
	}
	return roomOpenCreated, created.Index, nil
}

// roomReopen reopens a closed room issue.
func roomReopen(owner, repo string, index int64) error {
	url := fmt.Sprintf("%s/api/v1/repos/%s/%s/issues/%d", giteaURL, owner, repo, index)
	_, err := apiSendSafe("PATCH", url, `{"state": "open"}`)
	return err
}

// roomStatus posts one CI status comment on the room issue.
func roomStatus(owner, repo, branch, state, context, sha, targetURL, description string) (int64, error) {
	index, found, err := roomFindIssue(owner, repo, branch)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("no open room for branch %s (open it first with 'room open')", branch)
	}
	url := fmt.Sprintf("%s/api/v1/repos/%s/%s/issues/%d/comments", giteaURL, owner, repo, index)
	body := fmt.Sprintf(`{"body": %s}`, jsonString(robotroom.StatusComment(state, context, sha, targetURL, description)))
	if _, err := apiPostSafe(url, body); err != nil {
		return 0, err
	}
	return index, nil
}

// roomClose closes the room issue.
func roomClose(owner, repo, branch string) (int64, error) {
	index, found, err := roomFindIssue(owner, repo, branch)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("no open room for branch %s", branch)
	}
	url := fmt.Sprintf("%s/api/v1/repos/%s/%s/issues/%d", giteaURL, owner, repo, index)
	if _, err := apiSendSafe("PATCH", url, `{"state": "closed"}`); err != nil {
		return 0, err
	}
	return index, nil
}

// jsonString renders s as a JSON string literal.
func jsonString(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}

// roomArgs holds the flags shared by all room actions.
type roomArgs struct {
	Action      string
	Owner       string
	Repo        string
	Branch      string
	Head        string
	State       string
	Context     string
	SHA         string
	TargetURL   string
	Description string
}

// validateRoomArgs checks the flags shared by all room actions. It is split
// out of runRoomAction so the MCP tool can report invalid parameters as
// -32602 (invalid params) rather than -32603 (internal error).
func validateRoomArgs(a roomArgs) error {
	if a.Owner == "" || a.Repo == "" || a.Branch == "" {
		return errors.New("--owner, --repo, and --branch required")
	}
	if !strings.HasPrefix(a.Branch, "feat/") {
		return fmt.Errorf("branch %q is not a feat/* branch; rooms only exist for feat/* branches", a.Branch)
	}
	switch a.Action {
	case "open", "status", "close":
		return nil
	default:
		return fmt.Errorf("unknown room action %q (want open|status|close)", a.Action)
	}
}

// runRoomAction executes one room action and returns a human-readable result.
func runRoomAction(a roomArgs) (string, error) {
	if err := validateRoomArgs(a); err != nil {
		return "", err
	}
	switch a.Action {
	case "open":
		outcome, index, err := roomOpen(a.Owner, a.Repo, a.Branch, a.Head)
		if err != nil {
			return "", err
		}
		switch outcome {
		case roomOpenCreated:
			return fmt.Sprintf("✓ Room opened: issue #%d for %s", index, a.Branch), nil
		case roomOpenReopened:
			return fmt.Sprintf("✓ Room reopened: issue #%d for %s", index, a.Branch), nil
		default:
			return fmt.Sprintf("Room already exists: issue #%d for %s", index, a.Branch), nil
		}
	case "status":
		state := a.State
		if state == "" {
			state = "pending"
		}
		index, err := roomStatus(a.Owner, a.Repo, a.Branch, state, a.Context, a.SHA, a.TargetURL, a.Description)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("✓ Status %q posted on room issue #%d for %s", state, index, a.Branch), nil
	case "close":
		index, err := roomClose(a.Owner, a.Repo, a.Branch)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("✓ Room closed: issue #%d for %s", index, a.Branch), nil
	default:
		return "", fmt.Errorf("unknown room action %q (want open|status|close)", a.Action)
	}
}

// roomCmd implements `gitea-robot room open|status|close --owner X --repo Y --branch feat/foo`,
// the manual/ops path for the operations the room hook drives automatically.
func roomCmd() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "Error: room action required: open|status|close")
		os.Exit(1)
	}
	action := os.Args[1]

	fs := flag.NewFlagSet("room "+action, flag.ExitOnError)
	owner := fs.String("owner", "", "Repository owner")
	repo := fs.String("repo", "", "Repository name")
	branch := fs.String("branch", "", "Branch name (feat/* only)")
	head := fs.String("head", "", "Branch head SHA recorded in the room marker (open only)")
	state := fs.String("state", "pending", "CI state: pending|success|error|failure (status only)")
	context := fs.String("context", "", "CI context name (status only)")
	sha := fs.String("sha", "", "Commit SHA the status is for (status only)")
	targetURL := fs.String("target-url", "", "Link to CI details (status only)")
	description := fs.String("description", "", "Short status description (status only)")
	// Parse only errors on bad flags, and ExitOnError turns that into os.Exit
	// with the usage message, so the error itself never leaves this call.
	_ = fs.Parse(os.Args[2:])

	out, err := runRoomAction(roomArgs{
		Action: action, Owner: *owner, Repo: *repo, Branch: *branch,
		Head: *head, State: *state, Context: *context, SHA: *sha,
		TargetURL: *targetURL, Description: *description,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		fs.Usage()
		os.Exit(1)
	}
	fmt.Println(out)
}

// handleRoomTool executes the room MCP tool
func handleRoomTool(args json.RawMessage, id *json.RawMessage) any {
	var argsStruct struct {
		Action      *string `json:"action,omitempty"`
		Owner       *string `json:"owner,omitempty"`
		Repo        *string `json:"repo,omitempty"`
		Branch      *string `json:"branch,omitempty"`
		Head        *string `json:"head,omitempty"`
		State       *string `json:"state,omitempty"`
		Context     *string `json:"context,omitempty"`
		SHA         *string `json:"sha,omitempty"`
		TargetURL   *string `json:"target_url,omitempty"`
		Description *string `json:"description,omitempty"`
	}
	if err := json.Unmarshal(args, &argsStruct); err != nil {
		return MCPErrorResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error: &MCPError{
				Code:    -32602,
				Message: "Invalid arguments for room: " + err.Error(),
			},
		}
	}

	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}

	a := roomArgs{
		Action: str(argsStruct.Action), Owner: str(argsStruct.Owner), Repo: str(argsStruct.Repo),
		Branch: str(argsStruct.Branch), Head: str(argsStruct.Head), State: str(argsStruct.State),
		Context: str(argsStruct.Context), SHA: str(argsStruct.SHA),
		TargetURL: str(argsStruct.TargetURL), Description: str(argsStruct.Description),
	}
	// Argument validation is a client error (-32602), not an internal one.
	if err := validateRoomArgs(a); err != nil {
		return MCPErrorResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error: &MCPError{
				Code:    -32602,
				Message: "Invalid arguments for room: " + err.Error(),
			},
		}
	}

	out, err := runRoomAction(a)
	if err != nil {
		return MCPErrorResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error: &MCPError{
				Code:    -32603,
				Message: err.Error(),
			},
		}
	}

	return MCPResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  out,
	}
}
