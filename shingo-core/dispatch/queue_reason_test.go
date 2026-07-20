package dispatch

import (
	"strings"
	"testing"

	"shingo/protocol"
)

// TestFormatQueueSentence_Snapshot pins the operator-visible wording for each
// queue code. The strings are owner-approved; changing one is a deliberate
// wording decision, not an incidental edit, and this test makes that visible.
//
// The cases marked (F#) are the defects from the 2026-07-20 Springfield
// queue-reason study. Each one is a sentence an operator was actually shown that
// was missing context or was wrong.
func TestFormatQueueSentence_Snapshot(t *testing.T) {
	cases := []struct {
		name   string
		code   protocol.QueueCode
		params QueueParams
		want   string
	}{
		{
			name:   "material full payload",
			code:   protocol.QueueWaitingForMaterial,
			params: QueueParams{Payload: "SNF2-6SA0B.06", Kind: "full"},
			want:   "Waiting for material: SNF2-6SA0B.06",
		},
		{
			name:   "material empty carrier",
			code:   protocol.QueueWaitingForMaterial,
			params: QueueParams{Kind: "empty"},
			want:   "Waiting for an empty bin",
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
			params: QueueParams{Payload: "767D2-6SA0A.06", Group: "AMR Supermarket"},
			want:   "Waiting for material: 767D2-6SA0A.06 in AMR Supermarket",
		},
		{
			name:   "material partial set is called out",
			code:   protocol.QueueWaitingForMaterial,
			params: QueueParams{Payload: "SNF2-6SA0B.06", Partial: true},
			want:   "Waiting for material: SNF2-6SA0B.06 — partial set already held",
		},
		{
			name:   "slot at destination",
			code:   protocol.QueueWaitingForSlot,
			params: QueueParams{Destination: "ASRS_01"},
			want:   "Waiting for a slot at ASRS_01",
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
			params: QueueParams{Lane: "L12", Payload: "74368-6SA0A.06"},
			want:   "Rearranging lane L12 to reach 74368-6SA0A.06",
		},
		{
			// The scanner's reshuffle-congestion site has no lane NAME available
			// (BuriedError carries only LaneID), so it degrades to this rather
			// than printing a raw lane ID at an operator.
			name:   "storage rearranging without a lane still names the payload",
			code:   protocol.QueueStorageRearranging,
			params: QueueParams{Payload: "74368-6SA0A.06"},
			want:   "Rearranging storage to reach 74368-6SA0A.06",
		},
		{
			name: "storage rearranging with neither stays generic",
			code: protocol.QueueStorageRearranging,
			want: "Rearranging storage to reach this material",
		},
		{
			// (F3) Sibling was passed at the swap-hold call site and never read.
			// The pre-code free text explained which leg this is and what it
			// waits for; this restores that.
			name:   "partner names the sibling order",
			code:   protocol.QueueWaitingForPartner,
			params: QueueParams{Sibling: "2ad889a7-1be2-4ece-9bc4-8a4a616f8147"},
			want:   "Holding this leg until partner order 2ad889a7 secures a bin",
		},
		{
			name: "partner without a sibling falls back",
			code: protocol.QueueWaitingForPartner,
			want: "Waiting for partner robot",
		},
		{
			// (F5) Which leg of a multi-step order is blocked. Step 0 is a real
			// step, which is why HasStep exists.
			name:   "step index prefixes a multi-step order",
			code:   protocol.QueueWaitingForMaterial,
			params: QueueParams{Payload: "767D2-6SA0A.06", Group: "AMR Supermarket", Step: 0, HasStep: true},
			want:   "Step 0: Waiting for material: 767D2-6SA0A.06 in AMR Supermarket",
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
	const lineside = "ALN_003"
	got := FormatQueueSentence(protocol.QueueWaitingForMaterial, QueueParams{
		Payload: "767D2-6SA0A.06",
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
	const payload, delivery = "767D2-6SA0A.06", "ALN_003"

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
