## Visual principles

Three named principles generalize the look people like on the map — the
best-looking surface in the system — to every other surface. They sit above the
component sections and inform tokens, tables, tiles, meters, and animation
everywhere. Where a component rule and a principle disagree, the principle is
the intent; fix the component rule.

Three more came out of the flow-composer stream (U8/U9/U10). They are
narrower — they are about what a screen is FOR — but they settled a dozen
arguments each, so they are written down beside the three:

**The gate removes a button, not a screen.** When a permission is off, the
person still sees everything; what they lose is the one control that would
change it. The flow composer's gate is the worked example: gated off, the
operator still opens the picker, still reads the picture, still sees which
parts a style runs and where its bins come from — the "Change how it flows"
button is simply not there, and the screen says so in a sentence. The
alternative, hiding the screen, teaches people the system has a second half
they are not allowed to understand, and that is how a floor stops trusting a
tool. A permission is about acting, not about knowing.

**The desktop verb is Save, the HMI verb is Start.** The same picture, drawn
from the same model, over the same data, ends in two different words because
the surfaces answer different questions. An engineer says "this is how this
part should run" and saves it; an operator says "run this now" and starts it.
Nothing on the desktop dispatches a robot, and nothing on the HMI edits a part
the press is not about to run. When a screen grows a verb from the other side,
that is the signal it has taken on the other side's job.

**A preset is a shape, never a part.** A saved thing is worth reusing exactly
as far as it is the same for everybody who reuses it. A flow's SHAPE — which
positions, how they swap, where bins come from and go — is the same for every
part that runs it; the part is what differs, which is why it is the one field a
preset does not carry. Applying a preset therefore replaces the shape and
carries the parts across, and a position the new shape drops releases its part
rather than keeping it somewhere nothing can see. The test of the line is what
a compare reports: a preset that carried the part would report every member as
drifted the moment a second part used it, which is the whole reason to have
presets. Generalizes past flows — whenever a screen offers to save "this
setup" for reuse, the thing to save is what is common and the thing to leave
out is what identifies this one.

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

