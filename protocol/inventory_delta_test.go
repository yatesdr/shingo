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

// TestLinesideBucketLevel_RoundTrip pins the wire shape of the lineside
// bucket level: the row's state and level, the window's drains, and no delta,
// reason, style or pair.
func TestLinesideBucketLevel_RoundTrip(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 1, 14, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		l    LinesideBucketLevel
	}{
		{
			name: "active_after_drains",
			l: LinesideBucketLevel{
				CoreNodeName: "LOADER-A1", PayloadCode: "PAY-A", State: LinesideBucketActive,
				Qty: 44, Drained: 3, SequenceID: 17, WindowEnd: t0,
			},
		},
		{
			name: "stranded_at_cutover",
			l: LinesideBucketLevel{
				CoreNodeName: "LOADER-B2", PayloadCode: "PAY-B", State: LinesideBucketStranded,
				Qty: 20, SequenceID: 3, WindowEnd: t0,
			},
		},
		{
			name: "row_gone",
			l: LinesideBucketLevel{
				CoreNodeName: "LOADER-B2", PayloadCode: "PAY-B", State: LinesideBucketActive,
				Qty: 0, SequenceID: 42, WindowEnd: t0,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.l)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			for _, gone := range []string{`"delta"`, `"reason"`, `"style_id"`, `"pair_key"`, `"net"`} {
				if strings.Contains(string(b), gone) {
					t.Errorf("level carries %s on the wire: %s", gone, string(b))
				}
			}
			var got LinesideBucketLevel
			testutil.MustNoErr(t, json.Unmarshal(b, &got), "unmarshal")
			if got != tc.l {
				t.Errorf("round-trip differs:\ngot:  %+v\nwant: %+v", got, tc.l)
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
	if SubjectLinesideBucketLevel != "inventory.lineside_bucket_level" {
		t.Errorf("SubjectLinesideBucketLevel = %q; same caveat as SubjectBinUOPDelta", SubjectLinesideBucketLevel)
	}
}
