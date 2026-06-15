package engine

import (
	"errors"
	"sync"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store"
)

// cacheLoader writes one Core-owned loader into the Edge cache for the aggregate
// store to project. A shared_window loader with no positions is the single-window
// shape (its anchor is the sole window).
func cacheLoader(t *testing.T, db *store.DB, info protocol.LoaderInfo) {
	t.Helper()
	if err := db.ReplaceCoreLoaders([]protocol.LoaderInfo{info}); err != nil {
		t.Fatalf("ReplaceCoreLoaders: %v", err)
	}
}

// TestResolve_CleanMiss_ReturnsSentinel: a genuine miss returns ErrLoaderNotFound
// so a caller may take its fallback — distinct from a real error.
func TestResolve_CleanMiss_ReturnsSentinel(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	agg := newAggregateLoaderStore(db, func(string, ...any) {})

	if _, err := agg.LoaderForPayload("NOPE", domain.RoleProduce, false); !errors.Is(err, ErrLoaderNotFound) {
		t.Errorf("clean miss = %v, want ErrLoaderNotFound", err)
	}
}

// TestResolve_DBError_KeepsLastKnownGood: the aggregate store keeps its
// last-known-good snapshot across a failed Refresh (resolution never touches the
// DB), so a DB flicker can't turn a real loader into a clean miss and reroute
// demand to the wrong loader.
func TestResolve_DBError_KeepsLastKnownGood(t *testing.T) {
	t.Parallel()

	// Seed a good loader, snapshot it, then break the DB. Refresh errors but the
	// snapshot is retained, so resolution still works.
	dbA := testEngineDB(t)
	cacheLoader(t, dbA, protocol.LoaderInfo{
		Name: "Keep", LoaderKey: "loader:KEEP-LDR", Role: "produce",
		Layout: "shared_window", Replenishment: "threshold",
		Positions: []protocol.LoaderPosition{{CoreNodeName: "KEEP-LDR", Kind: "window"}},
		Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: "P1"}},
	})
	agg := newAggregateLoaderStore(dbA, func(string, ...any) {})
	_ = dbA.DB.Close() // break the DB
	if err := agg.Refresh(); err == nil {
		t.Error("Refresh on a closed DB should error")
	}
	if l, err := agg.LoaderForPayload("P1", domain.RoleProduce, false); err != nil || l == nil || l.ID() != "loader:KEEP-LDR" {
		t.Errorf("should keep last-known-good after failed refresh: loader=%v err=%v", l, err)
	}
}

// TestRace_CacheReplaceDuringClusterResolve hammers the aggregate snapshot:
// a writer swaps the cache+snapshot between two configs while readers resolve.
// Under -race the atomic swap must be clean, and every resolved loader must be a
// fully-valid domain.Loader — never a torn, half-projected cluster.
func TestRace_CacheReplaceDuringClusterResolve(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	cacheLoader(t, db, protocol.LoaderInfo{
		Name: "A", LoaderKey: "loader:RACE-A", Role: "produce", Layout: "shared_window",
		Replenishment: "threshold", Positions: []protocol.LoaderPosition{{CoreNodeName: "RACE-A", Kind: "window"}},
		Payloads: []protocol.LoaderPayloadInfo{{PayloadCode: "PA"}},
	})
	agg := newAggregateLoaderStore(db, func(string, ...any) {})

	configs := [][]protocol.LoaderInfo{
		{{Name: "A", LoaderKey: "loader:RACE-A", Role: "produce", Layout: "shared_window", Replenishment: "threshold", Positions: []protocol.LoaderPosition{{CoreNodeName: "RACE-A", Kind: "window"}}, Payloads: []protocol.LoaderPayloadInfo{{PayloadCode: "PA"}}}},
		{{Name: "B", LoaderKey: "loader:RACE-B", Role: "produce", Layout: "dedicated_positions", Replenishment: "operator", Positions: []protocol.LoaderPosition{{CoreNodeName: "RACE-B-1", PayloadCode: "PB", Kind: "dedicated"}}}},
	}

	var wg sync.WaitGroup
	// writer: alternate the cache + refresh the snapshot
	wg.Go(func() {
		for i := range 150 {
			if err := db.ReplaceCoreLoaders(configs[i%2]); err != nil {
				t.Errorf("replace: %v", err)
				return
			}
			_ = agg.Refresh()
		}
	})
	// readers: resolve continuously; every hit must be a valid loader
	for range 4 {
		wg.Go(func() {
			for range 400 {
				for _, l := range agg.snapshot() {
					if l.SlotCount() < 1 || len(l.DeliveryNodes()) < 1 {
						t.Errorf("torn loader observed: id=%s slots=%d nodes=%d", l.ID(), l.SlotCount(), len(l.DeliveryNodes()))
						return
					}
				}
			}
		})
	}
	wg.Wait()
}
