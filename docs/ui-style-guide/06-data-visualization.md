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

### Time series

Four rules for any chart with time on x. `components/charts.js` carries the
defaults; a page that overrides one is breaking a rule, not tuning a chart.

**Lines are straight.** `tension: 0`, as the default and never overridden per
dataset. A curve between two hourly points asserts values nobody measured:
two cancelled orders three hours apart drew a hump across five hours. A point
with no drawn neighbour gets a marker (`isolatedPointRadius`), because
otherwise a one-bucket series draws nothing at all.

**The axis is continuous.** Every bucket in the window is present and an empty
bucket is a measured zero. A throughput axis that skips empty hours reads
`00, 02, 04, 05`, and the gap between two bars stops meaning anything. The
fill happens at the source (the server returns a zero bucket) and the series
runs to the bucket "now" falls in, not to the end of the window, because a
future hour is not a measured zero. A **rate** over an empty bucket has no
value (a gap, `null`), never 0%. A day nothing finished on is not a day
everything failed.

**Buckets are plant hours and plant days.** A day bucket starts at the plant's
midnight, passed to SQL as a bound zone parameter. The Postgres session is
pinned to UTC, so a bare `date_trunc('day', ts)` cuts a Central plant's day at
19:00. Day steps go through calendar arithmetic in the plant zone, so a
25-hour fall-back day is one bucket. Labels come from `plantclock.js`
`bucketLabel`, never from the browser's `getHours`.

**The bucket in progress is drawn distinctly.** The bar is washed out and the
line segment into it is dashed (`progressBarColors`, `progressSegment`, with
`plantclock.js inProgress`). It is kept out of peak and trend sentences. A
partly-elapsed hour drawn like a finished one reads as a drop at the right
edge of every chart.

Two corollaries: **an axis fits its data** (only a success rate keeps the
0–100 frame; a cancellation rate under 1% on a 0–100 axis is a line on the
floor), and **counts take integer ticks** (`precision: 0`; a cancel count with
0.5 on its axis is a count nobody can have). Durations plot in minutes and
print through `formatDuration` in the tooltip.

The same buckets carry a **per-day rate**: "N / day" divides by the plant days
of the window through today, the empty ones included, never by the days that
had an event. Two faults on one day of a week are 0.3 a day, not 2.0. The
Missions fault card is the case: `per_day` is filled at the source like any
other series and the card divides by its length.

### The numbers themselves

Six rules. The first is the one that gets broken.

**1 — Never print more precision than the measurement supports.**

The temptation is mechanical: the float holds `47.2831`, the formatter will
happily print it, and it *looks* more rigorous than `47`. It is the opposite.
**Precision is a claim about the measurement, not about the arithmetic.** Every
digit beyond what the input supports is an assertion the data cannot back, made
by a formatter that has no idea what the data is.

Work it through on the number Phase 6 actually ships. Cycle time (5.10) is a
difference between two `bin_uop_ledger` timestamps on a stream that is
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
the digit by querying `bin_uop_ledger` over the same window. Nobody rounded it,
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

