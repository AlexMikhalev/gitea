# Testing Plan: Issue Graph Features (PageRank & Dependencies)

## Overview

This issue tracks the comprehensive testing plan for the new issue graph features including PageRank calculation, dependency tracking, and agent API.

## PageRank Validation for Tasks

### Mathematical Correctness

| Test Case | Expected PageRank | Description |
|-----------|-------------------|-------------|
| **Single issue, no deps** | ~0.15 (1-damping) | Baseline with no incoming links |
| **A → B (A blocks B)** | A: ~0.15, B: ~0.28 | B receives PageRank from A |
| **Star pattern (A blocks B,C,D)** | A: ~0.15, B/C/D: ~0.24 each | Multiple children share parent's rank |
| **Chain A → B → C** | A: ~0.15, B: ~0.21, C: ~0.26 | Rank accumulates down the chain |
| **Diamond pattern** | Root: ~0.15, Middle: ~0.24, Leaf: ~0.31 | Multiple paths increase rank |
| **Closed issue** | 0 (or excluded) | Closed issues don't propagate rank |

### Convergence Tests

| Test | Criteria | Status |
|------|----------|--------|
| Convergence with 10 iterations | Δ < 0.001 between iterations | ⏳ TODO |
| Convergence with 100 iterations | Δ < 0.0001 between iterations | ⏳ TODO |
| Damping factor 0.85 (default) | Standard PageRank behavior | ⏳ TODO |
| Damping factor 0.5 | Faster convergence, lower spread | ⏳ TODO |

### Unit Tests (Models)

| Test File | Coverage | Status |
|-----------|----------|--------|
| `models/issues/issue_dependency_test.go` | CRUD operations, duplicates, circular detection | ✅ Created |
| `models/issues/cycle_test.go` | Cycle detection algorithms | ✅ Created |
| `models/issues/graph_cache_test.go` | **PageRank calculation, centrality** | ⏳ TODO |

**Required PageRank Tests:**
- [ ] `TestCalculatePageRank_SingleIssue` - Verify baseline (1-damping)
- [ ] `TestCalculatePageRank_SimpleChain` - A → B → C propagation
- [ ] `TestCalculatePageRank_StarPattern` - One parent, multiple children
- [ ] `TestCalculatePageRank_DiamondPattern` - Multiple paths to same node
- [ ] `TestCalculatePageRank_WithClosedIssues` - Closed issues excluded
- [ ] `TestCalculatePageRank_Convergence` - Verify algorithm converges
- [ ] `TestCalculatePageRank_DampingFactor` - Test different damping values
- [ ] `TestCalculateCentrality` - Verify centrality scores
- [ ] `TestGetCriticalPath` - Verify longest dependency chain
- [ ] `TestGetReadyIssues` - Verify unblocked issue detection
- [ ] `TestGetRankedIssues` - Verify ranking by PageRank
- [ ] `TestInvalidateCache` - Verify cache invalidation

### PageRank Test Implementation

```go
// Example: Test PageRank calculation
func TestCalculatePageRank_SimpleChain(t *testing.T) {
    require.NoError(t, unittest.PrepareTestDatabase())
    ctx := db.DefaultContext

    // Setup: A → B → C (A blocks B, B blocks C)
    // Issue IDs: 1, 2, 3
    err := AddDependency(ctx, 1, 2, 1, DepTypeBlocks) // B blocked by A
    require.NoError(t, err)
    err = AddDependency(ctx, 1, 3, 2, DepTypeBlocks)  // C blocked by B
    require.NoError(t, err)

    // Calculate PageRank
    err = CalculatePageRank(ctx, 1, 0.85, 100)
    require.NoError(t, err)

    // Verify: C should have highest rank (receives from B)
    rankA, _ := GetPageRank(ctx, 1, 1)
    rankB, _ := GetPageRank(ctx, 1, 2)
    rankC, _ := GetPageRank(ctx, 1, 3)

    // A should have lowest (no incoming)
    assert.Less(t, rankA, rankB)
    // C should have highest (receives from chain)
    assert.Greater(t, rankC, rankB)
    // Verify approximate values
    assert.InDelta(t, 0.15, rankA, 0.05)  // ~0.15
    assert.InDelta(t, 0.21, rankB, 0.05)  // ~0.21
    assert.InDelta(t, 0.26, rankC, 0.05)  // ~0.26
}
```

### Integration Tests (API)

| Endpoint | Test Scenarios | Status |
|----------|---------------|--------|
| `GET /api/v1/robot/triage` | Empty repo, with issues, with dependencies | ⏳ TODO |
| `GET /api/v1/robot/ready` | No ready issues, ready issues sorted by PageRank | ⏳ TODO |
| `GET /api/v1/robot/graph` | Empty graph, with nodes/edges with PageRank scores | ⏳ TODO |
| `GET /repos/{owner}/{repo}/issues/{index}/dependencies` | List dependencies | ⏳ TODO |
| `POST /repos/{owner}/{repo}/issues/{index}/dependencies` | Add valid, duplicate, circular | ⏳ TODO |
| `DELETE /repos/{owner}/{repo}/issues/{index}/dependencies/{id}` | Remove existing, non-existing | ⏳ TODO |

**PageRank API Validation:**
- [ ] `TestRobotTriage_PageRankSorting` - Issues sorted by PageRank descending
- [ ] `TestRobotTriage_PageRankValues` - Values in valid range [0, 1]
- [ ] `TestRobotReady_PageRankIncluded` - Ready issues include PageRank scores
- [ ] `TestRobotGraph_PageRankInNodes` - Graph nodes include PageRank

### Performance Tests

| Scenario | Target | Status |
|----------|--------|--------|
| PageRank calculation (1k issues, 100 deps) | < 5 seconds | ⏳ TODO |
| PageRank calculation (10k issues, 1k deps) | < 30 seconds | ⏳ TODO |
| PageRank incremental update | < 1 second | ⏳ TODO |
| Robot API response time | < 100ms | ⏳ TODO |
| Dependency query (100 deps) | < 50ms | ⏳ TODO |

### Manual Testing Checklist

#### PageRank Validation
- [ ] Create chain A → B → C, verify C has highest rank
- [ ] Create star pattern, verify children have equal rank
- [ ] Close issue B in chain, verify ranks recalculate
- [ ] Add many dependencies (50+), verify performance
- [ ] Verify PageRank values are between 0 and 1
- [ ] Verify sum of all PageRanks ≈ 1.0 (normalized)

#### Database Migration
- [ ] Fresh install: Migration runs successfully
- [ ] Upgrade: Migration preserves existing data
- [ ] Rollback: Down migration works correctly

#### Feature Flag
- [ ] With `ENABLED = false`: Endpoints return 404
- [ ] With `ENABLED = true`: All features work
- [ ] Toggle: Changing flag takes effect without restart

#### Web UI
- [ ] View dependencies on issue page
- [ ] Add dependency via form
- [ ] Remove dependency
- [ ] Error messages for circular dependencies

#### Agent API
- [ ] `triage` returns correct structure with PageRank
- [ ] `ready` returns unblocked issues sorted by PageRank
- [ ] `graph` returns valid nodes/edges with PageRank scores

### Edge Cases

| Case | Expected Behavior |
|------|-------------------|
| Circular dependency attempt | Blocked with error |
| Self-dependency | Blocked with error |
| Non-existent issue ID | 404 error |
| Duplicate dependency | 409 conflict |
| Very deep dependency chain (100+) | Performance acceptable |
| Repository with 0 issues | Empty results |
| All issues closed | Empty PageRank results |
| Single issue with self-loop | Blocked (circular) |

## Test Data Setup

```sql
-- Create test issues for PageRank validation
INSERT INTO issue (repo_id, index, title, is_closed) VALUES
(1, 1, 'Issue A - Root', false),
(1, 2, 'Issue B - Depends on A', false),
(1, 3, 'Issue C - Depends on B', false),
(1, 4, 'Issue D - Depends on A', false),
(1, 5, 'Issue E - Closed', true),
(1, 6, 'Issue F - Independent', false);

-- Create dependencies for chain and star pattern
INSERT INTO issue_dependency (repo_id, issue_id, depends_on, dep_type) VALUES
(1, 2, 1, 'blocks'),  -- B blocked by A
(1, 3, 2, 'blocks'),  -- C blocked by B (chain)
(1, 4, 1, 'blocks');  -- D blocked by A (star)

-- Expected PageRank order: C > B/D > A > F > E
```

## Running Tests

```bash
# Unit tests with PageRank focus
go test ./models/issues/... -v -run "PageRank"

# All unit tests
go test ./models/issues/... -v

# Integration tests
go test ./tests/integration/... -v -run "Robot"

# Performance benchmarks
go test ./models/issues/... -bench=BenchmarkPageRank

# All tests
go test ./... -v
```

## Acceptance Criteria

- [ ] All unit tests pass (>80% coverage)
- [ ] PageRank calculation matches expected mathematical values
- [ ] PageRank converges within configured iterations
- [ ] All integration tests pass
- [ ] Performance benchmarks met
- [ ] Manual testing checklist complete
- [ ] No circular dependency bugs
- [ ] Feature flag works correctly
- [ ] PageRank values in valid range [0, 1]

## Related

- Design: `.docs/design-gitea-pagerank.md`
- Implementation: PR #1 (fork)
- PageRank Algorithm: https://en.wikipedia.org/wiki/PageRank