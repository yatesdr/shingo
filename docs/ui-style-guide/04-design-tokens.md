## Design tokens

Verified against: fe06474a

### Naming

Tokens use **semantic names**, not visual ones. `--success` not
`--green-bright`. `--surface` not `--card-bg`.

### Shared tokens, and the station's own

There is **one shared token vocabulary**, `shared/tokens.css`, in two theme
blocks. Core and Edge admin load it on every page; the Operator HMI loads it
under its own stylesheet. The HMI changes no shared token's value. Its
`operator.css` declares names of its own, `--os-*`: a palette for a dark touch
panel and the aliases of the shared tokens it draws with; and it repeats the
shared `--z-*` layer scale, value for value (see
[The three surfaces](02-the-three-surfaces.md)).

The shared tokens, both themes (`shared/tokens.css`; `—` means the dark theme
keeps the light value):

<!-- generated: shared/tokens.css -->

| Token | Light (`:root`) | Dark (`[data-theme="dark"]`) |
|---|---|---|
| `--bg` | `var(--elev-base)` | `var(--elev-base)` |
| `--bg-dark` | `#e9ecef` | `#0d1117` |
| `--surface` | `var(--elev-surface)` | `var(--elev-surface)` |
| `--border` | `#dee2e6` | `#30363d` |
| `--elev-canvas` | `#eceef2` | `#0b0f16` |
| `--elev-base` | `#f4f6f8` | `#0d1117` |
| `--elev-surface` | `#ffffff` | `#161b22` |
| `--elev-raised` | `#ffffff` | `#1f2733` |
| `--text` | `#212529` | `#c9d1d9` |
| `--text-muted` | `#68717a` | `#8b949e` |
| `--text-strong` | `#11161c` | `#e6edf3` |
| `--accent` | `#4f4fd6` | `#7c7cf0` |
| `--accent-hover` | `#3e3ec0` | `#9a9af5` |
| `--accent-glow` | `color-mix(in srgb, var(--accent) 55%, transparent)` | — |
| `--accent-solid` | `#4f4fd6` | — |
| `--accent-solid-hover` | `#3e3ec0` | — |
| `--on-accent-solid` | `#ffffff` | — |
| `--on-success-fill` | `#ffffff` | `#0d1117` |
| `--on-staged-fill` | `#0d1117` | — |
| `--scrim` | `rgba(0, 0, 0, .5)` | `rgba(4, 6, 9, .62)` |
| `--loader-outline` | `#0a8f6a` | — |
| `--primary` | `var(--accent)` | `var(--accent)` |
| `--primary-hover` | `var(--accent-hover)` | `var(--accent-hover)` |
| `--primary-dark` | `var(--accent-hover)` | `var(--accent-hover)` |
| `--success` | `#198754` | `#3fb950` |
| `--danger` | `#dc3545` | `#f85149` |
| `--warning` | `#ffc107` | `#d29922` |
| `--info` | `#0dcaf0` | `#39d2f5` |
| `--viz-primary` | `#1b2027` | `#eaf0f6` |
| `--viz-secondary` | `var(--text-muted)` | — |
| `--viz-accent` | `var(--accent)` | — |
| `--viz-indigo` | `var(--accent)` | — |
| `--viz-violet` | `#7c3aed` | `#b07cf5` |
| `--viz-sky` | `#0284c7` | `#38bdf8` |
| `--viz-teal` | `#0d9488` | `#2dd4bf` |
| `--viz-amber` | `#b45309` | `#facc5b` |
| `--viz-orange` | `#c2410c` | `#f97316` |
| `--viz-coral` | `#e11d48` | `#fb7185` |
| `--viz-green` | `#059669` | `#34d399` |
| `--group-1` | `var(--viz-sky)` | `var(--viz-sky)` |
| `--group-2` | `var(--viz-orange)` | `var(--viz-orange)` |
| `--chip-ink-ok` | `#04714f` | `var(--viz-green)` |
| `--chip-ink-near` | `#984608` | `var(--viz-amber)` |
| `--chip-ink-below` | `#b6173a` | `var(--viz-coral)` |
| `--chip-ink-muted` | `#5c646c` | `#949ca6` |
| `--chip-ink-err` | `#ffffff` | `#1b0508` |
| `--viz-seq-1` | `#d5f5ef` | `#0c322e` |
| `--viz-seq-2` | `#7fd9c9` | `#14564e` |
| `--viz-seq-3` | `#2db3a0` | `#1c8074` |
| `--viz-seq-4` | `#0d9488` | `#2dd4bf` |
| `--viz-seq-5` | `#0b5f57` | `#7ff0e1` |
| `--viz-div-neg-2` | `#c81e4a` | `#fb7185` |
| `--viz-div-neg-1` | `#e9607c` | `#cc6b7d` |
| `--viz-div-mid` | `#b7bcc4` | `#6e7681` |
| `--viz-div-pos-1` | `#34a594` | `#2ca89a` |
| `--viz-div-pos-2` | `#0d8b7f` | `#2dd4bf` |
| `--status-pending-dot` | `#8b95a5` | — |
| `--status-queued-dot` | `#7aa2f0` | — |
| `--status-dispatched-dot` | `#4f9bff` | — |
| `--status-in-transit-dot` | `#34c3e0` | — |
| `--status-staged-dot` | `#15b8a0` | — |
| `--status-reshuffling-dot` | `#df6fb4` | — |
| `--status-blocked-dot` | `#f85149` | — |
| `--status-delivered-dot` | `#3fb950` | — |
| `--status-faulted-bg` | `#fed7aa` | `#4a2606` |
| `--status-faulted-ink` | `#9a3412` | `#fdba74` |
| `--status-faulted-border` | `#fdba74` | `#6b2c06` |
| `--status-failed-bg` | `#fecaca` | `#450a0a` |
| `--status-failed-ink` | `#991b1b` | `#f87171` |
| `--status-failed-border` | `#fca5a5` | `#5c1515` |
| `--sub-1` | `#d4dbe4` | `#2b3543` |
| `--sub-2` | `#b1bccd` | `#3c4a5e` |
| `--sub-3` | `#9cacc1` | `#45566e` |
| `--sub-4` | `#76859d` | `#66768f` |
| `--sub-5` | `#5e718d` | `#7a8ba6` |
| `--map-floor` | `#131a24` | — |
| `--map-aisle` | `var(--sub-3)` | — |
| `--map-node` | `var(--sub-4)` | — |
| `--map-node-action` | `var(--sub-5)` | — |
| `--map-bay-ring` | `var(--sub-2)` | — |
| `--map-bay-glyph` | `#6b7a90` | — |
| `--map-robot-idle` | `#8b949e` | — |
| `--map-dock-charge` | `#e3b341` | — |
| `--map-dock-park` | `#d98c4a` | — |
| `--robot-1` | `#22c9ae` | — |
| `--robot-2` | `#8a7bff` | — |
| `--station` | `#f2f5f8` | — |
| `--map-class-1` | `#6cb0ff` | — |
| `--map-class-2` | `#56d364` | — |
| `--map-class-3` | `#e3b341` | — |
| `--map-class-4` | `#d2a8ff` | — |
| `--map-class-5` | `#ff9b72` | — |
| `--map-class-6` | `#79c0ff` | — |
| `--map-class-7` | `#f0883e` | — |
| `--kpi-up` | `var(--viz-green)` | — |
| `--kpi-down` | `var(--viz-coral)` | — |
| `--font-xs` | `0.75rem` | — |
| `--font-sm` | `0.875rem` | — |
| `--font-base` | `1rem` | — |
| `--font-lg` | `1.25rem` | — |
| `--font-xl` | `1.5rem` | — |
| `--fw-normal` | `400` | — |
| `--fw-bold` | `600` | — |
| `--sp-1` | `0.25rem` | — |
| `--sp-2` | `0.5rem` | — |
| `--sp-3` | `0.75rem` | — |
| `--sp-4` | `1rem` | — |
| `--sp-5` | `1.5rem` | — |
| `--sp-6` | `2rem` | — |
| `--dur-fast` | `120ms` | — |
| `--dur-base` | `250ms` | — |
| `--ease` | `cubic-bezier(0.4, 0, 0.2, 1)` | — |
| `--z-raised` | `1` | — |
| `--z-sticky` | `10` | — |
| `--z-tooltip` | `20` | — |
| `--z-dropdown` | `30` | — |
| `--z-chrome` | `40` | — |
| `--z-modal` | `50` | — |
| `--z-modal-over` | `60` | — |
| `--z-popover` | `70` | — |
| `--z-toast` | `80` | — |
| `--radius` | `6px` | — |
| `--shadow` | `0 1px 3px rgba(0, 0, 0, 0.1)` | `0 1px 3px rgba(0, 0, 0, 0.3)` |
| `--shadow-sm` | `0 1px 3px rgba(0, 0, 0, 0.1)` | `0 1px 3px rgba(0, 0, 0, 0.3)` |
| `--shadow-md` | `0 4px 6px rgba(0, 0, 0, 0.1)` | `0 4px 6px rgba(0, 0, 0, 0.4)` |
| `--shadow-lg` | `0 4px 6px rgba(0, 0, 0, 0.1)` | `0 4px 6px rgba(0, 0, 0, 0.4)` |
| `--route-here` | `var(--accent)` | `var(--accent)` |
| `--route-on-here` | `#ffffff` | `#0d1117` |
| `--route-passed` | `var(--text-muted)` | `var(--text-muted)` |
| `--route-remaining` | `var(--text)` | `var(--text)` |
| `--route-chip` | `var(--bg-dark)` | `#21262d` |
| `--route-chip-ink` | `var(--text)` | `var(--text)` |

<!-- /generated -->

The operator station's own `:root` block
(`shingo-edge/www/static/operator-station/operator.css`):

<!-- generated: shingo-edge/www/static/operator-station/operator.css -->

| Token | Value |
|---|---|
| `--os-bg` | `#101418` |
| `--os-header-bg` | `#1a2230` |
| `--os-footer-bg` | `#151c26` |
| `--os-surface` | `#1b232d` |
| `--os-border` | `#2a3545` |
| `--os-text` | `#ffffff` |
| `--os-text-muted` | `#8a96a4` |
| `--os-text-dim` | `#5a6575` |
| `--os-green` | `#1a7a3d` |
| `--os-green-bright` | `#22a84e` |
| `--os-amber` | `#b8860b` |
| `--os-red` | `#c0392b` |
| `--os-blue` | `#2970d6` |
| `--os-purple` | `#7b5ea7` |
| `--os-orange` | `#d4712a` |
| `--os-gray` | `#3a3f47` |
| `--os-gray-light` | `#4a5060` |
| `--os-btn-radius` | `14px` |
| `--os-grid-gap` | `10px` |
| `--os-header-h` | `72px` |
| `--os-footer-h` | `40px` |
| `--z-raised` | `1` |
| `--z-sticky` | `10` |
| `--z-tooltip` | `20` |
| `--z-dropdown` | `30` |
| `--z-chrome` | `40` |
| `--z-modal` | `50` |
| `--z-modal-over` | `60` |
| `--z-popover` | `70` |
| `--z-toast` | `80` |
| `--os-station` | `var(--station)` |
| `--os-robot-1` | `var(--robot-1)` |
| `--os-robot-2` | `var(--robot-2)` |
| `--os-sub-3` | `var(--sub-3)` |
| `--os-sub-4` | `var(--sub-4)` |
| `--os-sub-5` | `var(--sub-5)` |
| `--os-accent-solid` | `var(--accent-solid)` |
| `--os-accent-solid-hover` | `var(--accent-solid-hover)` |
| `--os-accent` | `var(--accent)` |
| `--os-touch-min` | `56px` |

<!-- /generated -->

A row that reads `var(--…)` is an alias of the shared token, so it cannot
drift. The `--z-*` rows are a copy of the shared layer scale, held to it by
`TestOperatorStationZLayerScaleMatchesShared`. The rest is the station's own
palette.

### Rules

1. **Never hardcode a hex value in a component CSS file.** Use a token.
2. **Never hardcode hover/active variants.** Use `--primary-hover` etc.
3. **If you need a new color, add a token first.** Don't introduce `#7c3aed`
   inline; add `--accent-test: #7c3aed` to tokens.css and reference it.
4. **`--info` and `--primary` must remain visually distinct in all themes.**
   Check both light and dark when adjusting either; `TestTokensCSSInfoDistinctFromPrimary`
   holds them apart.
5. **Indigo is the UI accent, and never a status.** `--accent` (and its
   alias `--primary`) is for interactive chrome — links, focus, selection,
   primary action, section ticks. It **also** serves as **series-1** of the
   curated chart palette (see [Data visualization](06-data-visualization.md));
   that overlap is fine. The one hard line: indigo is **never a status hue** —
   status lives in the `--status-*-dot` tokens and the `.badge-*` classes; don't
   cross the streams.

### The substrate ramp

**Static structure is drawn from `--sub-1` … `--sub-5`, and from nothing else.**

"Structure recedes, state glows" (see [Visual principles](07-visual-principles.md))
says static structure carries no saturated colour. The ramp is the steel that
structure is drawn in, so a table rule or a gridline never reaches for
`--border` (a neutral grey, not steel), an `rgba()` picked by eye, or a step of
the text ramp.

The ramp **owns**: gridlines, axes, table rules, panel edges,
empty/skeleton/disabled states, and tracks with **no** semantic fill.

| Step | Role |
|---|---|
| `--sub-1` | hairline rules, gridlines, row separators, empty/disabled fills |
| `--sub-2` | panel edges, card borders, a table's outer edge |
| `--sub-3` | axes and header rules — structure you are meant to read *along* |
| `--sub-4` | tick marks, structural dots — marks that carry meaning (must clear 3:1) |
| `--sub-5` | emphasis structure |

There is no separate grid token: charts read `--sub-1` for gridlines. One token
family per property; a second family beside the ramp is two answers to one
question, and the two drift apart.

**Dark is the reference.** Steps 2–5 *are* the map's steel — `--map-bay-ring` /
`--map-aisle` / `--map-node` / `--map-node-action` alias them — because the map
is the surface this look was generalized from. Step 1 sits below bay-ring, for
hairlines. (The map pins `data-theme="dark"` on `<html>`, so its aliases always
resolve to the dark ramp.)

**Light is not a hue flip.** Inverting HSL lightness leaves the top of the ramp
too weak, because sRGB luminance is not symmetric about mid-grey. Each light
step keeps its dark counterpart's hue and saturation and has its **lightness
solved** so its contrast against `--surface` reproduces the dark step's.
`TestSubstrateRampContrast` holds the three properties that make it one ramp:
steps 4 and 5 clear the 3:1 non-text floor in both themes (they carry meaning:
tick marks, structural dots, emphasis), every step is stronger than the one
before it, and each light step's ratio is within 0.05 of its dark twin's. Steps
1–3 have no floor on purpose: a hairline at 3:1 is a border.

**If a step moves, re-derive it the same way — never eyeball a light value.**

### Robot identity hues

**A robot's colour is an identity, not a status and not the accent.**

Three tokens, theme-invariant, in `:root`:

| Token | Value | What it is |
|---|---|---|
| `--robot-1` | `#22c9ae` | Robot 1 — its chevrons and words on the cell picture, its supply path on the plant map, its chevrons in a swap-mode glyph |
| `--robot-2` | `#8a7bff` | Robot 2 — the same three things for the second robot |
| `--station` | `#f2f5f8` | the press itself: slot outlines, the line-side bar, and the NEW bin in a glyph — on the station; the desktop Processes page, which follows the light theme, draws these through `--os-station` mapped to `--text-strong` |

**How this sits with rule 5 (indigo is the accent, never a status).** A robot
hue is neither. It is an **identity**, in exactly the sense a chart series is
one: the mark means "this one, not that one", and it carries no judgement
about whether anything is good, bad or late. So it gets its own token rather
than borrowing the accent's or a status dot's, and the rules that follow are:

- **Never `--accent` for a robot, and never a robot hue for a control.** Where a
  robot line and an accent selection share a screen — the desktop composer's
  cell picture is exactly that — **the accent is the border and the button, the
  robot is the move.** Selecting a module outlines its cards in indigo; the
  chevrons leaving them stay teal.
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

### Recipe group identities

| Token | Value | What it is |
|---|---|---|
| `--group-1` | `var(--viz-sky)` | a recipe's first group (Left Hand on a two-off) — the fill of its L tag on the cell picture's cards |
| `--group-2` | `var(--viz-orange)` | its second group (Right Hand) — the fill of the R tag |

A recipe group is an identity in the robot hues' sense — "this one, not that
one" — so it has a token of its own rather than the accent, a robot hue or a
status, and it is drawn **on the tag only**: the card's border and fill stay
steel. The tokens alias mark hues, so they follow the theme with them (both
blocks of `tokens.css` declare them). `--viz-orange` sits near the warning
hue; the tag's shape and its letter carry the difference.

### The ink on a fill, and the scrim

**The ink on a fill and the backdrop behind a modal are tokens, never
literals.**

| Token | Value | What it is |
|---|---|---|
| `--on-accent-solid` | `#ffffff` | the ink that sits ON an `--accent-solid` fill — `.btn-primary`, a filled badge, the knob of an on switch |
| `--scrim` | `rgba(0, 0, 0, .5)` light / `rgba(4, 6, 9, .62)` dark | the backdrop behind any modal: `.modal-overlay` on both admin surfaces, and the desktop composer's own sheet |
| `--on-success-fill` | `#ffffff` light / `#0d1117` dark | the name on a has-payload node tile (see The Nodes page) |
| `--on-staged-fill` | `#0d1117` | the name on a staged node tile; the teal is theme-invariant, so its ink is too |

`--accent-solid` clears 6.14:1 against white and is theme-invariant, so its ink
is one value in both themes and is **not** overridden in the dark block — the
same reasoning as an identity hue, from the other end. The scrim *is*
overridden, because the page it darkens changes: neutral black over
`--elev-canvas` reads as a flat grey wash rather than a recess, so dark gets a
deeper, cooler value.

`shared/identity_hue_drift_test.go` holds both ends:
`TestSurfaceInkTokensAreDeclared` (the ink is one value, the scrim is per
theme), `TestStyleGuideQuotesTheSurfaceInkTokens` (this table quotes their
values) and `TestSurfaceInkTokensAreReadByBothAdminSurfaces`, which asserts that
`shingoedge.css` and Core's `style.css` read both tokens and `components.css`
reads the ink, rather than spelling the colour out. A token nothing references
is a token the next literal ignores.

**The station picture carries no literal colour.** `flow-picture.css` is the
one stylesheet the station and the desktop both draw from, and both pages load
`shared/tokens.css`, so every colour in it is a token
(`TestTheStationPictureCarriesNoLiteralColour`). A literal there would be one
theme's answer on a picture both themes draw.

The station's `--os-sub-*`, `--os-robot-*`, `--os-station` and `--os-accent*`
are aliases of the shared tokens (the station table above). They exist because
the swap-mode glyph file reads those names and is not edited per surface; the
desktop declares the same names in `processes-desktop.css`, with `--os-station`
mapped to `--text-strong` so the glyphs read on a light page.

### The loader-station outline

`--loader-outline` (`#0a8f6a`, theme-invariant) is the teal a Core loader
station is drawn in on the Nodes page: the box outline, each window tile's
outline, and the 12-18% tints mixed from it. It is an outline and a tint, never
an ink, so it needs no dark override. An identity like a robot hue ("this
belongs to a station"), never a status. Ink on a fill of it is
`--on-accent-solid`, the same white.

### Type scale

Five named steps, defined in `tokens.css`:

| Token | Size | Use |
|---|---|---|
| `--font-xs` | 0.75rem (12px) | labels, captions, table chrome |
| `--font-sm` | 0.875rem (14px) | secondary text, dense table cells |
| `--font-base` | 1rem (16px) | body default |
| `--font-lg` | 1.25rem (20px) | section titles / `h2` |
| `--font-xl` | 1.5rem (24px) | page titles / `.ops-title` |

Two weights only: `--fw-normal` (400) for labels and body, `--fw-bold` (600)
for emphasis, headings, and numbers. Display heroes (KPI numbers at 2rem+) are
**not** on this scale — they own their size via `.kpi-value` /
`.kpi-tile--hero`.

**Numbers always get `tabular-nums`.** Any count, quantity, duration, or metric
uses `font-variant-numeric: tabular-nums` (the `.tnum` utility) so digits align
column-to-column and don't jitter as live values tick.

### Spacing scale

A 4px-base scale (`--sp-1` … `--sp-6` = 4/8/12/16/24/32px) for gap, margin, and
padding. New spacing references the tokens, never a one-off `rem`; the `.gap-*`
/ `.mt-*` / `.mb-*` utilities keep their own values.

### Motion

Two durations and one easing, in `tokens.css`: `--dur-fast` (~120ms, hovers and
small state flips), `--dur-base` (~250ms, transitions where something moves),
and `--ease` (`cubic-bezier(0.4, 0, 0.2, 1)`).

**Reduced-motion is law.** A `@media (prefers-reduced-motion: reduce)` block in
`tokens.css` zeroes `--dur-fast` / `--dur-base`, so any animation that drives
its timing from the tokens is disabled automatically for users who ask for it —
no per-component opt-in. Animation MUST reference `var(--dur-*)` rather than a
hardcoded `0.25s` to inherit this; a hardcoded duration ignores the user's
setting. This is the mechanism behind "motion means motion" (see
[Visual principles](07-visual-principles.md)).

