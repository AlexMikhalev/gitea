# Testing Plan: Issue Graph Features

## PageRank Validation for Tasks (Priority)

### Mathematical Correctness Tests

| Test Case | Input Graph | Expected PageRank | Tolerance |
|-----------|-------------|-------------------|-----------|
| **Baseline** | Single issue, no deps | ~0.15 (1-damping) | ±0.01 |
| **Simple Chain** | A → B → C | A: ~0.15, B: ~0.21, C: ~0.26 | ±0.05 |
| **Star Pattern** | A → B,C,D | A: ~0.15, B/C/D: ~0.24 | ±0.05 |
| **Diamond** | A → B,C → D | D: ~0.31 (highest) | ±0.05 |
| **Closed Issue** | A(closed) → B | A: 0, B: ~0.15 | ±0.01 |

### Required PageRank Unit Tests

```go
// Test files to create: models/issues/graph_cache_test.go

TestCalculatePageRank_SingleIssue()      // Verify 1-damping baseline
TestCalculatePageRank_SimpleChain()      // A → B → C propagation
TestCalculatePageRank_StarPattern()      // One parent, multiple children  
TestCalculatePageRank_DiamondPattern()   // Multiple paths to same node
TestCalculatePageRank_WithClosedIssues() // Closed issues excluded
TestCalculatePageRank_Convergence()      // Algorithm converges
TestCalculatePageRank_DampingFactor()    // Test 0.5, 0.85, 0.95
TestCalculatePageRank_SumToOne()         // Sum of all ranks ≈ 1.0
```

### API PageRank Validation

| Endpoint | Validation |
|----------|------------|
| `/robot/triage` | Issues sorted by PageRank DESC |
| `/robot/ready` | Each issue includes `pagerank` field |
| `/robot/graph` | Each node includes `pagerank` field |

### Performance Benchmarks

| Scenario | Target |
|----------|--------|
| 1k issues, 100 deps | < 5 seconds |
| 10k issues, 1k deps | < 30 seconds |
| Incremental update | < 1 second |

### Manual PageRank Tests

- [ ] Create A → B → C, verify C has highest rank
- [ ] Verify all PageRank values in [0, 1]
- [ ] Verify sum of ranks ≈ 1.0
- [ ] Close middle issue, verify recalculation

---

**Full plan:** See testing-plan.md