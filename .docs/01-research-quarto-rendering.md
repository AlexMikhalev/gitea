# Research Document: Quarto Markdown (.qmd) Rendering Support in Gitea

## 1. Problem Restatement and Scope

**Problem:** Gitea currently lacks native support for rendering Quarto Markdown (.qmd) files, which are increasingly used for scientific and technical documentation. Users who store `.qmd` files in their repositories see raw file content instead of rendered output.

**In Scope:**
- Rendering `.qmd` files to HTML in repository file views
- Supporting standard Quarto HTML output (standalone document with CSS/JS)
- Integration with Gitea's existing markup rendering pipeline
- Post-processing for special Gitea link handling (issues, mentions, etc.)

**Out of Scope:**
- Code cell execution (Python, R, Julia, Observable JS) - requires kernel access
- PDF/Word output formats - only HTML rendering for preview
- Jupyter notebook (.ipynb) native rendering (separate concern)
- Quarto project websites (multi-file projects)

---

## 2. User & Business Outcomes

**User-Facing Changes:**
- Users viewing `.qmd` files in Gitea will see rendered HTML instead of raw markdown
- Quarto-specific features (equations, cross-references, callouts) will render correctly
- Code blocks will retain syntax highlighting via Chroma

**Business Outcomes:**
- Gitea becomes more attractive for data science and academic users
- Parity with GitHub's Quarto support
- Enables reproducible research workflows in Gitea

---

## 3. System Elements and Dependencies

| Component | Location | Role | Dependencies |
|-----------|----------|------|--------------|
| **markup.Renderer interface** | `modules/markup/renderer.go` | Defines renderer contract | None |
| **markup.RegisterRenderer** | `modules/markup/renderer.go` | Registers renderers | Called by init() |
| **markup.Render** | `modules/markup/render.go` | Main render orchestration | Uses renderer |
| **External Renderer** | `modules/markup/external/external.go` | External CLI renderer | Uses setting.MarkupRenderer |
| **setting.MarkupRenderer** | `modules/setting/markup.go` | External renderer config | Ini file parsing |
| **markdown renderer** | `modules/markup/markdown/markdown.go` | Reference implementation | goldmark, chroma |
| **orgmode renderer** | `modules/markup/orgmode/orgmode.go` | Similar native renderer | go-org library |

**Key Flow:**
1. File viewed → `render.go:Render()` called
2. `DetectMarkupRenderer()` matches by filename pattern (`*.qmd`)
3. Renderer found → `Render()` method called
4. Output sanitized and post-processed

---

## 4. Constraints and Their Implications

| Constraint | Why It Matters | Implications |
|------------|---------------|-------------|
| **Quarto CLI dependency** | External renderer requires `quarto` binary | Admin must install Quarto; fallback to raw if missing |
| **Security (sanitization)** | Rendered HTML must be sanitized | Need appropriate sanitizer rules for Quarto output |
| **Performance** | External process spawn is slow | Large files/timeouts must be handled |
| **Existing extension** | `.qmd` currently treated as generic file | Must add to markup system, not replace |
| **No code execution** | Can't execute code cells in preview | Render static HTML; code blocks as syntax-highlighted |
| **Standalone HTML output** | Quarto produces full HTML documents | May conflict with Gitea's post-processing pipeline |

---

## 5. Risks, Unknowns, and Assumptions

### Unknowns
1. **Quarto CLI availability**: Is `quarto` typically installed on servers running Gitea?
2. **HTML output format**: Does Quarto's `--to html` produce sanitized, safe HTML?
3. **Post-processing compatibility**: Does Quarto's HTML output break Gitea's link rewriting?

### Assumptions
- Quarto HTML output can be processed by Gitea's post-processor (issue links, mentions)
- External renderer sandboxing is sufficient for security
- `quarto render` command syntax is stable across versions
- Quarto outputs self-contained HTML with embedded CSS

### Risks
| Risk | Impact | Mitigation |
|------|--------|------------|
| Quarto not installed | Render fails | Fallback to raw file or error message |
| Quarto HTML breaks post-processing | Links not rewritten | Test thoroughly; may need iframe mode |
| Malicious Quarto file | Security issue | Sanitization rules; sandbox |
| Large file timeout | Render hangs | Timeout/cancellation support |
| Version compatibility | Command flags change | Document supported versions |

---

## 6. Context Complexity vs. Simplicity Opportunities

### Complexity Sources
1. **External process management** - spawning, timeout, error handling
2. **Quarto output post-processing** - Gitea expects certain HTML structure
3. **Link resolution** - Gitea rewrites links; Quarto uses relative paths

### Simplification Strategies
1. **Start with iframe mode** - Avoids post-processing complexity
2. **Use existing external renderer framework** - No new rendering architecture
3. **Minimal sanitizer rules** - Trust Quarto output initially

---

## 7. Questions for Human Reviewer

1. **Deployment assumption**: Is it acceptable to require administrators to install the `quarto` CLI? Or should we pursue a native Go implementation (more complex but no external dependency)?

2. **Output format**: Quarto produces standalone HTML with full CSS/JS. Should we:
   - (A) Use iframe rendering (simplest, isolated)
   - (B) Extract body content and integrate with Gitea's styling (more complex, consistent look)
   - (C) Render to raw HTML with post-processing (may break)

3. **Feature scope for MVP**: Should the initial implementation support only static `.qmd` rendering, or should we attempt any Quarto-specific features (callouts, cross-refs, math)?

4. **File size limits**: Quarto documents can be large with embedded plots. Should we impose a size limit similar to markdown, or allow larger files?

5. **Configuration**: Should Quarto rendering be:
   - (A) Always enabled when Quarto CLI is present
   - (B) Configurable via `[markup.quarto]` section (like other external renderers)

6. **Existing .qmd files**: Currently treated as plain text. Should we auto-detect and re-render existing files, or only render new uploads?

7. **Error handling**: If `quarto render` fails, should we:
   - (A) Show error message to user
   - (B) Fallback to raw file display
   - (C) Fallback to markdown rendering (if valid)

8. **Version compatibility**: Document should state minimum Quarto version requirement. Any preference on tested versions?

9. **Documentation**: Should we add Quarto to Gitea's markup documentation page?

10. **Testing approach**: Unit tests vs integration tests - any specific requirements for test coverage?
