package domain

// composer_view.go — what the Flow Composer reads, and nothing else.
//
// ONE BLOCK RATHER THAN SEVEN LOOSE FIELDS. Everything here exists for U8's
// screens: the picker's rows, the set-up card's parts, the position panel's
// option lists, and the waypoints the "Robot drives via" row offers. Grouping
// them says that plainly — a field that stops being read by the composer is a
// field to delete rather than one to wonder about — and it keeps the board's
// own read of OperatorStationView exactly as it was.
//
// WHAT RIDES THE POLL AND WHAT DOES NOT, and the comment that used to sit here
// said the opposite. It read "everything below is carried on the view the
// station already fetches" — true when it was written, and untrue since S8 cut
// the block in two: the station VIEW carries the picker's shape alone (Styles
// and RecentTargets), and every field past that is the composer's own fetch,
// made on a tap and held for the session. See station_composer.go's
// composerScope, which is the thing that decides.
//
// So the scan still waits on nothing — the picker's rows are on the poll — and
// opening the composer costs exactly one round trip, which is the tap the
// operator already made.
type ComposerData struct {
	// Styles is the picker's rows and the set-up card's contents: every live
	// style of this process with what the operator needs to choose between
	// them. AvailableStyles carries the same styles as bare names; this adds
	// the flow summary, the parts and the verdict.
	Styles []ComposerStyle `json:"styles"`
	// RecentTargets is the picker's RECENT group — the last distinct styles
	// this process changed over TO, newest first, at most four.
	RecentTargets []int64 `json:"recent_targets,omitempty"`
	// Routing is the enabled routing set (U5) — the sources, staging slots and
	// destinations the position panel may offer. Disabled rows are filtered
	// here rather than in the JS: a retired lane is not an option the operator
	// should have to know not to pick.
	Routing []ComposerRoutingNode `json:"routing,omitempty"`
	// RoutingOff is the rows of each role the process HAS and has not switched
	// on — role name to node names, and absent when every row is enabled.
	//
	// THE NAMES, NOT A COUNT, and the count was not enough. It shipped as one
	// and the first question off the floor was which nodes it meant: the 4x2's
	// card said "7 staging nodes … switched off" while the engineer was
	// looking at supermarket locations, and could not tell from the card that
	// the seven were SLN lanes and that the SMNs they had in mind were the
	// source and destination rows — a different seven. A count says a role is
	// empty; the names say whether the roles are right, which is the decision
	// the sentence is asking them to go and make.
	//
	// WHY A COUNT TRAVELS WHEN THE ROWS DO NOT. Routing above is filtered to
	// the enabled rows, deliberately: a retired lane is not an option the
	// operator should have to know not to pick. But a role with NO enabled
	// rows draws a heading over nothing, and an engineer at Hopkinsville read
	// that as the derivation being broken (owner, Amendment A 2026-09-16). It
	// is not broken — the set is empty — and the difference between "nobody
	// has put staging nodes in this process's routing set" and "two were found
	// from your flows and nobody has switched them on" is the difference
	// between two different next actions.
	//
	// NAMES, STILL NOT ROWS. These are not offerable and must not become
	// selectable by being listed: a disabled row is a decision somebody has
	// not made, and the screens say which names are waiting and where to go
	// and make it — they do not let it be made from here.
	RoutingOff map[string][]string `json:"routing_off,omitempty"`
	// Presets are this process's flow presets, newest version of each name.
	// The S4 strip renders a card per preset; empty means the strip carries
	// FLOW · Start blank · PARTS and nothing more.
	Presets []ComposerPreset `json:"presets,omitempty"`
	// Palette is the part set every part picker on both surfaces offers: the
	// process's stored payload rows unioned with what its live claims already
	// run (store/processes.ProcessPalette).
	//
	// NOT ComposerStyle.Parts, WHICH IS A DIFFERENT QUESTION. Parts is what ONE
	// style runs and it feeds the unplaced-part finding — every entry of it not
	// on a position blocks Start and Save. The palette is what a style COULD
	// run, and putting the two in one field is what made a cell with twelve
	// configured parts and one placed read "11 parts need a position" and
	// refuse to change over.
	//
	// AND IT IS NOT ON ComposerStyle EITHER. The part set belongs to the
	// process, so a copy per style would be the same list marshalled forty
	// times on a read that already carries forty style blocks.
	//
	// Set only from composerStation outward — never above the picker's early
	// return, which is the poll.
	Palette []string `json:"palette,omitempty"`
	// Cell is the press itself — positions in their true arrangement with the
	// running style's choreography on them, the same CellPicture the station
	// draws. Set only on the desktop's process-scoped read; the station view
	// carries its own (station-scoped) copy at the top level and would be
	// carrying the picture twice.
	Cell *CellPicture `json:"cell,omitempty"`
	// Scene is the adjacency the "Robot drives via" row walks to find the LM
	// waypoints along a position's supply path. Nil when the geometry cache has
	// no scene, and then the row offers "shortest way" alone — a waypoint list
	// invented without a map is a route the robot cannot drive.
	//
	// THE STATION'S HALF. Scene and Map are the same network — Scene is Map
	// with the coordinates collapsed into one length per edge — so a payload
	// carrying both carries it twice. The station read carries Scene and no
	// Map; the desktop read carries Map, whose edges carry Len, and no Scene.
	Scene *ComposerScene `json:"scene,omitempty"`
	// Map is the plant as the routing-set screen (D3) draws it: placed points
	// and the shape of every drivable segment, each with its length.
	//
	// THE DESKTOP'S HALF, and a superset of Scene: anything that walks the
	// network for waypoints can walk these edges by Len. Nil when the
	// geometry cache is empty, and then D3 says the map has not arrived
	// rather than drawing an invented one.
	Map *ComposerMap `json:"map,omitempty"`
}

// ComposerMap is the vendor map's geometry, as the desktop draws it.
type ComposerMap struct {
	// Revision is the cache's, so a screen can say which pull it is drawing.
	Revision string `json:"revision"`
	// Points is instance_name → placement, for every point in the cache. It is
	// the whole map because D3 draws the plant faint behind the press: a
	// subgraph would leave the press floating with nothing to be near.
	Points map[string]ComposerMapPoint `json:"points"`
	// Edges is every drivable segment with both endpoints and, on a curved
	// one, its two control handles. Screen space is the page's business;
	// nothing here is projected.
	Edges []ComposerMapEdge `json:"edges"`
}

// ComposerMapPoint is one placed scene point. Class travels because the join
// rule is on it — a position is the GeneralLocation of that name, never the
// ActionPoint half a metre away (LocateCellPosition).
type ComposerMapPoint struct {
	Class string  `json:"class"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
}

// ComposerMapEdge is one drivable segment. Handles is absent on a straight
// segment and four numbers on a curved one — all four or none, because three
// coordinates describe no cubic.
//
// AN ENDPOINT IS A NAME, NOT A PLACE. This used to carry fx/fy/tx/ty as well,
// which is Points[From] and Points[To] written out again — and written out
// once per incident edge, so a point on six lanes shipped its coordinates
// seven times. Measured on the S0 fixture's 383-point/635-edge Hopkinsville
// map, the Map block went 108.6 kB → 77.2 kB when the copies went.
// Both readers (the travel graph and D3's path builder) already had the points
// map in hand.
//
// composerMap guarantees every endpoint is in Points, so the lookup cannot
// miss — see it for why that is a repair and not just a precondition.
//
// Len is the straight-line length, the same number ComposerEdge carries, so
// a reader that walks the network for waypoints does not need Scene beside
// this and does not recompute 635 square roots in the browser.
type ComposerMapEdge struct {
	From    string      `json:"from"`
	To      string      `json:"to"`
	Len     float64     `json:"len,omitempty"`
	Handles *[4]float64 `json:"h,omitempty"`
}

// ComposerStyle is one picker row.
type ComposerStyle struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// CATID is the part identity a scan carries, so the picker's search field
	// matches what the gun reads and not only what the style is called.
	CATID string `json:"catid,omitempty"`
	// ClaimCount is zero for a style nobody has set up — the "Build" verdict,
	// and with the gate off the row that reads "Set up from the desktop".
	ClaimCount int `json:"claim_count"`
	// Modes and Nodes build the row's flow summary ("2-robot index · P01/P04").
	Modes []string `json:"claim_modes,omitempty"`
	Nodes []string `json:"claim_nodes,omitempty"`
	// Parts is the payloads this style runs, with the position each sits on
	// when it has one. The set-up card's chips.
	Parts []ComposerPart `json:"parts,omitempty"`
	// LastRun is the day this style was last changed over to, already
	// formatted — the row says "ran 08-29" and nothing computes a date in JS.
	LastRun string `json:"last_run,omitempty"`
	// THE VERDICT IS NOT HERE, AND THAT IS THE POINT. Core's sourceability
	// already reaches this screen as the view's top-level sourcing_by_style,
	// keyed by style NAME, and the board's picker reads it there. Copying it
	// into this block would be a second copy of a live feed, stale the moment
	// the two are assembled apart — so the composer's picker looks it up the
	// same way, with the same four codes and the same words.
	//
	// There is no test-payload field either: nothing on this tree marks one
	// (no column, no flag, no catalog entry — only a payload NAMED
	// "Test-Payload" at Hopkinsville and "testpayload" at Springfield), so the
	// word is deferred to the payload-identity work rather than guessed from a
	// spelling. Four verdicts, not five (owner, 2026-09-10).
	// SavedFrom and SavedOn are the flow's provenance, for the set-up card's
	// dim line: "Flow saved 09-02 from the desktop", or "... from Press 400".
	//
	// FROM THE CLAIMS, because a flow IS its claims — there is no flow row to
	// hang a timestamp on. SavedFrom is the surface (ClaimSourceAdmin reads
	// "the desktop", ClaimSourceHMI reads the station's called_by) and SavedOn
	// is the newest updated_at among them, formatted here like LastRun so
	// nothing computes a date on a touch screen.
	//
	// Both are empty for a style with no flow yet, and SavedFrom is empty when
	// the claims disagree about where they came from — a flow half-written
	// from each surface has no single answer, and inventing one would be the
	// caption lying about the thing this field exists to stop it lying about.
	SavedFrom string `json:"saved_from,omitempty"`
	SavedOn   string `json:"saved_on,omitempty"`
	// Claims is the style's stored cells, so the composer can open a flow
	// without a second read. Collapse()d on the way out.
	Claims []FlowCell `json:"claims,omitempty"`
	// Advanced is what the desktop's Advanced modal shows, keyed by position:
	// the twelve columns of each stored claim that the picture has no line for.
	//
	// BESIDE THE CELLS RATHER THAN INSIDE THEM. A cell is the shape
	// flow_presets validates and the shape a save sends back, and on both
	// counts these columns are absent by default — a preset carries a flow,
	// not a policy, and a save speaks them only when an engineer opened the
	// modal. Reading and writing through the same field would make "what the
	// row holds" and "what the draft says" one value, which is exactly the
	// distinction FlowCell.Advanced's nil exists to keep.
	//
	// Set only on the desktop's process-scoped read, like Cell above: the
	// station HMI has no Advanced modal, and a screen on a Pi over plant WiFi
	// should not carry a policy block per position per style it will never
	// draw.
	Advanced map[string]FlowAdvanced `json:"advanced,omitempty"`
}

// ComposerPart is one payload of a style, and where it currently sits.
type ComposerPart struct {
	PayloadCode string `json:"payload_code"`
	// Node is the position this part is claimed on, empty when the style
	// carries the part but no position has it — the amber "unplaced" chip.
	Node string `json:"node,omitempty"`
}

// ComposerRoutingNode is one enabled member of the routing set.
//
// NO LABEL. The routing table has a label column and the only writer that
// ever set it (the desktop's routingAdd) set it to the node's own name, so
// every reader's `label || core_node_name` resolved to the name either way.
// A second name for the same thing is a copy waiting to disagree.
//
// Sequence stays: composer-model.js's defaultRouting picks a cell's default
// source and destination by lowest sequence.
type ComposerRoutingNode struct {
	CoreNodeName string `json:"core_node_name"`
	Role         string `json:"role"`
	Sequence     int    `json:"sequence"`
}

// ComposerPreset is one preset card on the S4 strip.
type ComposerPreset struct {
	// ID is the preset's row id as a string: a tap has to resolve to ONE
	// version, and two versions of a name share the name. It was the name
	// until U10, which is why a second version of a preset would have been
	// untappable.
	//
	// CALLED WHAT IT IS. It was `key`, which named the role the field plays
	// on the strip rather than the fact it carries, and left the card the one
	// place in the feature where a preset's identity had a different name.
	//
	// There is no Version here. The strip does not draw one — the card says
	// the shape's name, its positions and how many parts run it — and the
	// desktop's preset table reads its versions from /presets, which carries
	// every version of every name. A version on the card would be a second
	// copy of that fact with no reader.
	ID   string `json:"id"`
	Name string `json:"name"`
	// Mode is the choreography its cells carry, for the card's glyph. Blank
	// when a preset mixes modes — the card then draws the empty glyph rather
	// than picking one of them to stand for the rest.
	Mode string `json:"mode,omitempty"`
	// Where is the shape's positions, and Use is how many parts run it: the
	// card's second and third lines (owner rulings R2 and R5, 2026-09-12).
	//
	// TWO FIELDS AND NOT ONE STRING. They are two facts and the card gives
	// them a line each — `PLN_01 / PLN_04` over `used by 7 parts` — because
	// one line carrying both ellipsised inside the card's 200 px and cut off
	// the count, which is the half that says why the card is worth tapping. A
	// server that joined them would be a server deciding a line break.
	Where string `json:"where,omitempty"`
	Use   string `json:"use,omitempty"`
	// Cells is the preset's flow, keyed by position, in the composer's own
	// cell shape.
	Cells map[string]FlowCell `json:"cells,omitempty"`
}

// ComposerScene is the travel graph, reduced to what a waypoint search needs.
//
// NO GROUP MEMBERSHIP. It used to hang off here, so the desktop asked for a
// travel graph it never walked in order to reach a membership map — and once
// it was on the block as well, the same map was on the payload twice.
// CellPicture.Groups is the one copy, and both surfaces already carry the
// picture.
type ComposerScene struct {
	Edges []ComposerEdge `json:"edges,omitempty"`
}

// ComposerEdge is one undirected segment of the travel network. Len is its
// length in metres; the search treats a missing length as 1 so a scene with
// unmeasured edges still returns waypoints in the right order.
type ComposerEdge struct {
	From string  `json:"from"`
	To   string  `json:"to"`
	Len  float64 `json:"len,omitempty"`
}
