package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
)

// The record-count fence on the wire (SYNTH-round2 S7): three optional fields
// on UOPAdjustment, omitted when unset, so every adjustment that is not a
// fenced count (lifecycle announcements, moves, an old Core) is byte-identical
// to before, and an old Edge decodes a fenced one and ignores the fence.

func TestCountFence_WireShape(t *testing.T) {
	t.Parallel()
	base := UOPAdjustment{
		BinID: 42, CoreNodeName: "NODE-A", NewRemaining: 50, Actor: "counter-1",
		AdjustedAt: time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC), Epoch: 3,
	}
	without, err := json.Marshal(base)
	testutil.MustNoErr(t, err, "marshal without fence")
	if strings.Contains(string(without), "as_of") {
		t.Errorf("an unset fence is on the wire: %s", without)
	}

	withFence := base
	net, seq := int64(-1234), int64(567)
	withFence.AsOfNet, withFence.AsOfSeq, withFence.AsOfStation = &net, &seq, "stn-test"
	with, err := json.Marshal(withFence)
	testutil.MustNoErr(t, err, "marshal with fence")
	want := len(`,"as_of_net":-1234,"as_of_seq":567,"as_of_station":"stn-test"`)
	if got := len(with) - len(without); got != want {
		t.Errorf("the fence adds %d bytes, want %d", got, want)
	}

	var back UOPAdjustment
	testutil.MustNoErr(t, json.Unmarshal(with, &back), "decode")
	if back.AsOfNet == nil || *back.AsOfNet != net || back.AsOfSeq == nil || *back.AsOfSeq != seq || back.AsOfStation != "stn-test" {
		t.Errorf("round trip = {%v, %v, %q}", back.AsOfNet, back.AsOfSeq, back.AsOfStation)
	}
}

// The declared-full stamp on the ingest (strict pack, lane P0): three optional
// fields, omitted when unset, so every ingest that is not a station's FULL on
// a short bin is byte-identical to before, and an older Core's decode — a
// struct without the fields — books the counted Quantity with no error.
func TestOrderIngestRequest_DeclaredFullWireShape(t *testing.T) {
	t.Parallel()
	base := OrderIngestRequest{
		OrderUUID: "8a0e5f8e-6a43-4a43-9a8e-3b8f1d2c4e01", PayloadCode: "SYN44-PARTA.06",
		BinID: 812, BinEpoch: 41, SourceNode: "ALN_011", Quantity: 131,
		ProducedAt: "2026-10-20T08:00:00Z",
	}
	without, err := json.Marshal(base)
	testutil.MustNoErr(t, err, "marshal without the declaration")
	for _, key := range []string{"declared_qty", "called_by", "order_id"} {
		if strings.Contains(string(without), key) {
			t.Errorf("an unset %s is on the wire: %s", key, without)
		}
	}

	stamped := base
	stamped.DeclaredQty, stamped.CalledBy, stamped.OrderID = 132, "operator-7", "c1d2e3f4-0000-4000-8000-00000000abcd"
	with, err := json.Marshal(stamped)
	testutil.MustNoErr(t, err, "marshal with the declaration")
	want := len(`,"declared_qty":132,"called_by":"operator-7","order_id":"c1d2e3f4-0000-4000-8000-00000000abcd"`)
	if got := len(with) - len(without); got != want {
		t.Errorf("the declaration adds %d bytes, want %d", got, want)
	}

	// An older Core: today's struct, field for field, without the three.
	var old struct {
		OrderUUID   string               `json:"order_uuid"`
		PayloadCode string               `json:"payload_code"`
		BinLabel    string               `json:"bin_label"`
		BinID       int64                `json:"bin_id,omitempty"`
		BinEpoch    int64                `json:"bin_epoch,omitempty"`
		SourceNode  string               `json:"source_node"`
		Quantity    int64                `json:"quantity"`
		Manifest    []IngestManifestItem `json:"manifest,omitempty"`
		ProducedAt  string               `json:"produced_at,omitempty"`
	}
	testutil.MustNoErr(t, json.Unmarshal(with, &old), "an older Core decodes a stamped ingest")
	if old.Quantity != 131 || old.BinID != 812 || old.BinEpoch != 41 || old.PayloadCode != base.PayloadCode {
		t.Errorf("older decode = %+v, want the counted 131 on bin 812 epoch 41", old)
	}

	var back OrderIngestRequest
	testutil.MustNoErr(t, json.Unmarshal(with, &back), "decode")
	if back.DeclaredQty != 132 || back.CalledBy != "operator-7" || back.OrderID != stamped.OrderID || back.Quantity != 131 {
		t.Errorf("round trip = %+v", back)
	}
}
