# Research Document: Gitea Robot API Import Path Fixes

## 1. Problem Restatement and Scope

**Problem:** The Gitea Robot API feature (PageRank, CLI tool, robot endpoints) cannot compile due to incorrect import paths in 3 source files and a missing import alias in test files.

**IN Scope:**
- Fix 3 incorrect import paths in `routers/api/v1/robot/` package
- Fix import alias in `tests/integration/robot_security_test.go`
- Verify build compiles successfully
- Run relevant tests to confirm fixes

**OUT of Scope:**
- Functional changes to PageRank algorithm
- New features or API endpoints
- Changes to CI/CD workflows
- Documentation updates

## 2. User & Business Outcomes

**Visible Changes After Fix:**
- Codebase compiles successfully (`go build .` passes)
- Robot API endpoints are functional:
  - `GET /api/v1/robot/triage` - Returns prioritized issues
  - `GET /api/v1/robot/ready` - Returns unblocked issues
  - `GET /api/v1/robot/graph` - Returns dependency graph
- CLI tool `cmd/gitea-robot` builds and works
- Integration tests pass

## 3. System Elements and Dependencies

| Component | Location | Role | Dependencies |
|-----------|----------|------|--------------|
| **Robot API Handlers** | `routers/api/v1/robot/` | HTTP API endpoints for robot features | Uses `services/context` for request context |
| **Context Package** | `services/context/` | Gitea's request context (APIContext, Context) | Core infrastructure |
| **Robot Service** | `services/robot/` | Business logic for triage, ready checks | Uses models, audit logging |
| **Security Tests** | `tests/integration/robot_security_test.go` | Integration tests for robot API security | Uses `services/repository` as `repo_service` |
| **Repository Service** | `services/repository/` | Repository CRUD operations | Models, git operations |

**Dependency Graph:**
```
routers/api/v1/robot/*.go
  ├── imports: services/context (WRONG: modules/context)
  ├── uses: services/robot
  └── uses: models/repo, models/issues

tests/integration/robot_security_test.go
  ├── imports: services/repository (MISSING ALIAS)
  └── uses: repo_service.CreateRepository (expects alias)
```

## 4. Constraints and Their Implications

| Constraint | Implication |
|------------|-------------|
| **Import path must match actual package location** | `services/context` exists at `services/context/`, not `modules/context/` - using wrong path causes compilation failure |
| **Import aliases must be consistent** | Test files across codebase use `repo_service` alias for `services/repository` - inconsistency breaks compilation |
| **Minimal changes principle** | Only fix what's broken; don't refactor working code |
| **Test compatibility** | Changes must not break existing tests; robot_security_test.go has 1000+ lines of tests that must still pass |
| **No functional changes** | Fix is purely mechanical - changing import paths, not logic |

## 5. Risks, Unknowns, and Assumptions

**ASSUMPTIONS:**
1. `services/context` package is the correct and intended location for context types
2. Other test files using `repo_service` alias are correct; robot_security_test.go is the outlier
3. No other files have similar import path issues (to be verified during implementation)

**RISKS:**
| Risk | Likelihood | Impact | Mitigation |
|------|------------|--------|------------|
| Other files have same import issues | Medium | Low | Run `go build` to catch all compilation errors |
| Context package has breaking API changes | Low | High | Check context package exports match usage |
| Tests fail after fix due to other issues | Medium | Medium | Run full test suite, investigate failures |
| Missing imports in other robot test files | Medium | Low | Check all robot-related test files |

**UNKNOWNS:**
- Are there other import path issues in robot_test.go specifically?
- Do the context types (APIContext) have the same interface in both locations?

## 6. Context Complexity vs. Simplicity Opportunities

**Sources of Complexity:**
1. Gitea's package structure evolved over time - `modules/context` may have moved to `services/context`
2. Multiple test files with varying import patterns
3. Large test file (1048 lines) with many `repo_service` references

**Simplification Strategies:**
1. **Mechanical fix approach**: Use find/replace for consistent changes
2. **Verify by compilation**: Let the compiler validate all changes
3. **Follow existing patterns**: Match import aliases used in other test files

## 7. Questions for Human Reviewer

1. **Q:** Is `services/context` the permanent home for context types, or is this a transitional state?
   **Why:** Determines if this is a temporary fix or permanent solution.

2. **Q:** Should we standardize all test files to use `repo_service` alias consistently?
   **Why:** The robot_security_test.go is inconsistent with other test files.

3. **Q:** Are there plans to run linting/static analysis to catch import issues automatically?
   **Why:** Prevents future occurrences of this issue.

4. **Q:** After fixing imports, should we run the full integration test suite or just robot-specific tests?
   **Why:** Determines testing scope and time investment.

5. **Q:** The robot_security_test.go has 22 occurrences of `repo_service.` - should we add the import alias or change all references to `repository.`?
   **Why:** Two valid approaches; want to align with project conventions.

---

## Appendix: Detailed File Analysis

### Files with Wrong Import Path

1. **routers/api/v1/robot/robot.go:15**
   ```go
   "code.gitea.io/gitea/modules/context"  // WRONG
   ```

2. **routers/api/v1/robot/ready_graph.go:15**
   ```go
   "code.gitea.io/gitea/modules/context"  // WRONG
   ```

3. **routers/api/v1/robot/robot_test.go:16**
   ```go
   "code.gitea.io/gitea/modules/context"  // WRONG
   ```

**Correct path:** `"code.gitea.io/gitea/services/context"`

### File with Missing Import Alias

**tests/integration/robot_security_test.go:20**
```go
"code.gitea.io/gitea/services/repository"  // Missing alias
```

**Should be:**
```go
repo_service "code.gitea.io/gitea/services/repository"
```

**Evidence:** Other test files like `actions_trigger_test.go:36` use this pattern consistently.

### Usage Count in robot_security_test.go

- `repo_service.CreateRepoOptions` - 1 occurrence (line 41)
- `repo_service.CreateRepository` - 21 occurrences (lines 69, 100, 131, 164, 214, 332, 359, 396, 440, 471, 502, 565, 634, 682, 747, 832, 872, 906, 946, 976, 1011)

**Total:** 22 references that need the import alias to work.

---

## Verification Plan

1. **Pre-fix verification:**
   ```bash
   go build . 2>&1 | head -20  # Should show import errors
   ```

2. **Post-fix verification:**
   ```bash
   go build .  # Should succeed
   go build ./cmd/gitea-robot  # Should succeed
   go test -c ./tests/integration/...  # Should compile tests
   ```

3. **Test execution:**
   ```bash
   make test-backend  # Run backend tests
   # OR specifically:
   go test -v ./tests/integration/... -run TestRobotAPI  # Run robot tests
   ```
