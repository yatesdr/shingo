//go:build docker

package payloads_test

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/payloads"
)

// TestPayloadCATIDs pins the catalog's part-identity feed: the DISTINCT
// manifest part numbers per payload, comma-joined in part-number order.
// A single-part payload yields that one value (the historical shape); a
// multi-part payload — a kit bin — yields its full list, which the edge
// splits into the style's part-identity SET (membership semantics, so a
// multi-part payload contributes every part instead of nothing).
// Payloads with no part numbers at all are still omitted.
func TestPayloadCATIDs(t *testing.T) {
	t.Parallel()
	sdb := testdb.Open(t)
	db := sdb.DB

	single := &payloads.Payload{Code: "CID-SINGLE", UOPCapacity: 1}
	testutil.MustNoErr(t, payloads.Create(db, single), "create single")
	multi := &payloads.Payload{Code: "CID-MULTI", UOPCapacity: 1}
	testutil.MustNoErr(t, payloads.Create(db, multi), "create multi")
	none := &payloads.Payload{Code: "CID-NONE", UOPCapacity: 1}
	testutil.MustNoErr(t, payloads.Create(db, none), "create none")

	item := func(pid int64, pn string) {
		t.Helper()
		testutil.MustNoErr(t, payloads.CreateItem(db, &payloads.ManifestItem{
			PayloadID: pid, PartNumber: pn, Quantity: 1,
		}), "create manifest item")
	}
	item(single.ID, "40016911")
	item(single.ID, "40016911") // same part number — still one distinct
	item(multi.ID, "50029999")
	item(multi.ID, "40016911") // two distinct — the FULL set now syncs
	item(multi.ID, "40016911") // duplicate row — still one distinct value
	// none: no manifest rows at all

	catids, err := payloads.PayloadCATIDs(db)
	testutil.MustNoErr(t, err, "PayloadCATIDs")

	if catids[single.ID] != "40016911" {
		t.Errorf("single-part payload CATID = %q, want 40016911", catids[single.ID])
	}
	// Multi-part: distinct values, comma-joined, ordered by part number —
	// NOT omitted. This is the multi-part catalog sync.
	if got, want := catids[multi.ID], "40016911,50029999"; got != want {
		t.Errorf("multi-part payload CATID = %q, want %q", got, want)
	}
	if v, ok := catids[none.ID]; ok {
		t.Errorf("no-manifest payload must be omitted, got %q", v)
	}
}
