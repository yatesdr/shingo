package dispatch

import (
	"testing"

	"shingo/protocol"
)

// TestFormatQueueSentence_Snapshot pins the operator-visible wording for each
// queue code. The strings are owner-approved (design §1 table); changing one is
// a deliberate wording decision, not an incidental edit, and this test makes
// that visible.
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
			name:   "material blank payload reads as empty",
			code:   protocol.QueueWaitingForMaterial,
			params: QueueParams{},
			want:   "Waiting for an empty bin",
		},
		{
			name:   "slot at destination",
			code:   protocol.QueueWaitingForSlot,
			params: QueueParams{Destination: "ASRS_01"},
			want:   "Waiting for a slot at ASRS_01",
		},
		{
			name: "storage rearranging",
			code: protocol.QueueStorageRearranging,
			want: "Rearranging storage to reach this material",
		},
		{
			name: "waiting for partner",
			code: protocol.QueueWaitingForPartner,
			want: "Waiting for partner robot",
		},
		{
			name: "fleet unavailable",
			code: protocol.QueueFleetUnavailable,
			want: "Robot system not responding — retrying",
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
