package engine

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// TestBinTypeSnapshot_ReadsSyncedCatalogNotCore: the changeover planner's bin
// types come from the catalog Core sends with the node list
// (SetPayloadBinTypes), not from an HTTP lookup per payload. A configured Core
// that is asked anything here fails the test, so a planner that still calls
// Core is caught even when that Core would have answered.
func TestBinTypeSnapshot_ReadsSyncedCatalogNotCore(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "the planner should not ask Core", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	eng := testEngine(t, testEngineDB(t))
	eng.coreClient = stubCoreClient(srv.URL)
	eng.SetPayloadBinTypes([]protocol.PayloadBinTypeInfo{
		{PayloadCode: "PART-A", BinTypeCode: "TYPE-1"},
		{PayloadCode: "PART-B", BinTypeCode: "TYPE-2"},
	})

	diffs := []ChangeoverNodeDiff{
		{CoreNodeName: "N1", Situation: SituationSwap,
			FromClaim: &processes.NodeClaim{PayloadCode: "PART-A"},
			ToClaim:   &processes.NodeClaim{PayloadCode: "PART-B"}},
		{CoreNodeName: "N2", Situation: SituationAdd,
			ToClaim: &processes.NodeClaim{PayloadCode: "PART-C"}},
	}
	got := eng.binTypeSnapshot(diffs)

	want := map[string]string{"PART-A": "TYPE-1", "PART-B": "TYPE-2", "PART-C": ""}
	if len(got) != len(want) {
		t.Errorf("snapshot = %v, want %v", got, want)
	}
	for code, bt := range want {
		if v, ok := got[code]; !ok || v != bt {
			t.Errorf("snapshot[%s] = %q (present %v), want %q", code, v, ok, bt)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("binTypeSnapshot made %d Core HTTP requests, want 0", n)
	}
}
