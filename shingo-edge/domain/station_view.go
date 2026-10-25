package domain

import (
	"encoding/json"
	"time"

	"shingo/protocol"
)

// NodeBinState is Core's bin at a node, from its row in the bins table, folded
// into a StationNodeView: what is loaded, whether it holds parts, whether the
// manifest has been confirmed.
type NodeBinState struct {
	BinID       int64  `json:"bin_id,omitempty"`
	BinLabel    string `json:"bin_label,omitempty"`
	BinTypeCode string `json:"bin_type_code,omitempty"`
	// Bare is Core's bin_types.bare for the carrier: it holds no container, so
	// the board hides plain PUSH EMPTY and PUSH AS a real type is the only way
	// out of the window.
	Bare              bool    `json:"bare,omitempty"`
	PayloadCode       string  `json:"payload_code,omitempty"`
	UOPRemaining      int     `json:"uop_remaining"`
	Manifest          *string `json:"manifest,omitempty"`
	ManifestConfirmed bool    `json:"manifest_confirmed"`
	Occupied          bool    `json:"occupied"`
	// UOPCapacity, HeldSince and HeldFor are Core's (protocol.NodeBinsRow):
	// the board labels a carrier PARTIAL from the count and the capacity, and
	// a held one with its line and when it entered its home.
	UOPCapacity int    `json:"uop_capacity,omitempty"`
	HeldSince   string `json:"held_since,omitempty"`
	HeldFor     string `json:"held_for,omitempty"`
	// Stocked and Partial are the row's (protocol.NodeBinsRow): the bin holds
	// parts, and fewer than its part's capacity. Set on the Production page,
	// whose bin modal shows them in place of a count; nil and false on the
	// station view, whose JSON carries its own stocked (stationBinState).
	Stocked *bool `json:"stocked,omitempty"`
	Partial bool  `json:"partial,omitempty"`
}

// CurtainPoint is one curtained node's interlock settings, as the view
// build hands them to the render stamp. SafeValue nil is "polarity not
// chosen", which the stamp renders as held, as the gate refuses it.
type CurtainPoint struct {
	Node      string
	PLCName   string
	TagName   string
	SafeValue *bool
}

// StationNodeView is the per-node section of an OperatorStationView —
// pairs the persisted process Node and its RuntimeState with the
// claim-and-orders context needed for the HMI's node tile, plus the
// most recent release-error string surfaced as a chip when applicable.
type StationNodeView struct {
	Node    Node          `json:"node"`
	Runtime *RuntimeState `json:"runtime,omitempty"`
	// ActiveClaim is the claim the node answers to. On a loader tile it is the
	// loader's synthesised claim, kept on the Go tile for the engine and the
	// Production page's template; the station's JSON leaves it off a loader
	// tile (MarshalJSON), where the board reads Loader instead.
	ActiveClaim *NodeClaim `json:"active_claim,omitempty"`
	// Loader is the Core loader this node is a window or position of, as the
	// board reads it. Nil on every other tile; its presence is what makes a
	// tile a loader tile. Pinned by TestStationView_ModelReadsAreWireKeys.
	Loader         *TileLoader `json:"loader,omitempty"`
	TargetClaim    *NodeClaim  `json:"target_claim,omitempty"`
	ChangeoverTask *NodeTask   `json:"changeover_task,omitempty"`
	// CanSkip offers the operator's ABANDON - SKIP THIS STEP on this node: its
	// changeover task is waiting for material, its supply is still queued, and
	// nothing is moving at the node. Decided by domain.AbandonRefusal, the same
	// rule the abandon refuses on, so the button is never offered for a step
	// the server would refuse.
	CanSkip bool    `json:"can_skip"`
	Orders  []Order `json:"orders"`
	// BinState is Core's bin at the node. The Production page marshals it
	// whole and shows whether it holds parts (Stocked, Partial); the station's
	// JSON carries its own shape (stationBinState: stocked in place of the count).
	BinState *NodeBinState `json:"bin_state,omitempty"`
	// BinStocked is whether the bin holds stock: the node-bins row's Stocked
	// (Core's domain.Bin.Stocked, the one spelling of "counted to nothing"),
	// set with BinState by StationService.StampBins from the Edge's copy of the
	// feed, which carries that and no count (BinState.UOPRemaining is 0 but for
	// a held partial's; the HTTP read that also served one is gone). Nil reads
	// as not stocked: the Edge never recomputes the fact from a count. Not
	// serialised: the station's JSON carries it as bin_state.stocked.
	BinStocked *bool `json:"-"`
	// BinTotes are the cart's tote numbers from its node-bins row (the Edge's
	// copy of the feed): nil when the row carried neither. Not serialised: the
	// station's JSON carries them on bin_state, and NodeBinState, which the
	// Production page marshals whole, carries none.
	BinTotes *BinTotes `json:"-"`
	// Assignment is what the cart at this window is asked to go back as, from
	// the group line of the Edge's copy of Core's node-bins feed: a bin type,
	// and for a type with layer wants a height of it. Nil on every other tile,
	// and on a Core that sends no group lines. Stamped by
	// StationService.StampBins.
	Assignment *TileAssignment `json:"assignment,omitempty"`
	// SwapReady is true when both tracked orders for a two-robot swap are
	// in "staged" status — i.e. both robots are holding at their wait
	// points and a single coordinated release can move both forward.
	// Non-two-robot nodes always report false.
	SwapReady bool `json:"swap_ready"`
	// ReleasesAsPair is true when this node's swap pair is released as ONE —
	// the /release-staged path — rather than leg by leg (store.ReleasesAsPair).
	// The board's pair-wait arm keys on it together with the order graph, so it
	// reads a property the server declares instead of a mode name: sequential
	// and single_robot link their legs too, and release them one at a time.
	ReleasesAsPair bool `json:"releases_as_pair"`
	// ReleasePurposes are the decisions this node owes, one per purpose a live
	// leg stands at or heads to: the board renders one RELEASE per
	// purpose, enabled when Ready (a leg parked at a station wait of it).
	// Built from the order rows the view already holds.
	ReleasePurposes []ReleasePurpose `json:"release_purposes,omitempty"`
	// CurtainOK is the FG light-curtain interlock's render half, read from
	// the PLC poll cache, never WarLink directly: the button is UX, the gate
	// at the click is the interlock. Nil (absent) when none of CurtainPoints
	// is curtained. False greys the RELEASE button: some curtained node does
	// not read its release value in the cache, or the cache has no value for
	// it (the gate refuses fail-closed, so the button must not promise). True
	// leaves the button normal.
	CurtainOK *bool `json:"curtain_ok,omitempty"`
	// CurtainPoints are the curtained nodes this tile's release sends a robot
	// into: the tile's own node and the paired positions of its configured
	// claim, those with the interlock on. Resolved by the view build from
	// node rows it already holds; the www layer turns them into CurtainOK
	// from the tag cache. Not serialised: the board reads CurtainOK.
	CurtainPoints []CurtainPoint `json:"-"`

	// ChildOfNode is set when this tile is rendered on a station only because
	// the node it EXTENDS lives here — a press-index position with no
	// operator_station_id of its own, shown under its press. Carries the
	// owning node's display name.
	//
	// Load-bearing for the UI, not decoration: a child tile must NOT offer a
	// release button. The position owns no task and no order, so there is nothing
	// to release; a button there would either no-op or, worse, release the
	// parent's work from a tile that does not represent it. Empty on ordinary
	// tiles.
	ChildOfNode string `json:"child_of_node,omitempty"`
	// LinesideActive is the node's active piles: pulled parts that drain
	// before the bin until the node's cutover. Rendered as the "active
	// lineside bar" beneath the node fill-bar.
	LinesideActive []LinesideBucket `json:"lineside_active,omitempty"`
	// LinesideStranded is the node's stranded piles: what active piles had
	// left at a cutover. A count anomaly, not parts on the bench (operators
	// run out what they pull), so it never drains or counts. Rendered as chips
	// labelled "count anomaly at cutover".
	LinesideStranded []LinesideBucket `json:"lineside_stranded,omitempty"`
	// LastReleaseError is set when one of the runtime's tracked orders has
	// been rolled back to StatusStaged after a Core-side release failure
	// (e.g. manifest_sync_failed). The operator UI surfaces this as a chip
	// on the node card with the detail string so the operator knows why
	// their release didn't take and can click release again to retry.
	// Empty when no recent release error is pending.
	LastReleaseError string `json:"last_release_error,omitempty"`
	// StrandedAlarm is the parked-ticks alarm sentence for this node: consume
	// ticks piling up in pending_uop_delta while no bin is bound at the node (the
	// carrier standing there was never bound to it). The node grid renders it as
	// a chip on the node's tile and the loader board as its card's note,
	// verbatim —
	// "CARRIER-XXXX staged Nh at <node>, not bound — Record Count on the bin tab."
	// Empty when the node has no active stranding alarm.
	StrandedAlarm string `json:"stranded_alarm,omitempty"`
	// KeepStagedNote is the keep-staged keeper's sentence for this line: that
	// it is paused and REQUEST resumes it, or why it cannot keep its spot.
	// Rendered as a chip on the node card, verbatim. Empty when there is
	// nothing to say.
	KeepStagedNote string `json:"keep_staged_note,omitempty"`
	// AskPending is the sentence for an unloader window whose unloader has a
	// full (U1) asked of Core and not yet answered: the ask went without
	// waiting, from a release or the window's own taps, and Core's answer clears
	// it. The loader board renders it as the window card's note, verbatim. Empty
	// when no ask is out.
	AskPending string `json:"ask_pending,omitempty"`
	// CountWaits is the sentence for a sequential position the line has moved
	// onto before its bin arrived: "The line is on <node>; its parts wait for
	// <node>'s bin." The count is held (pending_uop_delta) and replays when the
	// bin binds. Empty otherwise.
	CountWaits string `json:"count_waits,omitempty"`
	// ContainmentReleaseTarget marks this node as a QUALITY CONTAINMENT
	// position: non-empty when some producing claim names this node as its
	// containment destination, and carries that claim's OutboundDestination —
	// where a verified bin walks when the inspector releases it. The tile's
	// modal renders Verify Good off this field and nothing else: the
	// containment position itself carries no claim (it must stay inert to
	// demand and sourcing), so this stamp is the only way the screen knows
	// what the spot is. Refused-stamp rule: claims DISAGREEING about the
	// outbound (two producers, two FG drops) leave the field empty — a tile
	// with no action is the safe rendering of a config problem.
	ContainmentReleaseTarget string `json:"containment_release_target,omitempty"`
	// ContainmentFlagged is the FLAG's presence on the floor: the bin on this
	// tile carries a payload whose containment flag is ACTIVE on Core, so its
	// next FG-bound delivery diverts automatically. Rendered as a chip — an
	// operator watching the tile sees the state and the coming divert instead
	// of being surprised by a robot heading somewhere unexpected. Empty/false
	// on everything else.
	ContainmentFlagged bool `json:"containment_flagged,omitempty"`
	// BufferPartial names the buffer spot a clear of this home would bring
	// home: set on a dedicated home holding a stocked bin of its part when a
	// spot of its loader's buffer holds a stocked bin of the same part, the
	// first such spot in the plant's order. The board draws its Clear partials
	// card from it, naming that spot. Stamped by
	// StationService.StampBins; pinned by
	// TestStationView_BinsStampedFromTheCopy (www).
	BufferPartial *BufferPartial `json:"buffer_partial,omitempty"`
	// SupplyRefusals maps a payload code to the loader operator's standing
	// statement that they cannot fill the call on that card, keyed the same way
	// the card is: (this node, that payload). Populated for loader tiles on
	// BOTH layouts — a shared window's payload card and a dedicated
	// home's position card reach the same key.
	//
	// The card renders red-DORMANT when a refusal is present: same hue as the
	// unanswered QUEUED state, pulse removed. The pulse means "nobody has dealt
	// with this", so dropping it means "answered" — a channel the status palette
	// does not otherwise use, which matters because the guide records the palette
	// as being at capacity and the next status having to earn a weight rather
	// than a hue.
	SupplyRefusals map[string]SupplyRefusal `json:"supply_refusals,omitempty"`
	// SupplyRefusedForMe is set on a CELL node when a loader operator has said
	// they cannot fill a part this cell has an outstanding call for.
	//
	// THE FILTER IS "A CALL AND THE PART", and it is the same predicate the
	// supplier's endpoint enforces before accepting the refusal at all — one rule
	// applied at both ends of the channel rather than two that have to be kept in
	// step. It deliberately does NOT consult inbound_source: that column may hold
	// a node GROUP name, and matching on it would go silent exactly where it
	// cannot resolve.
	//
	// It also does not wait for the cell to be nearly empty. Material is called
	// BEFORE it runs out, so a cell with an outstanding call and forty minutes of
	// stock is the best possible moment to tell someone their part is not coming —
	// it is the only point where they still have room to change over on their own
	// terms. Gating on "lineside is low" would hold the warning until the line
	// stops, which is the failure this project exists to fix.
	SupplyRefusedForMe *CellSupplyRefusal `json:"supply_refused_for_me,omitempty"`
	// SendBack is SEND BACK's line on a tote press's core position, decided
	// in Go (SendBackLine) and rendered by the board, which spells no rule.
	// Nil when the line is absent. Stamped by StationService.StampBins from
	// SendBackFacts and the bins copy's rows for the outside and the core.
	SendBack *SendBack `json:"send_back,omitempty"`
	// SendBackFacts is the line's facts from the board's rows (the view
	// build), nil on a tile that is no fed produce press. Not serialised.
	SendBackFacts *SendBackFacts `json:"-"`
}

// CellSupplyRefusal is a refusal as the CUSTOMER sees it — the same fact as
// SupplyRefusal plus the window that said it, because the cell's sentence names
// a place the loader's does not need to.
type CellSupplyRefusal struct {
	LoaderNode  string    `json:"loader_node"`
	PayloadCode string    `json:"payload_code"`
	RefusedAt   time.Time `json:"refused_at"`
	RefusedBy   string    `json:"refused_by,omitempty"`
	// Answered false is what fires the modal. It is a STATE, not a schedule:
	// there is no interval to invent because the question stops being asked the
	// moment it is answered, and the answer is durable so it is never asked twice.
	Answered  bool   `json:"answered"`
	AckChoice string `json:"ack_choice,omitempty"`
}

// SupplyRefusal is one card's open refusal as the HMI sees it.
//
// Deliberately not the store row: the board needs what to render, not the
// storage shape. Answered is precomputed because "refused" and "refused and
// answered" are different card states and the render layer must not have to
// infer the second from a nullable timestamp.
type SupplyRefusal struct {
	RefusedAt time.Time `json:"refused_at"`
	// RefusedBy is the station name, not a person — the loader board carries no
	// operator identity. Rendered as attribution because a human said it, and
	// station-level is the honest granularity available.
	RefusedBy string `json:"refused_by,omitempty"`
	// Answered is false while the cell has been told and has not replied. That
	// state is the one worth surfacing: it is the original complaint — the
	// information exists and nobody has acted on it.
	Answered  bool   `json:"answered"`
	AckChoice string `json:"ack_choice,omitempty"`
}

// OperatorStationView is the top-level shape rendered by the operator
// HMI for a single Station. Composes the persisted Station + Process
// state with the active/target style, the in-flight Changeover (if
// any), and the StationNodeView per process node.
type OperatorStationView struct {
	Station          Station           `json:"station"`
	Process          Process           `json:"process"`
	CurrentStyle     *Style            `json:"current_style,omitempty"`
	TargetStyle      *Style            `json:"target_style,omitempty"`
	AvailableStyles  []Style           `json:"available_styles,omitempty"`
	ActiveChangeover *Changeover       `json:"active_changeover,omitempty"`
	StationTask      *StationTask      `json:"station_task,omitempty"`
	Nodes            []StationNodeView `json:"nodes"`
	// CellVersion is the cell picture's version, and the picture itself is NOT
	// here (owner ruling 4, 2026-09-17: "maps don't change often, especially
	// near process nodes… why is this info flying over the wire so much?").
	//
	// The picture rode this view: two queries and a deep copy of the plant's
	// NGRP map, rebuilt at 500 ms a board while events flow, on a Pi with one
	// SQLite connection, for a drawing that changes when an engineer edits a
	// cell. The page fetches it from GET …/cell when this string stops matching
	// the version on the picture it is holding, and never otherwise.
	//
	// Derived rather than announced: see domain.CellPictureVersion for why an
	// announcement-only scheme was rejected (the event stream has stalled in
	// production) and for the four inputs, every one of which the poll already
	// holds.
	CellVersion string `json:"cell_version,omitempty"`
	// Composer is everything U8's Flow Composer reads — the picker's rows, the
	// routing set, the presets and the travel graph. One block rather than seven
	// loose fields; see domain/composer_view.go. Present on every station,
	// because the changeover screen is on every station: the
	// flow_composer_enabled gate on Process ("HMI setup") decides whether it
	// offers setting a style up or only the ones already made.
	Composer *ComposerData `json:"composer,omitempty"`
	// Serves is what the board's loaders serve: one row per served cell's
	// claim and part, of the board's role (consume rows where a loader fills
	// cells, produce rows where an unloader empties them). Nil, so absent, on a
	// board with no loader tile or when the read failed; empty, so [], when it
	// was read and nothing is served. The board draws each served cell's coverage
	// from it: its cells row, which parts run, and a Load card's colour and
	// reason (operator-board-model.js coverage). Built by StationService.servedRows.
	Serves []ServedRow `json:"serves,omitzero"`
	// Empties is what the outbound group of the board's consume loader holds:
	// per bin type, the empties standing in it, and the group's open spots.
	// Absent on a board with no such loader, for a stage-1 unloader (its group
	// is stage 2's windows, not an empties source), and when the bins could
	// not be read. Stamped by StationService.StampBins.
	Empties *Empties `json:"empties,omitempty"`
	// Bins says whether this view's bins are known: false while the Edge holds
	// no copy of Core's node-bins feed from a Core that serves it, or when the
	// read over HTTP failed. With it false an absent bin_state means nothing.
	// AsOf is set when the held copy has not been current for longer than
	// engine.FeedAsOfAfter; the copy is drawn all the same. Absent where the
	// view was built with no bins stamp. Stamped by www stampStationBins.
	Bins *BinsKnown `json:"bins,omitempty"`
	// Containment is set, known false, only while containment is not known: the
	// Edge has containment claims, Core speaks feeds and no copy of its
	// containment feed is held yet, so no tile's chip or release target could
	// be stamped. Absent otherwise. The station's sign says it. Stamped by www
	// apiGetOperatorStationView; pinned by
	// TestStationView_SaysWhenContainmentIsNotKnown.
	Containment *ContainmentKnown `json:"containment,omitempty"`
	// Catalog says when the Edge's copy of Core's payload catalog (each
	// part's capacity and manifest lines; the load modal reads them) is old:
	// set with AsOf when the held copy has not been current for longer than
	// engine.FeedAsOfAfter, known false only while Core speaks feeds and no
	// copy is held at all. Absent otherwise. The station's sign says it.
	// Stamped by www stampStationCatalog; pinned by
	// TestStationCatalog_SaysAsOfAndNotKnown.
	Catalog *CatalogKnown `json:"catalog,omitempty"`
}

// CatalogKnown is whether a view's payload catalog is known, and when it was
// last current when that is long enough ago to say.
type CatalogKnown struct {
	Known bool       `json:"known"`
	AsOf  *time.Time `json:"as_of,omitempty"`
}

// ContainmentKnown is whether a view's containment is known.
type ContainmentKnown struct {
	Known bool `json:"known"`
}

// BinTotes are a cart's tote numbers: the totes standing on it and the totes
// a layer of its type holds. Nil is unknown; 0 totes is tops & bottoms.
type BinTotes struct {
	TotesOnCart   *int
	TotesPerLayer *int
}

// TileAssignment is what a window's cart is asked to go back as: a bin type,
// and a height in layers for a type with layer wants (nil: the type, any
// height).
type TileAssignment struct {
	BinType string `json:"bin_type"`
	Layers  *int   `json:"layers,omitempty"`
}

// BinsKnown is whether a view's bins are known, and as of when.
type BinsKnown struct {
	Known bool       `json:"known"`
	AsOf  *time.Time `json:"as_of,omitempty"`
}

// BufferPartial is the buffer spot holding the partial a home's clear would
// bring home.
type BufferPartial struct {
	Node string `json:"node"`
}

// The states of a buffer spot, as a loader's buffer strip draws them
// (StationService.StampBins decides them).
const (
	// SpotPartial: a bin holding parts of a part stands there.
	SpotPartial = "partial"
	// SpotEmpty: an emptied bin stands there (occupied, no part, not
	// stocked), or a home's clear is in flight to the spot: its emptied
	// carrier is on the way, and the glass draws the spot as coming from the
	// home's orders, where the clear's move of the empty stands.
	SpotEmpty = "empty"
	// SpotOpen: nothing stands there.
	SpotOpen = "open"
	// SpotUnknown: the board cannot place what stands there (a part that is
	// not stocked, or a stocked bin with no part), or the spot was not read.
	SpotUnknown = "unknown"
)

// BufferSpot is one buffer spot of a dedicated loader and what stands there.
type BufferSpot struct {
	Node  string `json:"node"`
	State string `json:"state"`
}

// Empties is a consume loader's outbound group as the board reads it.
type Empties struct {
	// Group is the outbound group's name.
	Group string       `json:"group"`
	Rows  []EmptiesRow `json:"rows"`
}

// EmptiesRow is one bin type's line: the empties of the type standing in the
// group, and the group's open spots (one count for the group, on every row).
type EmptiesRow struct {
	BinType string `json:"bin_type"`
	// Layers is set on a height's row: the empties of the type standing at
	// exactly that many layers (totes_on_cart = layers x totes_per_layer).
	// Nil on the type's row.
	Layers *int `json:"layers,omitempty"`
	Have   int  `json:"have"`
	// Want is the group's want of the type, or of the height on a height's
	// row, from the group line of the Edge's copy of Core's node-bins feed.
	// Nil where the group wants none, and on a Core that sends no group lines.
	Want  *int `json:"want,omitempty"`
	Spots int  `json:"spots"`
}

// TileLoader is a loader tile's facts as the board reads them: the Core
// loader the node belongs to, at this node. It replaces the synthesised claim
// on the station's JSON and the loader fields that used to ride beside it.
type TileLoader struct {
	// Role is Core's: produce (a loader filling cells) or consume (an
	// unloader emptying them).
	Role string `json:"role"`
	// Layout is Core's: shared_window or dedicated_positions.
	Layout string `json:"layout"`
	// Replenishment is Core's: operator (a person stages it) or threshold.
	Replenishment string `json:"replenishment"`
	// OutboundDest is where the loader's bins go; blank when Core names none,
	// and then the board offers no PULL FROM MARKET (the door refuses it,
	// engine/operator_window_pullback.go).
	OutboundDest string `json:"outbound_dest"`
	// Parts are the parts loadable at this node: the loader's set on a shared
	// window, the position's own part at a dedicated home, none at a buffer.
	Parts []TileLoaderPart `json:"parts"`
	// Carriers are the bin types an unloader's CLEAR panels offer for this
	// node's parts, from Core's payload-to-bin-type catalog. Consume role only.
	Carriers []string `json:"carriers,omitempty"`
	// LeavesBare marks a stage-1 window of a two-stage unloader: its CLEAR is
	// one tap, "Full off", with no type.
	LeavesBare bool `json:"leaves_bare"`
	// PulledDirectly marks a stage-2 window whose finished carts the process
	// pulls straight off it.
	PulledDirectly bool `json:"pulled_directly"`
	// BoardShowsAllParts is the Core-owned setting (Loader.BoardShowsAllParts):
	// the board lists every part the loader takes, not only what a running
	// style needs. It used to be read off the replenishment. Its sibling,
	// board_hides_idle, has no reader here: the board draws a card only for
	// work (boardModel), so there is no idle card to hide.
	BoardShowsAllParts bool `json:"board_shows_all_parts"`
	// Buffer is a dedicated loader's buffer spots in the plant's order (Core's
	// bin_loader_homes sort order), each with what stands there. Carried ONCE
	// per loader, on the tile of its first home on the board in that order;
	// absent on every other tile and on a loader with no buffer spot. Stamped
	// by StationService.StampBins (stripTile).
	Buffer []BufferSpot `json:"buffer,omitempty"`
}

// TileLoaderPart is one part a loader tile can load.
type TileLoaderPart struct {
	Code string `json:"code"`
}

// ServedRow is one served cell's claim of one part, as the board reads it.
type ServedRow struct {
	// Cell is the served process's name.
	Cell string `json:"cell"`
	// Part is the payload code.
	Part string `json:"part"`
	// Role is the claim's role: consume (the cell uses the part) or produce
	// (the cell makes it).
	Role string `json:"role"`
	// Carrier is the part's bin type from Core's catalog; blank when unknown.
	Carrier string `json:"carrier"`
	// HaveUOP is the cell's count of the part at the node: the node's bin
	// when the bin holds this part, plus the active piles of this part. Nil
	// when the node's runtime could not be read; below 0 is overpack.
	HaveUOP *int `json:"have_uop"`
	// CapacityUOP is the claim's bin capacity.
	CapacityUOP int `json:"capacity_uop"`
	// Incoming is true for a row of the cell's target style.
	Incoming bool `json:"incoming"`
	// Node is the cell's position (the Core node name).
	Node          string `json:"node"`
	ProcessID     int64  `json:"process_id"`
	ProcessNodeID int64  `json:"process_node_id"`
	// ReorderUOP is the level the claim acts on (NodeClaim.DemandLevel).
	ReorderUOP int `json:"reorder_uop"`
	// Waiting is true while the cell waits on this part from outside: it has
	// an order for it queued with Core's reason (every row), or Core's sourcing
	// verdict for an incoming style lists it missing (incoming consume rows).
	// See StationService.markWaiting.
	Waiting bool `json:"waiting"`
}

// stationBinState is a tile's bin as the station's JSON carries it:
// NodeBinState with Stocked in place of the count, and the cart's tote
// numbers. The board asks only whether a bin holds parts, never how many; the
// Production page marshals NodeBinState itself, with its Stocked and Partial.
// Pinned by
// TestStationNodeView_JSONShape.
type stationBinState struct {
	BinID             int64   `json:"bin_id,omitempty"`
	BinLabel          string  `json:"bin_label,omitempty"`
	BinTypeCode       string  `json:"bin_type_code,omitempty"`
	Bare              bool    `json:"bare,omitempty"`
	PayloadCode       string  `json:"payload_code,omitempty"`
	Stocked           bool    `json:"stocked"`
	Manifest          *string `json:"manifest,omitempty"`
	ManifestConfirmed bool    `json:"manifest_confirmed"`
	Occupied          bool    `json:"occupied"`
	// UOPCapacity, HeldSince and HeldFor are NodeBinState's: the part's
	// capacity, so the board can say a carrier is partial from the tile's own
	// count (runtime.remaining_uop_cached), and a held partial's line and when it
	// entered its home. None of them ticks.
	UOPCapacity int    `json:"uop_capacity,omitempty"`
	HeldSince   string `json:"held_since,omitempty"`
	HeldFor     string `json:"held_for,omitempty"`
	// HeldUOP is Core's count of a HELD partial, the one count the board reads:
	// a carrier waiting in its partial home is drawn from by nothing, so its
	// count does not tick. 0 for every carrier that is not held.
	HeldUOP int `json:"held_uop,omitempty"`
	// TotesOnCart and TotesPerLayer are the cart's tote numbers
	// (StationNodeView.BinTotes), absent when unknown.
	TotesOnCart   *int `json:"totes_on_cart,omitempty"`
	TotesPerLayer *int `json:"totes_per_layer,omitempty"`
}

// stationOrder is a tile's order as the station's JSON carries it: the keys
// the glass reads and no other (domain.Order has some forty; the board polls
// every tile's orders). The Go tile keeps the whole Order for the engine and
// the Production page's template. Pinned by
// TestStationView_OrderRowsCarryTheKeysTheGlassReads (www).
type stationOrder struct {
	ID             int64              `json:"id"`
	OrderType      protocol.OrderType `json:"order_type"`
	Status         protocol.Status    `json:"status"`
	PayloadCode    string             `json:"payload_code"`
	SourceNode     string             `json:"source_node"`
	DeliveryNode   string             `json:"delivery_node"`
	ETA            *string            `json:"eta"`
	AutoConfirm    bool               `json:"auto_confirm"`
	QueueReason    string             `json:"queue_reason"`
	QueueCode      string             `json:"queue_code"`
	SiblingOrderID *int64             `json:"sibling_order_id,omitempty"`
	LaneHeld       bool               `json:"lane_held"`
	Departed       bool               `json:"departed"`
	CreatedAt      time.Time          `json:"created_at"`
}

func stationOrders(orders []Order) []stationOrder {
	if orders == nil {
		return nil
	}
	out := make([]stationOrder, len(orders))
	for i, o := range orders {
		out[i] = stationOrder{
			ID: o.ID, OrderType: o.OrderType, Status: o.Status, PayloadCode: o.PayloadCode,
			SourceNode: o.SourceNode, DeliveryNode: o.DeliveryNode, ETA: o.ETA, AutoConfirm: o.AutoConfirm,
			QueueReason: o.QueueReason, QueueCode: o.QueueCode, SiblingOrderID: o.SiblingOrderID, LaneHeld: o.LaneHeld,
			Departed: o.Departed, CreatedAt: o.CreatedAt,
		}
	}
	return out
}

// MarshalJSON is the station's JSON for a tile: a loader tile carries no
// active_claim (the board reads Loader), the bin carries the feed's Stocked
// rather than its count (stationBinState, from BinStocked), and an order
// carries the keys the glass reads (stationOrder). The Go tile keeps all three
// whole for the engine and the Production page's template, which reads them
// through Go fields and marshals only the bin, as a NodeBinState. Pinned by
// TestStationNodeView_JSONShape.
func (v StationNodeView) MarshalJSON() ([]byte, error) {
	type plain StationNodeView
	p := plain(v)
	if p.Loader != nil {
		p.ActiveClaim = nil
	}
	var bin *stationBinState
	if b := p.BinState; b != nil {
		stocked := p.BinStocked != nil && *p.BinStocked
		bin = &stationBinState{
			BinID: b.BinID, BinLabel: b.BinLabel, BinTypeCode: b.BinTypeCode, Bare: b.Bare,
			PayloadCode: b.PayloadCode, Stocked: stocked, Manifest: b.Manifest,
			ManifestConfirmed: b.ManifestConfirmed, Occupied: b.Occupied,
			UOPCapacity: b.UOPCapacity, HeldSince: b.HeldSince, HeldFor: b.HeldFor,
		}
		if b.HeldSince != "" {
			bin.HeldUOP = b.UOPRemaining
		}
		if t := p.BinTotes; t != nil {
			bin.TotesOnCart, bin.TotesPerLayer = t.TotesOnCart, t.TotesPerLayer
		}
		p.BinState = nil
	}
	orders := stationOrders(p.Orders)
	p.Orders = nil
	return json.Marshal(struct {
		plain
		Orders   []stationOrder   `json:"orders"`
		BinState *stationBinState `json:"bin_state,omitempty"`
	}{p, orders, bin})
}

// ReleasePurpose is one decision a node owes: "ready", "tooling_done" or
// "swap", and whether a leg is parked at a station wait of it.
type ReleasePurpose struct {
	Purpose string `json:"purpose"`
	Ready   bool   `json:"ready"`
}
