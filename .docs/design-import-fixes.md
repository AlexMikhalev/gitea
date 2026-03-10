# Design & Implementation Plan: Gitea Robot API Import Path Fixes

## 1. Summary of Target Behavior

After implementation, the Gitea codebase will:
- Compile successfully without import path errors
- Build the main application (`go build .`) without errors
- Build the CLI tool (`go build ./cmd/gitea-robot`) without errors
- Pass all robot API integration tests
- Maintain consistency with existing codebase import patterns

## 2. Key Invariants and Acceptance Criteria

### Invariants (Must Always Be True)
1. **Import paths must reference existing packages**: All import statements must point to packages that exist in the codebase
2. **Import aliases must match usage**: If a package is imported with an alias, all references must use that alias
3. **No functional changes**: Only import paths change; no logic modifications
4. **Test compatibility**: All existing tests must compile and pass
5. **Code consistency**: Changes must align with patterns used elsewhere in the codebase

### Acceptance Criteria (Testable)
| Criterion | Verification Method |
|-----------|-------------------|
| Main application builds | `go build .` exits 0 |
| CLI tool builds | `go build ./cmd/gitea-robot` exits 0 |
| Integration tests compile | `go test -c ./tests/integration/...` succeeds |
| Robot API tests pass | `go test ./tests/integration -run TestRobotAPI` passes |
| No import errors | `go list ./...` shows no import errors |
| Consistent with codebase | grep shows matching patterns in other files |

## 3. High-Level Design and Boundaries

### Architecture Overview

```
┌─────────────────────────────────────────────────────────────┐
│  PHASE 2: DESIGN (Current)                                  │
│  └─ Document the exact changes needed                       │
├─────────────────────────────────────────────────────────────┤
│  PHASE 3: IMPLEMENTATION (Next)                             │
│  ├─ Step 1: Fix 3 import paths in routers/api/v1/robot/    │
│  ├─ Step 2: Add import alias to robot_security_test.go     │
│  ├─ Step 3: Verify compilation                              │
│  └─ Step 4: Run tests                                       │
└─────────────────────────────────────────────────────────────┘
```

### Change Boundaries

**INSIDE Boundary (files we modify):**
- `routers/api/v1/robot/robot.go`
- `routers/api/v1/robot/ready_graph.go`
- `routers/api/v1/robot/robot_test.go`
- `tests/integration/robot_security_test.go`

**OUTSIDE Boundary (files we do NOT touch):**
- `services/context/` - existing package
- `services/repository/` - existing package
- `services/robot/` - existing service
- All other source files
- CI/CD workflows
- Documentation (except this design doc)

### Design Principles Applied

1. **SIMPLE over EASY**: Single mechanical changes, no clever refactoring
2. **Separation of concerns**: Each file change is independent
3. **Interface over implementation**: Import paths are the interface; we fix the reference
4. **Minimize blast radius**: Only 4 files touched, no other changes

## 4. File/Module-Level Change Plan

### Change Set 1: Fix Context Import Path (3 files)

| File | Action | Before | After | Dependencies |
|------|--------|--------|-------|--------------|
| `routers/api/v1/robot/robot.go:15` | Modify import | `"code.gitea.io/gitea/modules/context"` | `"code.gitea.io/gitea/services/context"` | None - just path change |
| `routers/api/v1/robot/ready_graph.go:15` | Modify import | `"code.gitea.io/gitea/modules/context"` | `"code.gitea.io/gitea/services/context"` | None - just path change |
| `routers/api/v1/robot/robot_test.go:16` | Modify import | `"code.gitea.io/gitea/modules/context"` | `"code.gitea.io/gitea/services/context"` | None - just path change |

**Rationale:** The `services/context` package exists and contains `APIContext` and other context types. Other files in the codebase use this path.

### Change Set 2: Add Import Alias (1 file)

| File | Action | Before | After | Dependencies |
|------|--------|--------|-------|--------------|
| `tests/integration/robot_security_test.go:20` | Modify import | `"code.gitea.io/gitea/services/repository"` | `repo_service "code.gitea.io/gitea/services/repository"` | Aligns with 22 existing usages in file |

**Rationale:** The file uses `repo_service.` prefix 22 times but imports without alias. Other test files use this alias pattern consistently.

## 5. Step-by-Step Implementation Sequence

### Step 1: Pre-Flight Verification (Read-only)
**Purpose:** Confirm current state before changes
**Deployable:** Yes (no changes made)

```bash
# Verify the errors exist
go build . 2>&1 | head -20

# Verify expected files exist
ls -la routers/api/v1/robot/
grep -n "modules/context" routers/api/v1/robot/*.go
grep -n "repo_service\." tests/integration/robot_security_test.go | head -5
```

**Success Criteria:**
- Build shows import errors for the 3 robot files
- grep shows the wrong import paths
- grep shows repo_service. references

---

### Step 2: Fix Context Import Paths
**Purpose:** Correct the 3 wrong import paths
**Deployable:** Partial (will still have test error)

**Actions:**
1. Edit `routers/api/v1/robot/robot.go` line 15
2. Edit `routers/api/v1/robot/ready_graph.go` line 15
3. Edit `routers/api/v1/robot/robot_test.go` line 16

**Change:** Replace `"code.gitea.io/gitea/modules/context"` with `"code.gitea.io/gitea/services/context"`

**Verification:**
```bash
# Should show fewer errors
go build . 2>&1 | head -20
```

**Success Criteria:**
- Import errors for context package are gone
- Robot API files compile

---

### Step 3: Fix Repository Import Alias
**Purpose:** Add missing import alias for repository service
**Deployable:** Yes (all errors fixed)

**Actions:**
1. Edit `tests/integration/robot_security_test.go` line 20

**Change:**
```go
// Before:
"code.gitea.io/gitea/services/repository"

// After:
repo_service "code.gitea.io/gitea/services/repository"
```

**Verification:**
```bash
# Should compile without errors
go build .
go build ./cmd/gitea-robot
go test -c ./tests/integration/...
```

**Success Criteria:**
- All builds succeed
- Tests compile

---

### Step 4: Run Verification Tests
**Purpose:** Confirm fixes work correctly
**Deployable:** Yes

**Actions:**
```bash
# Format code
make fmt

# Lint code
make lint-go

# Run robot-specific tests
go test -v ./tests/integration -run TestRobotAPI

# Build verification
go build -o /tmp/gitea .
go build -o /tmp/gitea-robot ./cmd/gitea-robot
```

**Success Criteria:**
- Formatting passes
- Linting passes
- Robot tests pass
- Binaries build successfully

---

## 6. Testing & Verification Strategy

### Test Matrix

| Test Type | Location | Purpose | Command |
|-----------|----------|---------|---------|
| Compilation | Root | Verify no import errors | `go build .` |
| CLI Build | cmd/gitea-robot | Verify CLI compiles | `go build ./cmd/gitea-robot` |
| Unit Tests | routers/api/v1/robot | Verify robot handlers work | `go test ./routers/api/v1/robot/...` |
| Integration Tests | tests/integration | Verify security tests pass | `go test ./tests/integration -run TestRobotAPI` |
| Format Check | All Go files | Verify code style | `make fmt` |
| Lint Check | All Go files | Verify code quality | `make lint-go` |

### Expected Test Results

**Before Fix:**
- ❌ `go build .` - fails with import errors
- ❌ Robot tests - cannot compile

**After Fix:**
- ✅ `go build .` - succeeds
- ✅ `go build ./cmd/gitea-robot` - succeeds
- ✅ Robot tests - compile and pass
- ✅ Format check - passes
- ✅ Lint check - passes (or only unrelated issues)

## 7. Risk & Complexity Review

### Risks from Phase 1 and Mitigations

| Risk | Mitigation | Residual Risk |
|------|------------|---------------|
| Other files have same import issues | Run `go build .` to catch all - already done in research | Low - we'll catch any others during Step 1 |
| Context package has breaking API changes | Check exports match usage - both use APIContext type | Very Low - same type name, same package purpose |
| Tests fail after fix | Run full test suite in Step 4 | Low - changes are mechanical, not functional |
| Missing imports in other test files | Check all robot tests during verification | Low - robot_test.go is the only other test file |

### Complexity Assessment

| Aspect | Complexity | Justification |
|--------|-----------|---------------|
| Code changes | Very Low | 4 lines changed total |
| Cognitive load | Very Low | Purely mechanical, no logic |
| Blast radius | Very Low | 4 files only |
| Testing effort | Low | Standard test commands |
| Rollback | Very Low | Single git revert |

### Rollback Plan

If issues arise:
```bash
# Revert all changes
git checkout -- routers/api/v1/robot/*.go
git checkout -- tests/integration/robot_security_test.go

# Verify rollback
go build .
```

## 8. Open Questions / Decisions for Human Review

### Decision Required: Approach Confirmation

**Q1:** Confirm we should proceed with adding `repo_service` import alias rather than changing all 22 references to `repository.`?

**Context:** 
- Option A: Add alias (recommended) - consistent with other test files
- Option B: Change all refs - more explicit but inconsistent

**Recommendation:** Option A - add alias for consistency

### Decision Required: Testing Scope

**Q2:** Should we run the full integration test suite or just robot-specific tests?

**Context:**
- Full suite: Comprehensive but slow
- Robot-specific: Fast but limited coverage

**Recommendation:** Run robot-specific tests first, then spot-check other tests if time permits

### Decision Required: Pre-commit Checks

**Q3:** Should we run `make fmt` and `make lint-go` before committing?

**Context:** Per AGENTS.md instructions, yes

**Recommendation:** Yes, run formatting and linting before final commit

---

## Implementation Ready Checklist

- [x] Research document approved
- [x] All affected files identified
- [x] Exact changes specified (line numbers, before/after)
- [x] Verification commands documented
- [x] Risk mitigations in place
- [x] Rollback plan documented
- [x] Human review questions listed

**Status:** ✅ **READY FOR PHASE 3 IMPLEMENTATION**

**Next Action:** Proceed with Step 1 (Pre-Flight Verification)
