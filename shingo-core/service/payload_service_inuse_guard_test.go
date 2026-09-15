//go:build docker

package service

import (
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/payloads"
)

// mustPayload re-reads a payload and fails the test if the read errors, so a
// broken read reports its own cause instead of a nil dereference two lines on.
func mustPayload(t *testing.T, db *store.DB, id int64) *payloads.Payload {
	t.Helper()
	p, err := db.GetPayload(id)
	testutil.MustNoErr(t, err, "GetPayload")
	return p
}

// binCarrying creates a bin holding payloadCode, without needing a node.
//
// bins.node_id is nullable and nothing in this guard reads it: the question is
// only which bins name the code, wherever they physically are.
func binCarrying(t *testing.T, db *store.DB, label, payloadCode string) *bins.Bin {
	t.Helper()
	bt, err := db.GetBinTypeByCode("GUARD-TYPE")
	if err != nil || bt == nil {
		bt = &bins.BinType{Code: "GUARD-TYPE", Description: "in-use guard fixture"}
		testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	}
	b := &bins.Bin{BinTypeID: bt.ID, Label: label, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(b), "create bin")
	if payloadCode != "" {
		testutil.MustNoErr(t,
			db.SetBinManifest(b.ID, `{"items":[{"catid":"PART","qty":1}]}`, payloadCode, 1),
			"set manifest")
	}
	return b
}

// Deleting a template that bins still name would orphan them: the code stays on
// the bin, resolves no template, and the robot group silently degrades to the
// vendor default — any robot, including one too small for a full heavy bin.
func TestPayloadService_Delete_RefusedWhileBinsCarryTheCode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewPayloadService(db)

	p := makePayload(t, svc, "GUARD-DEL", "in use", 100)
	binCarrying(t, db, "GUARD-BIN-1", "GUARD-DEL")
	binCarrying(t, db, "GUARD-BIN-2", "GUARD-DEL")

	err := svc.Delete(p.ID)
	if err == nil {
		t.Fatal("Delete succeeded while two bins carried the code — the orphan this guard exists to prevent")
	}
	// The operator has to know how big the job is and where to go.
	if !strings.Contains(err.Error(), "2 bins are") {
		t.Errorf("message does not name the count: %v", err)
	}
	if !strings.Contains(err.Error(), "GUARD-BIN-1") {
		t.Errorf("message does not name the bins: %v", err)
	}
	if survived := mustPayload(t, db, p.ID); survived == nil {
		t.Error("payload was deleted despite the refusal")
	}
}

// Renaming is the same orphan by another route: the bins keep the OLD string.
func TestPayloadService_Update_RefusedWhenCodeChangesUnderLiveBins(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewPayloadService(db)

	p := makePayload(t, svc, "GUARD-REN", "in use", 100)
	binCarrying(t, db, "GUARD-BIN-3", "GUARD-REN")

	p.Code = "GUARD-REN-2"
	if err := svc.Update(p); err == nil {
		t.Fatal("Update renamed the code while a bin carried it")
	}
	got := mustPayload(t, db, p.ID)
	if got.Code != "GUARD-REN" {
		t.Errorf("code = %q, want the rename refused", got.Code)
	}
}

// The guard is on the CODE only. Everything else about a payload in active use
// must stay editable — including the near-empty relaxation this feature adds,
// which an engineer will want to tune while bins are on the floor.
func TestPayloadService_Update_OtherFieldsStayEditableWhileInUse(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewPayloadService(db)

	p := makePayload(t, svc, "GUARD-EDIT", "in use", 100)
	binCarrying(t, db, "GUARD-BIN-4", "GUARD-EDIT")

	p.Description = "retuned"
	p.NearEmptyEnabled = true
	p.NearEmptyRobotGroup = "LIGHT-600"
	p.NearEmptyThresholdPct = 20
	testutil.MustNoErr(t, svc.Update(p), "Update non-code fields")

	got := mustPayload(t, db, p.ID)
	if !got.NearEmptyEnabled || got.NearEmptyRobotGroup != "LIGHT-600" || got.NearEmptyThresholdPct != 20 {
		t.Errorf("near-empty config did not persist: %+v", got)
	}
	if got.Description != "retuned" {
		t.Errorf("description = %q, want retuned", got.Description)
	}
}

// Nothing carries the code: both operations proceed exactly as before.
func TestPayloadService_DeleteAndRename_AllowedWhenNoBinsCarryTheCode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewPayloadService(db)

	ren := makePayload(t, svc, "GUARD-FREE-1", "unused", 10)
	ren.Code = "GUARD-FREE-1B"
	testutil.MustNoErr(t, svc.Update(ren), "rename an unused payload")

	del := makePayload(t, svc, "GUARD-FREE-2", "unused", 10)
	testutil.MustNoErr(t, svc.Delete(del.ID), "delete an unused payload")
	// A deleted row may come back as (nil, nil) or as a not-found error
	// depending on the store's shape; both mean gone, and neither is a failure
	// worth asserting past. Only a live row is.
	if gone, err := db.GetPayload(del.ID); err == nil && gone != nil {
		t.Error("payload survived a delete that should have been allowed")
	}
}

// A bin already carrying a dead code keeps dispatching. The guard stops NEW
// orphans; it must not turn an existing one into a failure, which would convert
// a silent config problem into a stopped floor.
func TestPayloadService_ExistingOrphanIsLeftAlone(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	b := binCarrying(t, db, "GUARD-BIN-5", "NEVER-EXISTED")

	got, err := db.GetBin(b.ID)
	if err != nil {
		t.Fatalf("GetBin: %v", err)
	}
	if got.PayloadCode != "NEVER-EXISTED" {
		t.Fatalf("payload_code = %q, want the orphan preserved", got.PayloadCode)
	}
	// The joined payload columns come back at their defaults rather than
	// failing the read — which is what lets dispatch fall through to the
	// vendor default instead of erroring.
	if got.PayloadRobotGroup != "" || got.UOPCapacity != 0 {
		t.Errorf("orphaned bin joined non-default payload facts: %+v", got)
	}
}
