//go:build docker

package uop_test

import (
	"encoding/json"
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/uop"
)

// Lane H, "Also in H" (BRIEF-unification-C2c 2026-10-10; DESIGN-H §9.4):
// BinEpochRefresh is not sent for a bin that is not at a seat, and the one
// that is sent goes to the station whose count was dropped, not to every
// station. r6 (by the orc): 18 refreshes, 15 of them to _TRANSIT.
//
// At the base, repairEpoch (applier.go) skips only "at no node" and sends by
// broadcast, so both pins are RED. TestQuietPlant_TheDropAnswers parks its bin
// at the storage node and pins the old door; it converts with this.

// refreshDsts returns the destination station of every epoch refresh queued
// for the bin.
func refreshDsts(t *testing.T, db *store.DB, binID int64) []string {
	t.Helper()
	rows, err := db.Query(`SELECT payload FROM outbox WHERE msg_type=$1 ORDER BY id`,
		"data."+protocol.SubjectBinEpochRefresh)
	testutil.MustNoErr(t, err, "read outbox")
	defer rows.Close()
	var out []string
	for rows.Next() {
		var raw []byte
		testutil.MustNoErr(t, rows.Scan(&raw), "scan outbox row")
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(raw, &env), "decode envelope")
		var data protocol.Data
		testutil.MustNoErr(t, env.DecodePayload(&data), "decode data")
		var r protocol.BinEpochRefresh
		testutil.MustNoErr(t, json.Unmarshal(data.Body, &r), "decode refresh")
		if r.BinID == binID {
			out = append(out, env.Dst.Station)
		}
	}
	return out
}

// dropAStaleCount advances the bin a generation behind the station's back and
// applies one count stamped with the generation that ended.
func dropAStaleCount(t *testing.T, db *store.DB, svc *uop.InventoryDeltaService, binID int64) {
	t.Helper()
	_, err := db.Exec(`UPDATE bins SET delta_epoch=2 WHERE id=$1`, binID)
	testutil.MustNoErr(t, err, "advance the carrier a generation")
	d := makeBinDelta(binID, "PART-A", -1, 1, protocol.ReasonConsumeTick)
	d.Epoch = 1
	if err := svc.ApplyBinUOPDelta(testStation, d); !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Fatalf("stale count = %v, want ErrInventoryDeltaSkipped", err)
	}
}

// TestPinH_EpochRefreshIsNotSentForABinAtStorage — a carrier at a storage slot
// is at no seat; no station models it, so no refresh.
func TestPinH_EpochRefreshIsNotSentForABinAtStorage(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := repairingService(db)
	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-H-REFRESH-STOR", "PART-A", 100)

	dropAStaleCount(t, db, svc, bin.ID)

	if got := refreshDsts(t, db, bin.ID); len(got) != 0 {
		t.Errorf("refreshes for a bin at a storage slot = %d, want 0", len(got))
	}
}

// TestPinH_EpochRefreshIsAddressedToTheDroppingStation — at a line seat the
// refresh is sent, to the station whose count was dropped (the one behind).
func TestPinH_EpochRefreshIsAddressedToTheDroppingStation(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := repairingService(db)
	bin := createTestBin(t, db, sd.LineNode.ID, "BIN-H-REFRESH-LINE", "PART-A", 100)

	dropAStaleCount(t, db, svc, bin.ID)

	got := refreshDsts(t, db, bin.ID)
	if len(got) != 1 {
		t.Fatalf("refreshes = %d, want 1", len(got))
	}
	if got[0] != testStation {
		t.Errorf("refresh addressed to %q, want %q (the station that is behind), not a broadcast", got[0], testStation)
	}
}
