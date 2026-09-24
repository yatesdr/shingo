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

// ownBufferFixture is Springfield's supermarket: a group whose slots are ALL
// loader positions. Dedicated loader OWN has two homes and a buffer in it; a
// second dedicated loader FOREIGN has a buffer in it too. Every slot but the
// requesting home holds an empty.
type ownBufferFixture struct {
	grpID                                  int64
	home                                   *nodes.Node
	ownBuffer, ownOtherHome, foreignBuffer *bins.Bin
}

func loaderOwnBufferFixture(t *testing.T, db *store.DB) ownBufferFixture {
	t.Helper()
	grpID, err := nodes.CreateGroup(db.DB, "OWNBUF-GRP")
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	slot := func(name string) *nodes.Node {
		n := &nodes.Node{Name: name, Enabled: true, ParentID: &grpID}
		if err := db.CreateNode(n); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return n
	}
	loader := func(name string) int64 {
		id, err := db.CreateLoader(store.Loader{
			Name: name, Role: loaders.RoleProduce, Layout: loaders.LayoutDedicatedPositions, Replenishment: "threshold",
		})
		if err != nil {
			t.Fatalf("create loader %s: %v", name, err)
		}
		return id
	}
	member := func(loaderID int64, pos *nodes.Node, kind string) {
		if err := db.UpsertLoaderHome(store.LoaderHome{LoaderID: loaderID, PositionNodeID: pos.ID, Kind: kind}); err != nil {
			t.Fatalf("member %s: %v", pos.Name, err)
		}
	}
	own, foreign := loader("OWNBUF-OWN"), loader("OWNBUF-FOREIGN")
	home, otherHome, buf, fbuf := slot("OWNBUF-HOME-1"), slot("OWNBUF-HOME-2"), slot("OWNBUF-BUF-1"), slot("OWNBUF-FBUF-1")
	member(own, home, loaders.HomeKindHome)
	member(own, otherHome, loaders.HomeKindHome)
	member(own, buf, loaders.HomeKindBuffer)
	member(foreign, fbuf, loaders.HomeKindBuffer)
	return ownBufferFixture{
		grpID:         grpID,
		home:          home,
		ownBuffer:     testdb.CreateBinAtNode(t, db, "", buf.ID, "BIN-OWNBUF-BUF"),
		ownOtherHome:  testdb.CreateBinAtNode(t, db, "", otherHome.ID, "BIN-OWNBUF-HOME-2"),
		foreignBuffer: testdb.CreateBinAtNode(t, db, "", fbuf.ID, "BIN-OWNBUF-FBUF"),
	}
}

// TestEmptyScan_AHomeDrawsFromItsOwnLoadersBuffer is the Springfield 2026-09-24
// regression: a retrieve_empty sourced from the supermarket group and delivered
// to one of the supermarket loader's homes queued "Waiting for an empty bin"
// beside empties on that loader's own buffers, because the loader arm hid every
// live loader position from every asker.
//
// Asked on behalf of the home, each finder offers exactly the OWN loader's
// buffer: not the foreign loader's buffer, and not the empty waiting on the own
// loader's other home. Asked on behalf of nobody, it still offers none of them,
// and the keeper's count still counts none of them.
func TestEmptyScan_AHomeDrawsFromItsOwnLoadersBuffer(t *testing.T) {
	t.Parallel()
	finders := map[string]func(db *store.DB, f ownBufferFixture, dest int64) func() (*bins.Bin, error){
		"FindEmptyCompatibleInGroup": func(db *store.DB, f ownBufferFixture, dest int64) func() (*bins.Bin, error) {
			return func() (*bins.Bin, error) {
				return db.FindEmptyCompatibleBinInGroup("", f.grpID, dest, reservations.DigAsker{})
			}
		},
		"FindEmptyOfTypeInGroup": func(db *store.DB, f ownBufferFixture, dest int64) func() (*bins.Bin, error) {
			return func() (*bins.Bin, error) {
				return db.FindEmptyBinOfTypeInGroup("DEFAULT", f.grpID, dest, reservations.DigAsker{})
			}
		},
		"FindEmptyCompatible": func(db *store.DB, _ ownBufferFixture, dest int64) func() (*bins.Bin, error) {
			return func() (*bins.Bin, error) {
				return db.FindEmptyCompatibleBin("", "", dest, bins.EmptyFence{}, reservations.DigAsker{})
			}
		},
		"FindEmptyOfType": func(db *store.DB, _ ownBufferFixture, dest int64) func() (*bins.Bin, error) {
			return func() (*bins.Bin, error) {
				return db.FindEmptyBinOfType("DEFAULT", "", dest, bins.EmptyFence{}, reservations.DigAsker{})
			}
		},
	}
	for name, mk := range finders {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db := testdb.Open(t)
			f := loaderOwnBufferFixture(t, db)

			found := drainEmpties(t, db, mk(db, f, f.home.ID))
			if !found[f.ownBuffer.ID] {
				t.Errorf("%s for home %s offered %v, want the empty on its own loader's buffer (bin %d)",
					name, f.home.Name, found, f.ownBuffer.ID)
			}
			if found[f.foreignBuffer.ID] {
				t.Errorf("%s for home %s took bin %d on ANOTHER loader's buffer", name, f.home.Name, f.foreignBuffer.ID)
			}
			if found[f.ownOtherHome.ID] {
				t.Errorf("%s for home %s took bin %d waiting on its loader's OTHER home", name, f.home.Name, f.ownOtherHome.ID)
			}

			if _, err := db.Exec(`UPDATE bins SET locked=false`); err != nil {
				t.Fatalf("unlock: %v", err)
			}
			if nobody := drainEmpties(t, db, mk(db, f, 0)); len(nobody) != 0 {
				t.Errorf("%s with no destination offered %v, want none: every slot is a live loader's", name, nobody)
			}
		})
	}

	t.Run("CountEmptyOfTypeInGroup", func(t *testing.T) {
		t.Parallel()
		db := testdb.Open(t)
		f := loaderOwnBufferFixture(t, db)
		n, err := db.CountEmptyBinsOfTypeInGroup("DEFAULT", f.grpID)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("CountEmptyOfTypeInGroup = %d, want 0: a loader's buffer is not group stock", n)
		}
	})
}
