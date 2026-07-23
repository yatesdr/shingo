# Shingo UI Style Guide

The canonical reference for the Shingo UI surfaces (Core admin, Edge admin,
Operator HMI). Captures the decisions reached during the UI consistency
refactor. Subsequent changes land via PR against this document.

History: the working draft for this guide lived as `style-guide-v0.md` in
`GitHub/shingo-ui-consistency/`. It moved here when all TBD entries were
closed.

## How to use this document

Read top-to-bottom once. After that, jump to the section that covers what
you're touching. The guide is opinionated — it picks defaults rather than
listing options. When you find yourself fighting one of these conventions
on a real task, open a PR against this document, not a workaround in the
code.

The guide covers UI consistency only. It does not cover backend code style
(see `shingo/AGENTS.md`), Go architecture (see `shingo-architecture/`), or
the operator HMI's domain-specific interaction patterns beyond what's
shared with the admin surfaces.

## The three surfaces

| Surface | Path | Audience | Loading model |
|---|---|---|---|
| **Core admin** | `shingo-core/www/` | Plant engineers, fleet operations | SSR + client-side enhancement, SSE for live updates |
| **Edge admin** | `shingo-edge/www/` | Plant engineers (per-cell), shift supervisors | SSR + HTMX partial swaps + per-page JS for complex forms |
| **Operator HMI** | `shingo-edge/www/static/operator-station/` | Line operators on a 10" touch panel | Empty-shell HTML + ES module JS render, SSE-driven |

These three surfaces have **intentionally different rendering models**. Do
not try to converge them. Do try to share primitives (tokens, utilities,
status vocabulary).

## Code organization

### Shared module structure

**Decided: third Go module + Go workspace.**

```
shingo/                          ← repo root
├── go.work                      ← workspace file, lists all three modules
├── shingo-core/
│   └── go.mod                   ← imports shingo/shared
├── shingo-edge/
│   └── go.mod                   ← imports shingo/shared
└── shared/                      ← new third Go module
    ├── go.mod
    ├── static.go                ← go:embed *.css *.js *.html
    ├── tokens.css               ← semantic design tokens
    ├── status-classes.css       ← per-status badge classes
    └── utils.js                 ← h, el, escapeHtml, api, modal, confirm, toast, SSE factory
```

The `go.work` file at the repo root declares all three modules as a
workspace. Local development picks up edits to `shared/` immediately; no
version bumps or `replace` directives needed during normal work. Plant
deploys (`git pull` + service restart + rebuild) work transparently — the
workspace file is detected automatically by every `go` command. The
self-contained Go binary embeds the shared static files at build time;
there's no runtime dependency on the `shared/` directory.

### Static file serving

Each consumer module imports `shingo/shared` and serves its files at a
predictable URL prefix (e.g. `/static/shared/utils.js`,
`/static/shared/tokens.css`). The Go side wires this up via:

```go
import "shingo/shared"

http.Handle("/static/shared/", http.StripPrefix("/static/shared/",
    http.FileServer(http.FS(shared.Files))))
```

Template references use the prefixed path:

```html
<link rel="stylesheet" href="/static/shared/tokens.css">
<script type="module" src="/static/shared/utils.js"></script>
```

### Adding to shared/

Promote a file to `shared/` only when **both** Core and Edge need it
identically. Don't preemptively populate. Concrete current candidates
(known to belong in shared based on the consistency refactor): tokens,
status-classes CSS, the JS utility module. Add others as a real shared
need surfaces.

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

## Status indicators

### Signal theme

Badge colors follow a scheme called **Signal**. **Hue** encodes *where* an order
is in its lifecycle; **weight** is held flat. Every non-alert badge sits at one
calm, low-saturation weight, so the two alert states — `faulted` (amber) and
`failed` (red) — are the only loud pills and clearly out-weigh everything else on
a crowded table. Grey is reserved for `cancelled` alone.

**The lifecycle:**

```
EARLY (3 graduated calm tints:           →  SUBMITTED (steel blue)
  pending slate · sourcing sand · queued periwinkle)
→  ACTIVE (per-phase hue, calm weight)   →  SUCCESS (green)
→  ATTENTION (amber, loud)  →  FAILURE (red, loud)  ·  cancelled (grey)
```

**Weight rule** (for anyone adding a status): non-alert light backgrounds stay
light (L≥86) and dark text stays bright (L≥68); only `faulted` and `failed` may
go below that. All text-on-pill pairs clear WCAG AA (≥4.5:1) in both themes.

**Per-phase hues in the active band:**

Each active phase has its own color so it's distinguishable at a glance:

| Phase | Hue | Why |
|---|---|---|
| dispatched | Blue | Robot assigned, mission queued — "assignment blue" |
| in_transit | Cyan | Robot physically moving — "movement cyan" |
| staged | Teal | Bin at destination, awaiting next step. **Was indigo** — moved to teal so Indigo could be reserved as the UI accent (P13); teal sits beside in-transit cyan and stays clear of the success green |
| reshuffling | Pink | Rearranging bins — active handling, **not** a fault. **Was violet** — moved to free the accent and to read as benign activity rather than an alarm (P13) |

**Light theme palette** (defined in `shared/status-classes.css`):

| Signal | Statuses | Background | Text |
|---|---|---|---|
| Early: pending | pending | `#e2e8f0` | `#475569` |
| Early: sourcing | sourcing | `#fef3e2` | `#92660c` |
| Early: queued | queued | `#dde6fb` | `#3457b0` |
| Submitted | submitted, acknowledged | `#dbeafe` | `#1e40af` |
| Active: dispatched | dispatched | `#cfe0fd` | `#1d4ed8` |
| Active: in_transit | in_transit | `#c5edf6` | `#155e75` |
| Active: staged (teal) | staged | `#c5eee3` | `#0c6b54` |
| Active: reshuffling (pink) | reshuffling | `#f8dcec` | `#8f2f64` |
| Success | delivered, confirmed | `#c6f6d5` | `#166534` |
| No-op | skipped | `#e0e7f0` | `#51607a` |
| Attention (loud) | faulted | `#fde68a` | `#92400e` |
| Failure (loud) | failed | `#fecaca` | `#991b1b` |
| Cancelled (the one grey) | cancelled | `#e5e7eb` | `#52525b` |

**Dark theme** uses deeper backgrounds and brighter text, tuned for
shop-floor LCDs under fluorescent lighting. See `shared/status-classes.css`
for exact values.

### One palette, three renderers (P13)

There is **one** status palette. It feeds the badges (above), the robot-map
status dots, and the floor-display board rows. Before P13 the map kept its own
`STATUS_COLOR` table that disagreed with the badges; now both read the same
`--status-<status>-dot` tokens from `shared/tokens.css`. The "dot" tokens are the
saturated hue; the badge bg/text pairs above are the calm-weight pills derived
from the same hue.

| Status | Dot token | Dot hue |
|---|---|---|
| pending | `--status-pending-dot` | `#8b95a5` slate |
| queued | `--status-queued-dot` | `#7aa2f0` periwinkle |
| dispatched | `--status-dispatched-dot` | `#4f9bff` blue |
| in_transit | `--status-in-transit-dot` | `#34c3e0` cyan |
| staged | `--status-staged-dot` | `#15b8a0` teal |
| reshuffling | `--status-reshuffling-dot` | `#df6fb4` pink |
| blocked | `--status-blocked-dot` | `#f85149` red (map/board only — not a protocol badge status) |
| delivered | `--status-delivered-dot` | `#3fb950` green |

Robot states are unchanged: ready green, charging amber (`#e3b341`), error red,
offline gray; a moving robot tracks the in-transit cyan.

### The Indigo accent (P13)

**Indigo `#7C7CF0` (dark) / `#4F4FD6` (light) is the reserved UI accent** —
`--accent`, and `--primary` aliases it. Use it for **interactive / UI chrome
only**: links, focus rings, selection, the primary action (`.btn-primary`),
active tabs, section ticks, and the map focus ring. In charts it also serves as
**series-1** of the curated data-viz palette (P19, see Data visualization) —
that's fine; the only hard rule is that indigo never becomes a *status* hue.

**Foreground vs filled (P15).** The accent has two values for two jobs.
`--accent` (light indigo) is the *foreground* — text, links, focus rings, active
states *on* a surface. Surfaces that put **white text on an accent background**
(filled buttons, solid badges) use **`--accent-solid`** (`#4F4FD6`,
theme-invariant — and `--accent-solid-hover` `#3E3EC0`) instead: the foreground
indigo in dark (`#7C7CF0`) under white text is only **3.50:1** (fails AA), while
`--accent-solid` is **6.14:1** in both themes. Rule of thumb: accent *on* a
surface → `--accent`; accent *as* the surface under light text → `--accent-solid`.

**Indigo is NEVER a status hue.** This is the rule that drove moving staged off
indigo and reshuffling off violet. One restrained accent *glow* is allowed on
genuinely live/active elements (the route comet, a live pill); everywhere else
the accent is a flat fill or stroke. `--info` (cyan) and the status blues
(`dispatched`) stay their own tokens — never fold a data/semantic color into the
accent. If a screen needs `--primary` to *mean* something (a status, a series),
give that spot its own token instead.

### Surface elevation + text (P13)

Cards read by **elevation** — each surface one shade lighter than what sits
behind it — so most hard borders can drop. Tokens (dark values shown; light
mode inverts to light-cards-on-grey):

| Token | Dark | Role |
|---|---|---|
| `--elev-canvas` | `#0B0F16` | the void behind everything |
| `--elev-base` | `#0D1117` | page background |
| `--elev-surface` | `#161B22` | cards / panels |
| `--elev-raised` | `#1F2733` | raised elements on a card |

Text: `--text` primary (`#E6EDF3` dark via `--text-strong` on boards) ·
`--text-muted` secondary (`#8B949E`) · `--text-tertiary` faint labels
(`#6E7681`). **Never pure white or black.** Chart series use the **curated
data-viz palette** (`--viz-*`) — one designed, vibrant set used generously (P19,
see Data visualization); hero numbers stay white.

**Rule: Core and Edge admin surfaces consume `shared/status-classes.css`
exclusively for order-lifecycle badges.** Core's local `style.css` must not
redefine `.badge-pending`, `.badge-delivered`, or any other protocol-status
class. The only badge classes that belong in Core's `style.css` are
Core-specific non-protocol badges (`.badge-available`, `.badge-claimed`,
`.badge-robot-*`, etc.).

### One pattern

One CSS class per protocol status. The class name matches the status string:

```html
<span class="badge badge-pending">Pending</span>
<span class="badge badge-delivered">Delivered</span>
<span class="badge badge-failed">Failed</span>
```

**Mission-surface aliases.** `missions.js`, `mission-detail.js`, and
`rds-explorer.js` render the human labels *Completed* / *Created*, but these are
**not** protocol statuses — so they must emit a real `badge-<status>` class, not
`badge-completed`/`badge-created` (which have no CSS rule and fall back to the
unstyled grey base). The mapping: *completed* → `badge-confirmed` (green),
*created* → `badge-pending` (slate). Keep the label in the text; use the
protocol class on the element.

### One source file

Status classes live in `shared/status-classes.css`, embedded by both Core and
Edge admin via Go's embed.FS. The Operator HMI may use a touch-sized variant
(`.badge.badge--touch`) but the class set is the same.

### Drift test

A Go test (extending the pattern in `shingo-edge/www/order_status_js_drift_test.go`)
asserts that every value in `protocol/Status` (and any other UI-rendered
enum) has a corresponding `.badge-<status>` definition in
`status-classes.css`. The test reads the CSS literally and compares.

Adding a status to `protocol/status.go` without adding the CSS class fails
the test in CI. This is the **only** mechanism that prevents drift; do not
rely on review discipline.

**Blind spot:** the drift test validates CSS-vs-protocol coverage but does
**not** scan `.js`/`.html` emit sites. A JS-invented class name like
`badge-completed` escapes CI and silently renders the grey fallback — this is
exactly what bit the mission surfaces (see *Mission-surface aliases* above).
Emit sites must use protocol-status class names.

### Fallback styling

Define a base `.badge` style that's readable even without a status modifier:

```css
.badge {
  display: inline-block;
  padding: 0.2em 0.6em;
  border-radius: var(--radius);
  font-size: 0.8rem;
  font-weight: 600;
  background: var(--surface);
  color: var(--text);
  border: 1px solid var(--border);
}
```

This ensures a transitional status (added to the protocol but not yet to CSS)
renders as a neutral pill rather than invisible text.

### Templates

In Go templates, always emit both the base and modifier class:

```go
{{/* GOOD */}}
<span class="badge badge-{{.Status}}">{{.Status}}</span>

{{/* BAD — drops the per-status color */}}
<span class="status-badge">{{.Status}}</span>
```

Edge admin's `orders-body.html` and similar partials need updating to match.

## Data visualization

Charts, KPI numbers, and other data marks use ONE **curated, vibrant palette**
(P19) — a single designed set, used generously and consistently. This supersedes
two earlier dead ends: the original ad-hoc "grab a semantic token per series"
**rainbow** (chaotic), and P18's **monochrome** white/gray rule (lifeless). The
fix for both is the same — one harmonious palette, applied with intent. Color is
welcome; it just all comes from this one set.

**The palette** (`--viz-*`, both themes — dark values shown; the light variants
are deepened to ~600-level so they stay saturated and legible on a light
surface):

| Token | Dark | Role |
|---|---|---|
| `--viz-indigo` | `#7C7CF0` | series-1 (= the UI accent) |
| `--viz-teal` | `#2DD4BF` | series-2 |
| `--viz-violet` | `#B07CF5` | series-3 |
| `--viz-amber` | `#FACC5B` | series-4 · **warning / ceiling / target** |
| `--viz-sky` | `#38BDF8` | series-5 |
| `--viz-coral` | `#FB7185` | series-6 · **failure / bad** |
| `--viz-green` | `#34D399` | **success / good / live** |

The rows are listed in **categorical scale order** (series-1 → series-6). Note
the token *values* are unchanged from earlier revisions — only the assignment
order moved (see P19 CVD fix under law 2).

**The law:**

1. **One palette, used generously.** Charts draw from the `--viz-*` set above —
   never raw semantic tokens grabbed ad hoc, and never monochrome.
2. **Two roles.** *Categorical* (tell series apart) — assign in scale order:
   **indigo → teal → violet → amber → sky → coral** (P19 CVD fix, see below).
   *Semantic* (the color means something) — success/good = green, failure/bad =
   coral, warning / ceiling / target = amber. When a series is inherently
   semantic, use the semantic hue over its categorical slot.

   **P19 categorical-order fix (CVD).** The original scale put indigo next to
   violet — a pair that collapses under protanopia (worst-pair ΔE only 2.4
   protan / 8.0 normal): two "different" series read as one to a red-weak
   viewer, and nearly one to everyone. Teal now separates them, so the *worst*
   adjacent pair in the whole ramp clears ΔE 17.0 under CVD simulation / 27.1
   normal. **Same seven hues, assignment order only** — no token value changed,
   so nothing that already references `--viz-teal`/`--viz-violet` by name moves.
   Only code that assigns *by series index* (series-2 was violet, is now teal)
   is affected; migrate those opportunistically.
3. **Soft area fills.** Primary line series get a translucent fill at **~13%** of
   the line color (`color-mix(in srgb, <viz-token> 13%, transparent)`). This soft
   wash carries much of the "premium" feel — use it on the lead series.
4. **Hero numbers stay white.** KPI heroes are `--viz-primary` (white /
   near-black, theme-aware) — the *charts* carry the color, not the big numbers.
   Delta arrows are green (up/good) / coral (down/bad).
5. **Chrome accents.** Section-title tick = indigo; live pill = green. Indigo
   remains the UI accent (P13) and doubles as series-1 — that overlap is fine.
   **Indigo never becomes a status hue.**
6. **Badges are a separate system.** Status **badges** keep the Signal
   categorical palette (see Status indicators) — that governs lifecycle pills,
   this governs data marks. Don't let one leak into the other.

**Tokens.** The `--viz-*` palette above plus `--viz-primary` (white / near-black,
theme-aware — hero numbers + chart text). Series colors reference these, never
inline hex; area fills use `color-mix(in srgb, <viz-token> 13%, transparent)`.

### Sequential and diverging ramps

The categorical palette tells *series* apart; two more ramps encode *magnitude*.
Together they complete the palette: **categorical + semantic + sequential +
diverging.**

- **Sequential** — `--viz-seq-1` … `--viz-seq-5`, a single-hue teal ramp for
  magnitude/density surfaces (heatmaps, the congestion layer). `seq-1` is the
  lowest value, `seq-5` the highest. Steps are ordered by luminance so the ramp
  still reads in greyscale; on dark surfaces the direction inverts (higher =
  brighter) via the dark-theme override.
- **Diverging** — `--viz-div-neg-2 / -1`, `--viz-div-mid`, `--viz-div-pos-1 / -2`,
  a teal ↔ coral ramp around a neutral gray for *signed* data (e.g. bin-sum
  drift): teal = positive/above, coral = negative/below, mid = zero.

Both ramps are chosen for monotonic luminance; validate any new step the same
way the categorical set was (perceptual spacing, CVD check). Reference tokens by
name — never inline the hex.

**Reference implementation: `/overview`.** Throughput bars = indigo; success-rate
line green + soft green fill (dashed bridge over thin buckets kept); duration P50
sky / P95 violet; cancellation amber, failure coral; fleet-load avg teal fill,
peak indigo line, ceiling amber-dashed; footprint two palette hues (teal +
indigo) with soft fills. KPI heroes white (delta arrows green up / coral down);
section titles get an indigo tick; the live pill is green.

## Visual principles

Three named principles generalize the look people like on the map — the
best-looking surface in the system — to every other surface. They sit above the
component sections and inform tokens, tables, tiles, meters, and animation
everywhere. Where a component rule and a principle disagree, the principle is
the intent; fix the component rule.

### Structure recedes, state glows

Static structure carries no saturated color. The floor plan, table chrome, node
geometry, card borders, grid lines — all neutral steel and muted tones.
Saturated color is **reserved for live state**: robots, order status, health,
the number that just changed. This is the generalization of the map's look — a
calm grey scaffold with a few vivid, meaningful marks on top.

Concretely:
- **Tables** — muted header/border chrome; color lives only in the status chips
  and health dots, never in the row fill (except a soft state tint like the
  >30 d staleness wash).
- **Tiles / node cells** — neutral base; the state color (has-payload, staged,
  maintenance) is the one saturated thing on the tile.
- **Meters** — the track is `--bg-dark`; only the fill and the threshold tick
  carry hue.

If a surface feels loud, the fix is almost always *desaturate the structure*,
not *tone down the state* — the state is the point.

### Motion means motion

Animation is reserved for **real physical movement or live data flow**. It is
never decoration and never plays on a stationary thing.

- A robot moving on the map → its comet flows. Stopped mid-route → the comet
  freezes to a faint static lane. Blocked/faulted → static red lane, no flow.
- A value updating live → a brief flash on the changed cell. A value sitting
  still → nothing.
- The one restrained accent *glow* allowed on genuinely live/active elements
  (route comet, live pill) folds under this rule — it is motion standing in for
  "this is alive right now." The older "one restrained accent glow" line in the
  Indigo-accent section is a special case of this principle, not a separate one.

Corollary: decorative hover-wobble, idle-pulsing buttons, spinners still
spinning after the data arrived — all forbidden. Every animation honors
`prefers-reduced-motion` (see Motion tokens under Design tokens).

### Focus dims siblings

The standard focus pattern across surfaces: **one element lit, its siblings
dimmed, the background unchanged.** Clicking a robot on the map focuses it and
fades the rest of the fleet; highlighting a payload on Inventory lights its
holding bins and dims the others; the material layer's `?highlight=` deep-link
reuses the same machinery. Dim the peers (lower opacity / desaturate) — do not
grey out the whole canvas. The context stays readable; the focus just wins.

## Icons

**No emoji, ever.** Emoji render inconsistently across platforms, can't take
`currentColor`, and drift from the monochrome look. Use a vendored icon or plain
text — never a pictographic unicode character.

### The sprite

A ~20-icon subset of **Lucide** (ISC license) is vendored as a single SVG symbol
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

Starter set: search, refresh, close, chevron-right/down, arrow-up-right,
sort-asc/desc, lock, box, map-pin, layers, zoom-in/out, crosshair,
alert-triangle, info, check, trash, pencil, download, battery. Add one by copying
the Lucide 24×24 geometry into a new `<symbol id="icon-…">` (geometry only — no
per-shape stroke/fill, so it inherits `.icon`).

### Drift test

`TestNoEmojiInTemplatesAndPageJS` (in both `shingo-core/www` and
`shingo-edge/www`) fails CI on any emoji in a template or page-JS file. The
shared detector `shared.IsEmoji` draws the line: the supplementary emoji planes
and any VS16-qualified symbol are rejected; the monochrome geometric glyphs the
surfaces use as affordances (arrows, chevrons, bullets, `✓`/`✗`, the bare `⚠`)
are allowed. First catch: a lock emoji in `bins.js`.

## Modals

### One mechanism

Pick **Core's `.modal-overlay` + `.active` class** pattern. CSS-driven,
theme-aware, no inline `style.display` toggling, no DOM race conditions.

```html
<div class="modal-overlay" id="my-modal">
  <div class="modal">
    <div class="modal-header">
      <h2>Modal Title</h2>
      <button class="modal-close" data-action="close-modal">&times;</button>
    </div>
    <div class="modal-body">
      <!-- content -->
    </div>
    <div class="modal-footer">
      <button class="btn" data-action="close-modal">Cancel</button>
      <button class="btn btn-primary" data-action="save">Save</button>
    </div>
  </div>
</div>
```

```js
import { showModal, hideModal } from '/static/shared/modal.js';
showModal('my-modal');
hideModal('my-modal');
```

### Lifecycle contract — decided defaults

| Behavior | Default | Opt-in override |
|---|---|---|
| Open | `showModal(id)` adds `.active` class | — |
| Close | `hideModal(id)` removes `.active` class | — |
| **Backdrop click** | **Does NOT close** (button-only dismissal — safer for data-input modals) | `showModal(id, { closeOnBackdrop: true })` for info/confirm modals |
| **Escape key** | Closes (same effect as clicking the X) | — |
| **Form state on close** | **Cleared** (no stale data on reopen) | `hideModal(id, { preserveState: true })` for wizards / edit-flows |

The pair of defaults (button-only-close + clear-on-close) work together:
closing a modal — by any deliberate means — discards state, and closing
requires a deliberate action. Accidental clicks on the backdrop don't
silently nuke the user's work.

**When to opt into `closeOnBackdrop: true`:** info modals, simple
confirmations, anything where state preservation isn't a concern and
quick dismissal is a UX win.

**When to opt into `preserveState: true`:** multi-step wizards, long
edit forms where accidental close-then-reopen shouldn't lose work.
Concrete examples: Core's bins cycle-count wizard, test-orders command
form.

Most modals — the claim editor, station editor, anything with serious
input — get the safe defaults automatically. The combo of "button-only
close + clear-on-close" means an accidental backdrop click does nothing,
and a deliberate close starts the next session fresh.

### Touch variant

Operator HMI uses the same mechanism with a `.modal--touch` modifier for
sizing:

```css
.modal--touch .modal {
  min-width: 480px;
  font-size: 16px;
}
.modal--touch button { min-height: var(--os-touch-min); }
```

### What not to do

- ❌ `style="display:none"` toggled by JS — fragile, no transitions
- ❌ HTML5 `hidden` attribute — inconsistent browser styling
- ❌ Per-page modal markup — use the shared structure
- ❌ Inline `onclick="closeXModal()"` — use `data-action="close-modal"`

## Dialog UX — confirmation, prompt, toast

### Never use native dialogs

`alert()`, `confirm()`, `prompt()` are forbidden. Use the shared helpers.

```js
import { confirm, toast, prompt } from '/static/shared/dialog.js';

// Confirmation — Promise-based, styled overlay
if (!await confirm('Delete this style?')) return;

// Toast — auto-dismissing notification
toast('Saved', 'success');
toast('Network error', 'error', { sticky: true });

// Prompt — styled input dialog, not native
const count = await prompt('Remaining parts?', { type: 'number', min: 0 });
if (count === null) return;
```

### Migration rule

When touching a file with `confirm()` / `alert()` / `prompt()`, migrate them
in the same PR. The migration is mechanical (`if (!confirm(...))` →
`if (!await confirm(...))`), but every call site needs to be in an `async`
context — verify the enclosing function is `async` or refactor.

### Toast levels

| Level | Use for | Default duration |
|---|---|---|
| `success` | Mutation succeeded | 3.2s |
| `error` | Mutation failed, network error | sticky if `{ sticky: true }`, else 5s |
| `warning` | Validation failure, soft block | 3.2s |
| `info` | Background event the user should know about | 3.2s |

Sticky errors are the default for async/SSE-delivered failures (operator
might have looked away).

## Buttons

### Class taxonomy

```css
.btn          /* base — neutral background, border */
.btn-primary  /* primary action */
.btn-danger   /* destructive */
.btn-sm       /* size variant */
.btn-icon     /* icon-only square button */
.btn-block    /* full-width */
```

That's the entire taxonomy. Resist adding `.btn-secondary`, `.btn-success`
etc. — if you need a green button, it's usually a primary action in a
different context, not a new variant.

### Touch sizing

Operator HMI buttons get `min-height: var(--os-touch-min)` via the
`.modal--touch` scope (or equivalent). Don't introduce a parallel
`.btn--touch` modifier; the scope handles it.

### What not to do

- ❌ Hardcoded `padding: 12px 24px` for touch buttons — use the scope
- ❌ `.os-action-btn`, `.os-header-btn` parallel taxonomies — fold into `.btn`
- ❌ Tab buttons styled as primary buttons (the `.process-tab.btn-primary`
  pattern on Edge) — tabs are not CTAs

## Forms

### Markup

Every form input is wrapped in a `.form-group` with an explicit `<label>`
and the input class:

```html
<div class="form-group">
  <label for="process-name">Name</label>
  <input type="text" id="process-name" class="form-input">
  <div class="form-error" data-error-for="process-name"></div>
</div>
```

The `.form-input` class is **mandatory** on inputs, selects, and textareas.
This is the Edge convention; Core inputs need the class added during
migration.

### Form-state convention

Non-trivial forms (modals with conditional fields, multi-step flows,
anything more complex than a save-three-fields modal) follow this pattern:

```js
// state lives in one place
let formState = {
  name: '',
  role: 'consume',
  swapMode: 'single_robot',
  // ...
};

// render(state) → builds/updates the form from state
function render(state) {
  document.getElementById('form-name').value = state.name;
  document.getElementById('form-role').value = state.role;
  // visibility derived from state, not toggled imperatively
  document.getElementById('staging-fieldset').classList.toggle(
    'is-hidden',
    !needsStaging(state.role, state.swapMode)
  );
}

// readFromForm() → snapshots current input values into state
function readFromForm() {
  return {
    name: document.getElementById('form-name').value.trim(),
    role: document.getElementById('form-role').value,
    swapMode: document.getElementById('form-swap-mode').value,
  };
}

// validate(state) → returns { ok, errors }
function validate(state) {
  const errors = [];
  if (!state.name) errors.push({ field: 'name', msg: 'Required' });
  if (state.swapMode === 'two_robot_press_index' && !state.pairedNode) {
    errors.push({ field: 'pairedNode', msg: 'Back Press Node required' });
  }
  return { ok: errors.length === 0, errors };
}

// save(state) → calls the API
async function save(state) {
  const v = validate(state);
  if (!v.ok) { showErrors(v.errors); return; }
  await api.post('/api/style-node-claims', state);
}
```

### Rules

1. **State lives in one object.** Not 30 `getElementById` calls scattered
   across 5 functions.
2. **Conditional visibility is computed from state.** Not toggled by
   imperative event handlers.
3. **`validate(state)` is a pure function** — same input, same output, no
   DOM reads. This lets it be unit-tested.
4. **Backend mirrors frontend validation.** Frontend validation is for UX
   (immediate feedback); backend is for correctness. They check the same
   rules.

### Anti-patterns to avoid

- ❌ Reading element values inside the save function (`document.getElementById('foo').value.trim()` in `save()`)
- ❌ Setting `element.style.display = 'none'` from event handlers
- ❌ Storing state in `data-*` attributes for retrieval later
- ❌ Multiple "reset", "populate", "save" functions that each touch the same 20 IDs

### Worked example

The canonical example is `shingo-edge/www/static/js/pages/processes.js`'s
claim editor. Concretely, the file demonstrates each convention piece:

- **One state object.** `claimState` is the only place form values live
  between user input and POST. `_payloadCatalog`, `_claimsStyleID`, and
  `_currentClaims` are module-scoped caches, not form state.
- **Pure `claimFieldVisibility(role, swap)`.** Returns a map of
  fieldset/group element ID → boolean. The lookup table is the source
  of truth for what shows when; the prior version's 31 scattered
  `style.display` assignments collapse to one function plus one table.
- **Pure `validateClaimState(state)`.** Returns `{ ok, errors }`. No
  DOM reads, no toasts. The caller (`saveClaim`) translates errors
  into UI feedback; validate doesn't know about UI.
- **`readClaimStateFromForm()` / `writeClaimStateToForm(state)`.**
  Single-direction snapshot functions. `readClaimStateFromForm` is
  pure DOM → state; `writeClaimStateToForm` is state → DOM.
- **`renderClaimForm()` as the single DOM mutation entry point.**
  Reads role/swap from inputs, applies the visibility map, sets
  disabled/labels for the special cases. Replaces the prior
  `toggleClaimsAddPayload + validateClaimStaging` pair (the old names
  survive as thin shims because inline `onchange` handlers in the
  template still reference them).
- **`saveClaim()` is the read→validate→POST pipeline.** Form-shape
  side effects (NGRP bulk-expansion, manual_swap's allowed-codes →
  payload_code coercion) are clearly named branches in saveClaim, not
  mixed into the payload assembly.

Characterization tests pin the (role × swap_mode) visibility matrix
and `saveClaim` payload shape at CI time. See
`shingo-edge/www/static/js/pages/processes.characterization.test.js`
(202 assertions across 10 cells + three payload-shape cases). The
test harness loads `processes.js` in a Node `vm.runInContext` with a
hand-rolled DOM stub — no jsdom dependency, no npm install.

The conventions above are the parts to copy when a new form needs
this treatment. Two-field "save three values" modals don't need the
full machinery — apply the convention when conditional visibility or
multi-step validation enter the picture.

## JavaScript primitives

### Use these helpers

Don't reimplement. The shared module at `shared/utils.js` exports:

```js
import {
  // HTML construction
  escapeHtml,    // last-resort string escape
  h,             // tagged template — auto-escapes interpolations
  el,            // DOM builder — el(tag, props, children)

  // HTTP
  api,           // api.get(url), api.post(url, body), .put, .delete

  // Time
  timeAgo,       // relative ("3m ago")
  formatTime,    // local-time string
  formatDuration,
  convertTimestamps, // for <time data-utc="..."> elements

  // SSE
  createSSE,     // EventSource with backoff + build-id reload

  // Modals & dialogs
  showModal, hideModal, confirm, toast, prompt,

  // Misc
  debounce,
} from '/static/shared/utils.js';
```

### Module shape

**Decided: ES modules.** All shared utilities and consuming JS use
`import`/`export`. Script tags get `type="module"`. This matches the
operator station's existing pattern. Core's bare globals and Edge's IIFE
wrap get migrated to modules as part of the refactor.

Rationale: operator station already uses modules successfully; the
three-pattern divergence collapses to one; modern tooling (linters,
formatters, possible future TypeScript, bundlers) assumes modules; AI
agents parse explicit `import` statements more reliably than implicit
`window.X` globals. The cost is a one-time pain (script tag changes,
loading semantics shift to deferred-by-default) instead of perpetual
maintenance of the divergence.

Browser support: ES modules require Chromium 60+ / Firefox 60+ / Safari
11+ (2017-2018 vintage). Modern plant kiosks should be fine; verify the
oldest device in the field before shipping.

### HTML construction

Always prefer `h\`\`` over string concatenation:

```js
// GOOD
container.innerHTML = h`<div class="row">${name}</div>`;

// BAD — manual escaping, easy to miss one
container.innerHTML = '<div class="row">' + escapeHtml(name) + '</div>';
```

`h\`\`` auto-escapes interpolations, joins arrays without escape, supports an
opt-out for pre-built safe HTML (`{ __html: true, value: safe }`).

### Avoid

- ❌ Raw `innerHTML += '...'` with concatenated user data
- ❌ Bare `fetch()` — use `api.*` for consistent error handling
- ❌ Bare `EventSource` — use `createSSE` for reconnect + build-id detection

## Templates and composition

The three surfaces use three different Go template composition models, and
that's fine. Don't migrate.

| Surface | Pattern | Used because |
|---|---|---|
| Core | `{{define "layout"}}` + `{{block "content" .}}` (inheritance) | SSR + client enhancement, single layout |
| Edge admin | `{{template "header" .}}` + `{{template "footer" .}}` (sandwich) | HTMX partial swaps need standalone named templates |
| Operator HMI | Empty-shell HTML + JS render (no Go templates beyond the shell) | Fully client-rendered from JSON, single persistent connection |

### Shared partials

A small `templates/shared/` directory contains primitives that any surface
can `{{template}}` in:

- `status-badge.html` — `{{template "status-badge" .Status}}`
- `fieldset-card.html` — wrap form sections
- `form-field.html` — label + input + error layout

Add to this directory **only when a concrete need surfaces** — a partial that
two or more surfaces need to render identically. Don't speculatively populate.

### Inline scripts

**Don't.** Edge templates currently have significant inline `<script>` blocks
(notably `material.html:39-251`). New code does not add inline scripts;
existing inline scripts are extracted to `static/js/pages/<page>.js` when
the file is touched.

The one allowed inline `<script>` pattern is data-handoff from server to
client, and it should use JSON-in-attribute, not `window.foo = ...`:

```html
<!-- GOOD -->
<div id="page-data" data-claims='{{.ClaimsJSON}}'></div>

<!-- BAD — quote-fragile, no type safety -->
<script>window.claimedByStation = {{json .ClaimedByStation}};</script>
```

The Go handler emits `ClaimsJSON` via `json.Marshal`; the page JS reads
`JSON.parse(document.getElementById('page-data').dataset.claims)`.

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

## Event handling

### Delegation over inline onclick

```html
<!-- GOOD -->
<button class="btn" data-action="delete-style" data-style-id="42">Delete</button>

<script>
  document.addEventListener('click', (e) => {
    const btn = e.target.closest('[data-action]');
    if (!btn) return;
    if (btn.dataset.action === 'delete-style') {
      deleteStyle(parseInt(btn.dataset.styleId, 10));
    }
  });
</script>
```

```html
<!-- BAD — forces deleteStyle to be a window-global -->
<button onclick="deleteStyle(42)">Delete</button>
```

Inline `onclick=` is forbidden for new code. Reasons:

1. Handler functions must be `window`-global to be reachable, blocking ES
   module adoption.
2. The handler isn't visible at the JS module level (grep finds the HTML
   call, not the binding).
3. CSP-friendly code disallows inline event handlers.

Existing inline `onclick` handlers in `processes.html` and elsewhere are
migrated as part of the rewrite.

### Async handlers

Event handlers that await something must be `async`:

```js
list.addEventListener('click', async (e) => {
  const btn = e.target.closest('[data-action="delete"]');
  if (!btn) return;
  if (!await confirm('Sure?')) return;
  await api.delete('/api/items/' + btn.dataset.id);
});
```

## Tabs

One implementation. CSS:

```css
.tabs            { display: flex; gap: 0.25rem; border-bottom: 1px solid var(--border); }
.tab             { padding: 0.5rem 1rem; cursor: pointer; border: none; background: none; color: var(--text-muted); }
.tab:hover       { color: var(--text); }
.tab.active      { color: var(--primary); border-bottom: 2px solid var(--primary); margin-bottom: -1px; }
.tab-panel       { display: none; }
.tab-panel.active { display: block; }
```

Markup:

```html
<div class="tabs">
  <button class="tab active" data-tab="general">General</button>
  <button class="tab" data-tab="claims">Node Claims</button>
  <button class="tab" data-tab="stations">Operator Screens</button>
</div>
<div class="tab-panel active" id="tab-general">...</div>
<div class="tab-panel" id="tab-claims">...</div>
<div class="tab-panel" id="tab-stations">...</div>
```

JS handler is shared. No more `.tab-bar` / `.diag-tabs` / `.spot-tabs` /
`.to-tabs` / `.process-tab`.

### Tabs are not CTAs

Don't style tabs as `.btn-primary` with `.active`. Tabs are navigation;
primary buttons are actions. The `.tabs` styling above keeps them
visually distinct.

## Domain glossary

Names matter — drift in component naming follows drift in domain naming.
This glossary is the source of truth; use these names in code, templates,
and UI labels.

Each entry was verified against the code (citations below). Where the codebase
uses inconsistent names for the same concept today, the entry says which name
wins and the inconsistency is flagged in **Cross-surface terminology to
reconcile** at the end of this section.

### Production hierarchy

| Term | Definition | Code reference |
|---|---|---|
| **Process** | A production sequence configured for a cell (e.g. "Front Rail"). Has one ActiveStyleID and many Styles. Owns the production counter config. **One Process is active per cell at a time** (Process has `ActiveStyleID`; cell switches active style via changeover) | `shingo-edge/domain/process.go:32` `type Process struct` |
| **Style** | A variant produced under a Process (e.g. "Style A", "Style B"). Belongs to one Process via `ProcessID`. The active Style drives which NodeClaims are in effect. Also written **"Job Style"** in UI labels and changeover docs — both names are acceptable; `Style` is the code identifier, "Job Style" is fine in operator-facing text | `shingo-edge/domain/process.go:41` `type Style struct` |
| **NodeClaim** | A per-Style binding to a Core Node — declares the payload, capacity, reorder behaviour, swap mode, staging. The active Style's NodeClaims drive material orders. **One Claim type exists** (the verb "claims" is used in unrelated relationships — see Claim disambiguation below) | `shingo-edge/domain/process.go:116` `type NodeClaim struct` |
| **Claim Role** | What a node does for a payload under a NodeClaim. Two live values: `consume` (node consumes upstream material), `produce` (node produces material for downstream). **Deprecated:** `changeover` — present in `protocol/types.go:235` and referenced in `engine/changeover.go`, `operator_node_changeover.go`, and `processes.js`, but does **not** reflect how changeovers actually work. Actual changeover mechanic: operator selects a new Style → active NodeClaims change → each claim's `swap_mode` drives add/drop commands per node. No separate "changeover role" needed. Slated for removal — see deprecations tracker | `protocol/types.go:230-235` |
| **Swap Mode** | How a node's bin gets replaced. Active values: `sequential`, `single_robot`, `two_robot`, `two_robot_press_index`, `manual_swap`. **Deprecated:** `simple` (hidden in UI, legacy data still has it — see deprecations tracker) | `protocol/swap_mode.go:17-22` |

### Node concepts

| Term | Definition | Code reference |
|---|---|---|
| **Core Node** | A physical, robot-addressable location in the cell (lane, slot, station). Owned by Core, identified by a stable name string (e.g. `LANE_03_SLOT_2`). Exists whether any Edge process uses it or not. Edge receives the list via sync from Core | Referenced everywhere as `CoreNodeName string` |
| **Process Node** | An Edge-side record that says "Process X uses Core Node Y in role Z." Has its own ID, references a Core Node by name (`CoreNodeName`), carries process-scoped config (owning operator station, sequence, display name) plus a separate `RuntimeState` row (active bin, remaining UOP, active orders). Many Process Nodes can reference one Core Node (different processes sharing the same physical slot) | `shingo-edge/domain/process.go:53` `type Node struct` (the comment on line 49 explicitly says "process node") |

Person/Employee analogy: Core Node is the human (one per body), Process Node is the employment record (many possible per person, each carrying per-employer context).

### Edge installation vs HMI

This is the worst overloading in the codebase today — the word "Station" means two different things at two different scales. The reconciliation table at the end of this section lists the rename targets.

| Term | Definition | Code reference |
|---|---|---|
| **Edge Cell** | One Edge installation — a physical production cell with its own Edge instance, controllers, HMIs, and Core sync. Identified by `StationID` in `Config.Messaging`. Core's `NodeType` code `EDGE` and `Order.StationID` refer to this concept. **The term "Edge Cell" is the proposed unified name** — code currently uses "Station" (Edge config), "edge-station" (Core docstrings), and `StationID` (both) | `shingo-edge/config/...` `StationID`; `shingo-core/domain/order.go:23` `StationID`; `shingo-core/domain/node_type.go:6` "EDGE (edge station)" |
| **Operator Station** | A specific HMI screen inside an Edge Cell. Configured to claim a subset of the cell's Process Nodes; renders an operator-facing UI for those nodes. Multiple Operator Stations exist per cell. **The term "Operator Station" wins** — code currently mixes this with "Station" (the domain type) and "Operator Screen" (the processes-tab UI label) | `shingo-edge/domain/station.go:8` `type Station struct`; API at `/api/operator-stations`; URL `/operator/station/{id}` |

### Orders

| Term | Definition | Code reference |
|---|---|---|
| **Order** | A material-movement request between nodes. The canonical noun across the system. Edge produces orders driven by demand wiring; Core receives and dispatches them. Edge URL is `/orders` (renamed from `/kanbans`; a 301 redirect preserves old bookmarks). No `Kanban` data type exists. | `shingo-edge/domain/order.go`; Edge handler `handleOrders` calls `OrderService().ListActiveByProcess()` |
| **Manual Order** | An admin-created one-off order. On Edge, submitted via the `/manual-order` page (types: `move`, `retrieve`, `store`, `complex`). On Core, submitted via Core's `/orders` admin modal (subtypes: `transport`, `staged`, `swap`, `send_to_location`), historically called "Spot Order" — Core rename to "Manual Order" is outstanding. Flows to Core via the protocol like any other Edge-originated order | Edge: `shingo-edge/www/handlers_manual_order.go`; Core: `shingo-core/www/handlers_orders.go:203` (`apiSpotOrderSubmit` — rename pending) |
| **Test Order** | A developer/QA tool on Core's `/test-orders` page for exercising order paths during development. Not an operator-facing concept | `shingo-core/www/handlers_test_orders.go`; don't use this term in operator UI |

**Spot Order vs Manual Order are not the same thing** even though they cover the same admin-need category. Different surfaces, different forms, different type vocabularies. See reconciliation table below for the proposed unified term.

### Claim disambiguation (one noun, two unrelated verb uses)

The word "claim" appears in three places. Only one of them is a data type:

1. **NodeClaim** (data type) — per-Style binding to a Core Node. The configured "Style X wants payload Y at node Z."
2. **Operator Station claims nodes** (verb / many-to-many relationship) — operator-station → claimed-nodes (`apiSetStationClaimedNodes`). Says "this HMI is responsible for these physical nodes." No `Claim` table; just an assignment.
3. **Robot claims bin** (runtime fleet concept) — a robot taking ownership of a bin for transport. Not in the process domain at all.

When writing about "claims," qualify which one. "NodeClaim" for the noun; "station node assignment" or "robot-bin ownership" for the verbs.

### Cross-surface terminology to reconcile

These are same-concept-different-name drifts where the system should pick one
name and migrate. Listed in rough order of impact × ease.

| Concept | Names today | Proposed unified name | Rename mechanics |
|---|---|---|---|
| **Edge installation / cell** | "Station" (Edge config UI, `StationID`), "edge-station" (Core docstrings), `EDGE` (Core NodeType code) | **"Edge Cell"** in UI labels and new docs. `StationID` field name stays in code (too disruptive to rename a serialized field across the protocol), but its meaning is "Edge Cell ID" | Update Edge config UI labels: "Station ID" → "Edge Cell ID". Update Core docstrings. Don't rename `StationID` in JSON/structs |
| **HMI screen inside a cell** | "Station" (domain type `shingo-edge/domain/station.go`), "OperatorStation" (API endpoint, JSON field), "Operator Screen" (processes-tab UI label) | **"Operator Station"** in code (matches existing API). In UI labels, **"Operator Station"** too — drop the "Operator Screen" label in `processes.html:46, 154` etc. | Rename UI strings only; data types and APIs already match. Single-PR Edge change |
| **Order list page on Edge** | **Done.** URL `/orders`, page identifier `"orders"`, handler `handleOrders`. A 301 redirect from `/kanbans` preserves old bookmarks. HTMX targets use `/orders/partial`. | — (completed) | — |
| **Admin-created one-off order** | Edge: "Manual Order" (types move/retrieve/store/complex). Core: still "Spot Order" (subtypes transport/staged/swap/send_to_location) — rename to "Manual Order" is outstanding. | **"Manual Order"** — clearer than "Spot," and Edge's term is the broader one. Core's `/orders` admin modal should be renamed to "Manual Order." Subtype vocabularies stay distinct because they represent genuinely different operations | Rename Core's `apiSpotOrderSubmit` to `apiManualOrderSubmit`. Rename `.spot-tabs` CSS to `.manual-order-tabs`. Update Core nav label "Spot Order" → "Manual Order" |
| **What Core calls "edge-station" in NodeType** | `EDGE` NodeType code described as "edge station" | Keep the code as `EDGE` (short codes are intentional). Rename the human description to "edge cell" | One docstring change on `shingo-core/domain/node_type.go:6` |

Reconciliation is opportunistic adjacency — bundle these into the consistency refactor PRs as files get touched. They're not blockers; they're cleanup.

### Units and casing

| Term | Definition | Casing rule |
|---|---|---|
| **UoP** | Units of Production — the count of finished parts a bin/payload carries, or that a cell has consumed. The atomic quantity the threshold monitor sums and reorder thresholds fire on. | Always **"UoP"** in UI text — labels, headers, table columns, prose, toasts, tooltips. Never "UOP" or "uop". The all-caps form had drifted across pages (34× "UOP" vs 7× "UoP"); standardized 2026-07. **Display text only:** code identifiers, JSON keys, struct fields, and `data-*` attributes keep their existing casing (`UOPRemaining`, `uop_remaining`, `data-uop`) — renaming a serialized field is out of scope and would break the protocol. |

### Working principle

The glossary is the source of truth. Where the UI uses an inconsistent name
today, the inconsistency is a defect to fix during migration. **When the
system as a whole means one thing, both surfaces should call it the same
thing** — there's no legitimate reason for Core to say "spot order" while Edge
says "manual order," or for "Station" to mean two different things at two different
scales. Each row in the reconciliation table is a small consistency win
available to anyone touching the relevant file.

## Drift detection

The codebase already has one drift test:
`shingo-edge/www/order_status_js_drift_test.go` pins the JS status arrays
in `operator-station/order-status.js` to the Go projectors in
`protocol/status.go`.

Extend this pattern to:

1. **CSS class coverage** — every `protocol.Status` value has a
   `.badge-<status>` class in `shared/status-classes.css`.
2. **Swap mode enum** — JS dropdown options match `protocol.SwapMode` values.
3. **Claim role enum** — same.
4. **Token name presence** — if a CSS file references `var(--foo)`, `--foo`
   exists in `tokens.css`. Extended to **templates** (shipped):
   `TestNoUndefinedCSSVarsInTemplates` in `shingo-core/www` fails when a
   template's `var(--foo)` resolves to no `--foo` in the shared/page CSS (the
   `--card-bg`-referenced-but-undefined class of bug), allowing inline
   template-local custom properties.
5. **No emoji** (shipped) — `TestNoEmojiInTemplatesAndPageJS` in both
   `www` packages fails on any emoji in a template or page-JS file, via
   `shared.IsEmoji`. See the Icons section.

Each test is ~30-50 LOC of Go reading source files literally with a regex.
Don't introduce a code generator; the test pattern is sufficient for the
current scale.

## Deprecations tracker

Scheduled removals live in `docs/ui-deprecations.md`:

```markdown
## Pending removal

### `swap_mode = "simple"` — RETIRED as a configurable mode (descriptor only)
- **Hidden in UI:** 2026-04
- **Retired as configurable:** 2026-07 (ingress lockdown)
- **Status:** "simple" is no longer a configurable claim mode. `UpsertClaim`
  and `plantspec.Validate` reject it, and the store no longer normalizes a
  blank swap_mode to it (blank now fails loud). It survives ONLY as the runtime
  `protocol.SwapModeSimple` CycleMode descriptor — the node-empty downgrade tag
  and the bare-move result tag (see consume_plan.go / operator_stations.go). A
  hidden `<option value="simple">` remains in the dropdown solely so an existing
  legacy row still renders when opened in edit mode. The allowlist, the
  dropdown, and its drift test all key on `protocol.ConfigurableSwapModes()`.

### `claim.keep_staged` column
- **UI removed:** 2026-03
- **Schema:** kept as backend safety net
- **Target removal:** when supermarket rewire ships
- **Blocking:** supermarket rewire project

### `ClaimRole = "changeover"` — REMOVED (UI consistency refactor)
- **Status:** removed. Surviving evacuate-during-changeover mechanic is
  driven by `swap_mode` + `EvacuateOnChangeover` on the active claim.
- **DB verification:** 2026-05-24, plant ITPI returned 0 rows
- **Removal commit:** UI consistency refactor (squashed)
- **Notes:** if non-ITPI plants discover non-zero rows post-deploy, run
  a DELETE migration. The engine no longer has a branch for this role,
  so legacy rows would fail validation on the next claim load.
```

Add an entry every time something is "hidden" or "kept for compatibility."
Without this list, the next pass through the code can't tell what's
load-bearing vs. what's residue.

## TBD log (closed)

Every TBD entry from the working draft has been resolved. The
decisions are referenced in the relevant sections above; the summary
below exists as a paper trail for anyone reading the doc and wondering
what was contested at the start.

- **ES modules in shared/, shared/ placement, modal backdrop default,
  form-state convention, per-page imports + delegateActions** —
  see Code organization, Module shape, Modals, Forms, and Event
  handling sections.
- **shingoedge.js / app.js interior cleanup** — both files are now
  flat top-level `export function` / `export const` declarations.
  `window.ShingoEdge` is retained at the bottom of `shingoedge.js`
  only for the two remaining non-module consumers (`traffic.html`
  inline `<script>` and `operator-station/operator.js`); when those
  migrate to module imports the bridge can go.
- **HTMX swap targets re-running `convertTimestamps`** — resolved as
  automatic. `shared/utils.js` exports
  `installHtmxTimestampConversion()`, which wires a single
  `document.body` listener for `htmx:afterSwap` that calls
  `convertTimestamps(event.detail.target)` against the swapped-in
  subtree. Edge's `shingoedge.js` calls it once at module load
  alongside `installBackdropClose()`. Templates emit
  `<time data-utc=…>` and the conversion happens automatically — no
  per-page wiring, no opt-in flag. Core admin doesn't use HTMX so the
  listener never fires there; the API is available if a future
  surface adopts HTMX.
- **Operator HMI `.os-modal*` rename** — the operator surface now
  uses `.modal-overlay.modal--touch` for the backdrop and
  `.modal--touch .modal-*` for the inner pieces, per the Modal
  section's canonical naming.

## Event handling — delegated actions

**Decided: no inline event handlers in templates. Every
DOM event is mediated through `data-action[-event]` attributes and a
per-page `delegateActions` call.**

```html
<!-- GOOD: click handler -->
<button class="btn" data-action="deleteOrder:42">Delete</button>

<!-- GOOD: select with change handler -->
<select data-action-change="navigateToProcess">…</select>

<!-- GOOD: form submit handler -->
<form data-action-submit="submitPLCreate" method="POST" action="/payloads/create">…</form>

<!-- GOOD: data-* attributes for JSON or multi-field payloads -->
<button class="btn" data-action="editStyle" data-style="{{json .}}">Edit</button>

<!-- GOOD: backdrop close opt-in on the overlay element -->
<div class="modal-overlay" id="order-detail-modal" data-backdrop-close>...</div>

<!-- BAD -->
<button onclick="deleteOrder(42)">Delete</button>
<select onchange="navigateToProcess()">…</select>
```

### Attribute → event mapping

| Attribute | DOM event | Notes |
|---|---|---|
| `data-action` | `click` | Default; what you'll use 90% of the time |
| `data-action-change` | `change` | Selects, checkboxes, file inputs |
| `data-action-input` | `input` | Live-update on every keystroke |
| `data-action-blur` | `focusout` (bubbling form of blur) | Cell-commit on losing focus |
| `data-action-keydown` | `keydown` | Per-key handling (Enter/Escape commit/cancel) |
| `data-action-submit` | `submit` | Form-level — handler can call `evt.preventDefault()` |

Add a new event type by extending the `eventRe` in
`shingo-edge/www/inline_onclick_drift_test.go` and adding the
`data-action-<event>` mapping to `delegateActions` in
`shared/utils.js`.

### Convention

- `data-action="verb"` → handler called as `verb(el, evt)`
- `data-action="verb:arg"` → handler called as `verb("arg", el, evt)`
- `data-action="verb:a:b"` → handler called as `verb("a", "b", el, evt)`
- The dispatcher binds `this` to the matched element so the old
  `onclick="foo(this)"` semantics survive unchanged.
- JSON-shaped or multi-key payloads go in `data-*` attributes that
  the handler reads off `this.dataset`. The element is also the
  first positional argument.

### Built-in verbs and attribute conventions

- `stopPropagation` — calls `event.stopPropagation()` and returns.
  Lets a child cell with its own data-action exist inside a row
  handler without firing the row handler.
- `data-backdrop-close` on a `.modal-overlay` removes `.active`
  when the click target IS the overlay (not an inner element).
  Wired by `installBackdropClose()` from `shared/utils.js`,
  called once per surface at module load.
- `data-skip-on-checkbox="1"` on a row handler skips the dispatch
  when the click originated inside a checkbox cell — lets row-click
  and per-row checkbox actions coexist cleanly.
- `data-prevent-default="1"` calls `event.preventDefault()` before
  dispatch. Used for `<a href="#">` navigation that shouldn't
  navigate, and form submits handled via fetch().

### Drift test

`TestNoInlineEventHandlersInTemplates` in both `shingo-edge/www/`
and `shingo-core/www/` walks every embedded template file and fails
CI on any line containing `on<event>=` for click / change / input /
blur / keydown / submit / focus / keyup / mousedown / mouseup. The
allowlist is empty; future justified exceptions land there with a
comment.

### Per-page handler registration

Every page script ends with an explicit `delegateActions` call
listing the handler functions used by that page. The `events: [...]`
option binds the same map across multiple event types in one call.

```js
import { api, toast, delegateActions } from '/static/js/shingoedge.js';

async function deleteOrder(orderID) { … }
async function navigateToProcess(el) { window.location = '?process=' + el.value; }
function renderClaimForm() { … }

delegateActions(document.body, {
    deleteOrder,
    navigateToProcess,
    renderClaimForm,
    // …every handler the template's data-action[-event] attrs reference
}, { events: ['click', 'change', 'input', 'blur', 'keydown', 'submit'] });
```

Page scripts that need a different handler set for an HTMX-swapped
sub-container can call `delegateActions(swapTarget, {…})` with a
scoped root. The dataset sentinel prevents double-binding when the
swap target survives a re-fill.

## How this document evolves

- Changes go via PR against `shingo/docs/ui-style-guide.md`. The
  "deprecations tracker" section is the feedback loop — anything you "had
  to" do that contradicts the guide is either a deprecation candidate
  (update the guide) or a missed convention (open an issue).

This document is opinionated on purpose. When you find yourself fighting it,
update it — don't work around it.

## Reference: the synthesis docs

The reasoning behind every decision in this guide lives in the
`GitHub/shingo-ui-consistency/` folder:

- `round-1-synthesis.md` — what's broken across the surfaces
- `round-2-synthesis.md` — argued positions on the open questions
- `round-3-synthesis.md` — convergence under the "we're doing it now" framing,
  plus the execution sequencing
- `round-4-synthesis.md` — ES-modules-everywhere argument
- `observations.md` — per-round DECISION / FLAG / refactor-candidate log

Read those if a convention here looks arbitrary or you want the trade-offs
that were considered.
