package engine

import (
	"reflect"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store/catalog"
)

// TestLoadBin_SendsTheBinItChecked pins the Edge half of I2: LOAD's occupancy
// check reads the carrier at the window, and the load names that carrier's id,
// so Core writes the bin the Edge verified empty rather than whichever one its
// own query returns first (and a multi-bin node without an id answers 409).
func TestLoadBin_SendsTheBinItChecked(t *testing.T) {
	t.Parallel()
	const window = "SC-LBID-W1"
	eng, _, core, _, nodeID := scLoaderEngine(t, window)
	core.set(window, true, "") // an empty carrier is at the window

	testutil.MustNoErr(t, eng.LoadBin(nodeID, "", nil, scManifest), "LoadBin")

	core.mu.Lock()
	defer core.mu.Unlock()
	if len(core.loadBinIDs) != 1 || core.loadBinIDs[0] != 77 {
		t.Errorf("bin-load bin_ids = %v, want [77] (the bin node-bins reported)", core.loadBinIDs)
	}
}

// TestLoadBin_SendsWhatItRead: the LOAD carries the generation of the carrier
// its check read (the node-bins row's delta_epoch) and what it read the
// carrier to be, in the door's terms: bare, the only carrier the LOAD fills
// (a labelled one is answered without a load, loadOntoLabelledCarrier). Core
// refuses the LOAD when either no longer holds.
func TestLoadBin_SendsWhatItRead(t *testing.T) {
	t.Parallel()
	const window = "SC-LREAD-W1"
	eng, _, core, _, nodeID := scLoaderEngine(t, window)
	core.set(window, true, "")

	testutil.MustNoErr(t, eng.LoadBin(nodeID, "", nil, scManifest), "LoadBin")

	core.mu.Lock()
	defer core.mu.Unlock()
	if len(core.loads) != 1 || core.loads[0].Epoch != 5 || core.loads[0].Expect != protocol.LoadExpectBare {
		t.Errorf("bin-load requests = %+v, want one at epoch 5 expecting bare", core.loads)
	}
}

// TestLoadBin_BuildsTheManifestFromTheCatalog: a LOAD that carries no manifest
// (the operator station sends the part and the count) is written with the
// payload's template from this Edge's catalog copy, each line's per-cycle
// ratio times the count, as the station's own script built it before; a row
// with no lines is its one-line template of itself (CatalogEntry.TemplateLines,
// what the modal shows). A payload the copy has no row for, or a request with
// no count, is refused before Core is asked; a request that carries a manifest
// is sent as it came, whatever the catalog says.
func TestLoadBin_BuildsTheManifestFromTheCatalog(t *testing.T) {
	t.Parallel()
	count := func(n int64) *int64 { return &n }
	twoLines := []protocol.ManifestItem{
		{PartNumber: "PN-SC-L", PartsPerCycle: 2, Description: "left"},
		{PartNumber: "PN-SC-R", PartsPerCycle: 1},
	}
	cases := []struct {
		name     string
		row      *catalog.CatalogEntry // nil: no row for the part
		uop      *int64
		sent     []protocol.IngestManifestItem
		want     []BinLoadItem // nil: refused, nothing sent
		refusal  string
		windowID string
	}{
		{
			name: "one line, the typed count", windowID: "SC-LMAN-W1", uop: count(90),
			row:  &catalog.CatalogEntry{ID: 1, Name: "PART-SC", Code: "PART-SC", UOPCapacity: 100, Manifest: twoLines[:1]},
			want: []BinLoadItem{{PartNumber: "PN-SC-L", Quantity: 180, Description: "left"}},
		},
		{
			name: "two lines, each scaled", windowID: "SC-LMAN-W2", uop: count(90),
			row: &catalog.CatalogEntry{ID: 1, Name: "PART-SC", Code: "PART-SC", UOPCapacity: 100, Manifest: twoLines},
			want: []BinLoadItem{{PartNumber: "PN-SC-L", Quantity: 180, Description: "left"},
				{PartNumber: "PN-SC-R", Quantity: 90}},
		},
		{
			name: "a row with no lines is a line of itself", windowID: "SC-LMAN-W3", uop: count(40),
			row:  &catalog.CatalogEntry{ID: 1, Name: "PART-SC", Code: "PART-SC", Description: "bare part", UOPCapacity: 100},
			want: []BinLoadItem{{PartNumber: "PART-SC", Quantity: 40, Description: "bare part"}},
		},
		{
			name: "a request's own manifest is sent as it came", windowID: "SC-LMAN-W4", uop: count(90), sent: scManifest,
			row:  &catalog.CatalogEntry{ID: 1, Name: "PART-SC", Code: "PART-SC", UOPCapacity: 100, Manifest: twoLines},
			want: []BinLoadItem{{PartNumber: "PN-SC", Quantity: 40, Description: "synthetic"}},
		},
		{name: "no row, no template", windowID: "SC-LMAN-W5", uop: count(90), refusal: "no manifest template for PART-SC"},
		{
			name: "no count, nothing to scale by", windowID: "SC-LMAN-W6", refusal: "set the UoP count",
			row: &catalog.CatalogEntry{ID: 1, Name: "PART-SC", Code: "PART-SC", UOPCapacity: 100, Manifest: twoLines},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng, db, core, _, nodeID := scLoaderEngine(t, tc.windowID)
			core.set(tc.windowID, true, "") // an empty carrier is at the window
			if tc.row != nil {
				testutil.MustNoErr(t, db.UpsertPayloadCatalog(tc.row), "seed the catalog row")
			}
			err := eng.LoadBin(nodeID, "PART-SC", tc.uop, tc.sent)
			core.mu.Lock()
			defer core.mu.Unlock()
			if tc.want == nil {
				if err == nil || !strings.Contains(err.Error(), tc.refusal) {
					t.Fatalf("LoadBin err = %v, want a refusal naming %q", err, tc.refusal)
				}
				if len(core.loads) != 0 {
					t.Errorf("bin-load requests = %+v, want none: a refused LOAD does not reach Core", core.loads)
				}
				return
			}
			testutil.MustNoErr(t, err, "LoadBin")
			if len(core.loads) != 1 || !reflect.DeepEqual(core.loads[0].Manifest, tc.want) {
				t.Fatalf("bin-load requests = %+v, want one carrying %+v", core.loads, tc.want)
			}
		})
	}
}
