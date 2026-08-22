package messaging

import (
	"testing"
	"time"

	"shingo/protocol"
)

// The fault clock is DERIVED from the status, exactly like queue_reason and for
// exactly the reason documented there: an empty field on the wire is
// indistinguishable from an absent one, so a pushed clear cannot be trusted and
// a set-only write would latch. See queue_reason_clear_test.go.

func faultedPush(uuid string, since time.Time) *protocol.OrderUpdate {
	deadline := since.Add(45 * time.Minute)
	return &protocol.OrderUpdate{
		OrderUUID:         uuid,
		Status:            string(protocol.StatusFaulted),
		Detail:            "Replanning",
		FaultSince:        &since,
		FaultDeadline:     &deadline,
		FaultNoticeAfterS: 60,
		FaultRef: &protocol.TermRef{
			Node: "ALN_003", VendorCode: 60011, VendorDesc: "cannot replan",
		},
	}
}

func TestHandleOrderUpdate_StoresTheFaultClock(t *testing.T) {
	db := testHandlerDB(t)
	h := testHandler(t, db)
	since := time.Now().UTC().Add(-30 * time.Second).Truncate(time.Second)

	id, err := db.CreateOrder("fault-clock-1", protocol.OrderTypeRetrieve, nil, false, 1, "LINE-1", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	h.HandleOrderUpdate(nil, &protocol.OrderUpdate{OrderUUID: "fault-clock-1", Status: string(protocol.StatusInTransit)})
	h.HandleOrderUpdate(nil, faultedPush("fault-clock-1", since))

	o, err := db.GetOrder(id)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if o.FaultSince == nil || !o.FaultSince.Equal(since) {
		t.Errorf("fault_since = %v, want %v", o.FaultSince, since)
	}
	if o.FaultDeadline == nil || !o.FaultDeadline.Equal(since.Add(45*time.Minute)) {
		t.Errorf("fault_deadline = %v", o.FaultDeadline)
	}
	if o.FaultNoticeAfterS != 60 {
		t.Errorf("fault_notice_after_s = %d, want 60", o.FaultNoticeAfterS)
	}
	if o.FaultRef == nil || o.FaultRef.VendorCode != 60011 || o.FaultRef.VendorDesc != "cannot replan" {
		t.Errorf("fault_ref = %+v, want the fleet's reason", o.FaultRef)
	}
}

// Moving again clears the clock. Without this the board would keep ticking a
// fault the order recovered from — the queue_reason defect, in a new field.
func TestHandleOrderUpdate_ClearsTheFaultClockOnceMoving(t *testing.T) {
	db := testHandlerDB(t)
	h := testHandler(t, db)
	since := time.Now().UTC().Add(-30 * time.Second).Truncate(time.Second)

	id, err := db.CreateOrder("fault-clock-2", protocol.OrderTypeRetrieve, nil, false, 1, "LINE-1", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	h.HandleOrderUpdate(nil, &protocol.OrderUpdate{OrderUUID: "fault-clock-2", Status: string(protocol.StatusInTransit)})
	h.HandleOrderUpdate(nil, faultedPush("fault-clock-2", since))
	if o, _ := db.GetOrder(id); o.FaultSince == nil {
		t.Fatal("setup: the faulted push should have stored a clock")
	}

	// The recovery: Core reports the order moving again. It carries none of the
	// fault fields, which is the whole point — the clear is derived, not sent.
	h.HandleOrderUpdate(nil, &protocol.OrderUpdate{
		OrderUUID: "fault-clock-2",
		Status:    string(protocol.StatusInTransit),
		Detail:    "Recovered after 30 s",
	})

	o, _ := db.GetOrder(id)
	if o.FaultSince != nil || o.FaultDeadline != nil {
		t.Errorf("clock survived the recovery: since=%v deadline=%v", o.FaultSince, o.FaultDeadline)
	}
	if o.FaultNoticeAfterS != 0 {
		t.Errorf("threshold survived the recovery: %d", o.FaultNoticeAfterS)
	}
	if o.FaultRef != nil {
		t.Errorf("the fleet's reason survived the recovery: %+v", o.FaultRef)
	}
}

// An older Core sends the status and no clock. The Edge stores the status and
// simply has no clock to tick — it must not invent one from the zero time.
func TestHandleOrderUpdate_FaultedWithNoClockFromAnOlderCore(t *testing.T) {
	db := testHandlerDB(t)
	h := testHandler(t, db)

	id, err := db.CreateOrder("fault-clock-3", protocol.OrderTypeRetrieve, nil, false, 1, "LINE-1", "", "", "", false, "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	h.HandleOrderUpdate(nil, &protocol.OrderUpdate{OrderUUID: "fault-clock-3", Status: string(protocol.StatusInTransit)})
	h.HandleOrderUpdate(nil, &protocol.OrderUpdate{
		OrderUUID: "fault-clock-3",
		Status:    string(protocol.StatusFaulted),
		Detail:    "fleet state: FAILED",
	})

	o, _ := db.GetOrder(id)
	if o.Status != protocol.StatusFaulted {
		t.Errorf("status = %q, want faulted", o.Status)
	}
	if o.FaultSince != nil || o.FaultDeadline != nil || o.FaultRef != nil {
		t.Errorf("an older Core's push must leave the clock empty: %+v", o)
	}
}
