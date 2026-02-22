# Robot CLI Testing Strategy

## Architecture Clarification

### Current Implementation

```
┌─────────────────────────────────────────────────────────┐
│  AGENT (Claude Code, Codex, etc.)                       │
│  ─────────────────────────────────────                  │
│  Uses HTTP API directly:                                │
│  curl /api/v1/robot/triage?owner=X&repo=Y               │
└─────────────────────────────────────────────────────────┘
                            │
                            ▼
┌─────────────────────────────────────────────────────────┐
│  GITEA API                                              │
│  ─────────────────────────────────────                  │
│  /api/v1/robot/triage  →  Robot Service                 │
│  /api/v1/robot/ready   →  Robot Service                 │
│  /api/v1/robot/graph   →  Robot Service                 │
└─────────────────────────────────────────────────────────┘
```

### NO tea CLI Update Needed

The robot API is **HTTP/JSON** - agents use it directly via HTTP requests, not through tea CLI.

## Testing Options

### Option 1: HTTP API Testing (Recommended)

Test the robot endpoints directly via HTTP:

```bash
# Test triage endpoint
curl -H "Authorization: token $GITEA_TOKEN" \
  "https://git.terraphim.cloud/api/v1/robot/triage?owner=terraphim&repo=gitea"

# Expected response:
{
  "quick_ref": {"total": 42, "open": 12, "blocked": 3, "ready": 5},
  "recommendations": [
    {"id": 123, "title": "...", "pagerank": 0.85, ...}
  ],
  ...
}
```

**Pros:**
- Direct testing of actual API
- No additional tooling needed
- Matches how agents will use it

**Cons:**
- Requires running Gitea instance

### Option 2: Standalone Robot CLI (Optional)

Create a thin CLI wrapper for testing/debugging:

```go
// cmd/robot/main.go
package main

import (
    "encoding/json"
    "fmt"
    "net/http"
    "os"
)

func main() {
    if len(os.Args) < 2 {
        fmt.Println("Usage: robot <triage|ready|graph> [owner] [repo]")
        os.Exit(1)
    }
    
    cmd := os.Args[1]
    owner := os.Args[2]
    repo := os.Args[3]
    
    token := os.Getenv("GITEA_TOKEN")
    baseURL := os.Getenv("GITEA_URL") // https://git.terraphim.cloud
    
    url := fmt.Sprintf("%s/api/v1/robot/%s?owner=%s&repo=%s", baseURL, cmd, owner, repo)
    
    req, _ := http.NewRequest("GET", url, nil)
    req.Header.Set("Authorization", "token "+token)
    
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        fmt.Fprintf(os.Stderr, "Error: %v\n", err)
        os.Exit(1)
    }
    defer resp.Body.Close()
    
    var result interface{}
    json.NewDecoder(resp.Body).Decode(&result)
    
    out, _ := json.MarshalIndent(result, "", "  ")
    fmt.Println(string(out))
}
```

**Usage:**
```bash
export GITEA_TOKEN=...
export GITEA_URL=https://git.terraphim.cloud

robot triage terraphim gitea
robot ready terraphim gitea  
robot graph terraphim gitea
```

**Pros:**
- Easy manual testing
- Can be used in scripts
- Familiar CLI interface

**Cons:**
- Extra maintenance
- Duplicates HTTP API functionality

### Option 3: Integration with tea CLI (NOT Recommended)

Adding robot commands to tea CLI:

```bash
tea robot triage --owner terraphim --repo gitea
tea robot ready --owner terraphim --repo gitea
```

**Why NOT recommended:**
- tea CLI is for Gitea administration, not agent workflows
- Would require upstream PR to gitea/tea
- Adds complexity for niche use case

## Recommended Approach

### Phase 1: HTTP API Testing (Now)

1. **Integration tests** in `tests/integration/api_robot_test.go`
2. **Manual testing** via curl/httpie
3. **Documentation** with example requests

### Phase 2: Standalone Robot CLI (Later, if needed)

Create separate tool if manual testing becomes tedious:
- Repository: `terraphim/robot-cli`
- Or: Include in `terraphim/gitea` as `cmd/robot/`

## Test Implementation

### Integration Test Example

```go
// tests/integration/api_robot_test.go
func TestRobotTriage(t *testing.T) {
    defer tests.PrepareTestEnv(t)()
    
    repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: 1})
    owner := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: repo.OwnerID})
    
    // Create test issues with dependencies
    // ... setup code ...
    
    req := NewRequest(t, "GET", "/api/v1/robot/triage?owner="+owner.Name+"&repo="+repo.Name)
    req = AddBasicAuthHeader(req, user.Name)
    
    resp := MakeRequest(t, req, http.StatusOK)
    
    var result robot.TriageResponse
    DecodeJSON(t, resp, &result)
    
    // Assertions
    assert.NotEmpty(t, result.Recommendations)
    assert.True(t, result.Recommendations[0].PageRank > 0)
    assert.True(t, result.Recommendations[0].PageRank <= 1)
}
```

### Manual Test Script

```bash
#!/bin/bash
# scripts/test-robot-api.sh

set -e

GITEA_URL=${GITEA_URL:-"http://localhost:3000"}
GITEA_TOKEN=${GITEA_TOKEN:-""}
OWNER=${1:-"terraphim"}
REPO=${2:-"gitea"}

if [ -z "$GITEA_TOKEN" ]; then
    echo "Error: GITEA_TOKEN not set"
    exit 1
fi

echo "Testing Robot API..."
echo "URL: $GITEA_URL"
echo "Owner: $OWNER, Repo: $REPO"
echo ""

echo "1. Testing /robot/triage..."
curl -s -H "Authorization: token $GITEA_TOKEN" \
    "$GITEA_URL/api/v1/robot/triage?owner=$OWNER&repo=$REPO" | jq .

echo ""
echo "2. Testing /robot/ready..."
curl -s -H "Authorization: token $GITEA_TOKEN" \
    "$GITEA_URL/api/v1/robot/ready?owner=$OWNER&repo=$REPO" | jq .

echo ""
echo "3. Testing /robot/graph..."
curl -s -H "Authorization: token $GITEA_TOKEN" \
    "$GITEA_URL/api/v1/robot/graph?owner=$OWNER&repo=$REPO" | jq .

echo ""
echo "All tests passed!"
```

## Summary

| Approach | Recommendation | Effort | Value |
|----------|---------------|--------|-------|
| HTTP API Testing | ✅ **Do this** | Low | High |
| Standalone Robot CLI | ⏳ **Later if needed** | Medium | Medium |
| tea CLI Integration | ❌ **Don't do** | High | Low |

The robot API is designed for AI agents that make HTTP calls directly. No CLI tool needed for core functionality.