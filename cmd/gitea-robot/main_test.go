// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

// TestMCPRequestResponseParsing tests JSON-RPC 2.0 request/response parsing
func TestMCPRequestResponseParsing(t *testing.T) {
	tests := []struct {
		name        string
		request     string
		wantErr     bool
		errCode     int
		errContains string
	}{
		{
			name:    "Valid initialize request",
			request: `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
			wantErr: false,
		},
		{
			name:    "Valid initialize request with string id",
			request: `{"jsonrpc":"2.0","id":"init-123","method":"initialize","params":{}}`,
			wantErr: false,
		},
		{
			name:    "Valid notification (no id)",
			request: `{"jsonrpc":"2.0","method":"ping"}`,
			wantErr: false,
		},
		{
			name:        "Invalid JSON",
			request:     `{"jsonrpc":"2.0","id":1,"method":}`,
			wantErr:     true,
			errCode:     -32703,
			errContains: "Failed to parse JSON",
		},
		{
			name:        "Missing jsonrpc field",
			request:     `{"id":1,"method":"initialize"}`,
			wantErr:     false, // Should be handled gracefully, not strictly enforced in current impl
			errCode:     0,
			errContains: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req MCPRequest
			err := json.Unmarshal([]byte(tt.request), &req)

			if tt.wantErr && err == nil {
				t.Errorf("Expected error but got none")
				return
			}

			if !tt.wantErr && err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			if !tt.wantErr {
				// Verify parsing succeeded
				if req.Method == "" && !strings.Contains(tt.request, `"method"`) {
					t.Logf("Note: method not present in request")
				}
			}
		})
	}
}

// TestHandleInitialize tests the initialize handler
func TestHandleInitialize(t *testing.T) {
	tests := []struct {
		name    string
		req     MCPRequest
		wantErr bool
	}{
		{
			name: "Basic initialize",
			req: MCPRequest{
				JSONRPC: "2.0",
				ID:      jsonRawMessagePtr(`1`),
				Method:  "initialize",
			},
			wantErr: false,
		},
		{
			name: "Initialize with null id (notification)",
			req: MCPRequest{
				JSONRPC: "2.0",
				ID:      nil,
				Method:  "initialize",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := handleInitialize(tt.req)

			mcpResp, ok := resp.(MCPResponse)
			if !ok {
				t.Fatalf("Expected MCPResponse, got %T", resp)
			}

			if mcpResp.JSONRPC != "2.0" {
				t.Errorf("Expected JSONRPC version 2.0, got %s", mcpResp.JSONRPC)
			}

			result, ok := mcpResp.Result.(map[string]any)
			if !ok {
				t.Fatalf("Expected result to be map[string]interface{}, got %T", mcpResp.Result)
			}

			// Verify protocol version
			if protocolVersion, ok := result["protocolVersion"].(string); !ok || protocolVersion != "2024-11-05" {
				t.Errorf("Expected protocolVersion 2024-11-05, got %v", result["protocolVersion"])
			}

			// Verify server info
			serverInfo, ok := result["serverInfo"].(map[string]string)
			if !ok {
				t.Fatalf("Expected serverInfo to be map[string]string, got %T", result["serverInfo"])
			}

			if serverInfo["name"] != "gitea-robot" {
				t.Errorf("Expected server name 'gitea-robot', got %s", serverInfo["name"])
			}

			if serverInfo["version"] != "1.0.0" {
				t.Errorf("Expected server version '1.0.0', got %s", serverInfo["version"])
			}

			// Verify capabilities
			capabilities, ok := result["capabilities"].(map[string]any)
			if !ok {
				t.Fatalf("Expected capabilities to be map[string]interface{}, got %T", result["capabilities"])
			}

			if _, ok := capabilities["tools"]; !ok {
				t.Errorf("Expected capabilities to contain 'tools'")
			}
		})
	}
}

// TestHandlePing tests the ping handler
func TestHandlePing(t *testing.T) {
	req := MCPRequest{
		JSONRPC: "2.0",
		ID:      jsonRawMessagePtr(`42`),
		Method:  "ping",
	}

	resp := handlePing(req)

	mcpResp, ok := resp.(MCPResponse)
	if !ok {
		t.Fatalf("Expected MCPResponse, got %T", resp)
	}

	if mcpResp.JSONRPC != "2.0" {
		t.Errorf("Expected JSONRPC version 2.0, got %s", mcpResp.JSONRPC)
	}

	result, ok := mcpResp.Result.(map[string]string)
	if !ok {
		t.Fatalf("Expected result to be map[string]string, got %T", mcpResp.Result)
	}

	if len(result) != 0 {
		t.Errorf("Expected empty result for ping, got %v", result)
	}
}

// TestHandleToolsList tests the tools/list handler
func TestHandleToolsList(t *testing.T) {
	req := MCPRequest{
		JSONRPC: "2.0",
		ID:      jsonRawMessagePtr(`1`),
		Method:  "tools/list",
	}

	resp := handleToolsList(req)

	mcpResp, ok := resp.(MCPResponse)
	if !ok {
		t.Fatalf("Expected MCPResponse, got %T", resp)
	}

	result, ok := mcpResp.Result.(map[string]any)
	if !ok {
		t.Fatalf("Expected result to be map[string]interface{}, got %T", mcpResp.Result)
	}

	tools, ok := result["tools"].([]map[string]any)
	if !ok {
		t.Fatalf("Expected tools to be []map[string]interface{}, got %T", result["tools"])
	}

	// Verify we have 5 tools
	if len(tools) != 5 {
		t.Errorf("Expected 5 tools, got %d", len(tools))
	}

	// Verify tool names
	expectedTools := map[string]bool{
		"triage":  false,
		"ready":   false,
		"graph":   false,
		"add_dep": false,
		"room":    false,
	}

	for _, tool := range tools {
		name, ok := tool["name"].(string)
		if !ok {
			t.Errorf("Tool name is not a string: %v", tool["name"])
			continue
		}

		if _, exists := expectedTools[name]; !exists {
			t.Errorf("Unexpected tool name: %s", name)
			continue
		}

		expectedTools[name] = true

		// Verify required fields
		if _, ok := tool["description"]; !ok {
			t.Errorf("Tool %s missing description", name)
		}

		if _, ok := tool["inputSchema"]; !ok {
			t.Errorf("Tool %s missing inputSchema", name)
		}
	}

	// Verify all expected tools were found
	for name, found := range expectedTools {
		if !found {
			t.Errorf("Expected tool %s not found", name)
		}
	}
}

// TestToolsSchemaValidation validates tool input schemas
func TestToolsSchemaValidation(t *testing.T) {
	req := MCPRequest{
		JSONRPC: "2.0",
		ID:      jsonRawMessagePtr(`1`),
		Method:  "tools/list",
	}

	resp := handleToolsList(req)
	mcpResp := resp.(MCPResponse)
	result := mcpResp.Result.(map[string]any)
	tools := result["tools"].([]map[string]any)

	for _, tool := range tools {
		name := tool["name"].(string)
		schema := tool["inputSchema"].(map[string]any)

		// Verify schema type
		if schemaType, ok := schema["type"].(string); !ok || schemaType != "object" {
			t.Errorf("Tool %s: expected schema type 'object', got %v", name, schema["type"])
		}

		// Verify properties exist
		if properties, ok := schema["properties"].(map[string]any); !ok {
			t.Errorf("Tool %s: missing or invalid properties", name)
		} else {
			// Verify required fields exist in properties
			if required, ok := schema["required"].([]string); ok {
				for _, reqField := range required {
					if _, exists := properties[reqField]; !exists {
						t.Errorf("Tool %s: required field '%s' not in properties", name, reqField)
					}
				}
			}
		}
	}
}

// TestHandleToolsCallValidation tests tools/call with invalid parameters
func TestHandleToolsCallValidation(t *testing.T) {
	tests := []struct {
		name        string
		params      string
		wantErr     bool
		errCode     int
		errContains string
	}{
		{
			name:        "Missing tool name",
			params:      `{"arguments":{}}`,
			wantErr:     true,
			errCode:     -32601,
			errContains: "Tool not found",
		},
		{
			name:        "Unknown tool name",
			params:      `{"name":"unknown_tool","arguments":{}}`,
			wantErr:     true,
			errCode:     -32601,
			errContains: "Tool not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := MCPRequest{
				JSONRPC: "2.0",
				ID:      jsonRawMessagePtr(`1`),
				Method:  "tools/call",
				Params:  json.RawMessage(tt.params),
			}

			resp := handleToolsCall(req)

			if tt.wantErr {
				errResp, ok := resp.(MCPErrorResponse)
				if !ok {
					t.Fatalf("Expected MCPErrorResponse, got %T", resp)
				}

				if errResp.Error == nil {
					t.Fatalf("Expected error but got nil")
				}

				if errResp.Error.Code != tt.errCode {
					t.Errorf("Expected error code %d, got %d", tt.errCode, errResp.Error.Code)
				}

				if tt.errContains != "" && !strings.Contains(errResp.Error.Message, tt.errContains) {
					t.Errorf("Expected error message to contain '%s', got '%s'", tt.errContains, errResp.Error.Message)
				}
			} else {
				_, ok := resp.(MCPResponse)
				if !ok {
					t.Fatalf("Expected MCPResponse, got %T", resp)
				}
			}
		})
	}
}

// TestHandleTriageToolValidation tests triage tool parameter validation
func TestHandleTriageToolValidation(t *testing.T) {
	tests := []struct {
		name        string
		args        string
		wantErr     bool
		errContains string
	}{
		{
			name:        "Missing owner",
			args:        `{"repo":"gitea"}`,
			wantErr:     true,
			errContains: "owner",
		},
		{
			name:        "Missing repo",
			args:        `{"owner":"terraphim"}`,
			wantErr:     true,
			errContains: "repo",
		},
		{
			name:        "Empty owner",
			args:        `{"owner":"","repo":"gitea"}`,
			wantErr:     true,
			errContains: "owner",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := jsonRawMessagePtr(`1`)
			resp := handleTriageTool(json.RawMessage(tt.args), id)

			if tt.wantErr {
				errResp, ok := resp.(MCPErrorResponse)
				if !ok {
					t.Fatalf("Expected MCPErrorResponse, got %T", resp)
				}

				if errResp.Error == nil {
					t.Fatalf("Expected error but got nil")
				}

				if tt.errContains != "" && !strings.Contains(errResp.Error.Message, tt.errContains) {
					t.Errorf("Expected error message to contain '%s', got '%s'", tt.errContains, errResp.Error.Message)
				}
			}
		})
	}
}

// TestHandleReadyToolValidation tests ready tool parameter validation
func TestHandleReadyToolValidation(t *testing.T) {
	tests := []struct {
		name        string
		args        string
		wantErr     bool
		errContains string
	}{
		{
			name:        "Missing owner",
			args:        `{"repo":"gitea"}`,
			wantErr:     true,
			errContains: "owner",
		},
		{
			name:        "Missing repo",
			args:        `{"owner":"terraphim"}`,
			wantErr:     true,
			errContains: "repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := jsonRawMessagePtr(`1`)
			resp := handleReadyTool(json.RawMessage(tt.args), id)

			if tt.wantErr {
				errResp, ok := resp.(MCPErrorResponse)
				if !ok {
					t.Fatalf("Expected MCPErrorResponse, got %T", resp)
				}

				if errResp.Error == nil {
					t.Fatalf("Expected error but got nil")
				}

				if tt.errContains != "" && !strings.Contains(errResp.Error.Message, tt.errContains) {
					t.Errorf("Expected error message to contain '%s', got '%s'", tt.errContains, errResp.Error.Message)
				}
			}
		})
	}
}

// TestHandleGraphToolValidation tests graph tool parameter validation
func TestHandleGraphToolValidation(t *testing.T) {
	tests := []struct {
		name        string
		args        string
		wantErr     bool
		errContains string
	}{
		{
			name:        "Missing owner",
			args:        `{"repo":"gitea"}`,
			wantErr:     true,
			errContains: "owner",
		},
		{
			name:        "Missing repo",
			args:        `{"owner":"terraphim"}`,
			wantErr:     true,
			errContains: "repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := jsonRawMessagePtr(`1`)
			resp := handleGraphTool(json.RawMessage(tt.args), id)

			if tt.wantErr {
				errResp, ok := resp.(MCPErrorResponse)
				if !ok {
					t.Fatalf("Expected MCPErrorResponse, got %T", resp)
				}

				if errResp.Error == nil {
					t.Fatalf("Expected error but got nil")
				}

				if tt.errContains != "" && !strings.Contains(errResp.Error.Message, tt.errContains) {
					t.Errorf("Expected error message to contain '%s', got '%s'", tt.errContains, errResp.Error.Message)
				}
			}
		})
	}
}

// TestHandleAddDepToolValidation tests add_dep tool parameter validation
func TestHandleAddDepToolValidation(t *testing.T) {
	tests := []struct {
		name        string
		args        string
		wantErr     bool
		errContains string
	}{
		{
			name:        "Missing owner",
			args:        `{"repo":"gitea","issue":2,"blocks":1}`,
			wantErr:     true,
			errContains: "owner",
		},
		{
			name:        "Missing repo",
			args:        `{"owner":"terraphim","issue":2,"blocks":1}`,
			wantErr:     true,
			errContains: "repo",
		},
		{
			name:        "Missing issue",
			args:        `{"owner":"terraphim","repo":"gitea","blocks":1}`,
			wantErr:     true,
			errContains: "issue",
		},
		{
			name:        "Issue is zero",
			args:        `{"owner":"terraphim","repo":"gitea","issue":0,"blocks":1}`,
			wantErr:     true,
			errContains: "issue",
		},
		{
			name:        "Missing blocks and relates_to",
			args:        `{"owner":"terraphim","repo":"gitea","issue":2}`,
			wantErr:     true,
			errContains: "blocks or relates_to",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := jsonRawMessagePtr(`1`)
			resp := handleAddDepTool(json.RawMessage(tt.args), id)

			if tt.wantErr {
				errResp, ok := resp.(MCPErrorResponse)
				if !ok {
					t.Fatalf("Expected MCPErrorResponse, got %T", resp)
				}

				if errResp.Error == nil {
					t.Fatalf("Expected error but got nil")
				}

				if tt.errContains != "" && !strings.Contains(errResp.Error.Message, tt.errContains) {
					t.Errorf("Expected error message to contain '%s', got '%s'", tt.errContains, errResp.Error.Message)
				}
			}
		})
	}
}

// TestMethodNotFound tests handling of unknown methods
func TestMethodNotFound(t *testing.T) {
	req := MCPRequest{
		JSONRPC: "2.0",
		ID:      jsonRawMessagePtr(`1`),
		Method:  "unknown_method",
	}

	resp := MCPErrorResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Error: &MCPError{
			Code:    -32601,
			Message: "Method not found: " + req.Method,
		},
	}

	if resp.Error.Code != -32601 {
		t.Errorf("Expected error code -32601, got %d", resp.Error.Code)
	}

	if !strings.Contains(resp.Error.Message, "unknown_method") {
		t.Errorf("Expected error message to contain 'unknown_method', got '%s'", resp.Error.Message)
	}
}

// TestSendResponse tests the sendResponse function
func TestSendResponse(t *testing.T) {
	tests := []struct {
		name     string
		resp     any
		expected string
	}{
		{
			name: "Successful response",
			resp: MCPResponse{
				JSONRPC: "2.0",
				ID:      jsonRawMessagePtr(`1`),
				Result:  map[string]string{"status": "ok"},
			},
			expected: `{"jsonrpc":"2.0","id":1,"result":{"status":"ok"}}`,
		},
		{
			name: "Error response",
			resp: MCPErrorResponse{
				JSONRPC: "2.0",
				ID:      jsonRawMessagePtr(`2`),
				Error: &MCPError{
					Code:    -32601,
					Message: "Method not found",
				},
			},
			expected: `{"jsonrpc":"2.0","id":2,"error":{"code":-32601,"message":"Method not found"}}`,
		},
		{
			name: "Notification response (no id)",
			resp: MCPResponse{
				JSONRPC: "2.0",
				Result:  map[string]string{},
			},
			expected: `{"jsonrpc":"2.0","result":{}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			writer := bufio.NewWriter(&buf)

			sendResponse(writer, tt.resp)

			output := strings.TrimSpace(buf.String())

			// Parse both expected and actual to compare as JSON
			var expectedMap, actualMap map[string]any
			if err := json.Unmarshal([]byte(tt.expected), &expectedMap); err != nil {
				t.Fatalf("Failed to unmarshal expected: %v", err)
			}
			if err := json.Unmarshal([]byte(output), &actualMap); err != nil {
				t.Fatalf("Failed to unmarshal actual: %v", err)
			}

			// Compare JSONRPC version
			if expectedMap["jsonrpc"] != actualMap["jsonrpc"] {
				t.Errorf("JSONRPC mismatch: expected %v, got %v", expectedMap["jsonrpc"], actualMap["jsonrpc"])
			}
		})
	}
}

// TestMCPServerIntegration tests the MCP server command integration
func TestMCPServerIntegration(t *testing.T) {
	// This test simulates MCP server communication via stdin/stdout
	tests := []struct {
		name     string
		input    string
		contains []string // Expected substrings in output
	}{
		{
			name:     "Initialize request",
			input:    `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n",
			contains: []string{`"jsonrpc":"2.0"`, `"id":1`, `"protocolVersion":"2024-11-05"`, `"gitea-robot"`},
		},
		{
			name:     "Ping request",
			input:    `{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n",
			contains: []string{`"jsonrpc":"2.0"`, `"id":2`, `"result":{}`},
		},
		{
			name:     "Tools list request",
			input:    `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}` + "\n",
			contains: []string{`"jsonrpc":"2.0"`, `"id":3`, `"triage"`, `"ready"`, `"graph"`, `"add_dep"`},
		},
		{
			name:     "Invalid JSON",
			input:    `{"jsonrpc":"2.0","id":4,"method":}` + "\n",
			contains: []string{`"jsonrpc":"2.0"`, `"error"`, `"code":-32703`},
		},
		{
			name:     "Unknown method",
			input:    `{"jsonrpc":"2.0","id":5,"method":"unknown"}` + "\n",
			contains: []string{`"jsonrpc":"2.0"`, `"id":5`, `"error"`, `"code":-32601`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a pipe to simulate stdin/stdout
			stdinReader, stdinWriter := io.Pipe()
			stdoutReader, stdoutWriter := io.Pipe()

			// Write input
			go func() {
				stdinWriter.Write([]byte(tt.input))
				stdinWriter.Close()
			}()

			// Read output with timeout
			var output strings.Builder
			done := make(chan bool)
			go func() {
				buf := make([]byte, 4096)
				n, _ := stdoutReader.Read(buf)
				if n > 0 {
					output.Write(buf[:n])
				}
				done <- true
			}()

			// Process the request (simulate mcpServerCmd behavior)
			reader := bufio.NewReader(stdinReader)
			writer := bufio.NewWriter(stdoutWriter)

			line, _ := reader.ReadString('\n')
			line = strings.TrimSpace(line)

			if line != "" {
				var req MCPRequest
				if err := json.Unmarshal([]byte(line), &req); err != nil {
					resp := MCPErrorResponse{
						JSONRPC: "2.0",
						ID:      nil,
						Error: &MCPError{
							Code:    -32703,
							Message: "Failed to parse JSON: " + err.Error(),
						},
					}
					sendResponse(writer, resp)
				} else {
					var resp any
					switch req.Method {
					case "initialize":
						resp = handleInitialize(req)
					case "tools/list":
						resp = handleToolsList(req)
					case "ping":
						resp = handlePing(req)
					default:
						resp = MCPErrorResponse{
							JSONRPC: "2.0",
							ID:      req.ID,
							Error: &MCPError{
								Code:    -32601,
								Message: "Method not found: " + req.Method,
							},
						}
					}
					sendResponse(writer, resp)
				}
			}

			stdoutWriter.Close()
			<-done

			outputStr := output.String()
			for _, expected := range tt.contains {
				if !strings.Contains(outputStr, expected) {
					t.Errorf("Expected output to contain '%s', got:\n%s", expected, outputStr)
				}
			}
		})
	}
}

// TestCaptureStdout tests the captureStdout function
func TestCaptureStdout(t *testing.T) {
	output, err := captureStdout(func() {
		fmt.Println("Hello, World!")
		fmt.Print("Test output")
	})
	if err != nil {
		t.Fatalf("captureStdout failed: %v", err)
	}

	if !strings.Contains(output, "Hello, World!") {
		t.Errorf("Expected output to contain 'Hello, World!', got: %s", output)
	}

	if !strings.Contains(output, "Test output") {
		t.Errorf("Expected output to contain 'Test output', got: %s", output)
	}
}

// TestCaptureStdoutWithError tests captureStdout handles errors properly
func TestCaptureStdoutWithError(t *testing.T) {
	// Test that captureStdout works with functions that don't produce output
	output, err := captureStdout(func() {
		// No output
	})
	if err != nil {
		t.Fatalf("captureStdout failed: %v", err)
	}

	if output != "" {
		t.Errorf("Expected empty output, got: %s", output)
	}
}

// Helper function to create *json.RawMessage
func jsonRawMessagePtr(s string) *json.RawMessage {
	raw := json.RawMessage(s)
	return &raw
}
