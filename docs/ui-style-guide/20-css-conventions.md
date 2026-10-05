## CSS conventions

### Utility classes

A small set of utility classes is available across surfaces:

```css
.flex          /* display: flex; */
.flex-center   /* align-items + justify-content center */
.flex-between  /* justify-content: space-between */
.gap-1, .gap-2, .gap-3       /* gap in 0.5/1/1.5rem */
.mt-1, .mt-2, .mt-3          /* margin-top */
.mb-1, .mb-2, .mb-3
.text-muted    /* color: var(--text-muted) */
.text-center
.nowrap        /* white-space: nowrap */
.mono          /* monospace font for technical strings */
.ml-auto       /* margin-left: auto */
```

These are intentionally limited — they're for layout grease, not a full
utility framework. If you need something not on the list, write a CSS class
in the page's stylesheet.

### Inline styles

**Forbidden for new code.** Existing inline styles are extracted to classes
when the surrounding code is touched. The two acceptable uses of inline
`style=`:

1. **Truly dynamic values** that depend on data (e.g., a progress bar
   width). Even then, prefer CSS custom properties: `style="--progress: 67%"`
   with the CSS using `width: var(--progress)`.
2. **One-off layout tweaks** that genuinely don't repeat anywhere (rare —
   if it's worth styling, it usually repeats).

The processes.html template currently has 118 inline styles. The rewrite
extracts them.

### Reusable component classes

A growing set of named patterns (see `shared/components.css`):

- `.fieldset-card` — bordered fieldset with legend, used for grouped form
  fields
- `.empty-cell` — table cell styling for "no data" states
- `.btn-group` — horizontal cluster of buttons with consistent spacing
- `.kv-list` — key-value display (`<dl>`-shaped)

Add new component classes here when the same inline pattern appears in 3+
places.

### Selector specificity

Keep specificity flat. Use class selectors. Avoid `#id` selectors in CSS
(IDs are for JS hooks). Avoid descendant chains deeper than `.parent .child`.

