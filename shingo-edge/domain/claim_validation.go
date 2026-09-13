package domain

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"shingo/protocol"
	"shingoedge/domain/flowspec"
)

// FieldError is one validation finding tagged with the request field it is
// about, so a client can render it ON that field instead of as one toast that
// says nothing about where to look.
//
// Field names are the WIRE names (snake_case, as on NodeClaimInput's json
// tags), not the editor's internal state names. The server is answering about
// the request body it was sent; mapping that to a DOM id is the client's job
// and the client is the only side that knows its own ids.
type FieldError struct {
	Field    string `json:"field"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
}

// Severity values. A warning is advisory: the write proceeds and the client
// shows the note.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

// ClaimNodeContext is the DB-resolved context the membership check needs.
// ValidateNodeClaim stays pure by taking it as a value rather than reaching
// for a database.
//
// Checked says whether the caller was ABLE to look. False means the lookup
// failed or was not attempted — and absence of data must never render as a
// finding, so the membership warning is simply not emitted. A caller that
// cannot resolve the node must not produce "this node is not on your process".
type ClaimNodeContext struct {
	Checked bool
	// StyleProcessID is the process the claim's style belongs to.
	StyleProcessID int64
	// NodeProcessIDs are the processes that have a process_node row for the
	// claim's core_node_name. Plural on purpose: one physical slot is named by
	// several processes routinely — a shared loader window is the ordinary
	// case — which is exactly why this is a warning and not a refusal.
	NodeProcessIDs []int64
	// KnownCoreNodes is Core's synced node set, as a name set.
	//
	// NIL/EMPTY MEANS "COULD NOT LOOK", not "Core has no nodes". Core's list
	// arrives on the wire every couple of minutes and a fresh Edge, a restart
	// or a Kafka gap all leave it empty — refusing a configuration write on
	// that basis would brick setup exactly when someone is most likely to be
	// doing it. This is the same rule, and the same reasoning, as
	// coreNodeNameIsUnknown at the process-node write; the two should stay in
	// step.
	KnownCoreNodes map[string]bool
	// KnownScenePoints is the VENDOR MAP's point set — every location the
	// fleet knows, not just the ones Shingo gave a job to.
	//
	// This is the universe a key route is expressed in, and it is why the node
	// list is the wrong thing to validate one against: Shingo works in APs, so
	// KnownCoreNodes is a subset, and a plain waypoint — the feature's primary
	// use — is absent from it. Same nil-means-could-not-look rule as above.
	KnownScenePoints map[string]bool
}

// ErrRunningPositionMove refuses the one mid-run edit the runtime cannot
// follow: a position appearing on, or leaving, the style the press is
// RUNNING.
//
// IN domain BECAUSE THE STORE RAISES IT AND THREE LAYERS MATCH ON IT. Every
// runtime reader of the running flow resolves its claim by (active_style_id,
// core_node_name) and none of them caches — so a changed source, destination
// or key route is simply what the next trip uses, and that is the flexibility
// owner ruling R3 asked for. A POSITION is different: move the running style
// off PLN_01 and the bin physically standing there has no live claim, so the
// level sweep never refills it, the PLC tick stops counting the parts made off
// it, and every delivered/completed handler looks past it.
var ErrRunningPositionMove = errors.New("that position is running")

// ClaimContextSet is ONE process's claim context, resolved once and then
// answered per cell.
//
// THERE USED TO BE TWO BUILDERS. engine.flowClaimContext said in its own
// comment that it mirrored www's claimNodeContext, and the two had already
// drifted: the empty-plant-map log fired at one door and not the other, so
// whether an unverifiable key route left a trace depended on which screen the
// engineer had used. A single validator with two ways of being told what it
// is validating against is a validator that answers two questions.
//
// RESOLVED ONCE PER REQUEST, ANSWERED PER CELL. ForNode is a map lookup; the
// engine's version walked every process_node on the Edge for every cell of
// every save.
//
// Checked stays FALSE on any lookup failure, and that is the load-bearing
// part: "this node is not on your process" and "I could not find out" are
// different sentences, and only one of them belongs in front of an engineer.
type ClaimContextSet struct {
	checked        bool
	styleProcessID int64
	// nodeProcessIDs is core node name -> the processes with a process_node
	// row for it.
	nodeProcessIDs map[string][]int64
	knownCore      map[string]bool
	scenePoints    map[string]bool
}

// ClaimContextInput is what NewClaimContextSet needs, as the caller already
// has it. Every field is optional in the could-not-look sense described on
// ClaimNodeContext: an empty Core node set or point set degrades the
// validator to a warning rather than refusing a write.
type ClaimContextInput struct {
	// StyleProcessID is the process whose flow is being validated.
	StyleProcessID int64
	// Nodes is every process_node on the Edge, as (core node name, process).
	Nodes []ClaimContextNode
	// KnownCoreNodes is Core's synced node set; KnownScenePoints is the
	// vendor map's point set.
	KnownCoreNodes   map[string]bool
	KnownScenePoints map[string]bool
}

// ClaimContextNode is one process_node, reduced to the two fields the
// membership check reads. A struct rather than the store's row so domain
// keeps no dependency on the store.
type ClaimContextNode struct {
	CoreNodeName string
	ProcessID    int64
}

// NewClaimContextSet indexes one process's context. A zero StyleProcessID
// leaves the set unchecked, which is the same "could not look" answer both
// doors gave before.
func NewClaimContextSet(in ClaimContextInput) ClaimContextSet {
	if in.StyleProcessID == 0 {
		return ClaimContextSet{}
	}
	byName := make(map[string][]int64, len(in.Nodes))
	for _, n := range in.Nodes {
		byName[n.CoreNodeName] = append(byName[n.CoreNodeName], n.ProcessID)
	}
	return ClaimContextSet{
		checked:        true,
		styleProcessID: in.StyleProcessID,
		nodeProcessIDs: byName,
		knownCore:      in.KnownCoreNodes,
		scenePoints:    in.KnownScenePoints,
	}
}

// Checked reports whether the set resolved. Callers log the empty-plant-map
// case off this plus KnownScenePoints.
func (s ClaimContextSet) Checked() bool { return s.checked }

// KnownScenePoints is the vendor map's point set, so a caller can say when it
// was empty — the one thing worth a line, because a key route saved against
// no map is saved unverified.
func (s ClaimContextSet) KnownScenePoints() map[string]bool { return s.scenePoints }

// ForNode is the per-cell context ValidateNodeClaim takes.
func (s ClaimContextSet) ForNode(coreNodeName string) ClaimNodeContext {
	if !s.checked || coreNodeName == "" {
		return ClaimNodeContext{}
	}
	return ClaimNodeContext{
		Checked:          true,
		StyleProcessID:   s.styleProcessID,
		NodeProcessIDs:   s.nodeProcessIDs[coreNodeName],
		KnownCoreNodes:   s.knownCore,
		KnownScenePoints: s.scenePoints,
	}
}

// validateKeyRoute is the Routing fieldset's half of ValidateNodeClaim, lifted
// out because it is a self-contained set of rules about one field pair and the
// parent had grown past the length the linter allows. It returns findings
// rather than appending, so the caller keeps the field order it controls.
//
// See NodeClaim.KeyRoute for the vendor semantics. The short version, because it
// is why these are ERRORS and not warnings: a point that does not exist or
// cannot be reached terminates the robot's waybill the moment the order is
// issued, so an unresolvable point stored quietly is an order that dies at
// dispatch with nothing on this side to explain it.
func validateKeyRoute(in NodeClaimInput, nodeCtx ClaimNodeContext) []FieldError {
	var out []FieldError
	add := func(field, msg string) {
		out = append(out, FieldError{Field: field, Message: msg, Severity: SeverityError})
	}
	route := OptValue(in.KeyRoute)
	// THE TABLE IS THE AUTHORITY, not a second copy of the mode check. This read
	// `in.IsLoaderNode()`, and KeyRoute is Forbidden for manual_swap and for
	// nothing else — so the two say the same thing today and the table is the
	// one that keeps saying it when a mode is added.
	if len(route) > 0 && flowspec.Steady(in.Role, in.SwapMode)[flowspec.KeyRoute] == flowspec.Forbidden {
		add("key_route", "Key route applies to robot-served claims; a manual_swap loader does not drive")
	}
	seenPoint := map[string]bool{}
	for i, pt := range route {
		if strings.TrimSpace(pt) == "" {
			add("key_route", fmt.Sprintf("Key route point %d is blank", i+1))
			continue
		}
		if seenPoint[pt] {
			// Not a vendor rule — a repeat is how a mis-click renders, and a
			// route that visits one point twice is never what was meant.
			add("key_route", fmt.Sprintf("Key route lists %q more than once", pt))
			continue
		}
		seenPoint[pt] = true
		// SELF_POSITION is the robot's own current location. The vendor
		// forbids it in keyRoute specifically, and it is the one value an
		// operator might reasonably type expecting "start where you are".
		if pt == "SELF_POSITION" {
			add("key_route", "SELF_POSITION is never valid in a key route")
			continue
		}
		// THE UNIVERSE IS THE MAP, NOT THE NODE LIST.
		//
		// A key route names points in the vendor's scene. Shingo works in APs,
		// so its node list is the subset it gave a job to, and a corridor
		// waypoint — the feature's primary use — is not in it. Validating
		// against the node list refused correct routes, confidently.
		//
		// EXACT MATCH. The node-list check used coreNodeResolves, which also
		// matches after the last dot so a bare child name resolves against
		// "Group.CHILD". That fallback belongs to node names; applied to map
		// points it makes "001" match "SMN.001", which is loose and narrow at
		// once. The scene stores instance names as SEER holds them and that is
		// what the fleet is handed.
		if len(nodeCtx.KnownScenePoints) > 0 {
			if !nodeCtx.KnownScenePoints[pt] {
				add("key_route", fmt.Sprintf(
					"Key route point %q is not on the plant map (%d points known). A point that does "+
						"not resolve terminates the robot's waybill the moment it is issued.",
					pt, len(nodeCtx.KnownScenePoints)))
			}
			continue
		}
		// NO MAP, NO REFUSAL — the CheckLocationTasks posture. An Edge that has
		// not heard from Core, or one whose Core predates the scene sync, knows
		// nothing about the map; saying so is honest and refusing on it would
		// make the field unusable exactly where it is most needed. The write
		// lands carrying a note that nobody checked it.
		out = append(out, FieldError{
			Field:    "key_route",
			Severity: SeverityWarning,
			Message: fmt.Sprintf(
				"Key route point %q could not be checked: the plant map has not been received from "+
					"Core. Saved unverified — a point that does not exist will terminate the robot's "+
					"waybill when the order is issued.", pt),
		})
	}
	// The vendor's literal values; anything else is silently ignored by SEER,
	// which is worse than being told.
	if task := OptValue(in.KeyTask); task != "" && task != "load" && task != "unload" {
		add("key_task", fmt.Sprintf("Key task must be \"load\", \"unload\", or empty; got %q", task))
	}

	return out
}

// KeepStagedWithheld is the refusal every door gives a claim that asks for
// keep_staged: API ingress (ValidateNodeClaim), the store (UpsertClaim) and the
// changeover planner. Why it is withheld is written once, beside the planner's
// refusal in planSwapAction.
const KeepStagedWithheld = "inbound-staging option not available yet"

// ValidateNodeClaim is the one server-side statement of what a claim must look
// like. Pure: no database, no HTTP, no logging.
//
// It consolidates checks that were previously split across three places — the
// browser's validateClaimState, the API handler, and UpsertClaim's own guards —
// with the browser as the ONLY holder of two of them (a required payload, and
// single_robot's staging pair). A check that lives only in the browser is not a
// check: the same write arrives from the HTTP API, an import and a stale tab.
//
// UpsertClaim keeps its own guards deliberately. This runs at API ingress and
// they run at the store, so a non-API caller still cannot write a claim that
// contradicts its swap mode. Duplication here is the point, not an oversight.
//
// Returns findings in field order, errors and warnings mixed; the caller
// decides what to do with each severity via HasErrors.
func ValidateNodeClaim(in NodeClaimInput, nodeCtx ClaimNodeContext) []FieldError {
	var out []FieldError
	reported := map[string]bool{}
	add := func(field, msg string) {
		reported[field] = true
		out = append(out, FieldError{Field: field, Message: msg, Severity: SeverityError})
	}

	if in.StyleID == 0 {
		add("style_id", "style_id is required")
	}
	if in.CoreNodeName == "" {
		add("core_node_name", "core_node_name is required")
	}

	// swap_mode is required and must be one the editor can actually produce.
	// No default is safe: two_robot needs inbound staging and single_robot
	// needs both, so picking one would only trade a mode error for a more
	// misleading staging error.
	switch {
	case in.SwapMode == "":
		add("swap_mode", "swap_mode is required")
	case !slices.Contains(protocol.ConfigurableSwapModes(), in.SwapMode):
		add("swap_mode", fmt.Sprintf("%q is not a configurable swap mode", in.SwapMode))
	}

	if in.KeepStaged != nil && *in.KeepStaged {
		add("keep_staged", KeepStagedWithheld)
	}

	// Board order. A negative position is not a position; absent means "no
	// opinion" and the store assigns the next free slot, which is why nil is
	// fine and -1 is not.
	if in.Sequence != nil && *in.Sequence < 0 {
		add("sequence", "Board order cannot be negative")
	}

	// THE PER-MODE HALF READS ONE TABLE. Which fields this claim's mode
	// requires and which it must not carry is flowspec.Steady's answer, shared
	// with the store's guards, the changeover planner and the editor; this
	// function chooses the wording and nothing else. An unknown mode has
	// already been refused above and answers here as the retired "simple" row
	// does, so the findings beside that refusal keep their shape.
	spec := flowspec.Steady(in.Role, in.SwapMode)

	// manual_swap loaders carry no edge-side payload: Core owns the loader's
	// payload set from the loader board. Every other mode needs a primary.
	// The role guard is the historical one: a claim with no role at all is not
	// told to pick a payload (the store defaults its role to consume).
	//
	// The loader guard that used to sit here (`!in.IsLoaderNode()`) is the table
	// now: PayloadCode is Forbidden for manual_swap, so it is never Required for
	// a loader and the Required test already excludes one. Core owns a loader's
	// payload set, which is the reason behind both spellings.
	if (in.Role == protocol.ClaimRoleConsume || in.Role == protocol.ClaimRoleProduce) &&
		spec[flowspec.PayloadCode] == flowspec.Required && !ClaimInputHas(in, flowspec.PayloadCode) {
		add("payload_code", "Select a payload")
	}

	validateSwapModeRouting(in, spec, add)

	// A MARKED NODE MUST BE ONE THIS CLAIM OCCUPIES.
	//
	// The marks name core nodes, so the only way to get them wrong is to name a
	// node this claim does not hold — a leftover from a re-pairing, or a typo.
	// Either way the clearance it asks for can never happen, because
	// MarkedEvacNodes correctly drops it, and the operator is never told.
	//
	// This is also what replaces the old indirection. When the marks named
	// positions ("front"/"paired"/"second") a re-pairing silently re-targeted
	// the clearance onto whatever node the claim now paired to. Refusing here
	// turns that same edit into a save-time message naming the node, which is
	// the direction every other gate in this file goes.
	marked := OptValue(in.ChangeoverEvacNodes)
	if len(marked) > 0 {
		if spec[flowspec.ChangeoverEvacNodes] == flowspec.Forbidden {
			add("changeover_evac_nodes",
				"Per-node changeover clearance applies to a cell whose claim names several nodes; use Evacuate on changeover for a single-node claim")
		} else {
			held := in.Positions()
			for _, node := range marked {
				if !slices.Contains(held, node) {
					add("changeover_evac_nodes", fmt.Sprintf(
						"%q is marked for changeover clearance but is not one of this claim's nodes", node))
				}
			}
		}
	}

	// CARRY-OVER: refuse a disposition the cell cannot carry out, at SAVE time
	// and by name.
	//
	// "outbound_staging" walks the kept bin to the cell's outbound staging spot
	// and back. With no such spot configured there is nowhere to walk it, and
	// the alternatives are both bad: silently falling back to clearing means an
	// operator who asked for a short hop gets a supermarket round-trip and is
	// never told, and refusing at CHANGEOVER time means finding out with a
	// press down and people waiting. The arm gate's doctrine is that a
	// configuration which cannot work is refused where it is written.
	if disp := OptValue(in.ChangeoverCarryoverDisposition); disp != "" {
		if !disp.Valid() {
			add("changeover_carryover_disposition", fmt.Sprintf("%q is not a carry-over disposition", disp))
		} else if disp == CarryoverOutboundStaging && in.OutboundStaging == "" {
			add("changeover_carryover_disposition",
				"Keeping a carried-over part at outbound staging requires an Outbound Staging node on this claim")
		}
		if disp != CarryoverReplace && len(marked) == 0 {
			add("changeover_carryover_disposition",
				"A carry-over disposition only applies to positions marked for changeover clearance; this claim marks none")
		}
	}

	// The flip is press-index choreography; nothing else has two robots to
	// swap between.
	if ClaimInputHas(in, flowspec.IndexRobotSupplies) && spec[flowspec.IndexRobotSupplies] == flowspec.Forbidden {
		add("index_robot_supplies",
			"Index robot fetches the replacement applies to 2-Robot Press Index only")
	}

	// STRICT MODES REFUSE EVERY FIELD THEY DO NOT USE (D4). The four rules
	// above are the ones the server always had; for a strict mode the rest of
	// the row's Forbidden entries are refused too, so a populated value the
	// editor would have cleared is refused when it arrives any other way. The
	// store reads the same answer through the same function, which is what
	// makes the two write paths agree. A field a rule above already reported
	// is not reported twice.
	for _, v := range SteadyViolations(in) {
		if v.Need != flowspec.Forbidden || reported[string(v.Field)] {
			continue
		}
		add(string(v.Field), fmt.Sprintf("%s does not use %s; clear it", swapModeLabel(in.SwapMode), flowspec.Label(v.Field)))
	}

	out = append(out, validateKeyRoute(in, nodeCtx)...)

	// Positions must be distinct, whatever the mode names them. Any two the
	// same is a step whose pickup and dropoff are one node — a robot asked to
	// move a bin to where it already is.
	//
	// The front/back pair is checked here for the first time: the browser
	// compared the THIRD position against both others and never the back
	// against the front, and neither did the store.
	//
	// SEQUENTIAL IS THE SECOND MODE THAT PAIRS, and the paragraph above already
	// said "whatever the mode names them" while the guard named one mode. An A/B
	// pair whose two positions are the same node is a press with one position
	// pretending to be two: resolveSequentialActivePull hands back that name for
	// both sides, and the parked order and the active order then evacuate and
	// refill the SAME slot — two robots, one bin, and a cutover gated on an
	// order that is clearing the position it is waiting for.
	if in.SwapMode == protocol.SwapModeTwoRobotPressIndex || in.SwapMode == protocol.SwapModeSequential {
		if in.PairedCoreNode != "" && in.PairedCoreNode == in.CoreNodeName {
			add("paired_core_node", "Paired position must differ from this claim's own (Core Node)")
		}
	}
	if in.SwapMode == protocol.SwapModeTwoRobotPressIndex {
		if in.SecondPairedCoreNode != "" {
			if in.SecondPairedCoreNode == in.CoreNodeName {
				add("second_paired_core_node", "Third press position must differ from the front (Core Node)")
			}
			if in.SecondPairedCoreNode == in.PairedCoreNode {
				add("second_paired_core_node", "Third press position must differ from the Back Press Node")
			}
		}
	}

	// ── membership: a WARNING, never a refusal ──────────────────────────
	//
	// A claim pointing at a node that is not on its style's process is very
	// often a mis-pick, and it produces orders that dispatch to a slot nobody
	// on this line owns. But it is NOT always wrong: one physical slot is
	// legitimately named by several processes — a shared loader window is the
	// ordinary case — so refusing would block a working configuration to catch
	// a likely typo. Say it and let the engineer decide.
	if nodeCtx.Checked && in.CoreNodeName != "" && in.StyleID != 0 {
		if len(nodeCtx.NodeProcessIDs) > 0 && !slices.Contains(nodeCtx.NodeProcessIDs, nodeCtx.StyleProcessID) {
			out = append(out, FieldError{
				Field: "core_node_name",
				Message: fmt.Sprintf("%q is not a node on this style's process — it belongs to %s. "+
					"That is legitimate for a shared slot such as a loader window, and a mis-pick otherwise.",
					in.CoreNodeName, describeProcessIDs(nodeCtx.NodeProcessIDs)),
				Severity: SeverityWarning,
			})
		}
	}

	return out
}

// HasErrors reports whether any finding is a refusal rather than advice.
func HasErrors(findings []FieldError) bool {
	for _, f := range findings {
		if f.Severity == SeverityError {
			return true
		}
	}
	return false
}

func describeProcessIDs(ids []int64) string {
	switch len(ids) {
	case 0:
		return "no process"
	case 1:
		return fmt.Sprintf("process %d", ids[0])
	default:
		return fmt.Sprintf("processes %v", ids)
	}
}

// routingRequiredMessages is the wording of each routing refusal, per mode.
//
// WHICH fields a mode requires is flowspec.Steady's answer, not this map's:
// the map only says how to phrase it, in the words the editor has always
// shown. An entry with no wording still refuses — validateSwapModeRouting
// falls back to the field's label — so a table change cannot be silenced by
// forgetting a sentence here; TestRoutingRequiredMessagesAreTotal says when
// one is missing.
//
// The sequential arm is the reason this map exists at all. Every other mode's
// routing was refused here, at save time, by the person who can fix it, and
// sequential fell straight through the switch: a claim with no partner, no
// destination or no source saved clean and failed much later as an EMPTY
// DISPATCH — the builder returned a zero ChangeoverDispatch, the planner
// turned that into a generic "cannot build swap steps for node X", and the
// node task landed in error naming a builder instead of a field. The three
// fields are the ones the per-node builder actually reads, and they are the
// same three requiredChangeoverFields demands at plan time.
var routingRequiredMessages = map[protocol.SwapMode]map[flowspec.Field]string{
	protocol.SwapModeSingleRobot: {
		// One robot does the whole swap, so it needs somewhere to park the
		// incoming bin AND somewhere to put the outgoing one.
		flowspec.InboundStaging:      "Single-robot swap requires inbound staging",
		flowspec.OutboundStaging:     "Single-robot swap requires outbound staging",
		flowspec.OutboundDestination: "Single-robot swap requires an outbound destination",
	},
	protocol.SwapModeTwoRobot: {
		// Robot A waits at the staging node until Robot B clears the line.
		// Without it BuildTwoRobotSwapSteps returns nil silently and the
		// operator's RELEASE click does nothing.
		flowspec.InboundStaging:      "Two-robot swap requires inbound staging",
		flowspec.OutboundDestination: "Two-robot swap requires an outbound destination",
	},
	protocol.SwapModeManualSwap: {
		// Without it the post-swap bin has nowhere to go and the node
		// deadlocks.
		flowspec.OutboundDestination: "Loader/unloader claims require an outbound destination",
	},
	protocol.SwapModeTwoRobotPressIndex: {
		flowspec.PairedCoreNode:      "2-Robot Press Index requires a Back Press Node",
		flowspec.OutboundDestination: "2-Robot Press Index requires an Outbound Destination",
	},
	protocol.SwapModeSequential: {
		flowspec.PairedCoreNode:      "Sequential A/B requires a Paired Position",
		flowspec.OutboundDestination: "Sequential A/B requires an Outbound Destination",
		flowspec.InboundSource:       "Sequential A/B requires an Inbound Source",
	},
}

// swapModeLabel is the mode's name as the editor's messages say it.
func swapModeLabel(mode protocol.SwapMode) string {
	switch mode {
	case protocol.SwapModeSingleRobot:
		return "Single-robot swap"
	case protocol.SwapModeTwoRobot:
		return "Two-robot swap"
	case protocol.SwapModeTwoRobotPressIndex:
		return "2-Robot Press Index"
	case protocol.SwapModeSequential:
		return "Sequential A/B"
	case protocol.SwapModeManualSwap:
		return "Loader/unloader claims"
	}
	return string(mode)
}

// validateSwapModeRouting refuses a claim whose ROUTING does not match the
// choreography its swap mode will run. It consults flowspec.Steady for the
// mode's Required routing fields — the same table the planner reads at plan
// time — and reports them in flowspec.RoutingFields order, which is the order
// the per-mode switch this replaced used to report them in.
//
// Split out of ValidateNodeClaim for length, and it is the right seam anyway:
// everything left there is invariant over every claim (a style, a node, a legal
// mode, a non-negative board order), while this is the per-mode half. It takes
// the caller's `add` so findings keep their original order.
func validateSwapModeRouting(in NodeClaimInput, spec map[flowspec.Field]flowspec.Need, add func(field, msg string)) {
	for _, f := range flowspec.RoutingFields() {
		if spec[f] != flowspec.Required || ClaimInputHas(in, f) {
			continue
		}
		msg, ok := routingRequiredMessages[in.SwapMode][f]
		if !ok {
			msg = fmt.Sprintf("%s requires %s", in.SwapMode, flowspec.Label(f))
		}
		add(string(f), msg)
	}
}
