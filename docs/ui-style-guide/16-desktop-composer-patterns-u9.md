## Desktop composer patterns

Verified against: 22caad6b

The Edge Processes page (`shingo-edge/www/static/js/pages/processes-desktop.js`)
is the engineer's half of the flow composer: a process list, and per process
two tabs, **Styles** and **Settings**, with a **Setup** tab before them until
the line is set up. There is no Operator screens tab and no Recipes tab: the
screen is a line under **Settings › Positions and routing**,
and the recipes sit under **Settings › Operator setup** (a `#tab=presets` or
`#tab=recipes` link lands there). A loader's or unloader's process — Core runs
its board, the process row's `core_boards` — has Settings alone: Line, Screens
and positions, a **Loader board** pointer to Core, and Danger; no Setup, no
Styles, no flow picture. Nothing on it is a pick: a position there that no
loader holds would make the process a line again. Positions on no screen are
chips with **Put them on <screen>**. One line per loader says which Edge Core
names for it; the Edge keeps the windows itself, and what it could not take is
one line each, **Core added <window>** with **Put it on <screen>** and **Core
removed <window>** with **Take it off <screen>**, drafts on the one Save. These are the patterns on it worth reusing. Each is
one paragraph because each is one idea.

**One editor per fact.** Each fact about a process is edited in one place, and
every other screen that shows it shows it read-only:

| Fact | Its editor |
|---|---|
| name, description, group | **Settings › Line** |
| production counter | **Settings › Production counter** |
| the positions, the routing set and the screen that holds the positions | **Settings › Positions and routing**, the set pick (and **Add process** and its Change…, the same pick) |
| the parts the process runs | **Settings › Parts** |
| a position counted somewhere other than the production counter | **Settings › Counted somewhere else** |
| where the CATID is read from, and what a confirmed change of it does (auto-arm) | **Settings › Part identity (CATID)** |
| a position's light curtain | **Settings › Light curtain** |
| HMI setup, recipe names, and which parts a recipe is applied to | **Settings › Operator setup** |
| whether the station shows the auto-reorder switch | **Settings › Auto-reorder** |
| quality hold | **Settings › Quality hold** |
| a style's flow, and what a position sets for that style | **Styles** (the positions table; each row's **Advanced** sheet) |
| a recipe's shape, and what a recipe group sets | **Styles**, from the rail (open a recipe, or **+ New recipe**) |
| a process, its screen, its positions, routing set and parts, when it is made | **Add process**, which lands on **Setup**; Setup's Change… is the same sheet |

**The set pick is one piece in two places** (`shared/set-pick.js`): four boxes,
Positions, Inbound, Staging and Outbound, each in its role's colour (the
`--role-*` tokens, never the robot hues) with its picks as chips, over one
always-open list, one row a line with nothing to open before a tick (a group's
nodes wait under its **show N**, as everywhere). Beside the name filter,
**All · Groups only · Free only**, then the name prefixes (PLN, SMN …), each a
filter: Groups only is the group rows, already listed; Free only is what the
open box may take. A node already in another box carries that box's name as a
tag in its colour, and is greyed with the reason where the open box may not
take it; nodes other processes hold wait under one line at the foot. What holds
for many rows of the open box is said once above the list (`hint`), not on each
row. The open box, its chips and the list's frame are neutral: only a box still
needed is outlined in its role's colour. **The pick has one scroll, what holds
it**: the list has no height of its own, the sheet's body (Add process) or the
Settings column scrolls it, and the boxes and the **Picking …** line stay in
view at the top of that scroll while the rows go under them. The Add process
sheet (and its Change…, the same sheet filled in) and Settings › Positions and
routing draw it; both save through the same
doors (`savePositions`, `saveRouting`), so it is one editor drawn twice, not two
forms. Positions are saved per screen through `PUT …/claimed-nodes` (a line's
positions on no screen through `PUT …/positions`), each from a list just read.
**A line has one screen**, made with the process; a new position goes on it.
A line with none offers **+ Add a screen**, which takes the positions on no
screen; one with two from before shows each with its positions, and nothing
there moves a position between them. **"+ Add a position" on the Styles tab is not a positions editor
either**: it turns a position on in the draft, nothing is written until the
flow is saved, and which positions the process works is still Settings' to
say. One phrase for it everywhere: "Add a position".

**Setup: a short path to a first line, skippable.** A new process opens on
Setup: three steps, each done where it is shown — **Nodes and part** (the Add
process sheet answered it, parts first; Change… opens it again), **First
style** (one position at a time: the positions as a checklist, done ones ticked
green; the open position's part, consume or produce, swap (it starts as the
position before it) and its picks from the routing set, only what its swap
uses; **Done · next position** or **Not in this style**; **+ Add a position**
and **Take … off this line** through Settings' positions write; Save style once
every position is done or left free, by the Styles tab's own save), and **Run
it** (the screen, opened, with its three presses written out). No step lands on
Settings. Beside the first and last, the line so far and what can wait for
Settings; First style's checklist is its line so far. Its loud colours are the
open position, green for done, and the dashed outline of a pick box still
needed; a role is a small dot. It is finished when the
line **can** run and its screen **has been opened** (the screen's
`last_seen_at`), not at a first changeover: some lines have none. **Skip setup,
use Settings** hides it in this browser; **Show setup** brings it back while the
process cannot run.

**The app-bar gate sentence.** Plain muted text on the right of the app bar
(`.pd-gate`: no border, no tone, no pill) states a setting that is decided
elsewhere — `HMI setup on: operators set up styles` / `HMI setup off: operators
pick from the styles`. It is not a switch on purpose: the same decision drawn
twice is a decision made in two places, and the second one is always the one
somebody changes by accident. It is not a pill either: a pill's shape claims
"status chip" for a thing that never changes on this screen, and the words
already carry the state. **It carries no link to where the setting is decided**
when that place is already a tab on the same bar: a `Settings ›` link beside the
Settings tab is the tab's own door drawn twice. Use this wherever a screen needs
to show a setting it is not the owner of.

**The grouped list table.** One table per group with an 11 px letter-spaced
group label and a count above it, the ungrouped bucket last, and a whole row as
the click target. Columns are facts, never controls — no inline editing on a
list — because a list is for finding the thing, and a row that edits is a row
you cannot click. A row's only buttons are its doors (*Styles*, *Settings*).

**The positions table with chip pickers.** A row per position, and every cell
that can change is a chip that opens a popover list — never a native `<select>`
(a select's dropdown is the browser drawing its own chrome over ours, and it
cannot carry a glyph). Every pick applies immediately to the draft and redraws
the picture; nothing is written until the page's one save button. **Every
list of unknown length on the page is the shared picker** ([Picker](34-picker.md):
`shared/picker.js` and its node and part adapters), node groups first — a
chip's list, a routing role, a screen's positions, the part set, + Add a
setting, a group, a PLC, a counter — so a list on this page reads, searches
and greys a row the way every other does. The role and swap-mode chips, two to
four fixed answers each, open a short menu: the table is not a settings page
([Settings pages](33-settings-pages.md) rule 6).

**Free positions fold.** The Styles picture draws the positions the style uses
and the nodes their rows name (partners, staging), then one line, `N more
positions on this process are free · show them`; the table's POSITIONS line
folds its free names the same way, to `N free` beside + Add a position until
*show them*. A flow that uses nothing yet draws every position: there is
nothing to fold to.

**One vertical scroll: the main column.** The picture above the table takes
its full height and never scrolls inside itself, and the table draws every
row, so the main column is the only thing that scrolls up and down. The table
scrolls sideways in its own box below its 900 px floor; because a box that
scrolls sideways would trap a sticky header, its headings are a strip of their
own (`#pd-poshead`, the same colgroup) sticky against the column and carried
sideways with the rows. The bar sticks to the foot of the column, so Save is
always on screen.

**The row's Advanced.** Every positions-table row ends with one control,
**Advanced**, or **Advanced · N set** where its position sets something for
this style (the count in the accent), opening that position's sheet: the
shared sheet shell, every setting flowspec offers the position under its
heading, each with its value control, edits the style's draft (no Cancel;
Done closes it). It never asks for the position again, and a setting that
takes a part starts from the row's part. The count is one per setting set,
and "is set" is the SAME predicate
the server uses for "has a value" (Robot 1 and Fresh bin are defaults, not
settings), so a chip and a save cannot disagree; a second spelling of it would
put an indigo count on a row the server reads as untouched. There is no gear
and no Advanced sheet: it drew every field at its default behind each row, so
one setting on one position was a sheet of a dozen "off".

**The exception list.** A setting that is the same everywhere except where it
differs is said as the rule once (`2 of 23 positions`, `all 23 at the
production counter`) and a row only where a place differs, each with its
controls and a ×, then `+ Add …` — never one row per place saying the default.
It is one shared piece, `exceptionList` in `shared/exceptions.js` (markup only;
the page's delegated handler acts on its × and its add), with two uses:
**Settings › Light curtain** and **Settings › Counted somewhere else** (each
the card's whole subject, so the list is `bare`: no name column of its own, `+
Add a position` at the card's right edge), and a recipe group's own list on its
card (**Set for this group**, one row named for the group). A style's position
is not a list: its settings are its row's Advanced sheet (above). A setting's label is the claim's own word (`Next carrier`, `Tool
change`) with one muted sub-line only where it changes what a person would
pick. What a list offers is flowspec's decision, as the positions table's
columns are: a setting this choreography forbids or never reads is not
offered. **A setting's second value is drawn only beside its first**: the
carry-over (`if the part stays: Fresh bin · Keep it · Out and back`) is a Tool
change's, drawn beside its marks and nowhere else, and a recipe group, which
marks nothing, carries none. **Next carrier is per style** — it can differ by
part — and a group's is one value for every part made from it. **Auto reorder
is on the row**, `off` or `on at <point>`, so a position does not list it
twice; a recipe group, which has no row, lists it as one row reading the same.

**The settings section with a live indicator in its title.** `Production
counter · counting` (or `not wired`). The live state goes in the TITLE, not
beside the Enabled switch: "is this counting right now" is a fact about the
section and the switch is a setting, and putting a moving dot next to a control
makes people think the control did it.

**The danger section.** Last, its own rule, heading in `--chip-ink-below`, one
outlined destructive button (`Delete <process>…`), and a sub-label saying what
else goes with it. Not a red-filled button: a filled destructive control
competes with the primary action for the eye, and the primary action on that
page is the foot bar's `Save`.

**Settings is one settings page.** The tab is `settingsPage`
(`shared/settings.js`; see [Settings pages](33-settings-pages.md)): every
section open, grouped by what it is about, each group under a small heading on
the page and in the list down the left — **The line** (Line · Parts ·
Positions and routing), **Counting and part identity** (Production counter ·
Part identity (CATID), holding *When the part changes* · Counted somewhere
else), **At the station** (Operator setup · Auto-reorder · Quality hold ·
Light curtain) and **Danger** — with Health first while it has items, a dot in
the list on a section that needs attention, each a card, the page's full
width, and one Save in the foot bar. Two half cards share a row and its
height; a card holding a table or a list takes the full width; every card's
label column is one width, so values and switches start on one line down the
page. Nothing on a card is cut: a segment of long answers stacks (`.pd-seg.col`)
rather than clipping, and a recipe's name and spots wrap. Nothing on it is saved as it
is picked: a pick, a switch and a typed value are the draft until Save, the
line's auto-reorder switch included (it is still recorded with who, where and
when). An on/off setting is a switch with what it means beside it.

**Recipes: two sections, and the second one is an offer.** A screen that names
reusable shapes has two lists on it and they are different kinds of thing. On
this page they sit under **Settings › Operator setup**, beside the switch they
serve. **Recipes** is what somebody named — name, shape, how many parts use it,
whether any have drifted, when it was saved — each name's newest version, the
older ones behind `show`. **Found in your flows** is one row per distinct shape
the process already runs that nobody has named, with a suggested name and a
`Name it…`, three and then the rest on ask; it applies nothing, ever, and the
section disappears when every shape has a name. The second one is why the first is
usable on arrival: an engineer meeting a press with ninety parts over two shapes
is offered two rows, not ninety, and that is the difference between an offer
and a list. A part already under a recipe is not on offer even when it has
drifted from it: drift is reported on the recipe's own row, and its control is
`Apply to parts…`. **Drift is in the warning hue, never the accent**: on that
page the accent means "the thing you are working on" and the warning hue means
"this needs you", and a drifted member is the second. Expanding a row lists
EVERY member, in step and drifted alike, because "which parts run this shape" is
the question the row was clicked to answer. **The positions belong to the
shape, not to the name**: a recipe is called whatever the engineer typed, so
every card and every row draws the shape's own positions beside it, and two
recipes a word apart are still told apart at a glance. **Naming a shape is not
declaring which parts run it**: `Save as recipe…` on one style's flow stamps
nobody until the recipe is applied, while naming a shape the process already
runs stamps the parts that run it — the offer recording a fact, not a rename
carrying one. **A rename moves every version of a name and nothing else**,
because a name is not a property of a version and a lineage split across two
names is two recipes that mean one shape.

**The previewed-diff apply modal.** A modal that writes to many rows shows what
each write would change before it offers to save: a checklist with **nothing
pre-ticked**, and ticking a row previews it and draws the diff in the words the
table heads its columns with (`PLN_04 · outbound destination: Supermarket Area
→ Empty Tote Return`; a whole row arriving or leaving is one line worded as the
event). `Save to N parts` then saves the previews that are on screen, one at a
time, reporting per row, and a stale one re-previews and says so. Nothing
pre-ticked is the load-bearing half — a modal that opened with eight rows
ticked would be one click from eight writes nobody read — and "never a save
without its preview on screen" is what makes the count in the button a promise
rather than an estimate. A row the server will refuse stays tickable and shows
the server's refusal BY NAME; a client-side block is the page guessing at a
rule the server owns, and it hides the one sentence that says what happened.
**A write that leaves something unfinished still saves, and says what it
left**: an apply whose shape does not name a position releases whatever part
was on it, and the row adds `… will need a position` to its diff rather than
refusing. What is blocked is the next irreversible step — starting a changeover
the robots could not serve — and it is blocked by a finding on the flow, on
every screen that shows one. **But a modal never offers a save the server would
refuse**: where the write is missing something the modal itself can ask for, it
asks — inline, on the row, one control per missing answer — rather than drawing
a warning over an enabled button. A row with a question outstanding keeps the
tick the engineer gave it and is not in the button's count, because a row that
cannot be saved must not also advertise what it would fire. The test of the
rule: if the only way to finish is to leave the screen you are on, the screen
has sent you away from the job it exists to do.

**The order sentence: `Robot · from → to`.** Every row that describes a move a
robot will make has one shape, and it names BOTH ends:
`Robot 1 · Supermarket Empty Totes → PLN_02 · PIA27`. The robot's hue goes on
the NAME and nothing else on the line — the hue is an identity here, and a
whole line in it reads as a warning. The row ends with the PART when a part is
what is moving and stops when it is not: an evacuation takes out whatever is in
there, so `Robot 2 · PLN_03 → Supermarket Area` is the whole sentence.
**No row for a move nobody drives**: a two-robot press index advances its own
bin from the back position to the line and no robot is dispatched for it, so
under a heading that says ORDERS there is no row for it. It stays on the
picture, where the heading is the press. `orderSentences` in `composer-model.js`
builds the rows, and the station's changeover modal prints them.

**No word for what a bin is.** Screens that describe material name the PART and
the PLACE, never the container. A screen telling an operator standing in front
of the bin what the bin is spends a line on nothing, and the word cannot be
right on a card drawn per press while the bin type is per style. What survives
is a fixed heading (`OLD BINS TO` is a column, not a word about this bin) and
node names the plant gave its own lanes (`Supermarket Empty Totes`). The rule
generalizes: when a screen would name a category the person reading it can see,
it is spending a line on nothing.

**A field is called what the claim calls it; invent no second name.** Where the
thing on screen has a name in the data, that name is the label — `Inbound
source`, `Outbound destination`, `Inbound staging`, `Outbound staging`, `Key
route`, `Paired position`. Every second name is a second thing to learn and a
thing to get wrong: an engineer who reads `outbound_destination` in a refusal
and `old bins to` in a column has to work out they are the same field, and a
screen that renames a field cannot be searched for the field. The words live in
one table, flowspec's labels, and `TestComposerSurfacesSpellNoFieldWordOfTheirOwn`
holds both surfaces to it. The exceptions are words the FLOOR owns and uses
consistently — `part` for `payload_code` is the plant's word on every surface
from the picker to the finding pill — and they are exceptions by being
consistent everywhere, not by being nicer in one place.

**No count of robots a preview cannot know.** A preview knows how many orders it
would fire, because it planned them; it does not know how many AMRs the fleet
will send, because that is the dispatcher's and the fleet's at the moment they
run. Name roles where a role is the subject — the picture's legend, `Robot 1 ·
…` on an order row, "Robot 1 and Robot 2" on the station's Go step — and count
only what the screen computed.

**The bar's problem line is the control.** When the composer bar is blocked,
its first finding that names a position is a link: a click selects that
position (picture and table row, the same selection a card click makes) and
scrolls its row into view (`gotoFinding`). A finding with no position is not
clickable — there is nothing to take you to. There is no one-tap fix button
beside it: every action such a button could take guesses at something the
engineer has not said (a part, a free back position, removal over placement),
and parts are never prefilled. Each field keeps its own picker, and taking an
unplaced part off the flow is a control on the unplaced part's own chip.
