package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
)

// The running net on the wire (SYNTH-round2 §3): an optional field on both
// count messages, omitted when nil so an old Edge's message and a new one with
// no net are byte-identical, and ignored by a decoder that does not know it.

// binUOPDeltaBeforeNet is BinUOPDelta as it was before the net: what an old
// Core decodes a new Edge's message into.
type binUOPDeltaBeforeNet struct {
	Station     string            `json:"station"`
	BinID       int64             `json:"bin_id"`
	PayloadCode string            `json:"payload_code"`
	Delta       int               `json:"delta"`
	Reason      BinUOPDeltaReason `json:"reason"`
	SequenceID  int64             `json:"sequence_id"`
	Epoch       int64             `json:"epoch"`
	WindowStart time.Time         `json:"window_start"`
	WindowEnd   time.Time         `json:"window_end"`
}

func TestRunningNet_WireShape(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 1, 14, 30, 0, 0, time.UTC)
	base := BinUOPDelta{
		BinID: 42, PayloadCode: "PART-A", Delta: -3, Reason: ReasonConsumeTick,
		SequenceID: 17, Epoch: 4, WindowStart: t0, WindowEnd: t0.Add(5 * time.Second),
	}

	without, err := json.Marshal(base)
	testutil.MustNoErr(t, err, "marshal without net")
	if strings.Contains(string(without), `"net"`) {
		t.Errorf("a nil Net is on the wire: %s", without)
	}

	withNet := base
	n := int64(-123456)
	withNet.Net = &n
	with, err := json.Marshal(withNet)
	testutil.MustNoErr(t, err, "marshal with net")
	// The per-message cost the synth estimated at ~12 bytes: `,"net":-123456`.
	if got := len(with) - len(without); got != len(`,"net":-123456`) {
		t.Errorf("net adds %d bytes, want %d", got, len(`,"net":-123456`))
	}

	var back BinUOPDelta
	testutil.MustNoErr(t, json.Unmarshal(with, &back), "decode new")
	if back.Net == nil || *back.Net != n {
		t.Errorf("round-trip Net = %v, want %d", back.Net, n)
	}
	var old BinUOPDelta
	testutil.MustNoErr(t, json.Unmarshal(without, &old), "decode old")
	if old.Net != nil {
		t.Errorf("an old Edge's message decoded with Net = %v, want nil", *old.Net)
	}

	// A new Edge's message into an old Core's struct: the net is dropped and
	// every other field survives, so an old Core applies it as it always did.
	var oldCore binUOPDeltaBeforeNet
	testutil.MustNoErr(t, json.Unmarshal(with, &oldCore), "decode into the old shape")
	if oldCore.Delta != -3 || oldCore.SequenceID != 17 || oldCore.Epoch != 4 || !oldCore.WindowEnd.Equal(base.WindowEnd) {
		t.Errorf("old-shape decode = %+v, want the same delta, seq, epoch and window", oldCore)
	}

	bucket := LinesideBucketDelta{CoreNodeName: "N", PayloadCode: "P", Delta: 5, SequenceID: 2, Net: &n}
	bj, err := json.Marshal(bucket)
	testutil.MustNoErr(t, err, "marshal bucket")
	var bb LinesideBucketDelta
	testutil.MustNoErr(t, json.Unmarshal(bj, &bb), "decode bucket")
	if bb.Net == nil || *bb.Net != n {
		t.Errorf("bucket round-trip Net = %v, want %d", bb.Net, n)
	}
}
