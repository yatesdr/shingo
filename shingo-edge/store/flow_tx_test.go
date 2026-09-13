package store

import (
	"database/sql"
	"errors"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// flow_tx_test.go — the claim and routing readers and writers accept a
// transaction.
//
// SaveFlow writes a whole flow in ONE transaction and checks the fingerprint
// inside it. On a store pinned to a single SQLite connection a tx holds that
// connection, so everything the save reads or writes has to go through the
// tx handle — a call on *sql.DB from inside would wait on itself. The
// processes-package functions the save needs therefore take a DBTX, which
// both *sql.DB and *sql.Tx satisfy; every existing caller still passes the
// *sql.DB it always did.

func seedTxStyle(t *testing.T, db *DB) (processID, styleID int64) {
	t.Helper()
	var err error
	processID, err = db.CreateProcess("TX-PROC", "", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	styleID, err = db.CreateStyle("TX-STYLE", "", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	return processID, styleID
}

func txClaim(styleID int64, node string) processes.NodeClaimInput {
	return processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: node, Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobot,
		PayloadCode: "P", InboundStaging: "STG", InboundSource: "SRC", OutboundDestination: "DST",
	}
}

// TestFlowTx_ClaimWritesRollBackWithTheTransaction: an upsert and a delete
// issued on the tx are visible inside it and gone after a rollback.
func TestFlowTx_ClaimWritesRollBackWithTheTransaction(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	processID, styleID := seedTxStyle(t, db)
	keptID, err := db.UpsertStyleNodeClaim(txClaim(styleID, "KEPT"))
	if err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	sentinel := errors.New("roll it back")
	err = db.Transaction(func(tx *sql.Tx) error {
		if _, err := processes.UpsertClaim(tx, txClaim(styleID, "NEW")); err != nil {
			return err
		}
		if err := processes.DeleteClaim(tx, keptID); err != nil {
			return err
		}
		inside, err := processes.ListClaims(tx, styleID)
		if err != nil {
			return err
		}
		if len(inside) != 1 || inside[0].CoreNodeName != "NEW" {
			t.Errorf("inside the tx: claims = %v, want just NEW", names(inside))
		}
		if _, err := processes.Get(tx, processID); err != nil {
			return err
		}
		if _, err := processes.GetStyle(tx, styleID); err != nil {
			return err
		}
		if _, err := processes.GetActiveChangeover(tx, processID); !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := processes.ListRoutingNodes(tx, processID); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Transaction returned %v, want the sentinel", err)
	}
	after, err := db.ListStyleNodeClaims(styleID)
	if err != nil {
		t.Fatalf("list after rollback: %v", err)
	}
	if len(after) != 1 || after[0].ID != keptID {
		t.Errorf("after rollback: claims = %v, want just KEPT (id %d) — the writes did not ride the transaction", names(after), keptID)
	}
}

// TestFlowTx_ClaimWritesCommitWithTheTransaction is the other half: on
// commit the same writes are there.
func TestFlowTx_ClaimWritesCommitWithTheTransaction(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, styleID := seedTxStyle(t, db)
	err := db.Transaction(func(tx *sql.Tx) error {
		in := txClaim(styleID, "NEW")
		in.Source, in.CalledBy = domain.ClaimSourceHMI, "Press 400"
		_, err := processes.UpsertClaim(tx, in)
		return err
	})
	if err != nil {
		t.Fatalf("Transaction: %v", err)
	}
	after, err := db.ListStyleNodeClaims(styleID)
	if err != nil {
		t.Fatalf("list after commit: %v", err)
	}
	if len(after) != 1 || after[0].CoreNodeName != "NEW" || after[0].Source != domain.ClaimSourceHMI {
		t.Errorf("after commit: %+v, want one hmi-stamped NEW claim", after)
	}
}

func names(claims []processes.NodeClaim) []string {
	out := make([]string, 0, len(claims))
	for _, c := range claims {
		out = append(out, c.CoreNodeName)
	}
	return out
}
