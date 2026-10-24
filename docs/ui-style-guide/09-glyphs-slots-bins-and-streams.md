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
| 22 px | the desktop positions table's Swaps column |

**Never below 16 px, never recoloured, never animated.** Below 16 the chevron
pairs merge and the glyph stops being countable, which is the one property it
has that a label does not.

The cell picture puts no glyph on its cards. It does not need one: each of its
modules is laid out like its mode's glyph, at the size of the cell (below).


### The cell picture: one module per position

The cell picture (`operator-station/operator-flow.js`, `renderFlowPicture`) is
the glyph's grammar drawn at the size of a cell. It is not a map: the
coordinates give the picture its **left and right** and its **front and
back** — which spot stands beside which, which stands nearest the press — and
never its distances; **a gap on the floor is never an empty column.** A cell
with a changeover is drawn by relation (the next section); every other cell —
and a changeover cell the map cannot place — is drawn as modules: **a module
is one position and everything its claim names, laid out like that mode's
glyph.** The station's flow panel, the station composer and the desktop
Flows tab all draw the same function.

**One fixed template per swap mode.** The template says where a move sits;
the model's legs say whether the move exists and which robot makes it. No
template names a robot.

| Mode | Module |
|---|---|
| 1‑Robot Swap | position card over a row of two staging slots, inbound left and outbound right; the new bin rises from the inbound slot and the old bin parks down into the outbound slot, both Robot 1. No move to the dock beside the park: the out-stub by the card's side is only for an old bin that leaves with no park leg drawn |
| 2‑Robot Swap | position card over its inbound staging slot; the old bin leaves by the side (Robot 2's out-stub). **No outbound slot**, even when the claim names one: Robot 2 takes the old bin straight to the dock, and a slot would draw a trip this choreography never makes |
| 2‑Robot Index | position card over its on-deck card, one module, so no layout can split the pair; next bin in behind the deck, index up into the press, old bin out by the side. An inbound staging the claim names is a slot under the on-deck card (under the press card when there is no deck), captioned `Inbound staging`, with **no chevron**: the running choreography has no leg to it, and its tooltip says it is used at changeover. A partner that runs a claim of its own keeps its own module, and the press draws without a deck |
| Sequential A/B | A and B side by side in one double-width module under one line-side bar |
| no claim | the position card, dashed, its bar dimmed. The bar of a position not in the flow is the only thing in the picture that is ever dimmed |

**A staging place is the module's slot, whether or not it is a position.** At
a press a swap usually stages at the back position behind it; that position is
drawn as the slot of every module whose claim names it — marked `shared` when
two do — and, with no claim of its own, gets no card of its own, so its name
appears once per module that uses it and never as a dashed "not in the flow"
card. A position that runs a claim of its own keeps its own module and is also
drawn as the slot of the module that stages there. **Every leg the model
gives is drawn once, between the two boxes it names**; each move's group
carries `data-from` and `data-to` naming them (`dock` for the dock band). A
legacy mode word draws a plain card, not a crash.

**Sizes are fixed numbers, never measured text.** The numbers table at the top
of `operator-flow.js` is the whole of the geometry: position card 188 × 92,
staging slot 116 × 44, on-deck card 188 × 56, module 244 wide (a sequential
pair 508), 20 between modules, 28 between rows, 20 of margin, a 74 px dock
band. Text is fitted by a character budget for its size, so no box is placed
or sized by measuring a string, and nothing can overlap or leave its card at
any width. **A node name that does not fit is cut in the middle**, both ends
kept — `…IN_01` and `…OUT_1` still read apart — and the full name is the
card's `<title>`; any other line is cut at its end. Node names are shown as
the plant wrote them: no `PLN_` → `P` shortening anywhere.

**Order, rows, and the wrap.** The line side comes first, then the back
(`modulesOf`). The line side is every module a claim covers, in whatever row
it stands — a running index press in the back row is still line-side — and
every unclaimed front-row card. After it come the back-row positions no claim
covers and no module draws: a back position no claim names stands alone at
the end, while an on-deck card, the B of an A/B pair, or a staging position
drawn as a slot stays inside its module, so a pair is never split. Within each group modules run left to right
by the world X of their lead position when every position has coordinates,
and by `sequence` when any does not, the node name breaking a tie. They wrap at the frame's width; a row is as tall as its tallest
module and is centred. **Front and back come from the coordinates, never from
the module grid** (`worldRows`): a module row is a fact about the screen — the
same cell is one row at 1280 and three at 640 — while the press's rows are its
positions' world Y, clustered within half a metre, line side highest. A cell
without full geometry falls back to each position's kind. A cell drawn by
relation has no row words: its grid's rows are the press's, but which of them
faces the aisle is not in x and y. Every "front" or "back" beside the picture
comes from `pictureRows` and nowhere else.

**The picture reports its size; the host takes it or scrolls, never
scales.** The layout consumes the frame's width and nothing else, and writes
its own height back through `opts.height` and its own width — the frame's, or
a wider relation grid's — through `opts.width`. Every host sets the svg's
width, height and viewBox to that reported size, one to one, so a card is
always 188 px on screen: the station's 1280 × 560 panel scrolls a taller
picture, and a wider one sideways. The desktop Flows tab's `.pd-pic` frame (`drawPicture` in
`processes-desktop.js`) takes the picture's full height and never shrinks or
scrolls vertically; the main column is the one vertical scroll at every window
height, so the positions table can never cut the picture in half. The frame
scrolls sideways only when one module is wider than it. Scaling the drawing to
fit is what once put 9 px titles on the desktop.

**Colour is the robot here too.** The rule above holds at this size: Robot 1's
teal and Robot 2's indigo go on the chevrons, the in/out corner marks, the
dock notes and the words "Robot 1" / "Robot 2" wherever a card line or a note
says them — a coloured `tspan` inside the sentence, never a second text
element. Which robot makes a move is read from the model (its legs and each
cell's own dock robots), so a flow mixing a 1‑robot cell with a 2‑robot one
clears each with its own robot. **A dock side both robots use stays neutral**:
there is no one robot's colour for its mark, so the mark is muted and the
note's words carry each robot in its own hue. **The legend shows the chevron,
not a line, and lists only the robots in the flow** — a 1‑robot cell lists
Robot 1 alone, and a cell with no flow lists nobody.

**Words: the card carries the moves; a move carries no label.** Each position
card reads its move sentences from the model's one card-line table
(`CARDLINE` in `composer-model.js`; a 1‑Robot Swap reads "Robot 1 moves in ·
Robot 1 clears old"). Chevrons are unlabelled, and a staging name appears
once, on its slot, with the field it is as the caption (`Inbound staging`,
`Outbound staging`). A **line-side bar** sits above every position card: the
glyph's bar, the station colour at width 3 with round caps, dimmed only over
a position not in the flow. It reads `--os-station` with `--station` as the
fallback, as the glyphs do: the station maps that to `--station`, the desktop
to `--text-strong`, so the bar shows in the light theme too. The
**unused-staging line** — dashed chips for routing-set lanes this flow does
not use — is drawn on the desktop only.

**The draft draws the picture.** Where a flow is being edited (the desktop
Flows tab, the station composer) the slots, captions and moves all come from
the draft — the same model state the legs come from — so a style that is not
the running one draws its own staging, never the running style's. The
read-only station panel builds the same model from the running claims and
draws through the same functions; this file computes no sentence of its own.

**Shared staging is drawn shared, and it is a warning, never a refusal.** A
staging place two positions in one flow name is drawn in every module that
names it, each slot marked `shared` on its caption row. The name keeps the
slot's full width; the caption shrinks to the field's first word (`Inbound`,
`Outbound`), drawn whole in the room between its own x and the tag, which
sits at a fixed x — the slot's place under the card already says it is
staging. One physical spot
legitimately serves several positions, so the composer raises a warning
finding — "staging shared by several positions" — that is shown beside the
picture and never blocks the save, the same answer the server's claim
validation gives. A 2‑Robot Swap's outbound staging does not count: its
module draws no slot for it.

**The dock and the key routes.** The dock strip runs under the modules: an IN
half and an OUT half, each with its source or destination, one note per robot
(`Robot 2 ← PLN_01, PLN_04`) and the positions it serves, every list cut to
its budget and **counted, not dropped** (`+N`). A key route hangs under the
slot or card its trip arrives at, in that trip's robot's colour: up to four
waypoints read bottom to top in driving order, the rest counted, `Robot N via`
at its foot.

**A click selects; the picture never edits.** On the desktop a card click
selects the **whole module** — the accent outline on its card(s) and every
slot it draws, nothing dimmed — and scrolls that position's table row into
view; the row and the card are one selection. A second click on the selected
card or row lets go, and so does Escape when no popover or sheet is open.
Every change is made in the table's chips. On the station composer a tap on a
card, or on a slot (which goes to the position it serves), opens that
position's panel. The accent is the selection and the robot hue is the
chevron, as in Design tokens › Robot identity hues.

### The cell picture by relation

A cell whose claims run a changeover (2‑Robot Index, 2‑Robot Swap, 1‑Robot
Swap, Sequential A/B — `relationScope`) is drawn **by relation** on every
surface that draws it: the station's flow panel, the HMI's recipe steps and
the desktop. Markets, loaders and unloaders keep the module picture, byte for
byte. The rule is `relationLayout` in `composer-model.js`, pure and pinned
under node:

- **One position, one cell.** The positions start in one cell and the plan is
  cut at its widest remaining gap, east–west or north–south (a north–south gap
  counts 1.25 times its size), only while two positions share a cell. Spots
  that line up stay one line however they drift; spots side by side are split
  however close. No column or row is ever empty, and nothing is to scale.
- **A deck rides its press.** An on-deck spot takes its press spot's row when
  the pairing is known (the caller's, held fixed while the operator moves
  places, else the claims'); otherwise a spot alone in its column takes the
  row it stands nearest, the one nearer the middle when it stands 30–70 % of
  the way between two.
- **A line is one row**, north end on the left.
- **Lanes that are not positions stand in a band under the grid**, each
  centred under the spots whose claims stage there; a lane the routing set
  offers and no claim uses goes at the end, faded.
- **Never shrink, never wrap.** Columns are one 188 card and a 22 gap — six fit
  the HMI's 1240 — and a wider grid makes the picture wider; the host scrolls
  it sideways, and the legend and the dock stay across the host's frame.
- **The fallback says why.** A position off the plant map, or two on one
  point, and the cell is drawn as modules in sequence order with one line in
  the margin opposite the legend: "Shown in order: PLN_008 is not on the plant
  map."

The cards keep the module card's content — the name, the model's lines with
each robot's words in its colour, the in/out marks, the part chip, findings
pill or "move here" — and gain a **chevron row**, so a card is 188 × 112 and an
on-deck or third card 188 × 76 (the module picture keeps 92 and 56). **Each
move is drawn on the card that makes it, centred under its words, and nothing
is drawn in the gaps**: the on-deck card carries Robot 1 up and Robot 2
toward its press spot, the press card one Robot 2 from the deck's side, a
lane Robot 1 up and its spot Robot 1 from the lane's side, a 1‑Robot Swap's
out-lane the old bin from its spot, a Sequential B card Robot 1 up. The
direction is the grid's (horizontal when the columns between the two cards
are at least the rows), the robot the model's. **What is not in play fades**
(`.faint`, opacity .3, dashed; a target never does), a position nothing names
has no caption (its kind is not its row), and a recipe group's
**L/R tag** sits over its cards' top-left corner, filled with `--group-1` /
`--group-2` — the only place those tokens are drawn.
