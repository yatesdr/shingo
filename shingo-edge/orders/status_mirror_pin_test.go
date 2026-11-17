package orders

// status_mirror_pin_test.go — the RELEASE tap's status write, pinned before the
// mirror moves to one versioned applier. The pins of Core's messages and the
// reconcile are engine/status_mirror_pin_test.go; this is the one write the
// Edge makes on its own word.

import (
	"encoding/json"
	"slices"
	"sort"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// TestStatusMirrorPin_TheReleaseTapWritesInTransit: a leg staged at the
// station's wait 0, with the release act's intent on the row. ReleaseOrder
// queues one order.release naming that wait and stamps the intent's sent_at in
// the same transaction, then moves the row to in_transit at the click, without
// waiting for Core: a history row "released from staging" and an
// EventOrderStatusChanged. (TestReleaseOrder_HappyPath asserts the in_transit
// alone.)
//
// Expected to flip when the tap stops writing the status of an order whose
// status Core versions: the outbox row and the sent_at stay, and the row reads
// staged until Core's own in_transit.
func TestStatusMirrorPin_TheReleaseTapWritesInTransit(t *testing.T) {
	t.Parallel()
	db := testManagerDB(t)
	emitter := &capturingEmitter{}
	mgr := NewManager(db, emitter, "edge")

	id, err := db.CreateOrder("tap-leg", TypeComplex, nil, false, 1, "LINE-1", "", "", protocol.Height{}, false, "", "", "")
	id = testutil.Must(t, id, err, "create the leg")
	testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(StatusStaged)), "the leg is staged")
	wait := 0
	testutil.MustNoErr(t, mgr.RecordWaitPoint("tap-leg", &wait, protocol.WaitKindStation), "at the station's wait 0")
	testutil.MustNoErr(t, db.SetOrderReleaseIntent(id, `{"station_wait":0,"purpose":"swap"}`), "the act's intent")

	testutil.MustNoErr(t, mgr.ReleaseOrder(id, nil, "operator", &wait), "the RELEASE tap")

	o, err := db.GetOrder(id)
	testutil.MustNoErr(t, err, "read the leg")
	if o.Status != StatusInTransit {
		t.Errorf("after the tap the row reads %s; pinned: in_transit at the click", o.Status)
	}
	var intent struct {
		StationWait int    `json:"station_wait"`
		SentAt      string `json:"sent_at"`
	}
	testutil.MustNoErr(t, json.Unmarshal([]byte(o.ReleaseIntent), &intent), "decode the intent")
	if intent.StationWait != 0 || intent.SentAt == "" {
		t.Errorf("the intent reads %s; want wait 0 with its sent_at stamped", o.ReleaseIntent)
	}

	var rel protocol.OrderRelease
	decodeOnlyOutboxPayload(t, db, protocol.TypeOrderRelease, &rel)
	if rel.OrderUUID != "tap-leg" || rel.StationWait == nil || *rel.StationWait != 0 || rel.CalledBy != "operator" {
		t.Errorf("the order.release names %q, wait %v, by %q; want tap-leg, wait 0, by operator",
			rel.OrderUUID, rel.StationWait, rel.CalledBy)
	}

	hs, err := db.ListOrderHistory(id)
	testutil.MustNoErr(t, err, "read the history")
	sort.Slice(hs, func(i, j int) bool { return hs[i].ID < hs[j].ID })
	var history []string
	for _, h := range hs {
		history = append(history, string(h.OldStatus)+"->"+string(h.NewStatus)+": "+h.Detail)
	}
	if want := []string{"staged->in_transit: " + ReleasedFromStagingDetail}; !slices.Equal(history, want) {
		t.Errorf("history %q, want %q", history, want)
	}
	if want := []string{"staged→in_transit"}; !slices.Equal(emitter.status, want) {
		t.Errorf("status events %q, want %q", emitter.status, want)
	}
}
