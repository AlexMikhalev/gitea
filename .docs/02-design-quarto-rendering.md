# Design & Implementation Plan: Quarto Markdown (.qmd) Rendering Support in Gitea

## 1. Summary of Target Behavior

**Core Functionality:**
- `.qmd` files are rendered as HTML using Gitea's existing markdown pipeline
- Quarto YAML frontmatter (between `---` delimiters) is stripped from content
- Mermaid diagram code blocks render client-side via existing Gitea mermaid support
- Quarto code cell options (`#| key: value`) are stripped (not executed)
- On render failure, fallback to basic markdown rendering

**What Users See:**
- `.qmd` files display as rendered HTML with proper syntax-highlighted code blocks
- Mermaid diagrams render as SVG/HTML diagrams
- Standard markdown features (headers, lists, links, images) work correctly

---

## 2. Key Invariants and Acceptance Criteria

### Invariants
1. `.qmd` files always render as HTML when rendering is enabled
2. YAML frontmatter never appears in rendered output
3. Mermaid code blocks always render as diagrams (never as code)
4. Quarto code cell options (`#|`) never appear in output
5. Gitea link/mention post-processing works on .qmd content

### Acceptance Criteria

| ID | Criterion | Test Type |
|----|-----------|-----------|
| AC1 | `.qmd` file viewed in repo renders as HTML | Integration |
| AC2 | YAML frontmatter (---...---) is stripped | Unit |
| AC3 | Mermaid blocks render as diagrams | Integration |
| AC4 | Code blocks have syntax highlighting | Visual |
| AC5 | #| Quarto options are stripped | Unit |
| AC6 | Links (issues, mentions) are post-processed | Integration |
| AC7 | Render failure falls back to markdown | Unit |
| AC8 | Config `[markup].ENABLED` controls rendering | Unit |

---

## 3. High-Level Design and Boundaries

### Option Selected: Add .qmd to Markdown Extensions

**Rationale:** User chose "Add to markdown extensions" - treat .qmd as markdown file.

### Architecture

```
.qmd file
    │
    ▼
[Frontmatter Stripper] ──► YAML metadata (discarded)
    │
    ▼
[Markdown Parser (goldmark)]
    │
    ├── Quarto #| options ──► [Stripper]
    │
    └── Markdown content ──► [Parser]
    │
    ▼
[HTML Output with mermaid marked]
    │
    ▼
[Sanitizer] ──► [Post-processor (links/mentions)]
    │
    ▼
[Client-side Mermaid JS renders diagrams]
```

### New Components
1. **`modules/markup/quarto/`** - New quarto package
   - `frontmatter.go` - YAML frontmatter extraction/stripping
   - `quarto.go` - Main renderer registration and pipeline
   - `strip.go` - Strip Quarto-specific syntax

### Modified Components
1. **`modules/setting/markup.go`** - Add `.qmd` to default markdown extensions
2. **`modules/setting/markup.go`** - Add `[markup.quarto]` config section

### Boundaries
- **In:** Raw .qmd file bytes
- **Out:** Sanitized HTML with mermaid code blocks marked for client rendering
- **Side Effect:** Quarto options stripped; frontmatter discarded

---

## 4. File/Module-Level Change Plan

| File/Module | Action | Before | After | Dependencies |
|-------------|--------|--------|-------|--------------|
| `modules/setting/markup.go` | Modify | Markdown extensions don't include .qmd | Add `.qmd` to default extensions | None |
| `modules/setting/markup.go` | Modify | No quarto config | Add `Quarto` struct with `Enabled` bool | None |
| `modules/markup/quarto/frontmatter.go` | Create | N/A | Strip YAML frontmatter from .qmd | yaml parser |
| `modules/markup/quarto/quarto.go` | Create | N/A | Main renderer; wraps markdown | markup, frontmatter |
| `modules/markup/quarto/strip.go` | Create | N/A | Strip #| Quarto options | regex |
| `modules/markup/render.go` | Modify | No quarto detection | Detect .qmd and route to quarto renderer | markup.RegisterRenderer |
| `modules/markup/main_test.go` | Modify | No quarto tests | Add quarto rendering tests | None |

---

## 5. Step-by-Step Implementation Sequence

### Step 1: Add Quarto Configuration (Setting Change)
**File:** `modules/setting/markup.go`
**Purpose:** Add configuration structure for Quarto rendering
**Deployable:** Yes

```go
// Quarto settings
var Quarto = struct {
    Enabled bool
}{}
```

Add `.qmd` to default markdown file extensions:
```go
markdownFileExtensions = []string{".md", ".markdown", ".mdown", ".mkd", ".livemd", ".qmd"}
```

### Step 2: Create Quarto Package Structure
**Directory:** `modules/markup/quarto/`
**Files:**
- `frontmatter.go` - Extract and strip YAML frontmatter
- `strip.go` - Strip `#|` Quarto options from code blocks
- `quarto.go` - Main renderer that chains to markdown

**Deployable:** Yes (but unused until registered)

### Step 3: Implement Frontmatter Stripper
**File:** `modules/markup/quarto/frontmatter.go`
**Logic:**
1. Read file content
2. If starts with `---`, find matching closing `---`
3. Strip frontmatter, return remaining content

### Step 4: Implement Quarto Option Stripper
**File:** `modules/markup/quarto/strip.go`
**Logic:**
1. Process code blocks (```...```)
2. Remove lines starting with `#|`
3. Return cleaned markdown

### Step 5: Create Quarto Renderer
**File:** `modules/markup/quarto/quarto.go`
**Implements:** `markup.Renderer` interface

```go
type Renderer struct{}

func (Renderer) Name() string         { return "quarto" }
func (Renderer) FileNamePatterns()    { return []string{"*.qmd"} }
func (Renderer) NeedPostProcess()     { return true }
func (Renderer) Render(ctx *markup.RenderContext, input io.Reader, output io.Writer) error {
    // 1. Strip frontmatter
    // 2. Strip #| options
    // 3. Pass to markdown renderer
}
```

### Step 6: Register Renderer
**File:** `modules/markup/quarto/quarto.go`
**In `init()`:** `markup.RegisterRenderer(Renderer{})`

### Step 7: Add Fallback Logic
**In Render method:** If processing fails, return error that triggers fallback to markdown

### Step 8: Add Tests
**File:** `modules/markup/quarto/quarto_test.go`
**Tests:**
- Frontmatter stripping
- Quarto option stripping
- Full render pipeline
- Fallback behavior

---

## 6. Testing & Verification Strategy

| Acceptance Criteria | Test Type | Test Location |
|---------------------|-----------|---------------|
| AC1: .qmd renders | Integration | `modules/markup/main_test.go` |
| AC2: Frontmatter stripped | Unit | `modules/markup/quarto/frontmatter_test.go` |
| AC3: Mermaid renders | Integration | Existing mermaid tests |
| AC4: Code highlighting | Visual | Existing chroma tests |
| AC5: Quarto options stripped | Unit | `modules/markup/quarto/strip_test.go` |
| AC6: Links post-processed | Integration | `modules/markup/main_test.go` |
| AC7: Fallback works | Unit | `modules/markup/quarto/quarto_test.go` |
| AC8: Config controls | Unit | `modules/setting/markup_test.go` |

---

## 7. Risk & Complexity Review

| Risk | Mitigation | Residual Risk |
|------|------------|---------------|
| Quarto syntax not fully compatible with goldmark | Strip problematic syntax; fallback to basic markdown | Low - fallback handles edge cases |
| YAML frontmatter regex fails on edge cases | Use proper YAML parser if needed | Low - frontmatter is optional |
| Client-side mermaid doesn't load | Existing error handling already in place | None - existing behavior |
| Performance impact on large .qmd files | Use existing file size limits | None - reuse existing limits |

---

## 8. Open Questions / Decisions for Human Review

### Already Decided:
1. **External CLI vs Native Go** → Native Go (integrated into markdown pipeline)
2. **Output integration** → Body extraction via existing pipeline
3. **MVP scope** → Basic markdown + mermaid
4. **Error handling** → Fallback to markdown
5. **Configuration** → Configurable via `[markup]`
6. **File extensions** → Add to markdown extensions

### Remaining:
1. **YAML Frontmatter handling** - Should we try to use it for anything (title, author, etc.) or just discard?
2. **Code block language detection** - Quarto uses `{python}`, `{r}` etc. Should we normalize to standard markdown?
3. **Quarto cell options** - Beyond stripping `#|`, should we handle any (like `#| label: fig-1` for cross-references)?

---

## Implementation Notes

### Frontmatter Stripping Algorithm
```go
func StripFrontmatter(input []byte) []byte {
    if !bytes.HasPrefix(input, []byte("---")) {
        return input
    }
    // Find closing ---
    end := bytes.Index(bytes[2:], []byte("\n---"))
    if end < 0 {
        return input
    }
    return bytes.TrimSpace(input[end+5:])
}
```

### Quarto Option Stripping
```go
// In code blocks, remove lines starting with #|
re := regexp.MustCompile(`(?m)^#\|.*$\n?`)
return re.ReplaceAll(content, nil)
```

### Renderer Registration
```go
func init() {
    markup.RegisterRenderer(Renderer{})
}
```
