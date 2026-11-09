## Icons

Verified against: ae059495

**No emoji, ever.** Emoji render inconsistently across platforms, can't take
`currentColor`, and drift from the monochrome look. Use a vendored icon or plain
text — never a pictographic unicode character.

### The sprite

A small subset of **Lucide** (ISC license) is vendored as a single SVG symbol
sprite at `shared/icons.svg` (`go:embed`), inlined **once per page** — Core
injects it into `layout.html` via the `{{iconSprite}}` template func so
`<use href="#icon-…">` resolves same-document. Reference an icon:

```html
<svg class="icon" aria-hidden="true"><use href="#icon-search"></use></svg>
```

Rules:

- **Monochrome, `currentColor` only.** The sprite symbols carry no stroke/fill;
  the `.icon` class (components.css) supplies `stroke: currentColor` — set the
  text color and the icon follows. Never hardcode an icon color.
- **Sizing:** `1em` when inline with text (the `.icon` default), fixed
  **16–20px** in buttons and table cells (`.icon-16` / `.icon-18` / `.icon-20`).
- **Icon-only controls carry an `aria-label`.** A bare icon button is invisible
  to a screen reader without one.
- **Icons accompany labels; they never replace status text.** An icon reinforces
  a word, it isn't the sole carrier of meaning — the label / status text stays.

The set: search, refresh, close, chevron-right/down, arrow-up-right,
sort-asc/desc, lock, box, map-pin, layers, zoom-in/out, crosshair,
alert-triangle, info, check, trash, pencil, download, battery. Add one by copying
the Lucide 24×24 geometry into a new `<symbol id="icon-…">` (geometry only — no
per-shape stroke/fill, so it inherits `.icon`).

**The sprite is wired on Core only.** `{{iconSprite}}` is a Core template func
(`shingo-core/www/helpers.go`) injected by Core's `layout.html` and
`dashboard-map.html`. **Neither Edge admin nor the Operator HMI injects the
sprite**, so a `<use href="#icon-…">` on those surfaces resolves to nothing;
an icon there needs the sprite wired into that surface first.

### Drift test

`TestNoEmojiInTemplatesAndPageJS` (in both `shingo-core/www` and
`shingo-edge/www`) fails CI on any emoji in a template or page-JS file. The
shared detector `shared.IsEmoji` draws the line: the supplementary emoji planes
and any VS16-qualified symbol are rejected; the monochrome geometric glyphs the
surfaces use as affordances (arrows, chevrons, bullets, `✓`/`✗`, the bare `⚠`)
are allowed.

**Known hole: numeric character references.** The scan reads raw bytes, so
`&#128274;` passes it and still renders as an emoji. Decoding entities before
the scan closes it; the one case left is the Robots page's charging bolt
(`&#9889;` in `robots.html` and `RobotTile.js`). The Bins page's row flags are
labelled chips (`locked`, `order #N`, `unconfirmed`, `counts refused`, `notes`),
pinned by `TestBinsPage_FlagsAreLabelled`.

**A flag carries its meaning in words.** A glyph whose meaning lives only in a
`title` is unreadable on a touch screen and at a glance; a row flag is a chip
with a one- or two-word label, and the title keeps the detail.

