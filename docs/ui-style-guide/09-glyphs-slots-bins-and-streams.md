## Glyphs: slots, bins and streams

**A glyph is a picture of the press, not an icon.** Icons are monochrome
affordances that take `currentColor` and live in the Lucide sprite (see Icons
above). A swap-mode glyph is the opposite of all three: it carries colour **by
rule**, it depicts a specific machine doing a specific thing, and it is never
in the sprite. If you are tempted to add one to `shared/icons.svg`, the thing
you have is not a glyph.

Source: `operator-station/composer-glyphs.js` — `glyph(mode, size, {rest})`
returns an inline SVG string and is the **only** renderer. The sprite form
(`composer-glyphs.svg`) and the classes (`composer-glyphs.css`) ship with it.
Locked 2026-09-10; the port files are the geometry's source of truth.

### The four parts

A glyph is a 28×28 picture drawn from four parts and nothing else:

- **Slot** — a position. A thin rounded outline (stroke 0.75), always in the
  same place for that press.
- **Bin** — solid. **White is the new bin, grey is the old one.** A bin sits in
  its slot or rides its stream, and **the old bin is never drawn twice**.
- **Stream** — one robot move is **one pair** of slender chevrons (stroke 0.95)
  in that robot's colour, centred in the gap it crosses. Count the pairs and you
  have counted the moves; count the colours and you have counted the robots.
  Orthogonal wherever the layout allows.
- **Bar** — the line side of the press (stroke 1.6, round caps), always at the
  top.

**One grid, one gap.** Slot 6.6, bin 4, and **one gap of 6 between any two
things** — slot to slot, slot to bin, bin to chevrons. Nothing touches an
outline. The 1-robot triangle is equilateral so its three gaps match.

### Colour is the robot, not the direction

`--robot-1` teal for Robot 1, `--robot-2` indigo for Robot 2, `--station` white
for slots, the bar and the new bin, `--sub-5` for the old bin. A stream does
not change colour because it points a different way; it changes colour because
a different robot is driving it.

In an unselected control (`rest`) the streams drop to 45% and the structure
falls back to the substrate ramp: **the chosen choreography is the one that
glows**, which is "structure recedes, state glows" applied inside a 28-pixel
square.

### The four, as drawn

| mode | picture |
|---|---|
| `two_robot_press_index` | press slot over an on-deck slot; a white tote on deck and the next white one riding in from the left on Robot 1; Robot 2 indexes up into the press and carries the grey one out to the right |
| `two_robot` | one press slot; white bin straight up into it on Robot 1, grey bin straight out to the right on Robot 2 |
| `single_robot` | three slots on an equilateral triangle — press (grey bin in it), park (white bin in it), empty park — and three Robot 1 legs round it |
| `sequential` | A holds the bin the line pulls from; B is where Robot 1 works — new bin up into B from below, empty out of B to the right |

There is a fifth symbol, `#swap-empty`: the slot and the bar with nothing in
them. It is what a cell with no choreography draws, and it is why a retired
mode does not need a hidden option anywhere — an unrecognised mode draws the
empty glyph and its name reads through beside it.

### Sizes, and where they appear

| Size | Where |
|---|---|
| 32 px | preset cards, picker rows |
| 26 px | the HMI's HOW IT SWAPS segmented control (`rest` on the unselected three) |
| 24 px | position cards on the cell picture, top-left, never overlapping the in/out corner glyphs |
| 22 px | the desktop positions table's Swaps column |

**Never below 16 px, never recoloured, never animated.** Below 16 the chevron
pairs merge and the glyph stops being countable, which is the one property it
has that a label does not.

