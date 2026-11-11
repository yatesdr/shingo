package dispatch

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingocore/rds"
	"shingocore/store/orders"
)

// TestFormatQueueSentence_Snapshot pins the operator-visible wording for each
// queue code. The strings are owner-approved; changing one is a deliberate
// wording decision, not an incidental edit, and this test makes that visible.
//
// The cases marked (F#) are the defects from the 2026-07-20 Springfield
// queue-reason study. Each one is a sentence an operator was actually shown that
// was missing context or was wrong.
func TestFormatQueueSentence_Snapshot(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		code   protocol.QueueCode
		params QueueParams
		want   string
	}{
		{
			// A VACATE HOLD names the bin whose leaving is the whole wait.
			name:   "slot held on a vacate stamp",
			code:   protocol.QueueWaitingForSlot,
			params: QueueParams{Destination: "LX-B1", VacateBin: "BIN-0042"},
			want:   "Waiting for BIN-0042 to leave LX-B1",
		},
		{
			name:   "slot held on a vacate stamp whose lifter ended",
			code:   protocol.QueueWaitingForSlot,
			params: QueueParams{Destination: "LX-B1", VacateBin: "BIN-0042", VacateLifterGone: true},
			want:   "Waiting for BIN-0042 to leave LX-B1 — the order that was to lift it has ended",
		},
		{
			// A TABLE TURN names the node whose table is turning, and the angle.
			name:   "lift held for the table turn",
			code:   protocol.QueueWaitingForSlot,
			params: QueueParams{TableNode: "SMN_008", TableWantDeg: 90},
			want:   "Turning the table at SMN_008 to 90°",
		},
		{
			name:   "table turn that cannot be sent",
			code:   protocol.QueueWaitingForSlot,
			params: QueueParams{TableNode: "SMN_008", TableWantDeg: 90, TableBlocked: "AMR-05 is e-stopped"},
			want:   "Cannot turn the table at SMN_008: AMR-05 is e-stopped",
		},
		{
			name:   "table turn that failed, angle read",
			code:   protocol.QueueWaitingForSlot,
			params: QueueParams{TableNode: "SMN_008", TableWantDeg: 90, TableFailed: true, TableReadsDeg: func() *float64 { v := 3.0; return &v }()},
			want:   "The table at SMN_008 did not reach 90°, reads 3° — Core stopped turning it",
		},
		{
			name:   "a lift whose load sequence cannot ride a turn",
			code:   protocol.QueueWaitingForSlot,
			params: QueueParams{TableNode: "SMN_008", Payload: "CHILD-CART", TableLoadSequence: true},
			want:   "SMN_008 turns the table before the lift, and CHILD-CART carries a load sequence that cannot run there — waiting for its configuration to be fixed",
		},
		{
			name:   "table turn that failed, angle unread",
			code:   protocol.QueueWaitingForSlot,
			params: QueueParams{TableNode: "SMN_008", TableWantDeg: 90, TableFailed: true},
			want:   "The table at SMN_008 did not reach 90° — Core stopped turning it",
		},
		{
			name:   "material full payload",
			code:   protocol.QueueWaitingForMaterial,
			params: QueueParams{Payload: "SYN-PART02B.06", Kind: "full"},
			want:   "Waiting for material: SYN-PART02B.06",
		},
		{
			name:   "material empty carrier",
			code:   protocol.QueueWaitingForMaterial,
			params: QueueParams{Kind: "empty"},
			want:   "Waiting for an empty bin",
		},
		{
			// SEND BACK's pull names the press's part, and the kind is tested
			// before the payload, so it never reads as a wait for the part.
			name:   "material tops & bottoms in its source",
			code:   protocol.QueueWaitingForMaterial,
			params: QueueParams{Kind: "tops_bottoms", Payload: "SYN-PART02B.06", Group: "SUPER-A"},
			want:   "Waiting for tops & bottoms in SUPER-A",
		},
		{
			name:   "material tops & bottoms, no source named",
			code:   protocol.QueueWaitingForMaterial,
			params: QueueParams{Kind: "tops_bottoms", Payload: "SYN-PART02B.06"},
			want:   "Waiting for tops & bottoms",
		},
		{
			// A part on no tote carrier: no tops & bottoms could serve the pull,
			// so the sentence names the configuration, not a shortage.
			name:   "material tops & bottoms, the part on no tote carrier",
			code:   protocol.QueueWaitingForMaterial,
			params: QueueParams{Kind: "tops_bottoms", Payload: "SYN-PART02B.06", Group: "SUPER-A", NoToteCarrier: true},
			want:   "No tote carrier is configured for SYN-PART02B.06 — waiting for its configuration to be fixed",
		},
		{
			// (F7) Was "Waiting for an empty bin" — the old branch tested
			// Payload == "" alongside the empty kind, so an unclassified FULL
			// wait told the operator to go find an empty. It now claims nothing
			// it cannot support.
			name:   "material unclassified does not claim an empty carrier",
			code:   protocol.QueueWaitingForMaterial,
			params: QueueParams{},
			want:   "Waiting for material",
		},
		{
			// (F1) The shortage is in the SOURCE group, not at the lineside
			// delivery node. This is the sentence Springfield should have been
			// shown instead of "Waiting for a slot at ALN_003".
			name:   "material names the group it is short in",
			code:   protocol.QueueWaitingForMaterial,
			params: QueueParams{Payload: "SYN-PART07A.06", Group: "AMR Supermarket"},
			want:   "Waiting for material: SYN-PART07A.06 in AMR Supermarket",
		},
		{
			// A FENCE IS NOT A SHORTAGE. The group holds empties and this asker
			// may not have them, so the sentence must not send anyone looking
			// for material that is standing right there. What ends this wait is
			// a config change or a different source — not a delivery.
			name:   "a reserved group says so instead of claiming a shortage",
			code:   protocol.QueueWaitingForMaterial,
			params: QueueParams{Kind: "empty", Group: "PRESS-BUFFER-A", Reserved: true},
			want:   "PRESS-BUFFER-A is kept for other equipment — waiting for an empty from elsewhere",
		},
		{
			// A NAMED SOURCE THAT DOES NOT EXIST. "Waiting for material: PANEL-A
			// in FG-DROP-09" would send someone to look in a place that is not on
			// the map; what ends this wait is a config change.
			name:   "a missing source says so instead of claiming a shortage",
			code:   protocol.QueueWaitingForMaterial,
			params: QueueParams{Payload: "PANEL-A", Group: "FG-DROP-09", SourceMissing: true},
			want:   "Source FG-DROP-09 no longer exists — waiting for its configuration to be fixed",
		},
		{
			name:   "a missing source with no name still says what happened",
			code:   protocol.QueueWaitingForMaterial,
			params: QueueParams{Kind: "empty", SourceMissing: true},
			want:   "The source this order names no longer exists — waiting for its configuration to be fixed",
		},
		{
			// AT LEVEL IS NOT OUT OF ROOM. "Waiting for a slot" sends an operator
			// to find space at a group that has space.
			name:   "an at-level group says so instead of claiming no room",
			code:   protocol.QueueWaitingForSlot,
			params: QueueParams{Destination: "PRESS-BUFFER-A", AtLevel: true},
			want:   "PRESS-BUFFER-A already holds the empties it is set to keep — waiting for one to leave",
		},
		{
			// A CONTAINMENT DESTINATION INSIDE A GROUP names the group to set:
			// the claim is what to fix, not the floor.
			name:   "a containment destination inside a group names the group",
			code:   protocol.QueueWaitingForSlot,
			params: QueueParams{Destination: "HOLD-L1-S2", MemberOf: "HOLD-AREA", Payload: "SYN-PART01"},
			want:   "Contained SYN-PART01 is held: its claim's containment names HOLD-L1-S2, a spot inside HOLD-AREA — set it to HOLD-AREA",
		},
		{
			name:   "material partial set is called out",
			code:   protocol.QueueWaitingForMaterial,
			params: QueueParams{Payload: "SYN-PART02B.06", Partial: true},
			want:   "Waiting for material: SYN-PART02B.06 — partial set already held",
		},
		{
			name:   "slot at destination",
			code:   protocol.QueueWaitingForSlot,
			params: QueueParams{Destination: "ASRS_01"},
			want:   "Waiting for a slot at ASRS_01",
		},
		{
			// A return to a dedicated loader whose home and buffers are full:
			// the spots that would take its carrier, any one of which frees it.
			name:   "loader return names the spots that would take it",
			code:   protocol.QueueWaitingForSlot,
			params: QueueParams{Destination: "SYN-HOME-OUT", EmptyOneOf: "SYN-HOME-OUT, SYN-BUF-1, SYN-BUF-2"},
			want:   "Waiting for a slot at SYN-HOME-OUT — empty one of SYN-HOME-OUT, SYN-BUF-1, SYN-BUF-2 and it goes",
		},
		{
			// (F2) "a bin is sitting there" — go clear it.
			name:   "slot blocked by bins present",
			code:   protocol.QueueWaitingForSlot,
			params: QueueParams{Destination: "SMN_001", BlockingBins: 1},
			want:   "Waiting for a slot at SMN_001 — 1 bin there now",
		},
		{
			// (F2) "another order is on its way" — wait. Rendered identically to
			// the case above until this change, though the fix differs.
			name:   "slot blocked by inbound orders",
			code:   protocol.QueueWaitingForSlot,
			params: QueueParams{Destination: "SMN_001", InboundOrders: 1},
			want:   "Waiting for a slot at SMN_001 — 1 order already inbound",
		},
		{
			name:   "slot plural bins",
			code:   protocol.QueueWaitingForSlot,
			params: QueueParams{Destination: "SMN_001", BlockingBins: 3},
			want:   "Waiting for a slot at SMN_001 — 3 bins there now",
		},
		{
			// (F6) A delivery-node lookup failure. Used to render
			// "Waiting for material: <payload>" and send the operator to
			// inventory for a destination problem.
			name:   "destination unresolved",
			code:   protocol.QueueWaitingForSlot,
			params: QueueParams{Destination: "ALN_003", DestUnresolved: true},
			want:   "Waiting on destination ALN_003 — cannot be resolved right now",
		},
		{
			// (F4) Lane and Payload were passed by every caller and read by none.
			name:   "storage rearranging names lane and payload",
			code:   protocol.QueueStorageRearranging,
			params: QueueParams{Lane: "L12", Payload: "SYN-PART06A.06"},
			want:   "Rearranging lane L12 to reach SYN-PART06A.06",
		},
		{
			// The scanner's reshuffle-congestion site has no lane NAME available
			// (BuriedError carries only LaneID), so it degrades to this rather
			// than printing a raw lane ID at an operator.
			name:   "storage rearranging without a lane still names the payload",
			code:   protocol.QueueStorageRearranging,
			params: QueueParams{Payload: "SYN-PART06A.06"},
			want:   "Rearranging storage to reach SYN-PART06A.06",
		},
		{
			name: "storage rearranging with neither stays generic",
			code: protocol.QueueStorageRearranging,
			want: "Rearranging storage to reach this material",
		},
		{
			// THE OWNER'S REDUNDANCY COMPLAINT. "Rearranging lane L12 to reach
			// X" is true and it is one word plus a lookup: which excavation, and
			// is it the one that frees me. Both are answerable from the dig id,
			// so the wait names it.
			//
			// THE CASE THAT WENT, quoted because it is what the floor used to be
			// told and this row is the record of the change:
			//
			//	name:   "storage rearranging names the dig and what it is uncovering"
			//	params: QueueParams{Lane: "L12", Payload: "SYN-PART06A.06",
			//	            DigOrderID: 4471, DigTarget: "LSD_011"},
			//	want:   "Rearranging lane L12 to reach SYN-PART06A.06 — dig 4471 is uncovering LSD_011"
			//
			// It had already stopped being reachable in the field: DigTarget came
			// from digTargetOf, reading a column whose only writer was deleted with
			// the folder, so every plant has rendered the sentence below since. The
			// test was the last place the long sentence still appeared to work.
			name: "storage rearranging names the dig",
			code: protocol.QueueStorageRearranging,
			params: QueueParams{Lane: "L12", Payload: "SYN-PART06A.06",
				DigOrderID: 4471},
			want: "Rearranging lane L12 to reach SYN-PART06A.06 — dig 4471 is working this lane",
		},
		{
			// The id is the join key, and it is the whole clause now.
			name:   "storage rearranging with a dig and no payload",
			code:   protocol.QueueStorageRearranging,
			params: QueueParams{Lane: "L12", DigOrderID: 4471},
			want:   "Rearranging lane L12 to reach this material — dig 4471 is working this lane",
		},
		{
			// AND AN UNRESOLVED DIG CHANGES NOTHING. A lane held by an ordinary
			// order, or a lock read that failed, renders exactly as it did before
			// the clause existed — a dig id that could not be resolved must not be
			// invented, and zero means "not known", never "none".
			name:   "storage rearranging with no dig resolved is unchanged",
			code:   protocol.QueueStorageRearranging,
			params: QueueParams{Lane: "L12", Payload: "SYN-PART06A.06"},
			want:   "Rearranging lane L12 to reach SYN-PART06A.06",
		},
		{
			// (F3) Sibling was passed at the swap-hold call site and never read.
			// The pre-code free text explained which leg this is and what it
			// waits for; this restores that. What it waits for is the partner's
			// order reaching Core — "secures a bin" described an order with no row.
			name:   "partner not yet received names the sibling order",
			code:   protocol.QueueWaitingForPartner,
			params: QueueParams{Sibling: "2ad889a7-1be2-4ece-9bc4-8a4a616f8147"},
			want:   "Waiting for partner order 2ad889a7 to be received — the two legs go together",
		},
		{
			name:   "partner digging its own bin out",
			code:   protocol.QueueWaitingForPartner,
			params: QueueParams{Sibling: "2ad889a7-1be2-4ece-9bc4-8a4a616f8147", SiblingStatus: StatusReshuffling},
			want:   "Waiting for partner order 2ad889a7 to dig out its bin — the two legs go together",
		},
		{
			// A leg parked at a station wait (MarkStationWaitIfOwned) names no
			// sibling: it waits for the station's RELEASE, not for a partner.
			// Springfield Press 6, 2026-10-21: all four legs read "Waiting for
			// partner robot" while parked for RELEASE.
			name:   "parked at a station wait names the node and the station",
			code:   protocol.QueueWaitingForPartner,
			params: QueueParams{Destination: "PLN_012"},
			want:   "Parked at PLN_012 — the station must release it.",
		},
		{
			name:   "a sibling keeps the pair sentence, a node or not",
			code:   protocol.QueueWaitingForPartner,
			params: QueueParams{Sibling: "2ad889a7-1be2-4ece-9bc4-8a4a616f8147", SiblingStatus: "staged", Destination: "PLN_012"},
			want:   "Waiting for partner order 2ad889a7 — the two legs go together",
		},
		{
			name: "partner without a sibling or a node falls back",
			code: protocol.QueueWaitingForPartner,
			want: "Waiting for partner robot",
		},
		{
			// (F5) Which leg of a multi-step order is blocked. Step 0 is a real
			// step, which is why HasStep exists.
			name:   "step index prefixes a multi-step order",
			code:   protocol.QueueWaitingForMaterial,
			params: QueueParams{Payload: "SYN-PART07A.06", Group: "AMR Supermarket", Step: 0, HasStep: true},
			want:   "Step 0: Waiting for material: SYN-PART07A.06 in AMR Supermarket",
		},
		{
			name: "fleet unavailable",
			code: protocol.QueueFleetUnavailable,
			want: "Robot system not responding — retrying",
		},
		{
			// Fleet trouble is a whole-order condition, not a step's.
			name:   "fleet unavailable takes no step prefix",
			code:   protocol.QueueFleetUnavailable,
			params: QueueParams{Step: 2, HasStep: true},
			want:   "Robot system not responding — retrying",
		},
		{
			// The fleet ANSWERED: its code and text, not "not responding" (2026-11-04).
			name:   "fleet refused with its own reason",
			code:   protocol.QueueFleetUnavailable,
			params: QueueParams{FleetReason: "RDS error 60009: Cannot find path to [SMN_011]"},
			want:   "Fleet refused: RDS error 60009: Cannot find path to [SMN_011] — retrying",
		},
		{
			// A held release the fleet refused after the bin left: the vacate
			// sentence ("waiting for it to leave") is no longer true.
			name: "vacate hold refused by the fleet",
			code: protocol.QueueWaitingForSlot,
			params: QueueParams{VacateBin: "CARRIER-0005", Destination: "PLN_04",
				FleetReason: "RDS error 60009: Cannot find path to new block target [PLN_05]"},
			want: "Fleet refused: RDS error 60009: Cannot find path to new block target [PLN_05] — retrying",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatQueueSentence(tc.code, tc.params)
			if got != tc.want {
				t.Fatalf("FormatQueueSentence(%s, %+v) = %q, want %q", tc.code, tc.params, got, tc.want)
			}
		})
	}
}

// TestFormatQueueSentence_Exhaustive walks every defined queue code through the
// formatter. A code with no handler renders the empty string — a regression
// gate so adding a code without a sentence branch is caught, not silently
// rendered blank. (The empty "" code is excluded — it means "uncoded", not a
// real category, and intentionally renders empty.)
func TestFormatQueueSentence_Exhaustive(t *testing.T) {
	t.Parallel()

	for _, code := range protocol.AllQueueCodes() {
		if got := FormatQueueSentence(code, QueueParams{}); got == "" {
			t.Fatalf("FormatQueueSentence has no sentence for code %q — every code must render", code)
		}
	}
}

// TestFormatQueueSentence_NeverNamesLinesideForGroupShortage is the F1
// regression gate stated as a property rather than a string: when the shortage
// is in a source group, the delivery node must not appear in the sentence. The
// Springfield failure was an operator sent to ALN_003 to look for a slot when
// AMR Supermarket had no bin of the payload.
func TestFormatQueueSentence_NeverNamesLinesideForGroupShortage(t *testing.T) {
	t.Parallel()

	const lineside = "ALN_003"
	got := FormatQueueSentence(protocol.QueueWaitingForMaterial, QueueParams{
		Payload: "SYN-PART07A.06",
		Group:   "AMR Supermarket",
	})
	if strings.Contains(got, lineside) {
		t.Fatalf("material shortage sentence names the lineside node %q: %q", lineside, got)
	}
	if !strings.Contains(got, "AMR Supermarket") {
		t.Fatalf("material shortage sentence must name the group it is short in: %q", got)
	}
}

// TestQueueParamsForCapacity_LocationRule pins the mapping from a classified
// capacity error to sentence params — specifically that a payload shortage
// carries the GROUP and never the order's delivery node, and that an
// unclassified error carries no payload to invent specificity with.
func TestQueueParamsForCapacity_LocationRule(t *testing.T) {
	t.Parallel()

	const payload, delivery = "SYN-PART07A.06", "ALN_003"

	t.Run("payload shortage uses the group, not the delivery node", func(t *testing.T) {
		p := queueParamsForCapacity(
			&capacityDetail{Kind: capacityPayload, Group: "AMR Supermarket"}, payload, delivery)
		if p.Group != "AMR Supermarket" {
			t.Errorf("Group = %q, want the source group", p.Group)
		}
		if p.Destination != "" {
			t.Errorf("Destination = %q, want empty — the lineside node is not where the shortage is", p.Destination)
		}
		if p.Payload != payload {
			t.Errorf("Payload = %q, want %q", p.Payload, payload)
		}
	})

	t.Run("genuine slot shortage keeps a destination", func(t *testing.T) {
		p := queueParamsForCapacity(
			&capacityDetail{Kind: capacitySlot, Group: "ASRS"}, payload, delivery)
		if p.Destination != "ASRS" {
			t.Errorf("Destination = %q, want the saturated group", p.Destination)
		}
	})

	t.Run("unclassified withholds the payload", func(t *testing.T) {
		p := queueParamsForCapacity(&capacityDetail{Kind: capacityUnknown}, payload, delivery)
		if p.Payload != "" {
			t.Errorf("Payload = %q, want empty — the classifier did not earn that claim", p.Payload)
		}
		if got := FormatQueueSentence(protocol.QueueWaitingForMaterial, p); got != "Waiting for material" {
			t.Errorf("unclassified sentence = %q, want the unspecific form", got)
		}
	})
}

// TestFleetRefusalCause_CarriesTheFleetsAnswer pins the split between a fleet
// that ANSWERED with a refusal and one that did not answer. HK, 2026-11-04:
// every create refusal read "Robot system not responding — retrying" while RDS's
// code sat in Core's log alone.
func TestFleetRefusalCause_CarriesTheFleetsAnswer(t *testing.T) {
	t.Parallel()
	answered := fmt.Errorf("create: %w", &rds.APIError{Code: 60009, Msg: "Cannot find path to [SMN_011]"})
	code, cause, params := FleetRefusalCause(answered, "PLN_05")
	if code != protocol.QueueFleetUnavailable || cause != CauseFleetRefusedCreate {
		t.Fatalf("answered refusal parks as (%s, %s), want (%s, %s)", code, cause,
			protocol.QueueFleetUnavailable, CauseFleetRefusedCreate)
	}
	if got, want := FormatQueueSentence(code, params), "Fleet refused: RDS error 60009: Cannot find path to [SMN_011] — retrying"; got != want {
		t.Errorf("answered refusal reads %q, want %q", got, want)
	}

	silent := errors.New("rds POST /setOrder: dial tcp 192.0.2.1:8088: i/o timeout")
	code, _, params = FleetRefusalCause(silent, "PLN_05")
	if got, want := FormatQueueSentence(code, params), "Robot system not responding — retrying"; got != want {
		t.Errorf("transport failure reads %q, want %q", got, want)
	}
}

// TestDeadLegWhy pins the partner's parenthesis: the dead leg's reason, with a
// fleet stop's repeated prefix dropped when the fleet's words follow it (the
// owner's short-text rule for the order page).
func TestDeadLegWhy(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ detail, want string }{
		{"fleet order stopped — RDS error 60009: Cannot find path to new block target [PLN_05]",
			" (RDS error 60009: Cannot find path to new block target [PLN_05])"},
		{"fleet order stopped", " (fleet order stopped)"},
		{"cancelled by admin", " (cancelled by admin)"},
		{"", ""},
	} {
		if got := deadLegWhy(&orders.Order{ErrorDetail: c.detail}); got != c.want {
			t.Errorf("deadLegWhy(%q) = %q, want %q", c.detail, got, c.want)
		}
	}
}

// TestLegFailedDemandDetail pins the demand's restart row after a leg ended its
// chapter: it names the newest FAILED leg and that leg's reason, while a
// cascade-cancelled sibling (which carries the bare marker) names nothing.
func TestLegFailedDemandDetail(t *testing.T) {
	t.Parallel()
	children := []*orders.Order{
		{ID: 3200, Status: StatusFailed, ErrorDetail: "Gave up after 45m · cannot replan (60011)"},
		{ID: 3201, Status: StatusFailed, ErrorDetail: "fleet order stopped — RDS error 60009: Cannot find path to new block target [PLN_05]"},
		{ID: 3202, Status: StatusCancelled, ErrorDetail: reshuffleLegFailedDetail},
	}
	want := "reshuffle dissolved: dig leg 3201 failed (RDS error 60009: Cannot find path to new block target [PLN_05]); the demand re-plans"
	if got := legFailedDemandDetail(children); got != want {
		t.Errorf("legFailedDemandDetail = %q, want %q", got, want)
	}
	// No failed leg (a swept one carries the marker): the bare sentence.
	if got := legFailedDemandDetail(children[2:]); got != reshuffleLegFailedDetail {
		t.Errorf("no failed leg: %q, want the bare %q", got, reshuffleLegFailedDetail)
	}
}
