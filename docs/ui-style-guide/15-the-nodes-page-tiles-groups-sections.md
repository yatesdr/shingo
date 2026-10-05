## The Nodes page — tiles, groups, sections

### Node tile

**One node tile is one size everywhere**: 6.5rem x 2.75rem, set on `.node-tile`
and nowhere else. The ungrouped grid, a group's nodes, a lane, a station box and
the synthetic `_TRANSIT` tile are all wrapping rows of that fixed box. A
container never sizes the tile: a stretching grid made ungrouped tiles wide and
a flex row made group tiles as wide as their text, so the same node was two
shapes a scroll apart. A long name truncates with an ellipsis and the full name
is the tile's `title`; it never wraps the tile taller. The one exception is
tap-to-assign, where the height gives so the reason fits on the tile (see Armed
slot above). Width always holds.

- **The name's ink is measured on its fill**, not white by habit. Empty tiles use
  `--chip-ink-muted` on `--bg-dark` (`--text-muted` there is 4.2:1 in light).
  Solid fills name their own ink: `--on-success-fill` (white in light, near-black
  in dark, where white on `--success` is 2.5:1) and `--on-staged-fill` (near-black
  in both; white on the staged teal is 2.5:1).
- **A hatched tile (empty bin, maintenance) puts the name on a plate** of
  `--elev-surface` in `--text`. No single ink clears 4.5:1 on both stripes; the
  plate makes the backdrop constant, the same fix as the robot tile's chip.
  `node_tile_contrast_test.go` pins every state.
- Hover lifts the border and shadow. It never scales the tile (Motion means
  motion).

### Sections and groups

- **Page sections use `.section-head h2`** with the count beside the title in
  muted type: *Node groups 4*, *Ungrouped nodes 42*, *Map infrastructure*. One
  treatment; no small bold labels, no uppercase rules.
- **Every block is the same card**: `--surface`, 1px `--border`, 0.5rem radius,
  `--sp-2` under it. That covers a group, the ungrouped grid (`.node-panel`) and
  a map accordion. Group boxes are not indigo-bordered: indigo is chrome, and a
  page of indigo outlines is structure shouting.
- **Inside a card, a sub-section is the uppercase caption** (`--font-xs`, bold,
  muted), the station boxes' stage title: a lane name, *Not in a lane*. The
  *Not in a lane* caption appears only when the group has lanes. Without lanes
  every node is outside one and the header already counts them.
- **A group header counts in plant words and names only what is there**:
  *11 nodes*, *4 lanes · 12 slots · 2 nodes*. Never *0 lanes, 0 slots*.
  `groupSummary` in `nodes-supermarket.js` is the one place that builds it.
- **An empty group says so in its body** (`.node-group-empty`), not a header over
  nothing: *No nodes in this group yet.* Signed in, the sentence sits inside the
  drop zone and says how to fill it: *Drag a node here, or add a lane.*
- A lane is a rail open at its mouth (the left edge), drawn in `--sub-3`.

