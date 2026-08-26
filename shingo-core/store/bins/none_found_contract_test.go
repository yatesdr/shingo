//go:build docker

package bins_test

import (
	"database/sql"
	"errors"
	"shingocore/store/reservations"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// none_found_contract_test.go — MG3-1a, the store half. EVERY FINDER IN THE
// FAMILY SPELLS "NOTHING MATCHED" THE SAME WAY, and it is sql.ErrNoRows.
//
// ── WHY THE CONTRACT NEEDS A TEST AND NOT A COMMENT ─────────────────────────
//
// The cascade now separates "the query ran and matched nothing" from "the query
// did not run" (dispatch.sourceSearchFailed), and it makes that separation with
// errors.Is(err, sql.ErrNoRows). That is only correct if every finder actually
// returns the sentinel for none-found. A finder that returned a hand-rolled
// error instead would send every dry group down the unreadable arm — reporting
// a plant-wide Core outage every time a market ran dry.
//
// The inverse costs more. A finder that returned nil, nil for none-found, or
// that wrapped a genuine failure in something the classifier reads as
// ErrNoRows, puts the collapse back exactly where MG2 found it: a broken query
// indistinguishable from an empty plant, green across the whole gate.
//
// GENERALIZED FROM ONE FINDER TO THE FAMILY. MG2-11's follow-up added a strict
// assertion to a single finder because that is where the bug was. The bug was
// never about that finder; it was about the shape of the contract, so the
// assertion belongs to every member of it.

// TestNoneFound_IsAlwaysErrNoRows runs every finder against a database where
// nothing can match, and requires the sentinel — not merely an error.
func TestNoneFound_IsAlwaysErrNoRows(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	// A real, EMPTY group, so the group-scoped finders have somewhere to look
	// and find nothing. Without it they would return no rows for the trivial
	// reason that the group does not exist.
	grpID, err := nodes.CreateGroup(sdb, "NF-EMPTY-GRP")
	testutil.MustNoErr(t, err, "CreateGroup")
	slot := &nodes.Node{Name: "NF-EMPTY-SLOT", Enabled: true, ParentID: &grpID}
	testutil.MustNoErr(t, nodes.Create(sdb, slot), "create slot")
	testutil.MustNoErr(t, sdb.QueryRow(
		`INSERT INTO bin_types (code) VALUES ('NF-45x58') RETURNING id`).Scan(new(int64)), "bin type")

	for _, tc := range []struct {
		name string
		find func() (*bins.Bin, error)
	}{
		{"typed, group-scoped", func() (*bins.Bin, error) {
			return bins.FindEmptyOfTypeInGroup(sdb, "NF-45x58", grpID, 0, reservations.Anyone)
		}},
		{"typed, group-scoped, blank code", func() (*bins.Bin, error) {
			// The blank-code guard returns before querying. It must still speak
			// the family's language, or a caller cannot handle one guard
			// differently from the query behind it without knowing which is which.
			return bins.FindEmptyOfTypeInGroup(sdb, "", grpID, 0, reservations.Anyone)
		}},
		{"untyped, group-scoped", func() (*bins.Bin, error) {
			return bins.FindEmptyCompatibleInGroup(sdb, "NF-NO-SUCH-PAYLOAD", grpID, 0, reservations.Anyone)
		}},
		{"typed, plant-wide", func() (*bins.Bin, error) {
			return bins.FindEmptyOfType(sdb, "NF-45x58", "", 0, bins.EmptyFence{}, reservations.Anyone)
		}},
		{"typed, plant-wide, with a zone preference", func() (*bins.Bin, error) {
			// The zone arm is a SECOND query with its own result handling, so it
			// gets its own case: a zone-preferring call that matches nothing must
			// arrive at the same sentinel as one without a preference.
			return bins.FindEmptyOfType(sdb, "NF-45x58", "NF-NO-SUCH-ZONE", 0, bins.EmptyFence{}, reservations.Anyone)
		}},
		{"typed, plant-wide, FENCED", func() (*bins.Bin, error) {
			// A non-zero fence renders a whole extra CTE and arm, so it is a
			// materially different query and gets its own case.
			return bins.FindEmptyOfType(sdb, "NF-45x58", "", 0,
				bins.EmptyFence{ProcessNode: "NF-NO-SUCH-PROCESS", OriginGroup: "NF-EMPTY-GRP"}, reservations.Anyone)
		}},
		{"untyped, plant-wide, FENCED", func() (*bins.Bin, error) {
			return bins.FindEmptyCompatible(sdb, "NF-NO-SUCH-PAYLOAD", "", 0,
				bins.EmptyFence{ProcessNode: "NF-NO-SUCH-PROCESS", OriginGroup: "NF-EMPTY-GRP"}, reservations.Anyone)
		}},
		{"untyped, plant-wide", func() (*bins.Bin, error) {
			return bins.FindEmptyCompatible(sdb, "NF-NO-SUCH-PAYLOAD", "", 0, bins.EmptyFence{}, reservations.Anyone)
		}},
		{"full-carrier FIFO", func() (*bins.Bin, error) {
			return bins.FindSourceFIFO(sdb, "NF-NO-SUCH-PAYLOAD", 0)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.find()
			if got != nil {
				t.Fatalf("returned bin %d against an empty database — the fixture matches "+
					"something it should not", got.ID)
			}
			if err == nil {
				t.Fatalf("returned (nil, nil). The caller cannot distinguish that from a " +
					"found bin without a nil check it has no reason to write, and every " +
					"other member of the family says ErrNoRows")
			}
			if !errors.Is(err, sql.ErrNoRows) {
				t.Errorf("err = %v (%T), want sql.ErrNoRows. The cascade classifies none-found "+
					"with errors.Is on this sentinel, so a finder that spells it differently "+
					"sends every dry group down the UNREADABLE arm — reporting a Core outage "+
					"every time a market runs dry", err, err)
			}
		})
	}
}

// AND A REAL FAILURE IS NOT THE SENTINEL — the other direction, which is the
// one that hides.
//
// A finder that swallowed a genuine error into ErrNoRows would put the MG2
// collapse straight back: a query that never ran, reported as an empty plant,
// green across the gate. Provoked with a type code the query itself is fine
// with but the DATABASE is not — the table is dropped underneath it.
func TestRealFailure_IsNotMistakenForNoneFound(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	// Break the join the finders depend on, in a transaction-free way this test
	// owns entirely: its own database, its own table.
	_, err := sdb.Exec(`ALTER TABLE bin_types RENAME TO bin_types_moved_by_test`)
	testutil.MustNoErr(t, err, "move bin_types out from under the query")
	t.Cleanup(func() {
		_, _ = sdb.Exec(`ALTER TABLE bin_types_moved_by_test RENAME TO bin_types`)
	})

	got, ferr := bins.FindEmptyOfType(sdb, "ANY", "", 0, bins.EmptyFence{}, reservations.Anyone)
	if got != nil {
		t.Fatalf("returned a bin from a query that cannot run: %d", got.ID)
	}
	if ferr == nil {
		t.Fatal("returned (nil, nil) from a broken query — indistinguishable from an empty " +
			"plant at every call site")
	}
	if errors.Is(ferr, sql.ErrNoRows) {
		t.Errorf("err = %v, and errors.Is says sql.ErrNoRows. A query that could not run has "+
			"been reported as an empty result — that is the MG2 collapse, and it survives "+
			"every gate", ferr)
	}
}
