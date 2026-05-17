package protocol

import (
	"encoding/json"
	"shingo/protocol/testutil"
	"strings"
	"testing"
	"time"
)

// TestBinUOPDelta_RoundTrip pins the wire shape of the BinUOPDelta
// envelope. Every field round-trips identically; reason strings
// preserve their typed-constant values.
func TestBinUOPDelta_RoundTrip(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 1, 14, 30, 0, 0, time.UTC)
	t1 := t0.Add(5 * time.Second)
	for _, tc := range []struct {
		name string
		d    BinUOPDelta
	}{
		{
			name: "consume_tick_negative",
			d: BinUOPDelta{
				Station: "ALN_001", BinID: 42, PayloadCode: "PART-A",
				Delta: -3, Reason: ReasonConsumeTick,
				SequenceID: 17, WindowStart: t0, WindowEnd: t1,
			},
		},
		{
			name: "produce_tick_positive",
			d: BinUOPDelta{
				Station: "SMN_003", BinID: 99, PayloadCode: "PART-B",
				Delta: 12, Reason: ReasonProduceTick,
				SequenceID: 1, WindowStart: t0, WindowEnd: t1,
			},
		},
		{
			name: "capture_reduction",
			d: BinUOPDelta{
				Station: "ALN_002", BinID: 7, PayloadCode: "PART-C",
				Delta: -47, Reason: ReasonCaptureReduction,
				SequenceID: 999, WindowStart: t0, WindowEnd: t0,
			},
		},
		{
			name: "ab_fallthrough",
			d: BinUOPDelta{
				Station: "ALN_001", BinID: 11, PayloadCode: "PART-D",
				Delta: -1, Reason: ReasonABFallthrough,
				SequenceID: 100, WindowStart: t0, WindowEnd: t1,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.d)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got BinUOPDelta
			testutil.MustNoErr(t, json.Unmarshal(b, &got), "unmarshal")
			if got != tc.d {
				t.Errorf("round-trip differs:\ngot:  %+v\nwant: %+v", got, tc.d)
			}
			// Assert wire-form reason is the exact constant string.
			if !strings.Contains(string(b), `"reason":"`+string(tc.d.Reason)+`"`) {
				t.Errorf("reason not on wire as %q: %s", tc.d.Reason, string(b))
			}
		})
	}
}

// TestLinesideBucketDelta_RoundTrip pins the wire shape of the
// LinesideBucketDelta envelope. Crucial: there is NO State field
// (Option C — buckets are location-only, active/inactive computed at
// query time). A future regression that adds it back will fail this
// test via the unmarshal assertion.
func TestLinesideBucketDelta_RoundTrip(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 1, 14, 30, 0, 0, time.UTC)
	t1 := t0.Add(5 * time.Second)
	for _, tc := range []struct {
		name string
		d    LinesideBucketDelta
	}{
		{
			name: "capture_fill_positive",
			d: LinesideBucketDelta{
				Station: "ALN_001", NodeID: 5, PairKey: "L1|U1",
				StyleID: 100, PartNumber: "PART-A",
				Delta: 47, Reason: ReasonCaptureFill,
				SequenceID: 17, WindowStart: t0, WindowEnd: t0,
			},
		},
		{
			name: "consume_drain_negative",
			d: LinesideBucketDelta{
				Station: "ALN_002", NodeID: 8, PairKey: "L2|U2",
				StyleID: 200, PartNumber: "PART-B",
				Delta: -3, Reason: ReasonConsumeDrain,
				SequenceID: 42, WindowStart: t0, WindowEnd: t1,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.d)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			// Defensive: prove no "state" field leaks onto the wire.
			// Option C is the load-bearing architectural decision here;
			// a refactor that adds back a state column must fail this
			// test before it ships.
			if strings.Contains(string(b), `"state"`) {
				t.Errorf("LinesideBucketDelta carries a state field on the wire — Option C requires location-only buckets: %s", string(b))
			}
			var got LinesideBucketDelta
			testutil.MustNoErr(t, json.Unmarshal(b, &got), "unmarshal")
			if got != tc.d {
				t.Errorf("round-trip differs:\ngot:  %+v\nwant: %+v", got, tc.d)
			}
		})
	}
}

// TestInventoryDelta_SubjectsStable pins the subject strings — these
// participate in routing across both modules and renames must come
// with a coordinated migration.
func TestInventoryDelta_SubjectsStable(t *testing.T) {
	t.Parallel()
	if SubjectBinUOPDelta != "inventory.bin_uop_delta" {
		t.Errorf("SubjectBinUOPDelta = %q; the wire string is part of Core's HandleData router, do not rename without a migration plan", SubjectBinUOPDelta)
	}
	if SubjectLinesideBucketDelta != "inventory.lineside_bucket_delta" {
		t.Errorf("SubjectLinesideBucketDelta = %q; same caveat as SubjectBinUOPDelta", SubjectLinesideBucketDelta)
	}
}
