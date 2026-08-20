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

## Code organization

### Shared module structure

**Decided: its own Go module + Go workspace.**

`shared/` was the third module when this section was written and the UI assets
below were all of it. The workspace now lists five (`protocol`, `shared`,
`shingo-core`, `shingo-edge`, `integration`), and `shared/` has since taken on
cross-surface answers and cross-module test fixtures alongside the assets. It is
still not the home for shared infrastructure — that is `protocol/`. See
[`shared-layer-promotion.md`](shared-layer-promotion.md).

```
shingo/                          ← repo root
├── go.work                      ← workspace file, lists all five modules
├── protocol/
│   └── go.mod                   ← wire protocol + shared infrastructure
├── shingo-core/
│   └── go.mod                   ← imports shingo/shared
├── shingo-edge/
│   └── go.mod                   ← imports shingo/shared
└── shared/
    ├── go.mod
    ├── static.go                ← go:embed *.css *.js *.html
    ├── tokens.css               ← semantic design tokens
    ├── status-classes.css       ← per-status badge classes
    ├── utils.js                 ← h, el, escapeHtml, api, modal, confirm, toast, SSE factory
    ├── windoworder/             ← a cross-surface answer, not an asset
    └── loadervectors/           ← cross-module fixtures pinning Core against Edge
```

The `go.work` file at the repo root declares all of them as a
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
identically, and only when a disagreement between them would actually be a
defect. Don't preemptively populate. The full criterion — all four clauses, and
the rule that the drift guard ships in the promoting commit — is in
[`docs/shared-layer-promotion.md`](shared-layer-promotion.md); read it before
promoting anything.

For UI specifically the candidates are tokens, status-classes CSS and the JS
utility module. Note that `shared/` is no longer UI-only: it also holds
cross-surface answers (`windoworder`) and cross-module fixtures
(`loadervectors`). Shared *infrastructure* does not go here — that is
`protocol/`.

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
calm, low-saturation weight, so the two alert states — `faulted` (orange) and
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

**Step rule — two pills one step apart in the story must be more than one step
apart in colour.** `pending` and `skipped` shipped 0.58 CIEDE2000 apart in
light for *normal* vision — a tenth of the 5.0 distinction floor, i.e. the same
colour with two labels — while meaning opposite ends of an order's life
("nothing has started" against "this was never needed"). Neither rule above
catches that: both pills were individually legal. Fixed by re-stepping the pair
on the slate ramp rather than re-hueing either one, which separates them on
**lightness** — the one axis all three dichromacies preserve, and the reason
the four measured values (7.43 / 7.46 / 7.85 / 7.39) are almost the same
number. A hue separation never looks like that. `shared/signal_cvd_test.go`
measures every adjacent pair and every same-family pair, and pins each one that
falls short with the value it actually measures.

### The chip floors

Two questions were asked of every pill. Only the first turned out to be a floor
for a *health chip*; the second belongs to badges and marks. Both are still
listed because knowing why the second was dropped is what stops it coming back.

1. **Text on pill ≥ 4.5:1.** Can you read the label. Enforced for badges by
   `TestSignalBadgeTextClearsAA` and for chips by `TestChipContrast`. A hard
   gate for both.
2. **Pill against surface ≥ 3:1.** Can you see there *is* a pill. WCAG 2.2
   SC 1.4.11 — asserted for **opaque** pills only.

**The structural diagnosis was right and the prescription was wrong.** A chip's
fill was `color-mix` of *its own label colour*, so the two floors pulled against
each other: lower the mix percentage and the text gets more readable while the
pill gets more invisible; raise it and the reverse. No percentage satisfied
both. A badge escapes this because its foreground and background are chosen
independently. Fifteen of twenty-eight (theme × chip × surface) combinations
were below AA on text, worst `.chip-ok` at 2.89:1.

The recorded fix — "an ink colour per chip, seven new values" — was measured in
4.4 and corrected in three ways:

- **Ink cannot move the second floor at all.** That ratio is fill-vs-surface;
  no text term appears in it. Per-chip ink closes the 15 text failures and
  leaves all 24 boundary failures untouched. The two floors were never one fix.
- **It was four values, not seven, and one theme.** `.chip-drift` had no use
  site and was deleted rather than derived; `.chip-near` and `.chip-warn` are
  amber two points apart and share an ink; **every dark combination already
  cleared the floor**. What remained was four light hues plus one dark pin.
- **The second floor does not reach a labelled translucent chip.** SC 1.4.11
  covers UI components and graphics "required to understand the content". A
  health chip is neither: it is not interactive, and its meaning is the word
  printed inside it. The pill is redundant encoding around a text label. The
  floor was borrowed from the `--viz-*` MARK tokens, where it *does* apply
  because a chart mark is the information and has no text alternative.

  Satisfying it would also have cost the vocabulary its reason to exist: a
  15%-wash fill only reaches 3:1 by ceasing to be a wash, which measures at
  69–89% opacity — i.e. by becoming a Signal badge, the loud vocabulary these
  chips were built quiet against. `TestChipBoundaryNeedsNearOpacity` pins that
  measurement so the ruling can be checked rather than believed, and fails if a
  chip ever reaches the floor cheaply enough for the ruling to be revisited.

**Precondition on the ruling: every chip prints a label.** An icon-only chip
puts the meaning back in the shape and the boundary floor applies to it again.

Ink lives in `--chip-ink-*` (tokens.css), never in a `--viz-*` or `--sub-*`
token: those are MARK and STRUCTURE colours held to 3:1, and using one as type
is the same category error in either direction. `TestChipInkIsNotItsFill`
asserts the separation directly, so a re-collapse fails even where the
resulting ratio happens to survive.

**Both ratchets are gone.** They were the right instrument while the family was
structurally unable to pass; once it can, a ratchet parked under reality is
weaker than the floor it stands in for. The text floor is now a hard 4.5:1 and
the opaque-boundary floor a hard 3:1, both green.

**Hue rule — warm is for alerts.** `faulted` moved off amber to orange because
amber put it in the same hue family as `sourcing`'s sand: pill *weight* was the
only thing distinguishing "quietly looking for material" from "a robot is
stuck", and in dark mode both rendered as brown-bg / gold-text. The warm ramp
now reads as three separable steps — sand (benign, early) → orange (attention)
→ red (failed) — which also tracks the lifecycle, since `faulted` either
recovers or becomes `failed`. When adding a status, do not put a benign state
on a warm hue.

**Amended 2026-07-26 — state the invariant, not the proxy.** The rule above is
a proxy, and the palette ran out of room to satisfy it. `sourcing` is a benign
state on a warm hue and always was; the dark theme showed the cost, with
`sourcing` and `failed` collapsing to 2.70 CIEDE2000 under protanopia and the
three warm steps *reordering* — benign beside dead-order, attention outside
them. The literal fix is unavailable: measured, every cool candidate for
`sourcing` lands 0.77–1.2 from `in_transit`, `pending` or `queued`, because
the cool band already carries eight statuses, and violet is barred by P13. A
13-status palette at one flat weight has no free hue.

**The invariant the proxy was protecting: a benign state must never be
confusable with an alert state, on any channel, under any of the three
dichromacies.** Hue is the usual way to get that and it is still the default.
When hue is unavailable, take it on **lightness** — the one axis all three
dichromacies preserve, which is why a lightness separation measures the same
number four times and a hue separation never does. `sourcing` was lifted to
`#3e3a1e` in dark and every pair it touches now clears the floor. Light was
left alone, because the same deepening there drops `delivered` to 0.52 under
protanopia — worse than the problem.

The real conclusion is about the palette rather than about `sourcing`: **13
statuses at one calm weight is at capacity.** The next status added cannot be
given a hue; it will have to be given a weight, or something has to leave.

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
| Attention (loud) | faulted | `#fed7aa` | `#9a3412` |
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

Text: `--text` primary · `--text-muted` secondary (`#8B949E` dark / `#68717A`
light) · `--text-strong` the brightest body text (`#E6EDF3`, floor boards).
**Never pure white or black.** Chart series use the **curated data-viz palette**
(`--viz-*`) — one designed, vibrant set used generously (P19, see Data
visualization); hero numbers stay white.

### The text ramp is two steps, and `--text-muted` is the quiet one

**There is no `--text-tertiary`, and there cannot be a third quiet step.** It
existed, it was documented here as "the faintest labels", and it was **below the
4.5:1 normal-text floor on every surface that hosted it, in both themes** —
3.15:1 on a card and 2.91:1 on the page in light; 3.77:1 on a card and
**3.27:1 on a raised panel** in dark. It was live on the KPI strips, the
overview support panels and the Replenishment Health threshold editor.

Nothing had measured it, and the reason generalises: every contrast test in the
repo measured a **specialised** ink — badge, chip, chart mark — and none measured
the ramp that paints ordinary text. `--text-muted` had been measured exactly
once and by accident, because `--viz-secondary` aliases it and it rode in through
the chart-ink test. `shared/text_contrast_test.go` measures the family directly
now, and its exhaustiveness check is the load-bearing half: **every `--text*`
token in `tokens.css` must appear in its table**, so a new one cannot be added
unmeasured.

**The interesting part is that it could not be fixed by moving it.** Solve for
the lightest grey of its own hue and saturation that clears 4.5:1 on every light
surface and you get `#656D78` — *darker* than `--text-muted`'s `#68717A`
directly above it. `--text-muted` was already nudged to sit barely over the floor
(**4.58:1** on the worse light surface, the worst figure in the whole family). So
the ramp has no room underneath: a third step quiet enough to read as quieter
than muted is too quiet to read, and one that clears the floor is not quieter.
The ramp inverts either way. **Two steps of body text is capacity** — the same
conclusion the Signal palette reached at 13 statuses, in a different property.

What makes a label read as a label is its **size, case and letter-spacing**,
which every one of the fifteen former declarations already set — four in
`components.css`, eleven in Core's `style.css`. A one-step
luminance difference on top of that is not an encoding; it is colour-alone
signalling for a distinction the type already carries — the ruling U5 reached for
`.de-muted` vs `.de-nodata`, applied to the token layer. **If a future surface
genuinely needs a third step, take it from weight or size, never from luminance.**

Two riders worth keeping:

- **A text token is not a mark token, in either direction.** The map's
  offline-robot dot was painted with `--text-tertiary`; it now reads `--sub-4`,
  whose documented role *is* "structural dots — marks that carry meaning". A
  nudge to a text token for a text reason would otherwise have silently moved a
  robot dot. (`paused` is still on `--text-muted` and is the same error, left
  alone: what replaces it is a question about the robot-state vocabulary.)
- **Measure against the worst surface a token actually lands on, and no others.**
  The record that first flagged this token quoted 2.91 light / 3.77 dark — two
  true figures measured against two *different* elevation steps, and the dark one
  was not that theme's worst (`--elev-raised`, 3.27, is). Going the other way,
  `--elev-canvas` is the weakest surface in the file and has **zero use sites**,
  so measuring against it would invent failures nobody can see. The surface list
  in the test carries the evidence for each entry.

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

### Which shape answers which question

**Pick the form from the question, not from the page.** Phase 6 adds the first
real charts to this system. The failure mode is that each page picks a shape for
itself and the reader relearns the encoding on every screen — so this rule is
written before the charts exist, which is the only cheap moment to write it.

Say the question out loud first. The question names the shape.

| The question | The form | Why that one |
|---|---|---|
| *How much? Which is bigger?* | **Bars**, baseline at zero | Length from a common baseline is the most accurately-read encoding there is. Sort by value unless the categories have a natural order |
| *Is it moving, and which way?* | **Line**, time on x | A line asserts continuity between its points, which is a claim. Use one only where the gap between points is genuinely traversed |
| *What is it made of?* | **Stacked bar or area — only when the totals beat the parts** | See the trap below |
| *What does normal look like?* | **Histogram** (box or violin when comparing groups) | The plant's numbers are heavy-tailed. A median hides precisely the tail you are looking for |
| *Does A move with B?* | **Scatter**, with `n` printed on it | Below roughly 30 points a scatter shows a shape that is not in the data |
| *Where is it happening?* | **The map**, as an overlay on the real floor plan | Spatial clustering is the one thing a table structurally cannot render |
| *Is this one number OK?* | **Not a chart.** Print the number | |

**Bars start at zero; lines need not.** A bar's *length* is the value, so a
truncated baseline turns a 3% difference into a 3× one. A line encodes position,
not length, and may start wherever the data lives.

**The stacked trap.** In a stacked bar only the bottom band sits on a common
baseline; every band above it is measured against a floor that moves. Readers
compare the bands anyway, and get it wrong. So: **stack only when the question is
about the total and the composition is a bonus.** *"How many closes a day, and
roughly how do they split"* — stack it. *"Is the sweep's share of closes
climbing?"* (5.6) is a question about **one part**, and it wants a line of that
part's share, where a drift toward 100% is visible at a glance and reads as the
alarm it is. When several parts each matter on their own, small multiples beat
one stack.

**Split the layer when the failure modes differ.** The dual-y-axis ban below and
the stacked trap above are the same disease in two organs — two things composed
onto one scale — and it has a third form that arrives disguised as a data question
rather than a chart question. **Two rates over two populations cannot share a
scale**, however alike they look and however much they are both "percentages of
something."

**The test is different denominators, not different causes.** "Different causes"
is a judgement about the domain, and two engineers will disagree about it in good
faith. The denominators are in the query: if one rate is measured over *every*
tick and the other only over the ticks that *produced a value*, they are measured
over different populations, and you can see that without knowing what a tick is.
Give them separate layers, separate panels, or small multiples — or redefine the
measurement so there is genuinely one population, which is the better fix wherever
the failure has a value that can be counted (see *Never band a conditioned
statistic*).

The localization work is the worked example, and it carried both rates. A lane's
**no-estimate rate** is over every tick the robot reported; its **confidence
aggregate** is over only the ticks that produced an estimate. Each channel is
blind to exactly the defect the other finds: the no-estimate channel finds all
nine reflector-less zones at Springfield and none of the LM13→LM14 corridor, and
the confidence channel finds the corridor and none of the zones. That is not one
measurement with noise in it; it is two measurements over two populations, and no
single band can be honest about both.

**The forms we do not draw, and why:**

- **No pie or donut charts.** Angle is read less accurately than length and the
  labels never fit. A sorted bar answers every question a pie does.
- **No gauges or speedometers.** Enormous ink for one number against one
  threshold. Use the number plus a meter track — neutral track, the fill carries
  the state (see Visual principles).
- **No dual y-axes.** The crossing point is an artifact of two arbitrary scales
  and someone will read it as an event. Two stacked panels sharing an x-axis.
- **No 3D, no drop shadows under marks, no animated draw-on.** Motion means
  motion; a bar growing on page load is decoration.
- **No two-point line.** `47 → 52` is the entire content. Print it.

**Reach for the table more often than feels right.** Under about five categories
a sorted table is faster to read than a bar chart, carries the exact figures, and
can be pasted into a spreadsheet — which is what the reader was going to do
anyway. Phase 6's demand browser (5.1) is a table for exactly this reason; a
chart earns its place only where the table cannot answer the question.

**Applied to Phase 6**, so the rule is checkable rather than decorative:

| Phase 6 item | Form | Because |
|---|---|---|
| `cost_ratio` across episodes (5.1 / 5.4) | Histogram, never a mean | The question is *what does a normal ratio look like*, and the answer is a shape. The mean of a ratio distribution is close to meaningless |
| Cause mix (5.2) | Chips on the row; small multiples for the trend | Four causes on one stack hide the one that is growing |
| `closed_by` share (5.6) | One line — the sweep's share | The alarm is a slope toward 100% |
| Transition matrix (5.9) | Heat-map matrix, greyed below minimum `n` | Two categorical axes and one value. Bars would need twenty of them |
| Orphan reconciliation (5.7) | Line of the count over time | The plan already says it: *the trend is the number that matters* |
| Cycle time (5.10) | Distribution per `(node, payload)`; median annotated **on** it, never alone | The tail is the material-downtime signal |

### The numbers themselves

Six rules. The first is the one that gets broken.

**1 — Never print more precision than the measurement supports.**

The temptation is mechanical: the float holds `47.2831`, the formatter will
happily print it, and it *looks* more rigorous than `47`. It is the opposite.
**Precision is a claim about the measurement, not about the arithmetic.** Every
digit beyond what the input supports is an assertion the data cannot back, made
by a formatter that has no idea what the data is.

Work it through on the number Phase 6 actually ships. Cycle time (5.10) is a
difference between two `bin_uop_audit` timestamps on a stream that is
deliberately lossy in known ways — a stale-epoch drop erases the interval it
lands in, and the drops are episodic rather than steady: zero on most days,
then a burst past 3,000 in one — stamped by service clocks not synchronised to
the millisecond. That number is worth about two significant figures. `47.283 s` asserts a millisecond-accurate
interval from a source that cannot supply one; **"about 47 seconds"** is what was
actually measured.

This is not cosmetic. An engineer who reads `47.283` will chase a 0.3-second
regression that is entirely quantisation noise, and will stop trusting the panel
when the chase comes up empty. **Round to the digit you would defend to the
person who has to act on it.**

| Kind of number | Precision |
|---|---|
| A count | Exact — **of a stated window**. `1,779` was a true 24-hour count and became an estimate the moment it was quoted as a rate. See below |
| A derived duration | Whole seconds under ten minutes, whole minutes above |
| A percentage | Whole percent, unless the denominator runs to thousands |
| A ratio | One decimal (`3.2×`), **greyed below the minimum `n`** (5.4, 5.9) |
| A figure from an upstream system | Exactly as that system publishes it, unchanged |

Two mechanical corollaries:

- **Carry full precision through the arithmetic and round once, at the render.**
  Rounding mid-pipeline produces totals that disagree with their own parts, and
  the reader finds that before you do.
- **Where the number is soft and there is room to say so, say so.** *"about
  47 s"* costs six characters. Where there is no room the rounding itself carries
  the message — which is exactly why the rounding has to be right.

**The count that became an estimate.** The rule above has a companion failure
that is much harder to catch, because the number involved is not wrong.

`1,779` was a true count: stale-epoch drops at Springfield in the 24 hours to
2026-07-25T05:18Z, read off a `journalctl` fingerprint and later reproduced to
the digit by querying `bin_uop_audit` over the same window. Nobody rounded it,
nobody estimated it, and it survived re-measurement in a different tool. It then
misled every document that carried it — and both reasons are about what failed
to travel *with* the number, not about the number.

**The window was stripped.** Those 24 hours happened to be the worst burst in
the dump. The drops are episodic, not steady — zero on most days, then
thousands — so `~1,779/day` reads as a rate and is a peak. Restated with its
window it is exact again. Restated as a mean over the same period it would be
about 188, which is equally true and equally misleading. **Neither number is
wrong; the missing window is.**

**The same population was counted twice.** It travelled as *"1,779 drops **plus**
1,779 replays"*, which invented a second population and doubled the first. One
stale-epoch drop emits both log lines: `uop.applier` logs the drop and returns
`ErrInventoryDeltaSkipped`, and `messaging.core_data_service` catches that
shared sentinel and logs "replay — already applied". Two lines, one event.
**The identical counts were the tell, and they read as corroboration.**

So the worked example stands, upgraded, and it is a better one than it was.
**A count is exact — and exactness is a property of the count and its window
together.** Print the window beside the count, or print neither. And when two
numbers agree to the digit, ask whether they are two measurements or one
measurement written down twice.

**2 — Tabular figures, always.**

Already law (see Type scale): every count, quantity, duration and metric takes
`font-variant-numeric: tabular-nums` via `.tnum`, so digits do not dance as live
values tick. Two things that rule does not say and that get missed:

- **Numeric columns are right-aligned.** Tabular figures align the glyphs; only
  right-alignment aligns the *magnitudes*, which is what makes a column scannable
  for the big one.
- **A column commits to one decimal count.** `2` and `2.00` in the same column
  move the decimal point and defeat tabular-nums entirely. Pick the precision for
  the column, not per cell.

**3 — One abbreviation rule, and this is it.**

Stated once, here, so nobody re-decides it per page:

- **Below 10,000, print in full with a thousands separator** — `9,481`.
- **At or above 10,000, and only in space-constrained chrome** (axis ticks,
  chips, tile heroes): `k` / `M`, at most one decimal, no space before the
  suffix — `12.4k`, `1.2M`.
- **Tables and detail views never abbreviate.** A table is where someone reads
  the exact figure and copies it out; `12.4k` destroys both uses.
- **Durations are compound, never decimal hours** — `1h 04m`, `4m 07s`, `47 s`.
  Never `0.78 h`; nobody converts that in their head.
- **One space before a unit word, none before a magnitude suffix** — `47 s`,
  `12.4k UoP`, `86%`.

**4 — No data, zero, and not applicable must look different from each other.**

This is the load-bearing rule in this section.

| State | What it means | How it renders |
|---|---|---|
| **Zero** | We measured, and the answer is zero | `0` — a real number, normal text colour, tabular |
| **No data** | We have not heard, the window holds no rows, or the source is unreachable | `—` (em dash) in `--text-muted`, with a title saying *which* of those it is |
| **Not applicable** | The question does not apply to this row | Empty cell, or `n/a` in the quietest text tone |

**Never coalesce absence into zero.** The bug has a face at every layer of the
stack and they are all the same bug:

```
COALESCE(x, 0)      -- SQL, on a display column
int                 // Go, whose zero value is indistinguishable from unset
x || 0              // JS, where null and 0 collapse identically
{{.Count}}          <!-- template, rendering a zero it has no way to question -->
```

The fix is at the **type**, not at the CSS: `*int`, `sql.NullInt64`,
`x != null ? x : null` — so the renderer still holds the information to tell the
two apart by the time it gets there. If the value arrives as a plain `int` the
distinction was destroyed upstream and no amount of styling recovers it. This is
the UI face of the principle in `AGENTS.md`: **a check must know whether it had
the input to check.**

**Why this one is load-bearing.** The A1 reachability defect on this very branch
was this exact mistake one layer down: the sweep inferred *"Core has recent
contact with this Edge"* from *"nothing has marked this Edge stale"* — reading an
unset flag as a positive finding — and closed real episodes in bulk whenever a
ticker in another service was wedged. A tile printing `0` where the truth is *"we
never heard"* is the same defect in a different costume, and worse in one
respect: a human reads it and acts on it. Phase 6 makes that concrete — **zero
orders against a real demand** is the plan's worst case, the one the heat-map
mockup calls out as *worse than a high ratio*. A UI that cannot separate that
from a dead feed will send someone to the floor to inspect a healthy cell, and
will say nothing at all on the day the feed genuinely breaks.

Three riders:

- **Do not over-rotate.** A real measured zero stays `0`, plainly. Dashing out
  true zeros hides the finding from the other direction — the same error
  mirrored.
- **A chart with no rows gets an empty state, not an empty axis frame.** A drawn
  frame with nothing in it reads as *"measured, all zero"*. Say what is missing
  and over what window. `.empty-cell` covers the table-cell case.
- **An unrecognised value is not an absent one.** Every `close_reason` /
  `origin_class` switch needs a `default` that renders the unknown value **as
  itself** (5.5). That vocabulary has already grown twice — `claim_removed`,
  `superseded` — and a `default` rendering blank turns every future addition into
  a silent data-loss bug in the UI.

**Corollary — return the question, don't merely expose it.** The rule above puts
the fix at the **type**, not at the CSS. At an API boundary that has a sharper
form: **when a value has a state that is indistinguishable from a legitimate
value, the accessor must RETURN that distinction, not merely make it available.**

*Available* is precisely the mode that has failed, every time, in this codebase.
`confidence`, `area_ids`, the battery block and `Suspended: false` were all present
on data Core was already receiving, and all four went untaken for months — not
because anyone weighed them and declined, but because no signature ever asked.
Availability is not a safeguard; un-skippability is.

**Worked example.** `fleet.SceneState.DisabledPaths` is an empty slice in two
different worlds: nothing is disabled, and the envelope has never been observed.
Springfield has four lanes disabled right now, so a caller reading the slice alone
renders those four as **enabled** — through the whole window after a boot, again
after a reconnect, and permanently against a backend that does not implement the
call at all. That is *no data rendered as zero*, one layer below the renderer, and
in the reassuring direction. An `ObservedAt.IsZero()` check makes the distinction
*available*; changing the signature from `GetSceneState() SceneState` to
**`GetSceneState() (SceneState, bool)`** makes it impossible to skip — the caller
cannot obtain the value without also being handed the question
(`shingo-core/fleet/optional.go`).

**The test for whether you have done it: can a caller get the value without the
question?** A second return value, an `(value, ok)` pair, a sum type — no, and
that is the point. A `Populated` field beside the data, a documented sentinel, a
zero value with a comment above it — yes, and each of those is exposure. Exposure
gets skipped, and it gets skipped silently, which is how a renderer ends up
holding a distinction that was destroyed three layers upstream.

**5 — Never band a conditioned statistic.**

An aggregate computed over a sample that was **selected by the very thing being
measured** is not on the same scale as an unconditioned one, and must not be
rendered as though it were — not banded on the same thresholds, not coloured from
the same ramp, not printed in the same column.

The general form is the reason this earns its space: **any "% success" whose
failures drop out of the denominator is this defect.** So is a mean over "valid
readings", a latency percentile over the requests that returned, a yield-per-hour
over the hours that produced. What you are holding is not a sample of the
population; it is a sample of the population *that survived* — and the thing that
removed the rest is usually exactly what you were trying to see.

Two remedies, in this order. Where the failure has a value, **count it** — a
localization miss is a confidence of zero — and the aggregate is over the full
population again, which can be banded honestly. Where the failure has no value to
count, **suppress the aggregate and render the selection rate instead**: the
selection rate is over the whole population by construction, and it is the channel
the defect actually lives in.

**The worked example.** At Springfield, path segments running through one of the
nine reflector-less zones average **0.897** localization confidence against **0.740**
for the rest of the plant. They read *better* than the plant. Inside those zones
the robot produces a good reading or none at all — a failure leaves the value
channel entirely, reported as a sentinel rather than as a low number — so what
survives is a **truncated** distribution, not a degraded one, and the zone looks
healthy because half of its ticks are missing. Banding that conditioned mean
against zone membership scored **AUC 0.081**: it predicted the dead zones almost
perfectly *backwards*. A map coloured from it would have painted the nine worst
areas on the floor the same green as the best.

**Its two siblings are already in this section, and the family is worth naming.**
*The count that became an estimate* is a true number that misled because its
**window** did not travel with it. *Never coalesce absence into zero* is a true
zero that misled because it was an **absence**. This is a true aggregate that
misleads because its **selection** did not travel with it. In all three the
arithmetic is correct, and the defect is entirely in what the number failed to
carry.

**6 — Match the threshold to the statistic it was defined for.**

A threshold defined for an instantaneous reading is not a threshold for a tail
statistic, and carrying it across produces a chart rather than an error — which is
why it survives review.

RDS bands robot localization confidence at **0.80 / 0.30**, green above and red
below. `reference/rds-user-manual.pdf` is explicit about what those cuts colour:
**one robot's live reading in the robot list.** Applied instead to a per-lane
**p05** over a week they put **91%** of path segments in the middle band and separated
nothing — and not because the plant is uniform. It is arithmetic: a tail statistic
sits systematically below the typical reading, so cuts chosen to be interesting
against typical readings land off the end of the tail's distribution and
everything piles up on one side of them. On the **mean** the same two cuts split
**29% / 71%**, which is a finding. **The thresholds never moved; the statistic
did.**

**The boundary with "a figure from an upstream system."** The precision table in
rule 1 says to print an upstream system's figure *exactly as that system publishes
it, unchanged*, and a reader who has internalised that row will read a vendor's
thresholds as sanctioned by it too. They are different objects. **Print the
upstream system's NUMBER unchanged; do not inherit its THRESHOLDS onto a statistic
they were never defined over.** A number is a measurement and carries its own
definition with it. A threshold is a *decision about a distribution*, and it is
only meaningful over the distribution it was drawn on — a percentile of that
distribution is a different distribution. Adopting a vendor's bands is still the
right instinct where the operators already read them off the vendor's own screens;
the adoption is legitimate on the statistic the vendor banded, and on any other
statistic only once the cuts have been re-derived on it.

### The palette

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

### Ordinal scales and the semantic triad

**An ordinal scale may not rely on the semantic triad as its only channel.**

`--viz-green` / `--viz-amber` / `--viz-coral` are documented above as **semantic**
hues for **discrete** states — the chip that reads *Failing* with the word printed
inside it, where the colour is redundant encoding wrapped around a label. Pressed
into service as an **ordered ramp** — good, marginal, bad along one axis, on a mark
that carries no text — they stop being redundant and become the entire encoding,
and they do not hold it. Measured under deuteranomaly on dark, `--viz-green` and
`--viz-coral` separate by **ΔE 6.0**: the two *ends* of the scale, the pair a
reader most needs to tell apart, read as one colour for roughly one man in twelve.

For scale, the categorical ramp holds its worst merely-**adjacent** pair at ΔE 17.0
under CVD simulation (P19, law 2 above). And this palette has twice been changed
over a smaller collapse than this one: indigo beside violet at 2.4 protan, fixed by
reassigning the scale order (P19), and `sourcing` beside `failed` at 2.70, fixed by
re-picking a token value (see *Status indicators*). Same class of defect, at the
two values that matter most.

**The rule is not "stop using the triad."** It is the instrument the sequential
ramp already uses one paragraph above — *steps ordered by luminance so the ramp
still reads in greyscale* — applied to a scale whose steps happen to be semantic:
**carry the ordering in a second channel** (stroke weight, dash pattern, size) so
the encoding survives desaturation, and let hue only confirm what the form already
says. **Order must survive in greyscale.** This is the same move the Signal palette
made when hue ran out of room — take the separation on an axis all three
dichromacies preserve — one level up, at the encoding rather than at the token.

**Two alternatives were measured and rejected. Recorded so they are not
re-proposed:**

- **The sequential teal ramp** (`--viz-seq-1` … `--viz-seq-5`) is the safer
  colour: its worst pair measures **9.8**, and it falls between *adjacent* steps
  rather than between the extremes, which is where a scale's smallest separation
  belongs. It was rejected for a non-colour reason — it abandons the vendor
  vocabulary the operators already read on the vendor's own screens (rule 6 in
  *The numbers themselves* is the other half of that argument), and on the surface
  that raised this, teal is already spoken for by the reflector mark.
- **The diverging coral↔teal ramp** is reserved for **signed** data and confidence
  is not signed. It has a floor, a ceiling and no meaningful zero; putting it on a
  diverging ramp asserts a midpoint the measurement does not have, and colours
  "moderate" as neutral.

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
- **Tables** — muted header/border chrome, drawn from the substrate ramp
  (`--sub-*`, see Design tokens); color lives only in the status chips
  and health dots, never in the row fill (except a soft state tint like the
  >30 d staleness wash).
- **Tiles / node cells** — neutral base; the state color (has-payload, staged,
  maintenance) is the one saturated thing on the tile.
- **Meters** — the track is `--bg-dark`; only the fill and the threshold tick
  carry hue. **A meter track is NEVER tone-on-tone with its fill (U9).** The
  external dataviz convention says to tint the track with a desaturated copy of
  the fill hue so state reads across the whole bar; ShinGo does not, and this
  line exists so nobody re-imports that rule. A desaturated state colour placed
  in structure is precisely what this principle forbids, and a tinted track is
  wrong on its own terms the moment the fill retints — `.cs-meter` had an indigo
  track under a fill that turns amber and red. A track carries no state; it is
  either a neutral `--bg-dark` (the fill is semantic) or a `--sub-1` substrate
  step (the fill is not).

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

**Adoption, as of 2026-07-26 — the sprite is wired on Core only.**
`{{iconSprite}}` is a Core template func (`shingo-core/www/helpers.go`) injected
by Core's `layout.html` and `dashboard-map.html`; thirteen `<use href="#icon-…">`
sites across six Core files resolve against it. **Neither Edge admin nor the
Operator HMI injects the sprite**, so a `<use>` reference on those surfaces
resolves to nothing — Edge's `header.html` still hand-inlines two raw bell SVGs
and `operator-display.html` includes no sprite at all. The sprite, the shared
detector and both drift tests exist and are green; what is outstanding is the
Edge and HMI *wiring*, not the asset.

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
| **UoP** | Units of Production — the count of finished parts a bin/payload carries, or that a cell has consumed. The atomic quantity the threshold monitor sums and reorder thresholds fire on. | Always **"UoP"** in UI text — labels, headers, table columns, prose, toasts, tooltips. Never "UOP" or "uop". **Display text only:** code identifiers, JSON keys, struct fields, and `data-*` attributes keep their existing casing (`UOPRemaining`, `uop_remaining`, `data-uop`) — renaming a serialized field is out of scope and would break the protocol. |

**Casing history — and a warning about how it was mis-measured twice.**

`752dec99` (2026-07-22) swept nine files and **held perfectly**: not one of them
has regressed. `8272aac0`'s follow-up (2026-07-26) finished the job across the
files that sweep never covered. What went wrong in between was the *measurement*,
not the code, and it went wrong the same way twice. (This paragraph first cited
`2c0d3c48` — the `--sub-*` substrate-ramp commit from the same merge. A wrong
SHA inside the passage about getting the record wrong; corrected here.)

**Raw `UOP` counts are meaningless here.** Most occurrences in the tree are
supposed to be uppercase — `{{.UOPRemaining}}`, `UOPCapacity`, `remainingUOP`,
`lsUOP`, `data-uop`, `bin_uop_audit`. An unfiltered grep returns ~199 and reads
as catastrophic drift; a differently-scoped one returned "34 vs 7" and then
"44 vs 30", which reads as a rule actively decaying. **Neither number described
a real problem**, and the second was used to justify re-opening an item that had
in fact held. Never quote an unfiltered count for this term.

The only question that means anything is: **which rendered text says `UOP`?**
Sort every hit into a bucket before touching anything:

| Bucket | Verdict |
|---|---|
| Identifier / field ref / `data-*` attribute | Correct. Leave it. Renaming a serialized field breaks the protocol |
| Rendered text in a file a previous sweep covered | A real regression |
| Rendered text in a file no sweep ever covered | Not drift — an unswept surface |
| Prose in a comment | Not the product, but it is where the next implementer copies the casing from |

Applied to the 2026-07-26 pass, all 19 rendered hits were in the **third**
bucket. `752dec99` enumerated templates plus three Core page scripts; the
**entire Operator HMI** (`shingo-edge/www/static/operator-station/`) and Edge's
`static/js/pages/` were never in its scope. The highest-visibility surface in
the system — what an operator reads all shift — had simply never been swept.
Comments were swept in the second pass on purpose, reversing the first pass's
call, on the grounds above.

**The discriminator, if you write a guard.** A test that bans the string `UOP`
fails against a *correct* tree, because it cannot tell a label from a field ref.
The predicate that can is a word-boundary match:

```
(?<![A-Za-z0-9_])UOP(?![A-Za-z0-9_])
```

Verify it **both ways** before trusting it — it must fire on `>UOP Remaining<`
and `' UOP)'`, and stay silent on `{{.UOPRemaining}}`, `data-uop-capacity`,
`remainingUOP` and `MinHysteresisUOP`. A guard that cannot separate those two
lists is not a guard. No such test exists yet.

**The checkable end state:** that pattern returns nothing in any `.js` / `.css` /
`.html` under `shingo-core/www`, `shingo-edge/www` or `shared/`. Verifiable in
ten seconds, unlike a count in a table.

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
