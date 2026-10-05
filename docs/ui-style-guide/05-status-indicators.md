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

