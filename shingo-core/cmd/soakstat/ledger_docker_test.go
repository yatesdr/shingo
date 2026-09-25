//go:build docker

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// ledgerLane seeds group -> LANE -> three slots (depth 1 at the mouth, 3 at the
// back) and returns the lane id and the slots' ids by depth.
func ledgerLane(t *testing.T, db *store.DB, prefix string) (laneID int64, slot [4]int64) {
	t.Helper()
	var group int64
	err := db.QueryRow(`INSERT INTO nodes (name, node_type_id) VALUES ($1, (SELECT id FROM node_types WHERE code = 'NGRP'))
		RETURNING id`, prefix+"_GRP").Scan(&group)
	testutil.MustNoErr(t, err, "seed the group")
	err = db.QueryRow(`INSERT INTO nodes (name, node_type_id, parent_id) VALUES ($1, (SELECT id FROM node_types WHERE code = 'LANE'), $2)
		RETURNING id`, prefix+"_LANE", group).Scan(&laneID)
	testutil.MustNoErr(t, err, "seed the lane")
	for d := 1; d <= 3; d++ {
		err = db.QueryRow(`INSERT INTO nodes (name, parent_id, depth) VALUES ($1, $2, $3) RETURNING id`,
			fmt.Sprintf("%s_S%d", prefix, d), laneID, d).Scan(&slot[d])
		testutil.MustNoErr(t, err, "seed a slot")
	}
	return laneID, slot
}

func seedRes(t *testing.T, db *store.DB, orderID int64, kind, state, reservedBy string, nodeID int64) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO reservations (order_id, resource_kind, state, reserved_by, node_id, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, orderID, kind, state, reservedBy, nodeID, time.Now().Add(time.Hour))
	testutil.MustNoErr(t, err, "seed a "+state+" "+kind+" reservation")
}

func ledgerOrderByID(t *testing.T, l *ledgerRows, id int64) ledgerOrder {
	t.Helper()
	for _, o := range l.orders {
		if o.ID == id {
			return o
		}
	}
	t.Fatalf("order %d not in the ledger", id)
	return ledgerOrder{}
}

func ledgerSlotByName(t *testing.T, l *ledgerRows, name string) ledgerSlot {
	t.Helper()
	for _, s := range l.slots {
		if s.Node == name {
			return s
		}
	}
	t.Fatalf("slot %s not in the ledger", name)
	return ledgerSlot{}
}

// TestLedger_BooksAndClasses renders the brief's three fixtures and asserts
// each one's books and at-rest class, and the slot rows they make:
//   - a pending order with a pending slot reservation: soft only, not at rest;
//     the slot is spoken for (pending) and the two in front of it entomb it;
//   - a dispatched order with a claimed bin and a fleet id: the claimed book;
//   - a dig parent, still sourcing, holding a confirmed `lanelock` mouth: the
//     committed book's dig lock, dig-lock-at-rest and not holds-at-rest.
//
// It also reads a lifted bin (in _TRANSIT, last set down in the middle slot).
// The statement count is the ledger's cost: measured and recorded in the B0
// report, not pinned here.
func TestLedger_BooksAndClasses(t *testing.T) {
	t.Parallel()
	testdb.DisableWedgeSweep(t, "seeds orders holding at rest on purpose")
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	lane, slot := ledgerLane(t, db, "LDG")

	soft := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = protocol.StatusQueued })
	seedRes(t, db, soft.ID, "slot", "pending", "", slot[3])

	moving := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = protocol.StatusDispatched })
	_, err := db.Exec(`UPDATE orders SET vendor_order_id = 'sg-ledger-1' WHERE id = $1`, moving.ID)
	testutil.MustNoErr(t, err, "give the order a fleet id")
	b := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "LDG-MOVING")
	_, err = db.Exec(`UPDATE bins SET claimed_by = $1 WHERE id = $2`, moving.ID, b.ID)
	testutil.MustNoErr(t, err, "claim the bin")

	dig := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = protocol.StatusSourcing })
	seedRes(t, db, dig.ID, "mouth", "confirmed", orders.DigLockTags[0], lane)

	var transit int64
	testutil.MustNoErr(t, db.QueryRow(`SELECT id FROM nodes WHERE name = '_TRANSIT'`).Scan(&transit), "find _TRANSIT")
	air := testdb.CreateBinAtNode(t, db, sd.Payload.Code, transit, "LDG-AIR")
	_, err = db.Exec(`INSERT INTO audit_log (entity_type, entity_id, action, new_value) VALUES ('bin', $1, 'moved', $2)`,
		air.ID, fmt.Sprintf("payload=%s node=%d", sd.Payload.Code, slot[2]))
	testutil.MustNoErr(t, err, "audit the bin's last set-down")

	l, err := takeLedgerSample(db.DB, 7, time.Now(), nil)
	testutil.MustNoErr(t, err, "take a sample")

	o := ledgerOrderByID(t, l, soft.ID)
	if o.Reserved != (ledgerKinds{Slot: 1}) || o.Committed == nil || *o.Committed != (ledgerHard{}) || o.Claimed != nil ||
		o.HoldsAtRest || o.DigLockAtRest {
		t.Errorf("pending order: reserved %+v committed %+v claimed %+v at-rest %v/%v; want one pending slot, "+
			"an empty committed book, not at rest", o.Reserved, o.Committed, o.Claimed, o.HoldsAtRest, o.DigLockAtRest)
	}

	o = ledgerOrderByID(t, l, moving.ID)
	if !o.Fleet || o.Committed != nil || o.Claimed == nil || *o.Claimed != (ledgerHard{Bins: 1}) || o.HoldsAtRest {
		t.Errorf("dispatched order: fleet %v committed %+v claimed %+v holds-at-rest %v; want the claimed book "+
			"holding one bin", o.Fleet, o.Committed, o.Claimed, o.HoldsAtRest)
	}

	o = ledgerOrderByID(t, l, dig.ID)
	if o.Committed == nil || *o.Committed != (ledgerHard{DigLock: 1}) || o.HoldsAtRest || !o.DigLockAtRest {
		t.Errorf("dig parent: committed %+v at-rest %v/%v; want one dig lock, dig-lock-at-rest only",
			o.Committed, o.HoldsAtRest, o.DigLockAtRest)
	}

	back := ledgerSlotByName(t, l, "LDG_S3")
	if !back.SpokenFor || !reflect.DeepEqual(back.ResPending, []int64{soft.ID}) || back.Entombing {
		t.Errorf("back slot: spoken %v pending %v entombing %v; want spoken for by %d's pending reservation",
			back.SpokenFor, back.ResPending, back.Entombing, soft.ID)
	}
	for _, name := range []string{"LDG_S1", "LDG_S2"} {
		s := ledgerSlotByName(t, l, name)
		if !reflect.DeepEqual(s.Entombs, []string{"LDG_S3"}) || !reflect.DeepEqual(s.LaneDigLock, []int64{dig.ID}) {
			t.Errorf("%s: entombs %v, lane dig lock %v; want [LDG_S3] and [%d]", name, s.Entombs, s.LaneDigLock, dig.ID)
		}
	}
	if s := ledgerSlotByName(t, l, "LDG_S2"); s.Lifted != "LDG-AIR" || s.Bin != "" {
		t.Errorf("middle slot: lifted %q stored %q; want LDG-AIR in the air from it, nothing stored", s.Lifted, s.Bin)
	}

	var buf bytes.Buffer
	testutil.MustNoErr(t, l.write(&buf), "write the sample")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if want := 1 + len(l.orders) + len(l.slots); len(lines) != want {
		t.Fatalf("wrote %d lines, want %d", len(lines), want)
	}
	var head map[string]any
	testutil.MustNoErr(t, json.Unmarshal([]byte(lines[0]), &head), "parse the sample record")
	if head["rec"] != "sample" || head["n"] != float64(7) {
		t.Errorf("first line = %s, want the sample record for n 7", lines[0])
	}
}

// TestLedger_ReadingsEqualTheSQL holds the ledger's three restatements to the
// SQL they restate, on one database: holds-at-rest against ListHoldsAtRest,
// dig-lock-at-rest against ListDigLocksAtRest, and the entombing flag against
// store.SlotsThatWouldEntombASpokenForSlot. The seeds cover every kind of hard
// hold, the four exceptions (dispatched, compound leg, terminal, pending only)
// and the three spoken-for spellings (claim, reservation, bare destination).
func TestLedger_ReadingsEqualTheSQL(t *testing.T) {
	t.Parallel()
	testdb.DisableWedgeSweep(t, "seeds orders holding at rest on purpose")
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	laneA, a := ledgerLane(t, db, "LPA")
	_, bSlots := ledgerLane(t, db, "LPB")

	order := func(status protocol.Status, parent *int64) int64 {
		return testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = status; o.ParentOrderID = parent }).ID
	}
	holdBin := func(orderID int64, label string) {
		b := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, label)
		_, err := db.Exec(`UPDATE bins SET claimed_by = $1 WHERE id = $2`, orderID, b.ID)
		testutil.MustNoErr(t, err, "claim a bin")
	}

	holdBin(order(protocol.StatusSourcing, nil), "LP-WAIT-BIN")
	claimer := order(protocol.StatusQueued, nil)
	_, err := db.Exec(`UPDATE nodes SET claimed_by = $1 WHERE id = $2`, claimer, a[3])
	testutil.MustNoErr(t, err, "claim a slot")
	seedRes(t, db, order(protocol.StatusQueued, nil), "slot", "confirmed", "", bSlots[3])
	seedRes(t, db, order(protocol.StatusSourcing, nil), "mouth", "confirmed", "lanegate", laneA)
	seedRes(t, db, order(protocol.StatusPending, nil), "occupancy", "confirmed", "", laneA)
	seedRes(t, db, order(protocol.StatusQueued, nil), "mouth", "confirmed", orders.DigLockTags[1], laneA)
	holdBin(order(protocol.StatusDispatched, nil), "LP-DISPATCHED")
	parent := order(protocol.StatusReshuffling, nil)
	holdBin(order(protocol.StatusPending, &parent), "LP-LEG")
	holdBin(order(protocol.StatusConfirmed, nil), "LP-TERMINAL")
	seedRes(t, db, order(protocol.StatusQueued, nil), "slot", "pending", "", bSlots[2])
	bare := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = protocol.StatusSourcing })
	var bName string
	testutil.MustNoErr(t, db.QueryRow(`SELECT name FROM nodes WHERE id = $1`, a[2]).Scan(&bName), "slot name")
	_, err = db.Exec(`UPDATE orders SET delivery_node = $1 WHERE id = $2`, bName, bare.ID)
	testutil.MustNoErr(t, err, "name a bare destination")

	l, err := takeLedgerSample(db.DB, 1, time.Now(), nil)
	testutil.MustNoErr(t, err, "take a sample")

	var gotHolds, gotDig []int64
	for _, o := range l.orders {
		if o.HoldsAtRest {
			gotHolds = append(gotHolds, o.ID)
		}
		if o.DigLockAtRest {
			gotDig = append(gotDig, o.ID)
		}
	}
	held, err := orders.ListHoldsAtRest(db.DB)
	testutil.MustNoErr(t, err, "ListHoldsAtRest")
	var wantHolds []int64
	for _, h := range held {
		wantHolds = append(wantHolds, h.OrderID)
	}
	if len(wantHolds) != 5 || !reflect.DeepEqual(gotHolds, wantHolds) {
		t.Errorf("ledger holds-at-rest %v, ListHoldsAtRest %v; want the same five", gotHolds, wantHolds)
	}
	dug, err := orders.ListDigLocksAtRest(db.DB)
	testutil.MustNoErr(t, err, "ListDigLocksAtRest")
	var wantDig []int64
	for _, d := range dug {
		wantDig = append(wantDig, d.OrderID)
	}
	if len(wantDig) != 1 || !reflect.DeepEqual(gotDig, wantDig) {
		t.Errorf("ledger dig-lock-at-rest %v, ListDigLocksAtRest %v; want the same one", gotDig, wantDig)
	}

	var groups []int64
	rows, err := db.Query(`SELECT id FROM nodes WHERE name IN ('LPA_GRP', 'LPB_GRP')`)
	testutil.MustNoErr(t, err, "groups")
	for rows.Next() {
		var id int64
		testutil.MustNoErr(t, rows.Scan(&id), "scan group")
		groups = append(groups, id)
	}
	testutil.MustNoErr(t, rows.Close(), "close")
	want := map[string]bool{}
	for _, g := range groups {
		ids, err := db.SlotsThatWouldEntombASpokenForSlot(g)
		testutil.MustNoErr(t, err, "SlotsThatWouldEntombASpokenForSlot")
		for id := range ids {
			var name string
			testutil.MustNoErr(t, db.QueryRow(`SELECT name FROM nodes WHERE id = $1`, id).Scan(&name), "slot name")
			want[name] = true
		}
	}
	got := map[string]bool{}
	for _, s := range l.slots {
		if s.Entombing && strings.HasPrefix(s.Node, "LP") {
			got[s.Node] = true
		}
	}
	// LPA: S3 claimed, S2 a bare destination, so S1 and S2 entomb. LPB: S3
	// confirmed, S2 pending, so S1 and S2 entomb.
	if !reflect.DeepEqual(got, want) || len(want) != 4 {
		t.Errorf("ledger entombing %v, SlotsThatWouldEntombASpokenForSlot %v; want the same four", keys(got), keys(want))
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
