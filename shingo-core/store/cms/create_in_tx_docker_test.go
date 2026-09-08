//go:build docker

package cms_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/cms"
	"shingocore/store/nodes"
)

// create_in_tx_docker_test.go — CreateInTx, and the ordering rule it exists for.
//
// The clear door writes its departure rows and clears the bin in ONE transaction.
// Rows written after the clear cannot be reconstructed if their write fails,
// because the clear destroyed the uop_remaining and the manifest they were derived
// from; rows committed before it become a departure the plant never made if the
// clear then fails. One transaction is the only arrangement with neither failure,
// and CreateInTx is what makes it reachable — cms.Create owns its own.

// TestCreateInTx_TheCallerOwnsTheCommit: rows written through CreateInTx are not
// there until the caller commits. A primitive that committed on its own would put
// the departure in the ledger while the clear it describes could still fail.
func TestCreateInTx_TheCallerOwnsTheCommit(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	node := &nodes.Node{Name: "CIT-NODE-ROLLBACK", Enabled: true}
	if err := nodes.Create(db.DB, node); err != nil {
		t.Fatalf("create node: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	rows := []*cms.Transaction{
		{NodeID: node.ID, NodeName: node.Name, CatID: "CAT-1", Delta: -240,
			SourceType: cms.SourceTypeClear, Storeroom: "ASTEST"},
	}
	if err := cms.CreateInTx(tx, rows); err != nil {
		t.Fatalf("CreateInTx: %v", err)
	}
	if rows[0].ID == 0 {
		t.Error("CreateInTx did not stamp the row id — the wire's EntryNumber is that id")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	got, err := cms.ListByNode(db.DB, node.ID, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a rolled-back CreateInTx left %d rows behind: %+v — the primitive must not "+
			"commit on its own, or a departure lands in the ledger for a clear that failed",
			len(got), got)
	}
}

// TestCreateInTx_AFailedRowTakesTheCallersWorkWithIt is the ordering rule as a
// test. A departure row naming a bin that does not exist violates the bin_id
// foreign key, so the insert fails — and the caller's own write, standing here for
// the manifest clear, must be gone with it.
//
// THE FAILURE THIS PINS is the one that cannot be repaired afterwards: a bin
// cleared with no row recording what left it. There is nothing to reconstruct the
// quantity from, because the quantity was derived from what the clear destroyed.
func TestCreateInTx_AFailedRowTakesTheCallersWorkWithIt(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	node := &nodes.Node{Name: "CIT-NODE-ATOMIC", Enabled: true}
	if err := nodes.Create(db.DB, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	// A row of plant state standing in for the bin the clear would empty.
	if _, err := db.Exec(`INSERT INTO node_properties (node_id, key, value)
		VALUES ($1, 'cit_standin', 'BEFORE')`, node.ID); err != nil {
		t.Fatalf("seed stand-in state: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// The caller's irreversible act goes FIRST inside the transaction, exactly as
	// the clear does not: this is the arrangement being tested, and the point is
	// that the order inside one transaction does not matter because a failure
	// unwinds all of it.
	if _, err := tx.Exec(`UPDATE node_properties SET value='CLEARED'
		WHERE node_id=$1 AND key='cit_standin'`, node.ID); err != nil {
		t.Fatalf("stand-in clear: %v", err)
	}

	missingBin := int64(9_000_000_001)
	rows := []*cms.Transaction{
		{NodeID: node.ID, NodeName: node.Name, CatID: "CAT-1", Delta: -240,
			SourceType: cms.SourceTypeClear, Storeroom: "ASTEST",
			BinID: &missingBin},
	}
	if err := cms.CreateInTx(tx, rows); err == nil {
		t.Fatal("a row naming a bin that does not exist inserted cleanly — the bin_id " +
			"foreign key is what makes this failure reachable in a test")
	}
	// What the caller does on the error: unwind everything.
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var value string
	if err := db.QueryRow(`SELECT value FROM node_properties
		WHERE node_id=$1 AND key='cit_standin'`, node.ID).Scan(&value); err != nil {
		t.Fatalf("read stand-in state: %v", err)
	}
	if value != "BEFORE" {
		t.Errorf("the stand-in state is %q, want BEFORE — the caller's act survived a failed "+
			"row write, which for the real door is a bin cleared with nothing recording "+
			"what left it", value)
	}
	if got, err := cms.ListByNode(db.DB, node.ID, 50, 0); err != nil || len(got) != 0 {
		t.Errorf("rows after the failure = %d (err %v), want none", len(got), err)
	}
}

// TestCreateInTx_AndCreateAgreeOnWhatTheyWrite: Create delegates to CreateInTx, so
// the two doors cannot drift in which columns they bind. A second copy of that
// INSERT is a second place for a column to go missing.
func TestCreateInTx_AndCreateAgreeOnWhatTheyWrite(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	node := &nodes.Node{Name: "CIT-NODE-AGREE", Enabled: true}
	if err := nodes.Create(db.DB, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	row := func(cat string) *cms.Transaction {
		return &cms.Transaction{
			NodeID: node.ID, NodeName: node.Name, CatID: cat, Delta: -12,
			SourceType: cms.SourceTypeClear, Storeroom: "SM01",
			BinLabel: "BIN-AGREE", PayloadCode: "P-AGREE", RobotID: "",
		}
	}
	if err := cms.Create(db.DB, []*cms.Transaction{row("VIA-CREATE")}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := cms.CreateInTx(tx, []*cms.Transaction{row("VIA-CREATEINTX")}); err != nil {
		t.Fatalf("CreateInTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	got, err := cms.ListByNode(db.DB, node.ID, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("wrote %d rows, want 2", len(got))
	}
	byCat := map[string]*cms.Transaction{}
	for _, g := range got {
		byCat[g.CatID] = g
	}
	a, b := byCat["VIA-CREATE"], byCat["VIA-CREATEINTX"]
	if a == nil || b == nil {
		t.Fatalf("both doors did not write: %+v", got)
	}
	if a.Storeroom != b.Storeroom || a.BinLabel != b.BinLabel ||
		a.PayloadCode != b.PayloadCode || a.SourceType != b.SourceType || a.Delta != b.Delta {
		t.Errorf("the two doors wrote different rows:\n Create      = %+v\n CreateInTx  = %+v", a, b)
	}
	if a.PostingID != nil || b.PostingID != nil {
		t.Error("a new row must have posting_id NULL — NULL is the unposted queue")
	}
}
