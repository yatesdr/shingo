//go:build docker

package service

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
)

// announceTopic is the dispatch topic these tests wire the service to. Any
// string works — the point is that it is not empty, so the announcement is on.
const announceTopic = "shingo.dispatch.test"

func announcingService(db *store.DB) *BinManifestService {
	return NewBinManifestService(db, EpochAnnounce{Topic: announceTopic, CoreStation: "core.test"})
}

// outboxAdjustments returns every UOP-adjustment message sitting in the outbox
// for the given bin, decoded.
func outboxAdjustments(t *testing.T, db *store.DB, binID int64) []protocol.UOPAdjustment {
	t.Helper()
	rows, err := db.DB.Query(`SELECT payload FROM outbox WHERE msg_type = $1 ORDER BY id`,
		"data."+protocol.SubjectUOPAdjustment)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer rows.Close()
	var out []protocol.UOPAdjustment
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
		var adj protocol.UOPAdjustment
		if err := json.Unmarshal(data.Body, &adj); err != nil {
			t.Fatalf("decode adjustment: %v", err)
		}
		if adj.BinID == binID {
			out = append(out, adj)
		}
	}
	return out
}

// TestSetForProduction_AnnouncesTheNewGenerationOnce is the prevention half of
// the epoch fix.
//
// set_for_production is the most frequent reset in the plant and told nobody.
// The station kept reporting counts under the generation that had just ended
// and Core discarded all of them — at Hopkinsville, half of every production
// count, continuously, for as long as anybody has measured.
func TestSetForProduction_AnnouncesTheNewGenerationOnce(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := announcingService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-ANN-1", "", 0)

	epoch, err := svc.SetForProduction(bin.ID, `{"items":[{"catid":"PART-A","qty":40}]}`, "PART-A", 40)
	if err != nil {
		t.Fatalf("SetForProduction: %v", err)
	}

	msgs := outboxAdjustments(t, db, bin.ID)
	if len(msgs) != 1 {
		t.Fatalf("outbox holds %d announcements for bin %d, want exactly 1 — a reset that "+
			"tells nobody is the count loss; two would double-apply", len(msgs), bin.ID)
	}
	got := msgs[0]
	if got.Epoch != epoch {
		t.Errorf("announced epoch = %d, want %d — the message must carry the generation the "+
			"same statement produced", got.Epoch, epoch)
	}
	if got.NewRemaining != 40 {
		t.Errorf("announced count = %d, want 40 — a reset is a declaration and a declaration "+
			"carries its number; the count and the generation come from the same write",
			got.NewRemaining)
	}
	if got.CoreNodeName != sd.StorageNode.Name {
		t.Errorf("announced node = %q, want %q — nothing can apply a message it cannot address",
			got.CoreNodeName, sd.StorageNode.Name)
	}
}

// TestSetForProduction_AnnouncementRollsBackWithTheReset is the adjudication in
// executable form, and it is the test an after-commit notification cannot pass.
//
// The announcement is an outbox ROW, not a send. It commits with the reset or
// it disappears with it. A hook that fired after the commit would have nothing
// to roll back and would announce a generation that never happened; a hook that
// fired before the commit would have a window where the commit lands and the
// process dies with the message never sent — a station quietly wrong with
// nothing recording that it is.
func TestSetForProduction_AnnouncementRollsBackWithTheReset(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := announcingService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-ANN-2", "", 0)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := svc.setForProductionTx(tx, bin.ID, `{"items":[{"catid":"PART-A","qty":40}]}`, "PART-A", 40); err != nil {
		tx.Rollback()
		t.Fatalf("setForProductionTx: %v", err)
	}
	testutil.MustNoErr(t, tx.Rollback(), "rollback")

	if msgs := outboxAdjustments(t, db, bin.ID); len(msgs) != 0 {
		t.Errorf("outbox holds %d announcements after the reset rolled back, want 0 — the "+
			"plant was told a carrier started a generation that it never did", len(msgs))
	}
}

// TestClearForReuse_AnnouncesTheNewGeneration: the operator's clear on Core's
// admin screen. One of five reset routes; the whole point of putting the
// announcement inside the shared bump is that naming them individually stops
// being necessary, so this covers a second route to show the shape holds and
// leaves the other three to the census.
func TestClearForReuse_AnnouncesTheNewGeneration(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := announcingService(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BIN-ANN-3", "PART-A", 100)

	epoch, err := svc.ClearForReuse(bin.ID, nil)
	if err != nil {
		t.Fatalf("ClearForReuse: %v", err)
	}

	msgs := outboxAdjustments(t, db, bin.ID)
	if len(msgs) != 1 {
		t.Fatalf("outbox holds %d announcements, want exactly 1", len(msgs))
	}
	if msgs[0].Epoch != epoch {
		t.Errorf("announced epoch = %d, want %d", msgs[0].Epoch, epoch)
	}
	if msgs[0].NewRemaining != 0 {
		t.Errorf("announced count = %d, want 0 — a clear declares the carrier empty", msgs[0].NewRemaining)
	}
}

// TestBumpWithNoNodeAnnouncesNothing: a carrier sitting at no node has no
// station modelling it. There is nobody to tell, and a broadcast naming an
// empty node is a message every station would have to decide to ignore.
func TestBumpWithNoNodeAnnouncesNothing(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	svc := announcingService(db)

	bt, err := db.GetBinTypeByCode("DEFAULT")
	if err != nil {
		t.Fatalf("bin type: %v", err)
	}
	bin := &bins.Bin{BinTypeID: bt.ID, Label: "BIN-ANN-4", Status: "available"} // NodeID nil: nowhere
	testutil.MustNoErr(t, db.CreateBin(bin), "create unplaced bin")

	if _, err := svc.SetForProduction(bin.ID, `{"items":[{"catid":"PART-A","qty":40}]}`, "PART-A", 40); err != nil {
		t.Fatalf("SetForProduction: %v", err)
	}
	if msgs := outboxAdjustments(t, db, bin.ID); len(msgs) != 0 {
		t.Errorf("outbox holds %d announcements for a carrier at no node, want 0", len(msgs))
	}
}
