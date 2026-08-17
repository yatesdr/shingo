//go:build docker

package store_test

import (
	"testing"

	"shingocore/internal/testdb"
)

// compound_sealed_docker_test.go — orders.open_for_children, the explicit
// sealedness of a compound parent.
//
// Sealed is !open. Two readers infer "this reshuffle is finished" from "all its
// children are terminal", and that inference holds only while every child
// exists up front. This column is what will carry the difference once it does
// not.

// TestCompoundSealed_BornSealed is the deploy ruling, asserted rather than
// assumed: an order arrives sealed without anybody saying so.
//
// This is the assertion behind "no backfill". The migration adds the column
// with DEFAULT false and writes nothing to existing rows, which is only correct
// if false is the TRUE reading for a row nobody has spoken for — including
// every parent that was mid-reshuffle when the deploy landed. Create does not
// bind the column (writer_totality_test pins that), so this exercises the same
// path those rows took.
//
// MUTATION (verified): change the DDL and migration default to `DEFAULT true`.
// This fires — "a freshly created order reads OPEN" — and it is the only test
// that would; the sweep and the success arm both treat open as "do nothing", so
// a wrong default makes the whole system quietly stop finishing reshuffles
// rather than fail anything.
func TestCompoundSealed_BornSealed(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	o := testdb.CreateOrder(t, db)

	got, err := db.GetOrder(o.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	if got.OpenForChildren {
		t.Fatal("a freshly created order reads OPEN for children. Every compound writes all of its " +
			"children in one transaction and never adds more, so sealed is the true reading for every " +
			"row that predates this column — and the migration states that by defaulting to false " +
			"rather than backfilling. An open default would strand every in-flight reshuffle at deploy")
	}
}

// TestCompoundSealed_OneWriterBothWays: the writer sets the fact and the reader
// reads it back, in both directions, through the production call.
//
// Both directions matter. Sealing is what the fold will do when it runs out of
// digging; opening is what it will do when it commits the first move of a
// reshuffle that expects more. A writer that only worked one way would pass a
// test that only checked the interesting direction.
//
// MUTATION (verified): drop `open_for_children` from SelectCols and scan a
// constant instead. The re-read arms fire — the column round-trips through
// exactly one scan path (ScanOrders delegates to ScanOrder), so there is no
// second destination list to forget, which is the trap the Edge side hit.
func TestCompoundSealed_OneWriterBothWays(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	parent := testdb.CreateOrder(t, db)

	if err := db.SetCompoundOpen(parent.ID, true); err != nil {
		t.Fatalf("open compound: %v", err)
	}
	if got, _ := db.GetOrder(parent.ID); !got.OpenForChildren {
		t.Fatal("parent did not read back OPEN after SetCompoundOpen(true)")
	}

	if err := db.SetCompoundOpen(parent.ID, false); err != nil {
		t.Fatalf("seal compound: %v", err)
	}
	if got, _ := db.GetOrder(parent.ID); got.OpenForChildren {
		t.Fatal("parent did not read back SEALED after SetCompoundOpen(false) — a reshuffle that " +
			"cannot be sealed never completes")
	}
}

// TestCompoundSealed_MissingOrderIsAnError: a seal aimed at a row that is not
// there is reported, not dropped.
//
// The cheap implementation of this writer is an UPDATE whose zero-row result
// nobody looks at, and that is the shape this branch has now paid for twice —
// most recently one commit ago, where a status CAS answered correctly and the
// caller dispatched anyway. A caller that believes it sealed a parent and did
// not would discover it from a reshuffle that completed half dug.
//
// MUTATION (verified): drop the RowsAffected check and return nil. This test's
// own "expected an error" assertion fires; the two tests above stay green,
// which is the point — the success path cannot see this.
func TestCompoundSealed_MissingOrderIsAnError(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	if err := db.SetCompoundOpen(9_000_000_001, true); err == nil {
		t.Fatal("SetCompoundOpen against a nonexistent order returned nil — the caller believes it " +
			"wrote a fact that no row carries")
	}
}
