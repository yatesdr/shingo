//go:build docker

package uop_test

import (
	"encoding/json"
	"errors"
	"testing"

	"shingo/protocol"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
	"shingocore/uop"
)

const repairTopic = "shingo.dispatch.test"

func repairingService(db *store.DB) *uop.InventoryDeltaService {
	ann := service.EpochAnnounce{Topic: repairTopic, CoreStation: "core.test"}
	return uop.NewInventoryDeltaService(db, service.NewBinManifestService(db, ann), ann)
}

// outboxRefreshes returns every epoch-refresh message queued for the bin.
func outboxRefreshes(t *testing.T, db *store.DB, binID int64) []protocol.BinEpochRefresh {
	t.Helper()
	rows, err := db.Query(`SELECT payload FROM outbox WHERE msg_type=$1 ORDER BY id`,
		"data."+protocol.SubjectBinEpochRefresh)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()
	var out []protocol.BinEpochRefresh
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan outbox row: %v", err)
		}
		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		var data protocol.Data
		if err := env.DecodePayload(&data); err != nil {
			t.Fatalf("decode data payload: %v", err)
		}
		var r protocol.BinEpochRefresh
		if err := json.Unmarshal(data.Body, &r); err != nil {
			t.Fatalf("decode refresh: %v", err)
		}
		if r.BinID == binID {
			out = append(out, r)
		}
	}
	return out
}

// TestQuietPlant_TheDropAnswers is the repair, and it is the only mechanism in
// the design that works at a plant with no traffic.
//
// Hopkinsville is quiet in ORDERS and loud in TICKS: every order is terminal,
// nothing delivers, and the plant counts 3,200 times a day into a wall. Every
// mechanism that re-seeds the station's generation is keyed to order flow, so
// at Hopkinsville nothing re-seeds and the station stays behind until a human
// clears a carrier by hand. Springfield looks healthy only because its order
// traffic keeps accidentally doing the job.
//
// The drop is the one point in the system that holds all four facts at once —
// which carrier, which generation is current, which station is behind, and
// proof that it is behind. So the drop stops being a dead end and becomes the
// channel: Core replies with the current generation, the station adopts it, and
// the next count lands.
//
// This fixture is that plant. No orders, no deliveries. A carrier is reset with
// its announcement thrown away — the mixed-version case, or a station that was
// down when the announcement went out. Two ticks: the first is discarded and
// answered, the second lands.
func TestQuietPlant_TheDropAnswers(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := repairingService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-QUIET-1", "PART-A", 100)
	// The reset happened and the station never heard: Core is at generation 2,
	// the station is still stamping its counts with 1.
	_, err := db.Exec(`UPDATE bins SET delta_epoch=2 WHERE id=$1`, bin.ID)
	testutil.MustNoErr(t, err, "advance the carrier a generation")

	// First tick, stamped with the generation that ended.
	first := makeBinDelta(bin.ID, "PART-A", -7, 1, protocol.ReasonConsumeTick)
	first.Epoch = 1
	if err := svc.ApplyBinUOPDelta(testStation, first); !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Fatalf("first tick = %v, want ErrInventoryDeltaSkipped", err)
	}

	refreshes := outboxRefreshes(t, db, bin.ID)
	if len(refreshes) != 1 {
		t.Fatalf("outbox holds %d repairs after the first discarded count, want exactly 1 — "+
			"without one, this plant discards every count it makes until a human "+
			"intervenes; that is 1,600 in a row over three hours at Hopkinsville",
			len(refreshes))
	}
	if refreshes[0].Epoch != 2 {
		t.Errorf("repair carries generation %d, want 2 — the reply is the current stamp or it "+
			"is nothing", refreshes[0].Epoch)
	}
	if refreshes[0].CoreNodeName != sd.StorageNode.Name {
		t.Errorf("repair addressed to %q, want %q", refreshes[0].CoreNodeName, sd.StorageNode.Name)
	}

	// The station adopts and reports again under the right generation.
	second := makeBinDelta(bin.ID, "PART-A", -7, 2, protocol.ReasonConsumeTick)
	second.Epoch = 2
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, second), "second tick must land")

	var got int
	testutil.MustNoErr(t, db.QueryRow(`SELECT uop_remaining FROM bins WHERE id=$1`, bin.ID).Scan(&got), "read bin")
	if got != 93 {
		t.Errorf("uop_remaining = %d, want 93 — the first count was correctly discarded and the "+
			"second had to land; a repair that does not end the stall is not a repair", got)
	}
}

// TestQuietPlant_TheRepairDoesNotStorm: the reply is fire-and-forget and can be
// lost, and the station may be an older build that does not know the message at
// all. Either way the counts keep arriving and keep being discarded — 3,200 in
// a day at one plant — and a reply per discarded count would be a flood aimed
// at a station that is not listening.
//
// One reply per generation. If the station never adopts, the next reset makes a
// new generation and the reply goes out again.
func TestQuietPlant_TheRepairDoesNotStorm(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := repairingService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-QUIET-2", "PART-A", 100)
	_, err := db.Exec(`UPDATE bins SET delta_epoch=2 WHERE id=$1`, bin.ID)
	testutil.MustNoErr(t, err, "advance a generation")

	for seq := int64(1); seq <= 5; seq++ {
		d := makeBinDelta(bin.ID, "PART-A", -1, seq, protocol.ReasonConsumeTick)
		d.Epoch = 1
		if err := svc.ApplyBinUOPDelta(testStation, d); !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
			t.Fatalf("tick %d = %v, want ErrInventoryDeltaSkipped", seq, err)
		}
	}
	if n := len(outboxRefreshes(t, db, bin.ID)); n != 1 {
		t.Errorf("outbox holds %d repairs after five discarded counts, want 1 — a reply per "+
			"discarded count is a flood aimed at a station that is not listening", n)
	}

	// Another reset: a new generation, so the station is behind again in a way it
	// has not been told about.
	_, err = db.Exec(`UPDATE bins SET delta_epoch=3 WHERE id=$1`, bin.ID)
	testutil.MustNoErr(t, err, "advance another generation")

	d := makeBinDelta(bin.ID, "PART-A", -1, 6, protocol.ReasonConsumeTick)
	d.Epoch = 1
	if err := svc.ApplyBinUOPDelta(testStation, d); !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Fatalf("tick after second reset = %v, want ErrInventoryDeltaSkipped", err)
	}
	refreshes := outboxRefreshes(t, db, bin.ID)
	if len(refreshes) != 2 {
		t.Fatalf("outbox holds %d repairs after a second reset, want 2 — a station that never "+
			"adopted must be told about the new generation too", len(refreshes))
	}
	if refreshes[1].Epoch != 3 {
		t.Errorf("second repair carries generation %d, want 3", refreshes[1].Epoch)
	}
}

// TestRepairNotSentForACarrierAtNoNode: a carrier sitting nowhere has no
// station modelling it. There is nobody the reply could be for.
func TestRepairNotSentForACarrierAtNoNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := repairingService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-QUIET-3", "PART-A", 100)
	_, err := db.Exec(`UPDATE bins SET delta_epoch=2, node_id=NULL WHERE id=$1`, bin.ID)
	testutil.MustNoErr(t, err, "advance a generation and unplace the carrier")

	d := makeBinDelta(bin.ID, "PART-A", -1, 1, protocol.ReasonConsumeTick)
	d.Epoch = 1
	if err := svc.ApplyBinUOPDelta(testStation, d); !errors.Is(err, uop.ErrInventoryDeltaSkipped) {
		t.Fatalf("tick = %v, want ErrInventoryDeltaSkipped", err)
	}
	if n := len(outboxRefreshes(t, db, bin.ID)); n != 0 {
		t.Errorf("outbox holds %d repairs for a carrier at no node, want 0", n)
	}
}
