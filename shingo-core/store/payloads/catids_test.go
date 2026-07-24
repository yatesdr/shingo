//go:build docker

package payloads_test

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/payloads"
)

// TestPayloadCATIDs returns the single distinct manifest part number per payload
// and omits payloads whose manifest is multi-part or empty — the "never guess"
// rule the edge auto-fill relies on.
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
	item(single.ID, "40016911") // same part number → still one distinct
	item(multi.ID, "40016911")
	item(multi.ID, "50029999") // two distinct → ambiguous
	// none: no manifest rows at all

	catids, err := payloads.PayloadCATIDs(db)
	testutil.MustNoErr(t, err, "PayloadCATIDs")

	if catids[single.ID] != "40016911" {
		t.Errorf("single-part payload CATID = %q, want 40016911", catids[single.ID])
	}
	if v, ok := catids[multi.ID]; ok {
		t.Errorf("multi-part payload must be omitted, got %q", v)
	}
	if v, ok := catids[none.ID]; ok {
		t.Errorf("no-manifest payload must be omitted, got %q", v)
	}
}
