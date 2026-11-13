## CSS conventions

Verified against: 74c3eb77

### Utility classes

A small set of utility classes, in `shared/components.css`, is on every admin
page:

```css
.flex          /* display: flex */
.gap-1, .gap-2               /* gap: 0.5rem / 1rem */
.mt-2, .mt-3, .mt-4          /* margin-top */
.mb-0, .mb-2, .mb-3, .mb-4   /* margin-bottom */
.text-muted    /* color: var(--text-muted) */
.text-center
.nowrap        /* white-space: nowrap */
.ml-auto       /* margin-left: auto */
.tnum          /* tabular figures, for every number */
```

Each app's sheet adds a few of its own: `.flex-center` and `.flex-between` on
both, `.mono` on Edge. These are intentionally limited — they're for layout
grease, not a full utility framework. If you need something not on the list,
write a CSS class in the page's stylesheet.

**A class name means one thing on every page.** A class declared bare in two
stylesheets one page loads is two rules where the later one wins in silence.
`TestNoBareClassDeclaredInTwoCoLoadedStylesheets` (Core) fails on any such pair
outside `knownCSSCollisions`, which pins the ones that remain with what they
cost to resolve: `.mt-2` and `.mb-2` are 0.5rem in `components.css` and 1rem in
Core's `style.css`, so on Core they are the larger value.

### Inline styles

**Forbidden for new code.** Inline styles are extracted to classes when the
surrounding code is touched. The two acceptable uses of inline `style=`:

1. **Truly dynamic values** that depend on data (e.g., a progress bar
   width). Even then, prefer CSS custom properties: `style="--progress: 67%"`
   with the CSS using `width: var(--progress)`.
2. **One-off layout tweaks** that genuinely don't repeat anywhere (rare —
   if it's worth styling, it usually repeats).

### Reusable component classes

Named patterns both apps share live in `shared/components.css`; `.fieldset-card`
(a bordered fieldset with legend, for grouped form fields) is one. A pattern
one app alone uses stays in that app's sheet: Edge's `.empty-cell` (a table cell
for a "no data" state) is in `shingoedge.css`.

Add a component class when the same inline pattern appears in 3+ places, and
move it to `components.css` only when both apps need it identically (see
[Code organization](03-code-organization.md) › Adding to shared/).

### Selector specificity

Keep specificity flat. Use class selectors. Avoid `#id` selectors in CSS
(IDs are for JS hooks). Avoid descendant chains deeper than `.parent .child`.
