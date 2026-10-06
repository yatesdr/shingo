## Desktop composer patterns (U9)

The Edge Processes page is the engineer's half of the flow composer, and it
introduced seven patterns worth reusing; U10's Presets tab added seven more at
the end of the section. Reference shots:
`hmi-flow-composer-design-2026-09-02/desktop/reference/P0,D1–D5.png` at
1440×900. Each is one paragraph because each is one idea.

**One editor per fact.** Each fact about a process is edited in exactly one
place, and every other screen that shows it shows it read-only:

| Fact | Its one editor |
|---|---|
| name, description, group, counter, curtain, changeover arm, HMI gate, quality hold, routing set | **Settings** |
| the screens, and the positions each one works | **Operator screens › Edit** |
| flows, and their Advanced fields | **Flows** |
| the part set | **the part picker**, inline wherever a part is chosen |
| presets | **Presets** |
| all of it, once, when the process is made | **Add process** |

The process list finds a process and opens it; it has no Edit sheet of its
own. A second editor for a fact is two forms that can disagree about it, and
the one the engineer did not open is the one holding the stale value. When a
fact gains a better editor, the old one is deleted with its routes rather than
kept in step. **"+ Add a position" on the Flows tab is not a second positions
editor**: it turns a position on in the draft, nothing is written until the
flow is saved, and which positions the process works is still Operator
screens' to say. One phrase for it everywhere: "Add a position".

**The app-bar gate sentence.** Plain muted text on the right of the app bar
(`.pd-gate`: no border, no tone, no pill) states a setting that is decided
elsewhere — `Operators may change flows` / `Operators run flows as set up`.
It is not a switch on purpose: the same decision drawn twice is a decision
made in two places, and the second one is always the one somebody changes by
accident. It is not a pill either: a pill's shape claims "status chip" for a
thing that never changes on this screen, and the words already carry the
state. **It carries no link to where
the setting is decided** when that place is already a tab on the same bar: a
`Settings ›` link beside the Settings tab is the tab's own door drawn twice.
Use this wherever a screen needs to show a setting it is not the owner of.
Reference: `D1-flows-selected.png` (drawn when the gate was still a pill with
a link).

**The grouped list table.** One table per group with an 11 px letter-spaced
group label and a count above it, the ungrouped bucket last, and a whole row
as the click target. Columns are facts, never controls — no inline editing on
a list — because a list is for finding the thing, and a row that edits is a row
you cannot click. Reference: `P0-processes.png`.

**The positions table with chip pickers.** A row per position, and every cell
that can change is a chip that opens a popover list — never a native `<select>`
(see "Never use native dialogs"; a select's dropdown is the browser drawing its
own chrome over ours, and it cannot carry a glyph). Every pick applies
immediately to the draft and redraws the picture; nothing is written until the
page's one save button. Reference: `D1-flows-selected.png`.

**One vertical scroll: the main column.** The picture above the table takes
its full height and never scrolls inside itself, and the table draws every
row, so the main column is the only thing that scrolls up and down. The table
scrolls sideways in its own box below its 900 px floor; because a box that
scrolls sideways would trap a sticky header, its headings are a strip of their
own (`#pd-poshead`, the same colgroup) sticky against the column and carried
sideways with the rows. The bar sticks to the foot of the column, so Save is
always on screen.

**The per-row `N set` count.** A row that opens a sheet of settings says how
many of them carry a value, in the accent, and reads `defaults` when none do.
The count is computed from the SAME predicate the server uses for "has a
value", so a badge and a save cannot disagree; a second spelling of it would
put an indigo count on a row the server reads as untouched.

**The Advanced sheet's field shape.** The one sheet width over the scrim (see
Modals › Sheets); a section per group of fields with an 11 px label and its
own `N set`; each field a 230 px label column carrying a **one-line,
plain-language sub-label** and a value column beside it. The sub-label is not
optional — these are the fields nobody can name from memory, and the sheet
exists precisely because they had no home.
Footer states what the sheet does NOT do (`Nothing here changes the flow or the
picture`) before the two buttons. Reference: `D2-advanced.png`.

**The settings section with a live indicator in its title.** `Production
counter · counting · last tick 4 s ago`. The live state goes in the TITLE, not
beside the Enabled switch: "is this counting right now" is a fact about the
section and the switch is a setting, and putting a moving dot next to a control
makes people think the control did it. Reference: `D5-settings.png`.

**The danger section.** Last, its own rule, heading in `--status-alarm`, one
outlined destructive button, and a sub-label saying what else goes with it.
Not a red-filled button: a filled destructive control competes with the primary
action for the eye, and the primary action on that page is `Save settings`.

**Presets: two sections, and the second one is an offer (U10).** A screen that
names reusable shapes has two lists on it and they are different kinds of
thing. `PRESETS · N` is what somebody named — name, shape, how many parts use
it, whether any have drifted, who saved it and when. `FOUND IN YOUR FLOWS · N
shapes` is one row per distinct shape the press already runs that nobody has
named, with a suggested name and a `Name it…`; it applies nothing, ever, and
the section disappears when every shape has a name. The second one is why the
first is usable on arrival: an engineer meeting a press with ninety parts over
two shapes is offered two rows, not ninety, and that is the difference between
an offer and a list. A part already under a preset is not on offer even when it
has drifted away from it — drift is reported on the preset's own row and its
control is `Apply to parts…`, and offering it twice once produced a suggested
name identical to the preset it had drifted from. **Drift is in the warning
hue, never the accent**: on that page the accent means "the thing you are
working on" and the warning hue means "this needs you", and a drifted member is
the second. Expanding a row lists EVERY member, in step and drifted alike,
because "which parts run this shape" is the question the row was clicked to
answer. **The positions belong to the shape, not to the name**: a preset is
called whatever the engineer typed — the suggestion is the choreography's word
alone, `2‑robot index` — so every card and every row draws the shape's own
positions beside it (`PLN_01 / PLN_04 · used by 7 parts`), and two presets a
word apart are still told apart at a glance. **Naming a shape is not declaring
which parts run it**: `Save as preset…` on one part's flow stamps nobody and
the card reads `not used yet` until it is applied, while naming a shape the
press ALREADY runs stamps the parts that run it — that is the migration offer
recording a fact, not a rename carrying one. **An apply moves `updated_at` and
a provenance stamp does not**: the apply goes through the flow save and really
does change the flow, while recording where a shape came from changes nothing
an operator can see, and that column is the set-up card's "Flow saved 09‑12"
sentence. **A rename moves every version of a name and nothing else**, because
a name is not a property of a version and a lineage split across two names is
two presets that mean one shape. There is no PNG for this tab; it is built from
the grouped list table and the Advanced sheet's modal shell at two more widths.

**The previewed-diff apply modal (U10).** A modal that writes to many rows
shows what each write would change before it offers to save: a checklist with
**nothing pre-ticked**, and ticking a row previews it and draws the diff in the
words the table heads its columns with (`PLN_04 · outbound destination: Supermarket Area
→ Empty Tote Return`; a whole row arriving or leaving is one line worded as the
event). `Save to N parts` then saves the previews that are on screen, one at a
time, reporting per row, and a stale one re-previews and says so. Nothing
pre-ticked is the load-bearing half — a modal that opened with eight rows
ticked would be one click from eight writes nobody read — and "never a save
without its preview on screen" is what makes the count in the button a promise
rather than an estimate. A row the server will refuse stays tickable and shows
the server's refusal BY NAME; a client-side block is the page guessing at a
rule the server owns, and it hides the one sentence that says what happened.
**A write that leaves something unfinished still saves, and says what it left**:
an apply whose shape does not name a position releases whatever part was on it,
and the row adds `PIA09, PIA10 will need a position` to its diff rather than
refusing. What is blocked is the next irreversible step — here, starting a
changeover the robots could not serve — and it is blocked by a finding on the
flow, on every screen that shows one. **But a modal never offers a save the
server would refuse (F1)**: where the write is missing something the modal
itself can ask for, it asks — inline, on the row, one control per missing
answer — rather than drawing a warning over an enabled button. A row with a
question outstanding keeps the tick the engineer gave it and is not in the
button's count, and findings replace the order count on it, because a row that
cannot be saved must not also advertise what it would fire. The test of the
rule: if the only way to finish is to leave the screen you are on, the screen
has sent you away from the job it exists to do.

**The order sentence: `Robot · from → to` (U10).** Every row that
describes a move a robot will make has one shape, and it names BOTH ends.
`Robot 1 · Supermarket Empty Totes → PLN_02 · PIA27`. The robot's hue goes
on the NAME and nothing else on the line — the hue is an identity here, and a
whole line in it reads as a warning. The row ends with the PART when a part is
what is moving and stops when it is not: an evacuation takes out whatever is in
there, so `Robot 2 · PLN_03 → Supermarket Area` is the whole sentence.
**No row for a move nobody drives:**
a two-robot press index advances its own bin from the back position to the line
and no robot is dispatched for it, so under a heading that says ORDERS there is
no row for it. It stays on the picture, where the heading is the press. What
this replaced named one end per move (`brings the new bin to PLN_02`), and the
missing half was the one that says whether the right supermarket was picked.

**No word for what a bin is (U10, owner ruling R2).** Screens that describe
material name the PART and the PLACE, never the container. `totes`/`bins` was
resolved per style from the payload-to-bin-type catalog and appended in six
places — the strip card's second line, the dock notes, the index card line, the
order rows, the set-up card — and every one of them was a screen telling an
operator standing in front of the bin what the bin is. It also could not be
right on a card drawn per PRESS while the word is per STYLE, which is why it
was appended in the render layer rather than sent by the server: the same card
read `· totes` beside one part and `· bins` beside another. What survives is
the fixed heading (`OLD BINS TO` is a column, not a word about this bin) and
node names the plant gave its own lanes (`Supermarket Empty Totes`). The rule
generalizes: when a screen would name a category the person reading it can see,
it is spending a line on nothing.

**A field is called what the claim calls it; invent no second name (U10, owner
ruling F3).** Where the thing on screen has a name in the data, that name is the
label — `Inbound source`, `Outbound destination`, `Inbound staging`, `Outbound
staging`, `Key route`, `Paired position`. D1's positions table headed those
columns `New bins from`, `Old bins to`, `Robot drives via` and `Partner`; the
station's panel called them `New bin comes from`, `Old bin goes to`, `Stage the
new bin at`, `A/B partner`; the compare table and the apply modal then had to
pick one of the two sets. Every second name is a second thing to learn and a
thing to get wrong: an engineer who reads `outbound_destination` in a refusal
and `old bins to` in a column has to work out they are the same field, and a
screen that renames a field cannot be searched for the field. The exceptions are
words the FLOOR owns and uses consistently — `part` for `payload_code` is the
plant's word on every surface from the picker to the finding pill — and they are
exceptions by being consistent everywhere, not by being nicer in one place.

**No count of robots a preview cannot know (U10, owner ruling R1).** The
composer's bar read `Preview OK · 6 orders · 2 robots`, counting the ROLES the
choreography uses. A preview knows how many orders it would fire, because it
planned them; it does not know how many AMRs the fleet will send, because that
is the dispatcher's and the fleet's at the moment they run. Name roles where a
role is the subject — the picture's legend, `Robot 1 · …` on an order row — and
count only what the screen computed.

**The bar's problem line is the control.** When the composer bar is blocked,
its first finding that names a position is a link: on the desktop a click
selects that position (picture and table row, the same selection a card click
makes) and scrolls its row into view. On the station the same line's job is a
tap that opens that position's panel — the station's way of going to a
position, through the same `data-tap="pos"` door a card tap uses. A finding with no position is
not clickable — there is nothing to take you to. There is no one-tap fix
button beside it: every action such a button could take guesses at something
the engineer has not said (a part, a free back position, removal over
placement), and parts are never prefilled. Each field keeps its own picker,
and taking an unplaced part off the flow is a control on the unplaced part's
own chip.

**The scroll fade (U10).** A panel that scrolls its own content carries a 12 px
fade at the scrolling edge, over the panel's OWN surface token, present only
while there is more to scroll. A label cut in half by a hard edge reads as a
rendering bug; a label fading under one reads as "there is more below". Use a
sticky pseudo-element painting the panel's colour, **not** `mask-image` — a
mask fades the element, so the panel's own border and corners dissolve with the
content and the panel looks like it is failing to paint. It needs no z-index:
being the last positioned child is what puts it over its siblings, and the
page's z-scale is for page layers. The fade comes OFF at the end of the scroll;
one that never leaves is a panel that always looks unfinished.

### The map is a screen, not a widget

D3 puts the routing set on the plant. Two rules came out of it that generalize:
the projection is **always** `shared/scene-geom.js` (Core's map, the station's
cell picture and this screen project the same plant with the same function, so
a route weighed on one surface is the route drawn on the others), and the
orientation decision is made over the **whole plant**, never over the region
being framed — a region that happens to be tall must not flip the map under the
person reading it. Fit margins are in screen pixels, not as a fraction of the
region: a label is drawn 11 px outside its mark whatever the plant measures.
Reference: `D3-routing-set.png`.

