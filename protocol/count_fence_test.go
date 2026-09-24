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
