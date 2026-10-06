## Design tokens

### Naming

Tokens use **semantic names**, not visual ones. `--success` not
`--green-bright`. `--surface` not `--card-bg`.

### Shared base + per-surface values

There is **one shared token vocabulary** with **per-surface value overrides**.
The Operator HMI specifically tunes color saturation for shop-floor lighting,
so it redefines values within its own scope while keeping the same token
names.

```css
/* shared/tokens.css — applies to Core and Edge admin */
:root {
  /* Surfaces */
  --bg: #f8f9fa;
  --surface: #ffffff;
  --border: #dee2e6;
  /* Text */
  --text: #212529;
  --text-muted: #6c757d;
  /* UI accent (Indigo) — reserved for interactive chrome, never a status. */
  --accent: #4f4fd6;            /* foreground: text/links/focus/active */
  --accent-hover: #3e3ec0;
  --accent-solid: #4f4fd6;      /* filled buttons/badges under white text (P15) */
  --accent-solid-hover: #3e3ec0;
  /* Semantic — --primary aliases the accent so every CTA/link/tab adopts it. */
  --primary: var(--accent);
  --primary-hover: var(--accent-hover);
  --success: #198754;
  --danger: #dc3545;
  --warning: #ffc107;
  --info: #0dcaf0;  /* must remain distinct from --primary in all themes */
  /* Robot identity hues (U8) — see "Robot identity hues" below. An identity,
   * like a chart series; never the accent, never a status. */
  --robot-1: #22c9ae;
  --robot-2: #8a7bff;
  --station: #f2f5f8;
  /* Elevation steps (cards read one shade lighter than their background). */
  --elev-canvas: #eceef2; --elev-base: #f4f6f8; --elev-surface: #ffffff; --elev-raised: #ffffff;
  /* Geometry */
  --radius: 0.375rem;
  --shadow-sm: 0 1px 3px rgba(0, 0, 0, 0.1);
  --shadow-md: 0 4px 6px rgba(0, 0, 0, 0.1);
}

[data-theme="dark"] {
  --bg: #161b22;
  --surface: #1c2128;
  /* ... etc ... */
  --info: #39d2f5;  /* NOT #58a6ff — that's --primary in dark */
}
```

```css
/* operator-station/operator.css — shop-floor-tuned values */
:root {
  /* Shared semantic names, operator-tuned values */
  --success: #22a84e;   /* brighter for fluorescent-lit floor */
  --danger: #c0392b;
  --warning: #b8860b;
  --primary: #2970d6;
  /* Operator-specific structural tokens */
  --os-touch-min: 56px;
  --os-header-h: 72px;
  --os-footer-h: 40px;
  --os-btn-radius: 14px;
}
```

### Rename mapping (one-time)

The original operator tokens used visual names. Rename to semantic:

| Old | New |
|---|---|
| `--os-green-bright` | `--success` (redefined in operator scope) |
| `--os-blue` | `--primary` (redefined in operator scope) |
| `--os-red` | `--danger` (redefined in operator scope) |
| `--os-amber` | `--warning` (redefined in operator scope) |
| `--os-bg` | `--bg` (redefined in operator scope) |
| `--os-surface` | `--surface` (redefined in operator scope) |
| `--os-touch-min`, `--os-header-h`, `--os-footer-h`, `--os-btn-radius` | unchanged — structural, not semantic |

### Rules

1. **Never hardcode a hex value in a component CSS file.** Use a token.
2. **Never hardcode hover/active variants.** Use `--primary-hover` etc.
3. **If you need a new color, add a token first.** Don't introduce `#7c3aed`
   inline; add `--accent-test: #7c3aed` to tokens.css and reference it.
4. **`--info` and `--primary` must remain visually distinct in all themes.**
   This was a real bug — check both light and dark mode when adjusting.
5. **Indigo is the UI accent, and never a status (P13).** `--accent` (and its
   alias `--primary`) is for interactive chrome — links, focus, selection,
   primary action, section ticks. It **also** serves as **series-1** of the
   curated chart palette (P19, see Data visualization); that overlap is fine. The
   one hard line: indigo is **never a status hue** — status lives in the
   `--status-*-dot` tokens and the `.badge-*` classes; don't cross the streams.

### The substrate ramp (U8)

**Static structure is drawn from `--sub-1` … `--sub-5`, and from nothing else.**

"Structure recedes, state glows" (see Visual principles) says static structure
carries no saturated colour. That principle was stated and **unenforced**: there
was no steel token outside `--map-*`, so anyone implementing a table rule or a
gridline reached for `--border` (`#30363d` — a neutral grey, not steel) or
hardcoded an `rgba()` by eye. `--chart-grid` was the tell — it was
`color-mix(in srgb, var(--text-muted) 25%, transparent)`, i.e. **gridlines
derived from the text ramp**, because structure had nowhere else to come from.

The ramp **owns**: gridlines, axes, table rules, panel edges,
empty/skeleton/disabled states, and tracks with **no** semantic fill.

| Step | Role |
|---|---|
| `--sub-1` | hairline rules, gridlines, row separators, empty/disabled fills |
| `--sub-2` | panel edges, card borders, a table's outer edge |
| `--sub-3` | axes and header rules — structure you are meant to read *along* |
| `--sub-4` | tick marks, structural dots — marks that carry meaning (must clear 3:1) |
| `--sub-5` | emphasis structure |

`--chart-grid` **no longer exists.** It was absorbed as step 1 rather than left
sitting beside the ramp: two token families for one property is how U5's two
colliding `.chip` systems produced a 1.2:1 invisible chip, and a ramp that does
not consolidate recreates that bug in a different property. Charts read
`--sub-1` for gridlines.

**Dark is the reference.** Steps 2–5 *are* the map's steel — `--map-bay-ring` /
`--map-aisle` / `--map-node` / `--map-node-action` now alias them — because the
map is the surface this whole look was generalized from. Step 1 is new,
extrapolated below bay-ring for hairlines. (The map pins `data-theme="dark"` on
`<html>`, so its aliases always resolve to the dark ramp.)

**Light is not a hue flip.** Inverting HSL lightness leaves the top of the ramp
far too weak, because sRGB luminance is not symmetric about mid-grey. The
first-pass flipped values put light step 4 at **2.33:1** — below the 3:1
non-text-contrast floor — so a tick mark that reads in dark would have shipped
invisible in light. Each light step keeps its dark counterpart's hue and
saturation and has its **lightness solved** so its contrast against
`--elev-surface` reproduces the dark step's:

| Step | Dark hex | vs `#161b22` | Light hex | vs `#ffffff` |
|---|---|---|---|---|
| 1 | `#2b3543` | 1.39 | `#d4dbe4` | 1.40 |
| 2 | `#3c4a5e` | 1.92 | `#b1bccd` | 1.92 |
| 3 | `#45566e` | 2.31 | `#9cacc1` | 2.31 |
| 4 | `#66768f` | 3.75 | `#76859d` | 3.74 |
| 5 | `#7a8ba6` | 5.00 | `#5e718d` | 4.97 |

Step-to-step separation is identical in both themes (1.38 / 1.20 / 1.62 / 1.33),
which is the property that makes them the same ramp rather than two ramps that
happen to share a hue. Steps 4 and 5 clear 3:1 in **both** themes.

**If a step moves, re-derive it the same way — never eyeball a light value.**

### Robot identity hues (U8/U9)

**A robot's colour is an identity, not a status and not the accent.**

Three tokens, theme-invariant, in `:root`:

| Token | Value | What it is |
|---|---|---|
| `--robot-1` | `#22c9ae` | Robot 1 — its chevrons and words on the cell picture, its supply path on the plant map, its chevrons in a swap-mode glyph |
| `--robot-2` | `#8a7bff` | Robot 2 — the same three things for the second robot |
| `--station` | `#f2f5f8` | the press itself: slot outlines, the line-side bar, and the NEW bin in a glyph — on the station; the desktop Processes page, which follows the light theme, draws these through `--os-station` mapped to `--text-strong` |

The operator station keeps its own copies — `--os-r1`, `--os-r2`,
`--os-station` — for the reason the z-layer and substrate copies exist:
`operator-display.html` links only `operator.css`, so a `var(--robot-1)` there
resolves to nothing and a robot's chevrons silently disappear. Both copies move
together, and `TestOperatorStationRobotColoursMatchShared` /
`TestOperatorStationStationHueMatchesShared` fail if one moves alone.

**How this sits with P13 (indigo is the accent, never a status).** A robot hue
is neither. It is an **identity**, in exactly the sense a chart series is one
(P19): the mark means "this one, not that one", and it carries no judgement
about whether anything is good, bad or late. So it gets its own token rather
than borrowing the accent's or a status dot's, and the rules that follow are:

- **Never `--accent` for a robot, and never a robot hue for a control.** Where a
  robot line and an accent selection share a screen — the desktop composer's D1
  is exactly that — **the accent is the border and the button, the robot is the
  move.** Selecting a module outlines its cards in indigo; the chevrons leaving
  them stay teal.
- **Never a robot hue for a status.** Status lives in `--status-*-dot` and the
  `.badge-*` classes, as always.
- **`--status-staged-dot` (teal) and `--robot-1` (teal) never share a surface
  element.** They are close enough to be confused and mean entirely different
  things — one says a bin is waiting, the other says which robot is moving. A
  chip that carries both is a chip that says neither; put the state in the chip
  and the robot on the line.

Robot 2's indigo is close to `--accent`, and deliberately so: it is the second
identity in a two-identity set, and the set was chosen for separation from each
other first. The separation from the accent is carried by ROLE, not by hue —
an accent mark is a border or a filled control, a robot mark is a stroked path
or a chevron, and no element is both.

### The ink on a fill, and the scrim (U9)

**Two tokens that did not exist, so every component wrote the literal instead.**

| Token | Value | What it is |
|---|---|---|
| `--on-accent-solid` | `#ffffff` | the ink that sits ON an `--accent-solid` fill — `.btn-primary`, a filled badge, the knob of an on switch |
| `--scrim` | `rgba(0, 0, 0, .5)` light / `rgba(4, 6, 9, .62)` dark | the backdrop behind any modal: `.modal-overlay` on both admin surfaces, and the desktop composer's own sheet |
| `--on-success-fill` | `#ffffff` light / `#0d1117` dark | the name on a has-payload node tile (see The Nodes page) |
| `--on-staged-fill` | `#0d1117` | the name on a staged node tile; the teal is theme-invariant, so its ink is too |

`--accent-solid` was chosen for 6.14:1 against white and is theme-invariant, so
its ink is one value in both themes and is **not** overridden in the dark block
— the same reasoning as an identity hue, from the other end. The scrim *is*
overridden, because the page it darkens changes: neutral black over
`--elev-canvas` reads as a flat grey wash rather than a recess, so dark gets a
deeper, cooler value.

The light scrim is `.modal-overlay`'s own `rgba(0,0,0,.5)`, kept verbatim so
nothing moved when the literal became a token. Core's overlay had been
`rgba(0,0,0,.45)` — two scrims nobody had decided were different — and now
matches Edge's.

Rule 3 above ("if you need a new colour, add a token first") is the rule these
two were added under, and the drift test is what makes it stick:
`shared/identity_hue_drift_test.go` holds each token's value against this table
**and** asserts that `shingoedge.css`, Core's `style.css` and
`processes-desktop.css` reference the token rather than spelling the colour
out. A token nothing references is a token the next literal ignores.

**There is no exception any more.** `flow-picture.css` used to carry literal
colours in four declarations, moved verbatim out of `operator.css` by U9a's
extraction, and the reason was that `operator-display.html` linked no shared
stylesheet — so a `var(--scrim)` there resolved to nothing. It links
`shared/tokens.css` now (and pins `data-theme="dark"`, the kiosk convention),
which was the stated condition for retiring them. Two of the five values were
`--text-strong`'s **dark** value spelled out, so on a light Processes page the
position name rendered near-white on a white card.

The same drift test survives inverted: `flow-picture.css` must carry **no**
literal colour. A literal creeping back into the one stylesheet two surfaces
share is worth catching in either direction.

The station's hand-copied `--os-*` ramp went with it. `operator.css` declared
its own copies of the substrate steps, the robot hues, the station hue and the
accent, with a drift test holding each copy to the master in both directions;
the copies are aliases now (`--os-sub-3: var(--sub-3)`), kept only because the
locked swap-mode glyph file reads those names, and an alias cannot drift.

### The loader-station outline

`--loader-outline` (`#0a8f6a`, theme-invariant) is the teal a Core loader
station is drawn in on the Nodes page: the box outline, each window tile's
outline, and the 12-18% tints mixed from it. It was a literal repeated through
Core's `style.css` until the station boxes were redrawn; it is an outline and a
tint, never an ink, so it needs no dark override. An identity like a robot hue
("this belongs to a station"), never a status. Ink on a fill of it is
`--on-accent-solid`, the same white.

### Type scale

Five named steps, defined in `tokens.css`, replace the ~12 ad-hoc `rem` sizes
that had accreted across pages:

| Token | Size | Use |
|---|---|---|
| `--font-xs` | 0.75rem (12px) | labels, captions, table chrome |
| `--font-sm` | 0.875rem (14px) | secondary text, dense table cells |
| `--font-base` | 1rem (16px) | body default |
| `--font-lg` | 1.25rem (20px) | section titles / `h2` |
| `--font-xl` | 1.5rem (24px) | page titles / `.ops-title` |

Two weights only: `--fw-normal` (400) for labels and body, `--fw-bold` (600)
for emphasis, headings, and numbers. Display heroes (KPI numbers at 2rem+) are
**not** on this scale — they own their size via `.kpi-value` / `.ov-hero`.

**Numbers always get `tabular-nums`.** Any count, quantity, duration, or metric
uses `font-variant-numeric: tabular-nums` (the `.tnum` utility) so digits align
column-to-column and don't jitter as live values tick.

### Spacing scale

A 4px-base scale (`--sp-1` … `--sp-6` = 4/8/12/16/24/32px) for gap, margin, and
padding. Use these instead of the 0.3 / 0.35 / 0.45 / 0.55rem soup. The existing
`.gap-*` / `.mt-*` / `.mb-*` utilities keep their values; new spacing references
the tokens.

### Motion

Two durations and one easing, in `tokens.css`: `--dur-fast` (~120ms, hovers and
small state flips), `--dur-base` (~250ms, transitions where something moves),
and `--ease` (`cubic-bezier(0.4, 0, 0.2, 1)`).

**Reduced-motion is law.** A `@media (prefers-reduced-motion: reduce)` block in
`tokens.css` zeroes `--dur-fast` / `--dur-base`, so any animation that drives
its timing from the tokens is disabled automatically for users who ask for it —
no per-component opt-in. New animation MUST reference `var(--dur-*)` rather than
a hardcoded `0.25s` to inherit this; existing hardcoded transitions migrate
opportunistically. This is the mechanism behind "motion means motion" (see
Visual principles).

