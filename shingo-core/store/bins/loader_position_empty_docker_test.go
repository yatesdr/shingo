//go:build docker

package bins_test

import (
	"database/sql"
	"errors"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// loader_position_empty_docker_test.go — a carrier standing on a live loader's
// own position belongs to that loader, and the plant-wide empty scan may not
// take it.
//
// An L1 drops an empty at a loader window for the operator to fill. The window
// is in bin_loader_homes, so the arrival lands `available` rather than
// `staged`; arrival releases the order's claim; and the cell-position arm of
// EmptyCarrierWhere reads style_claims, which the Edge never publishes loader
// claims into. So until the operator loads it, any plant-wide empty request can
// drive off with it.
//
// The second half asserts the other direction: an ordinary bank position, and a
// window of an ARCHIVED loader, stay in the pool.

func loaderPositionFixture(t *testing.T, db *store.DB) (window, archivedWin, bank *nodes.Node) {
	t.Helper()
	mk := func(name string) *nodes.Node {
		n := &nodes.Node{Name: name, Enabled: true}
		if err := db.CreateNode(n); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return n
	}
	window, archivedWin, bank = mk("LDRPOS-WIN-1"), mk("LDRPOS-OLDWIN-1"), mk("LDRPOS-BANK-1")
	home := func(name string, pos *nodes.Node) int64 {
		id, err := db.CreateLoader(store.Loader{
			Name: name, Role: loaders.RoleProduce, Layout: loaders.LayoutSharedWindow, Replenishment: "operator",
		})
		if err != nil {
			t.Fatalf("create loader %s: %v", name, err)
		}
		if err := db.UpsertLoaderHome(store.LoaderHome{LoaderID: id, PositionNodeID: pos.ID}); err != nil {
			t.Fatalf("home %s: %v", name, err)
		}
		return id
	}
	home("LDRPOS-LOADER", window)
	old := home("LDRPOS-OLD-LOADER", archivedWin)
	if _, err := db.Exec(`UPDATE bin_loaders SET archived_at=now() WHERE id=$1`, old); err != nil {
		t.Fatalf("archive loader: %v", err)
	}
	return window, archivedWin, bank
}

// drainEmpties calls find until it runs dry, locking each carrier it returns so
// the next call moves on, and returns the set of bin ids it took.
func drainEmpties(t *testing.T, db *store.DB, find func() (*bins.Bin, error)) map[int64]bool {
	t.Helper()
	found := map[int64]bool{}
	for i := 0; i < 6; i++ {
		b, err := find()
		if errors.Is(err, sql.ErrNoRows) || b == nil {
			break
		}
		if err != nil {
			t.Fatalf("empty scan: %v", err)
		}
		found[b.ID] = true
		if _, err := db.Exec(`UPDATE bins SET locked=true WHERE id=$1`, b.ID); err != nil {
			t.Fatalf("take carrier: %v", err)
		}
	}
	return found
}

func TestEmptyScan_SkipsALiveLoadersOwnPosition(t *testing.T) {
	t.Parallel()
	finders := map[string]func(db *store.DB) func() (*bins.Bin, error){
		"FindEmptyCompatible": func(db *store.DB) func() (*bins.Bin, error) {
			return func() (*bins.Bin, error) {
				return db.FindEmptyCompatibleBin("", "", 0, bins.EmptyFence{}, reservations.DigAsker{})
			}
		},
		"FindEmptyOfType": func(db *store.DB) func() (*bins.Bin, error) {
			return func() (*bins.Bin, error) {
				return db.FindEmptyBinOfType("DEFAULT", "", 0, bins.EmptyFence{}, reservations.DigAsker{})
			}
		},
	}
	for name, mk := range finders {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db := testdb.Open(t)
			window, archivedWin, bank := loaderPositionFixture(t, db)
			atWindow := testdb.CreateBinAtNode(t, db, "", window.ID, "BIN-LDRPOS-WIN")
			atOld := testdb.CreateBinAtNode(t, db, "", archivedWin.ID, "BIN-LDRPOS-OLDWIN")
			atBank := testdb.CreateBinAtNode(t, db, "", bank.ID, "BIN-LDRPOS-BANK")

			found := drainEmpties(t, db, mk(db))
			if found[atWindow.ID] {
				t.Errorf("%s took the empty standing on live loader window %s — it belongs to that "+
					"loader until the operator loads it", name, window.Name)
			}
			if !found[atBank.ID] {
				t.Errorf("%s did not take the empty at bank %s — the exclusion is too broad", name, bank.Name)
			}
			if !found[atOld.ID] {
				t.Errorf("%s did not take the empty at %s, a window of an ARCHIVED loader — only a live "+
					"loader owns its positions", name, archivedWin.Name)
			}
		})
	}
}
