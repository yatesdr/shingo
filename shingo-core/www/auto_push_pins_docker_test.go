//go:build docker

package www

import (
	"net/http"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store"
	"shingocore/store/loaders"
)

// auto_push_pins_docker_test.go — an unloader's auto_push through the Core
// loader door. Before v125 a request carrying it was accepted and the value
// went nowhere, so the Edge was never told and every Core-owned unloader's
// three re-pull gates (CLEAR, PUSH EMPTY, U2 landed) stayed off.

func loaderInfoAutoPush(t *testing.T, db *store.DB, id int64) bool {
	t.Helper()
	infos, err := db.BuildLoaderInfos()
	testutil.MustNoErr(t, err, "build loader infos")
	for _, li := range infos {
		if li.LoaderKey == loaders.Key(id) {
			return li.AutoPush
		}
	}
	t.Fatalf("loader %d not projected", id)
	return false
}

// TestLoaderUpdate_AutoPushReachesLoaderInfo: the door stores it and the node
// list carries it; a re-save that omits it reads false (the door's contract).
func TestLoaderUpdate_AutoPushReachesLoaderInfo(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	id, err := db.CreateLoader(loaders.Loader{
		Name: "APPIN-UNL", Role: loaders.RoleConsume,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentOperator,
	})
	testutil.MustNoErr(t, err, "create unloader")

	rec := postJSON(t, h.apiUpdateLoader, "/api/loader/update",
		map[string]any{"id": id, "name": "APPIN-UNL", "auto_push": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rec.Code, rec.Body.String())
	}
	if !loaderInfoAutoPush(t, db, id) {
		t.Fatal("LoaderInfo.AutoPush = false after an update that set it")
	}
	rec = postJSON(t, h.apiUpdateLoader, "/api/loader/update", map[string]any{"id": id, "name": "APPIN-UNL"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rec.Code, rec.Body.String())
	}
	if loaderInfoAutoPush(t, db, id) {
		t.Error("auto_push survived an update that omitted it; the door's contract is absent = false")
	}
}

// TestLoaderUpdate_AutoPushOnAProduceLoaderIsRefused: a produce loader pulls
// no fulls, so the switch is refused (400) rather than stored and inert.
func TestLoaderUpdate_AutoPushOnAProduceLoaderIsRefused(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	id, err := db.CreateLoader(loaders.Loader{
		Name: "APPIN-LDR", Role: loaders.RoleProduce,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentOperator,
	})
	testutil.MustNoErr(t, err, "create loader")

	rec := postJSON(t, h.apiUpdateLoader, "/api/loader/update",
		map[string]any{"id": id, "name": "APPIN-LDR", "auto_push": true})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if loaderInfoAutoPush(t, db, id) {
		t.Error("the refused auto_push landed")
	}
}
