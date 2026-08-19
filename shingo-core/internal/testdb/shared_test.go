//go:build docker

package testdb

import (
	"sync"
	"testing"

	"shingocore/store/nodes"
)

// TestOpenShared_SameKeySameDB is the contract every shared-DB test file
// relies on: two tests (here: two goroutines standing in for parallel tests)
// asking for the same key get the SAME *store.DB — one clone, one database.
func TestOpenShared_SameKeySameDB(t *testing.T) {
	db1 := OpenShared(t, "selftest-same-key")
	db2 := OpenShared(t, "selftest-same-key")
	if db1 != db2 {
		t.Fatal("OpenShared with the same key handed out two different DBs")
	}
	sharedMu.Lock()
	if got := sharedRefs["selftest-same-key"]; got != 2 {
		sharedMu.Unlock()
		t.Fatalf("refcount = %d, want 2", got)
	}
	sharedMu.Unlock()
}

// TestOpenShared_DistinctKeysDistinctDBs guards the other half of the
// contract: different files (different keys) never land on one database.
func TestOpenShared_DistinctKeysDistinctDBs(t *testing.T) {
	db1 := OpenShared(t, "selftest-key-a")
	db2 := OpenShared(t, "selftest-key-b")
	if db1 == db2 {
		t.Fatal("OpenShared with different keys handed out the same DB")
	}
}

// TestOpenShared_RefcountDropsLastOut exercises the lifetime: when the last
// holder of a key releases, the DB leaves the registry and its database name
// is dropped. We can't observe the DROP itself from inside the process (the
// connection is gone), but we can observe the registry becoming empty —
// which is what stops a re-open after the drop from resurrecting a stale DB.
func TestOpenShared_RefcountDropsLastOut(t *testing.T) {
	key := "selftest-refcount"
	db1 := OpenShared(t, key)

	releaseShared(key)
	sharedMu.Lock()
	_, stillThere := sharedDBs[key]
	refs := sharedRefs[key]
	sharedMu.Unlock()
	if stillThere || refs != 0 {
		t.Fatalf("after last release, key still registered (present=%v refs=%d)", stillThere, refs)
	}

	// Re-open: must be a FRESH clone, not the retired one. The NAME is
	// deterministic (key+pid), so the name repeats — the database itself was
	// dropped and cloned anew, observable as a different *store.DB handle
	// and a working connection.
	db2 := OpenShared(t, key)
	if db2 == db1 {
		t.Fatal("re-open after drop returned the retired DB handle")
	}
	if _, err := db2.GetNodeByName("STORAGE-A1"); err == nil {
		// Nothing seeded on this fresh clone — a hit means we are talking to
		// the retired database (drop failed), or the handle is stale.
		t.Fatal("fresh re-open unexpectedly sees seeded data from the retired DB")
	}
}

// TestOpenShared_SetupStandardDataIdempotent proves the idempotency the
// sharing model needs: two SetupStandardData calls against one shared DB
// return the same entities (same IDs), and a concurrent storm of them does
// not die on nodes_name_key / payloads_code_key / bin_types_code_key.
func TestOpenShared_SetupStandardDataIdempotent(t *testing.T) {
	db := OpenShared(t, "selftest-idempotent")
	sd1 := SetupStandardData(t, db)
	sd2 := SetupStandardData(t, db)
	if sd1.StorageNode.ID != sd2.StorageNode.ID ||
		sd1.LineNode.ID != sd2.LineNode.ID ||
		sd1.Payload.ID != sd2.Payload.ID ||
		sd1.BinType.ID != sd2.BinType.ID {
		t.Fatalf("sequential SetupStandardData drifted: %+v vs %+v", sd1, sd2)
	}

	// Concurrent storm — the shape a file of parallel tests actually
	// produces, where every test's fixture helper calls SetupStandardData.
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sd := SetupStandardData(t, db)
			if sd.StorageNode.ID != sd1.StorageNode.ID {
				errs <- nil // shape drift; detect via ID compare below
			}
		}()
	}
	wg.Wait()
	close(errs)
	for range errs {
		t.Fatal("concurrent SetupStandardData produced drifting entities")
	}

	// And the store still answers by natural key.
	if got, err := db.GetNodeByName("STORAGE-A1"); err != nil || got.ID != sd1.StorageNode.ID {
		t.Fatalf("GetNodeByName(STORAGE-A1) = (%v, %v), want id %d", got, err, sd1.StorageNode.ID)
	}
	_ = nodes.Node{}
}
