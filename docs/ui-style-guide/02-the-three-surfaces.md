## The three surfaces

| Surface | Path | Audience | Loading model |
|---|---|---|---|
| **Core admin** | `shingo-core/www/` | Plant engineers, fleet operations | SSR + client-side enhancement, SSE for live updates |
| **Edge admin** | `shingo-edge/www/` | Plant engineers (per-cell), shift supervisors | SSR + HTMX partial swaps + per-page JS for complex forms |
| **Operator HMI** | `shingo-edge/www/static/operator-station/` | Line operators on a 10" touch panel | Empty-shell HTML + ES module JS render, SSE-driven |

These three surfaces have **intentionally different rendering models**. Do
not try to converge them. Do try to share primitives (tokens, utilities,
status vocabulary).

**Known: the Operator HMI does not follow this guide, and never has.** Every
consistency pass this document records — the token work, the shared component
CSS, the status vocabulary, the icon sprite — landed on Core admin and, less
completely, on Edge admin. The HMI got none of it. As of 2026-07-26,
`operator-display.html` loads exactly one stylesheet, `operator.css`, and pulls
in neither `shared/tokens.css`, `shared/components.css` nor
`shared/status-classes.css`; `operator.css` declares its own parallel
twenty-token `--os-*` palette in raw hex and then uses a further 122 hardcoded
hex literals across 1,223 lines; there is no light theme and no icon sprite. It
is a fourth design system that happens to share a repo with the other three, and
it is the surface an operator looks at for an entire shift. Treat any HMI file
you touch as un-migrated — the conventions below describe where it should end
up, not where it is. The scoped item is queued in `PLAN-master-2026-07-26.md`
(Stage 4).

The one rule the HMI *is* already held to is the no-emoji policy: Edge's
`//go:embed static/*` is recursive, so `TestNoEmojiInTemplatesAndPageJS` walks
`static/operator-station/` along with everything else.

