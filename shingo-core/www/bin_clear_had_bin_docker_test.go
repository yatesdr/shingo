//go:build docker

package www

import (
	"net/http"
	"testing"

	"shingocore/internal/testdb"
)

// bin_clear_had_bin_docker_test.go — a successful bin-clear means a bin WAS at
// the node. apiBinClear refuses a node that holds no bin (400, "no bin at node")
// before it clears anything, so the Edge can learn "the window held a carrier"
// from the clear's own success instead of reading node-bins first.
func TestApiBinClear_NodeWithoutBin_Refuses(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	// The line node holds no bin in the standard fixture.
	rec := postJSON(t, h.apiBinClear, "/api/telemetry/bin-clear",
		map[string]any{"node_name": sd.LineNode.Name})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec.Body.Bytes(), "no bin at node")
}
