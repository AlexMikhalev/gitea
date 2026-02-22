# Testing Plan: Issue Graph Features

See testing-plan.md in repository root for full details.

## Quick Summary

### Unit Tests (Created)
- ✅ issue_dependency_test.go
- ✅ cycle_test.go
- ⏳ graph_cache_test.go (TODO)

### Integration Tests (TODO)
- ⏳ api_robot_test.go

### Performance Tests (TODO)
- ⏳ PageRank 1k/10k issues
- ⏳ API response times

### Manual Testing
- ⏳ Database migration
- ⏳ Feature flag
- ⏳ Web UI
- ⏳ Agent API

## Running Tests

```bash
go test ./models/issues/... -v
go test ./tests/integration/... -v
```

## Acceptance Criteria

- [ ] All unit tests pass (>80% coverage)
- [ ] All integration tests pass
- [ ] Performance benchmarks met
- [ ] Manual testing complete

---

**Full plan:** See testing-plan.md
