package www

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/go-chi/chi/v5"

	"shingo/protocol"
	"shingoedge/engine"
	"shingoedge/store/catalog"
)

// manifestPinEngine is the stub engine with a CoreClient pointed at a Core
// that does not answer, so a handler that still asked Core would see it fail.
type manifestPinEngine struct {
	*stubEngine
	core *engine.CoreClient
}

func (e *manifestPinEngine) CoreAPI() *engine.CoreClient { return e.core }

// TestAPIPayloadManifest_ServesTheCatalogCopy: the load modal's manifest is
// read from the Edge's copy of Core's catalog feed, with Core unreachable. A
// row with lines answers them and the row's UOP capacity; a row with none
// answers the payload as a one-line template of itself (ratio 1, the payload's
// description), which is what Core's manifest endpoint answered for it; a
// payload the copy has no row for answers no lines, the one case the modal
// calls "No manifest template".
func TestAPIPayloadManifest_ServesTheCatalogCopy(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandlers(t)
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	h.engine = &manifestPinEngine{stubEngine: h.engine.(*stubEngine), core: engine.NewCoreClient(closed.URL)}

	lines := []protocol.ManifestItem{
		{PartNumber: "PN-MF1", PartsPerCycle: 2, Description: "left"},
		{PartNumber: "PN-MF2", PartsPerCycle: 1},
	}
	for _, e := range []*catalog.CatalogEntry{
		{ID: 990101, Name: "MF-LINES", Code: "MF-LINES", UOPCapacity: 24, Manifest: lines},
		{ID: 990102, Name: "MF-BARE", Code: "MF-BARE", Description: "bare part", UOPCapacity: 50},
	} {
		if err := h.engine.CatalogService().Upsert(e); err != nil {
			t.Fatalf("seed %s: %v", e.Code, err)
		}
	}
	r := chi.NewRouter()
	r.Get("/api/payload/{code}/manifest", h.apiPayloadManifest)

	type answer struct {
		Items       []protocol.ManifestItem `json:"items"`
		UOPCapacity int                     `json:"uop_capacity"`
	}
	for code, want := range map[string]answer{
		"MF-LINES": {Items: lines, UOPCapacity: 24},
		"MF-BARE":  {Items: []protocol.ManifestItem{{PartNumber: "MF-BARE", PartsPerCycle: 1, Description: "bare part"}}, UOPCapacity: 50},
		"MF-NONE":  {Items: []protocol.ManifestItem{}, UOPCapacity: 0},
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/payload/"+code+"/manifest", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200 (body %s)", code, rec.Code, rec.Body.String())
			continue
		}
		var got answer
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Errorf("%s: decode %v (%s)", code, err, rec.Body.String())
			continue
		}
		if got.Items == nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: answer %s, want %+v", code, rec.Body.String(), want)
		}
	}
}
