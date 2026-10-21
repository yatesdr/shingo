//go:build docker

package engine

import (
	"reflect"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
)

// The station CLEAR calls the bin hook once, with the bin, after it commits:
// on the plain path (no CMS boundary) and on the path that books a departure.
// The hook set on the engine also reaches the LOAD and the count through the
// bin manifest service (TestStationDoors_CallTheBinHook).
func TestClearForReuseStampAndBookDeparture_CallsTheBinHook(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newUnstartedEngine(t, db, testdb.NewTrackingBackend())
	var got []int64
	eng.SetBinChangedFunc(func(binID int64) { got = append(got, binID) })

	sd := testdb.SetupStandardData(t, db)
	plain := createTestBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "BIN-HOOK-PLAIN")
	testutil.MustNoErr(t, db.SetBinManifest(plain.ID, `{"items":[]}`, sd.Payload.Code, 3), "manifest")
	if _, err := eng.ClearForReuseStampAndBookDeparture(plain.ID, sd.LineNode.ID, nil, bins.StampNone, nil); err != nil {
		t.Fatalf("plain clear: %v", err)
	}

	booked, nodeID := clearBoundaryBin(t, db, "HOOK-CLEAR-PAYLOAD", 1, 4)
	if _, err := eng.ClearForReuseStampAndBookDeparture(booked.ID, nodeID, nil, bins.StampNone, nil); err != nil {
		t.Fatalf("clear at a CMS boundary: %v", err)
	}
	if want := []int64{plain.ID, booked.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("hook calls = %v, want %v", got, want)
	}

	// The manifest service holds the same hook: a LOAD through it is heard.
	if _, _, err := eng.BinManifest().LoadAtLine(plain.ID, `{"items":[]}`, sd.Payload.Code, nil); err != nil {
		t.Fatalf("LoadAtLine: %v", err)
	}
	if len(got) != 3 || got[2] != plain.ID {
		t.Errorf("after a LOAD through the engine's manifest service: hook calls = %v", got)
	}
}
