## Status indicators

Verified against: b9210bce

### Signal theme

Badge colors follow a scheme called **Signal**. **Hue** encodes *where* an order
is in its lifecycle; **weight** is held flat. Every non-alert badge sits at one
calm, low-saturation weight, so the two alert states — `faulted` (orange) and
`failed` (red) — are the only loud pills and clearly out-weigh everything else on
a crowded table. Grey is reserved for `cancelled` alone.

**The lifecycle:**

```
EARLY (2 graduated calm tints:           →  SUBMITTED (steel blue)
  pending slate · queued periwinkle)
→  ACTIVE (per-phase hue, calm weight)   →  SUCCESS (green)
→  ATTENTION (orange, loud)  →  FAILURE (red, loud)  ·  cancelled (grey)
```

**Weight rule** (for anyone adding a status): non-alert light backgrounds stay
light (L≥86) and dark text stays bright (L≥68); only `faulted` and `failed` may
go below that. All text-on-pill pairs clear WCAG AA (≥4.5:1) in both themes
(`TestSignalBadgeTextClearsAA`).

**Step rule — two pills one step apart in the story must be more than one step
apart in colour.** Two pills that are each individually legal can still be the
same colour with two labels. `pending` and `skipped` mean opposite ends of an
order's life ("nothing has started" against "this was never needed"), so they
sit a full step apart on the slate ramp: separated on **lightness**, the one
axis all three dichromacies preserve. `TestSignalPaletteSeparatesUnderCVD`
(`shared/signal_cvd_test.go`) measures every adjacent pair of the lifecycle
ramp, and the pairs that share a hue family, in both themes, under normal vision
and the three dichromacies, against a 5.0 CIEDE2000 floor. Each combination that
falls short is listed in `knownCollapses` with the value it measures, so a
re-hue cannot make one worse, or fix one, without the test saying so.

**Hue rule — warm is for alerts.** `faulted` is orange and `failed` red, and the
warm ramp tracks the lifecycle: a faulted order either recovers or becomes
failed. Do not put a benign state on a warm hue. The invariant underneath it:
**a benign state must never be confusable with an alert state, on any channel,
under any of the three dichromacies.** Hue is the usual way to get that. When
hue is unavailable, take the separation on lightness.

**The palette is at capacity.** At one calm weight there is no free hue left
for another status: the cool band already carries most of them, and violet is
barred because indigo is the accent. The next status added gets a weight, or
something leaves.

**`sourcing` has no colour of its own.** It is not a status an order can be in;
`.badge-sourcing` is an alias in `.badge-queued`'s rule, in both themes, so an
old history row renders as the wait it was (`TestSourcingBadgeIsQueuedsAlias`),
and there is no `--status-sourcing-dot`.

**Per-phase hues in the active band:**

Each active phase has its own color so it's distinguishable at a glance:

| Phase | Hue | Why |
|---|---|---|
| dispatched | Blue | Robot assigned, mission queued — "assignment blue" |
| in_transit | Cyan | Robot physically moving — "movement cyan" |
| staged | Teal | Bin at destination, awaiting next step. Teal, not indigo: indigo is the UI accent. It sits beside in-transit cyan and stays clear of the success green |
| reshuffling | Pink | Rearranging bins — active handling, **not** a fault. Pink, not violet: it reads as benign activity rather than an alarm, and leaves the accent alone |

**The badges, by status** (values in `shared/status-classes.css`; `faulted` and
`failed` read the `--status-faulted-*` / `--status-failed-*` tokens, see
[Design tokens](04-design-tokens.md)):

| Signal | Statuses | Hue |
|---|---|---|
| Early: pending | pending | slate, the lightest |
| Early: queued | queued (and the `sourcing` alias, history rows only) | periwinkle |
| Submitted | submitted, acknowledged | steel blue |
| Active: dispatched | dispatched | blue |
| Active: in_transit | in_transit | cyan |
| Active: staged | staged | teal |
| Active: reshuffling | reshuffling | pink |
| Success | delivered, confirmed | green |
| No-op | skipped | slate, a full step below pending; never the cancelled grey |
| Attention (loud) | faulted | orange |
| Failure (loud) | failed | red |
| Cancelled (the one grey) | cancelled | grey |

**Dark theme** uses deeper backgrounds and brighter text, tuned for shop-floor
LCDs under fluorescent lighting.

### The chip floors

A health chip (`.chip-ok`, `.chip-near`, `.chip-below`, `.chip-muted`,
`.chip-warn`, `.chip-err` in `shared/components.css`) is not a badge. Two
questions are asked of every pill, and only the first is a floor for a chip:

1. **Text on pill ≥ 4.5:1.** Can you read the label. Enforced for badges by
   `TestSignalBadgeTextClearsAA` and for chips by `TestChipContrast`. A hard
   gate for both.
2. **Pill against surface ≥ 3:1.** Can you see there *is* a pill. WCAG 2.2
   SC 1.4.11 — asserted for **opaque** pills only.

A chip's fill is a translucent wash of its hue, and SC 1.4.11 covers UI
components and graphics "required to understand the content". A health chip is
neither: it is not interactive, and its meaning is the word printed inside it,
so the pill is redundant encoding around a label. The floor belongs to the
`--viz-*` MARK tokens, where a chart mark is the information and has no text
alternative. Reaching it would also cost the chip its calm: a wash only reaches
3:1 by becoming nearly opaque, i.e. by becoming a Signal badge.
`TestChipBoundaryNeedsNearOpacity` pins that, and fails if a chip could reach
the floor at an opacity that still reads as a wash, which is the cue to revisit
the rule for that chip.

**Every chip prints a label.** An icon-only chip puts the meaning back in the
shape, and the boundary floor applies to it again.

**A chip's ink is its own token.** The label colour is `--chip-ink-*`
(tokens.css), chosen independently of the fill: a chip whose label is the same
token its fill washes from cannot be tuned, because the two move together.
Ink is never a `--viz-*` or `--sub-*` token either; those are MARK and STRUCTURE
colours held to 3:1, and using one as type is the same category error in either
direction. `TestChipInkIsNotItsFill` asserts the separation directly, so a
re-collapse fails even where the resulting ratio happens to survive.
`.chip-near` and `.chip-warn` are amber two points apart and share an ink.

### One palette, three renderers

There is **one** status palette. It feeds the badges (above), the robot-map
status dots and the floor-display board rows: the map and the board read the
same `--status-<status>-dot` tokens from `shared/tokens.css`. The dot tokens are
the saturated hue; the badge pairs above are the calm-weight pills derived from
the same hue. Values are in [Design tokens](04-design-tokens.md).

| Status | Dot token | Dot hue |
|---|---|---|
| pending | `--status-pending-dot` | slate |
| queued | `--status-queued-dot` | periwinkle |
| dispatched | `--status-dispatched-dot` | blue |
| in_transit | `--status-in-transit-dot` | cyan |
| staged | `--status-staged-dot` | teal |
| reshuffling | `--status-reshuffling-dot` | pink |
| blocked | `--status-blocked-dot` | red (map and board only — no order status is `blocked`) |
| delivered | `--status-delivered-dot` | green |

Robot states on the map draw from the same set: ready is the delivered green, a
moving robot the in-transit cyan, error the blocked red, offline `--sub-4`, and a
charging robot takes its bay's `--map-dock-charge` amber.

### The Indigo accent

**Indigo is the reserved UI accent** — `--accent`, and `--primary` aliases it.
Use it for **interactive / UI chrome only**: links, focus rings, selection, the
primary action (`.btn-primary`), active tabs, section ticks, and the map focus
ring. In charts it also serves as **series-1** of the curated data-viz palette
(see [Data visualization](06-data-visualization.md)) — that's fine; the only
hard rule is that indigo never becomes a *status* hue.

**Foreground vs filled.** The accent has two values for two jobs. `--accent` is
the *foreground* — text, links, focus rings, active states *on* a surface.
Surfaces that put **white text on an accent background** (filled buttons, solid
badges) use **`--accent-solid`** (theme-invariant, with `--accent-solid-hover`)
instead: the dark theme's foreground indigo under white text is only **3.50:1**
(fails AA), while `--accent-solid` is **6.14:1** in both themes. Rule of thumb:
accent *on* a surface → `--accent`; accent *as* the surface under light text →
`--accent-solid`.

**Indigo is NEVER a status hue.** One restrained accent *glow* is allowed on
genuinely live/active elements (the route comet, a live pill); everywhere else
the accent is a flat fill or stroke. `--info` (cyan) and the status blues
(`dispatched`) stay their own tokens — never fold a data/semantic color into the
accent. If a screen needs `--primary` to *mean* something (a status, a series),
give that spot its own token instead.

### Surface elevation + text

Cards read by **elevation** — each surface one shade lighter than what sits
behind it — so most hard borders can drop. Light mode inverts to light cards on
grey. Values are in [Design tokens](04-design-tokens.md).

| Token | Role |
|---|---|
| `--elev-canvas` | the void behind everything |
| `--elev-base` | page background (`--bg`) |
| `--elev-surface` | cards / panels (`--surface`) |
| `--elev-raised` | raised elements on a card |

Text: `--text` primary · `--text-muted` secondary · `--text-strong` the brightest
body text (floor boards). **Never pure white or black.** Chart series use the
curated data-viz palette (`--viz-*`); hero numbers stay white.

### The text ramp is two steps, and `--text-muted` is the quiet one

**There is no third quiet step of body text, and there cannot be.**
`--text-muted` sits just over the 4.5:1 normal-text floor on the worst light
surface it lands on, so a step quiet enough to read as quieter than muted is
too quiet to read, and one that clears the floor is not quieter. **Two steps of
body text is capacity**, the same conclusion the Signal palette reaches, in a
different property.

What makes a label read as a label is its **size, case and letter-spacing**. A
one-step luminance difference on top of that is not an encoding; it is
colour-alone signalling for a distinction the type already carries. **If a
surface genuinely needs a third step, take it from weight or size, never from
luminance.**

`TestBodyTextTokensClearAA` (`shared/text_contrast_test.go`) measures every
`--text*` token against each surface that hosts text, and its exhaustiveness
check is the load-bearing half: **every `--text*` token in `tokens.css` must
appear in its table**, so a new one cannot be added unmeasured.

Two riders:

- **A text token is not a mark token, in either direction.** A structural dot
  takes `--sub-4`, whose role *is* "marks that carry meaning"; the map's
  offline-robot dot does. A nudge to a text token for a text reason must never
  silently move a mark.
- **Measure against the worst surface a token actually lands on, and no
  others.** Measuring against a surface with no use site invents failures nobody
  can see. The test's surface list carries the evidence for each entry.

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

**Emit the protocol status, never a label of your own.** A page that renders a
human word for a state still puts the protocol status in the class: a
JS-invented name like `badge-completed` has no rule, and the pill falls back to
the unstyled base. The mission pages emit `badge-<status>` straight from the
order's status; where a page shows a vendor's word for the same state, the word
goes in the `title`, not in the class.

### One source file

Status classes live in `shared/status-classes.css`, embedded by both Core and
Edge admin through `shared.Files`. The Operator HMI does not load it (see
[The three surfaces](02-the-three-surfaces.md)).

### Drift tests

`TestStatusClassesCoversAllProtocolStatuses` asserts that every value in
`protocol.AllStatuses()` has a `.badge-<status>` rule in `status-classes.css`,
reading the CSS literally; `TestStatusClassesNoStrayBadgeClasses` is the
inverse, so a status retired from the protocol cannot leave its class behind.
Adding a status to `protocol/status.go` without the class fails CI. Do not rely
on review discipline.

The JS side: `TestJSStatusTablesAreKeyedOnRealStatuses` holds Core's
status-keyed JS tables (the map's colours and labels, the Missions page's) to the
real status set, and `TestOrderStatusJSAgreesWithProtocol` holds the station's
`order-status.js` to protocol's projectors.

**Blind spot:** none of these scans the `.js`/`.html` sites that *emit* a badge
class. A class built from an invented word escapes CI and renders the grey
fallback. Emit sites must use protocol-status class names.

### Fallback styling

The base `.badge` rule is readable without a status modifier (surface fill,
body text, a border), so a status added to the protocol before its class renders
as a neutral pill rather than invisible text.

### Templates

In Go templates, always emit both the base and modifier class:

```go
{{/* GOOD */}}
<span class="badge badge-{{.Status}}">{{.Status}}</span>

{{/* BAD — drops the per-status color */}}
<span class="status-badge">{{.Status}}</span>
```
