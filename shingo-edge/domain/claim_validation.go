package domain

import (
	"fmt"
	"slices"

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
	}

	// Seats can only be marked on a cell that HAS seats, and only seats the
	// layout actually holds. Marking the third position of a 2-position press
	// is not an unlikely configuration, it is a reference to a seat that does
	// not exist — and the evacuation it asks for can never happen, silently,
	// because MarkedEvacSeatNodes correctly drops it.
	if len(in.ChangeoverEvacSeats) > 0 {
		if in.SwapMode != protocol.SwapModeTwoRobotPressIndex {
			add("changeover_evac_seats",
				"Per-seat tooling evacuation applies to 2-Robot Press Index only; use Evacuate on changeover for a single-position node")
		} else {
			for _, seat := range in.ChangeoverEvacSeats {
				switch seat {
				case EvacSeatFront:
					// The front seat is CoreNodeName, always present.
				case EvacSeatPaired:
					if in.PairedCoreNode == "" {
						add("changeover_evac_seats", "The back press position is marked for tooling evacuation but no Back Press Node is set")
					}
				case EvacSeatSecond:
					if in.SecondPairedCoreNode == "" {
						add("changeover_evac_seats", "The third press position is marked for tooling evacuation but this claim has no third position")
					}
				default:
					add("changeover_evac_seats", fmt.Sprintf("%q is not a press seat", seat))
				}
			}
		}
	}

	// The flip is press-index choreography; nothing else has two robots to
	// swap between.
	if in.IndexRobotSupplies != nil && *in.IndexRobotSupplies &&
		in.SwapMode != protocol.SwapModeTwoRobotPressIndex {
		add("index_robot_supplies",
			"Index robot fetches the replacement applies to 2-Robot Press Index only")
	}

	// Positions must be distinct, whatever the mode names them. Any two the
	// same is a step whose pickup and dropoff are one node — a robot asked to
	// move a bin to where it already is.
	//
	// The front/back pair is checked here for the first time: the browser
	// compared the THIRD position against both others and never the back
	// against the front, and neither did the store.
	if in.SwapMode == protocol.SwapModeTwoRobotPressIndex {
		if in.PairedCoreNode != "" && in.PairedCoreNode == in.CoreNodeName {
			add("paired_core_node", "Back Press Node must differ from the front (Core Node)")
		}
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
