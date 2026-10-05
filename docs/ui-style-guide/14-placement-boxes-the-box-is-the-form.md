## Placement boxes — the box is the form

When most of what configures a thing is **which nodes and groups it uses**, draw
it as a box on the page that holds the nodes, with one slot per place, instead of
a modal of name inputs. A typed name accepts any string and says nothing about
the node; the grid under the box already shows every node, and a slot can say
what it still needs. The Nodes page's loader stations are the worked example
(`shingo-core/www/static/pages/loaders.js`: `stationSlots`, `assignReason`,
`settingsHtml`). The create card then asks only what the thing IS (see Choice
buttons above) and hands off to the box.

### Slots

- One labelled row per place, **noun first, in plant words**, under eight words:
  *Windows*, *Fulls come from*, *Empties go to*, *Carts wait at*. No "inbound",
  "outbound", "layout" or "directive".
- A required slot that is empty is **red and says what it needs**: *Needs a
  place*, *Needs a window*, *Needs a part*, with one line of how (*tap, then tap
  a group or node*). Red is border and `--danger` ink on `--surface`, not a pink
  wash: the ink clears 4.5:1 on the surface and falls under it on any tint.
- A value the system derives is **shown, not asked** — a read-only row, never an
  input the save then overwrites.
- The slots are data first. `stationSlots(item)` returns `{key, label, required,
  empty}` per slot and the HTML is drawn from that, so what the box asks for is
  unit-testable without a DOM.

### The not-running status

The box header counts the empty required slots: **Not running yet — N slots
need a place** (singular for one), as a `.badge-warn`. When nothing is missing
the header says nothing. It does not say "Running": the page knows the
configuration is complete, not that the Edge accepted it or that robots are
moving.

### Armed slot and tap-to-assign

Drag alone does not work on a long page on a touch screen. So:

- **Tapping a slot arms it** (`.is-armed`, accent outline). One slot at a time;
  tapping it again, **Done**, or **Esc** disarms.
- While armed, a sticky bar over the grid says what is being set and holds a
  **Find** box that filters tiles by name.
- **Every tile shows whether it can go there, and why not, on the tile** — *in
  AMR Supermarket*, *window of PRESS4-UNLOAD*, *has a bin on it*. On
  the tile, not in a `title`: touch has no hover. A group on a window slot reads
  *a group can’t be a window*, the same words a refused drop toasts. Tiles that can take it are
  outlined (`.assign-lit`), the rest dimmed (`.assign-dim`), the current value
  marked (`.assign-current`).
- **Tapping a lit tile assigns it with the same API call a drop makes.** The
  interception is a capture-phase click listener, so the tile's own action (open
  the node) does not also fire.
- One pure rule, `assignReason(slot, target, ctx) → {ok, current, note}`, decides
  both what the grid dims and whether a drop is taken, so tap and drag cannot
  disagree.
- A slot that takes a set (windows) stays armed after an assignment; a slot that
  takes one value disarms.

### Drag sources

Each drag source sets **only its own MIME type**, so a drop target that does not
recognise it does nothing instead of misreading it:

| Source | Type | Accepted by |
|---|---|---|
| Node tile | `text/plain` (node id) | windows, place slots, the grid and lanes (reparent) |
| Station window tile | `application/x-loader-member` | its own box (reorder, re-kind) |
| Group header | `application/x-node-group` (group name) | every place slot — *… come from*, *… go to*, *Carts wait at* — through the same assign call as a tap. Refused on a windows box with a toast, *a group can’t be a window*; ignored by the grid and lanes |

Where a refusal needs a reason (a group on a windows box), the target still
takes the `dragover` so the drop fires, and the drop says why. Where nothing
useful can be said (a group over the grid), `dragover` returns without
`preventDefault`, so the browser shows no drop cursor, and `drop` ignores the
type as a second line.

### Settings that save as ticked

Everything with a sensible default sits behind **one Settings link** on the box
and is drawn in place of the slots (**Close settings** returns). Each control
commits on `change`, and the footer says so: *Changes save as you tick them.*

- The update body is **the stored row with that one field changed**
  (`settingUpdate(loader, field, value)`), because the update endpoint is a
  full-row write: a body built from what the panel shows would flatten every
  field it does not show.
- Which rows appear is `formShape(state)`, the same function the create card
  uses. A row that does not apply is absent.
- A change that loses data (a layout switch that drops members) asks first with
  `uiConfirm`; so does the destructive action at the foot (**Delete station**).

