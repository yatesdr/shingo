package domain

import (
	"fmt"
	"slices"
	"strings"

	"shingo/protocol"
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
	if len(route) > 0 && in.SwapMode == protocol.SwapModeManualSwap {
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
	add := func(field, msg string) {
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

	// Board order. A negative position is not a position; absent means "no
	// opinion" and the store assigns the next free slot, which is why nil is
	// fine and -1 is not.
	if in.Sequence != nil && *in.Sequence < 0 {
		add("sequence", "Board order cannot be negative")
	}

	// manual_swap loaders carry no edge-side payload: Core owns the loader's
	// payload set from the loader board. Every other mode needs a primary.
	if in.SwapMode != protocol.SwapModeManualSwap &&
		(in.Role == protocol.ClaimRoleConsume || in.Role == protocol.ClaimRoleProduce) &&
		in.PayloadCode == "" {
		add("payload_code", "Select a payload")
	}

	validateSwapModeRouting(in, add)

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
		if in.SwapMode != protocol.SwapModeTwoRobotPressIndex {
			add("changeover_evac_nodes",
				"Per-node changeover clearance applies to a cell whose claim names several nodes; use Evacuate on changeover for a single-node claim")
		} else {
			held := map[string]bool{}
			for _, n := range []string{in.CoreNodeName, in.PairedCoreNode, in.SecondPairedCoreNode} {
				if n != "" {
					held[n] = true
				}
			}
			for _, node := range marked {
				if !held[node] {
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
	if in.IndexRobotSupplies != nil && *in.IndexRobotSupplies &&
		in.SwapMode != protocol.SwapModeTwoRobotPressIndex {
		add("index_robot_supplies",
			"Index robot fetches the replacement applies to 2-Robot Press Index only")
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

// validateSwapModeRouting refuses a claim whose ROUTING does not match the
// choreography its swap mode will run. One arm per mode, and a mode with no arm
// is a mode whose missing fields are only discovered mid-changeover — which is
// how sequential went for as long as it had none.
//
// Split out of ValidateNodeClaim for length, and it is the right seam anyway:
// everything left there is invariant over every claim (a style, a node, a legal
// mode, a non-negative board order), while this is the per-mode half. It takes
// the caller's `add` so findings keep their original order.
func validateSwapModeRouting(in NodeClaimInput, add func(field, msg string)) {
	switch in.SwapMode {
	case protocol.SwapModeSingleRobot:
		// One robot does the whole swap, so it needs somewhere to park the
		// incoming bin AND somewhere to put the outgoing one.
		if in.InboundStaging == "" {
			add("inbound_staging", "Single-robot swap requires inbound staging")
		}
		if in.OutboundStaging == "" {
			add("outbound_staging", "Single-robot swap requires outbound staging")
		}
	case protocol.SwapModeTwoRobot:
		// Robot A waits at the staging node until Robot B clears the line.
		// Without it BuildTwoRobotSwapSteps returns nil silently and the
		// operator's RELEASE click does nothing.
		if in.InboundStaging == "" {
			add("inbound_staging", "Two-robot swap requires inbound staging")
		}
	case protocol.SwapModeManualSwap:
		// Without it the post-swap bin has nowhere to go and the node
		// deadlocks.
		if in.OutboundDestination == "" {
			add("outbound_destination", "Loader/unloader claims require an outbound destination")
		}
	case protocol.SwapModeTwoRobotPressIndex:
		if in.PairedCoreNode == "" {
			add("paired_core_node", "2-Robot Press Index requires a Back Press Node")
		}
		if in.OutboundDestination == "" {
			add("outbound_destination", "2-Robot Press Index requires an Outbound Destination")
		}
	case protocol.SwapModeSequential:
		// ── THIS ARM DID NOT EXIST, AND SEQUENTIAL IS THE MODE THAT NEEDS
		//    IT MOST ──
		//
		// Every other mode's routing is refused here, at save time, by the
		// person who can fix it. Sequential fell straight through the switch:
		// a claim with no partner, no destination or no source saved clean and
		// failed much later as an EMPTY DISPATCH — the builder returns a zero
		// ChangeoverDispatch, the planner turns that into a generic "cannot
		// build swap steps for node X", and the node task lands in error
		// naming a builder instead of a field. The operator is told the
		// changeover failed, not which box to fill in.
		//
		// The three fields are the ones the per-node builder actually reads:
		// the partner position (A/B is two positions by definition), where the
		// old bin goes, and where the new carrier comes from. They are the same
		// three requiredChangeoverFields already demands at plan time — this
		// arm moves the discovery from mid-changeover to the save button.
		if in.PairedCoreNode == "" {
			add("paired_core_node", "Sequential A/B requires a Paired Position")
		}
		if in.OutboundDestination == "" {
			add("outbound_destination", "Sequential A/B requires an Outbound Destination")
		}
		if in.InboundSource == "" {
			add("inbound_source", "Sequential A/B requires an Inbound Source")
		}
	}
}
