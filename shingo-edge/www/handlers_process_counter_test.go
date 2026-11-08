package www

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// handlers_process_counter_test.go — the strip's click on a slot the PLC
// counts on: POST /api/processes/{id}/counter is the counter edit to the
// sibling's tag, the process's other counter settings kept.

func counterRepoint(t *testing.T, h *Handlers, r *chi.Mux, path, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Code
}

// TestCounterRoute_RepointsKeepingThePLCAndTheFlags: the edit carries the
// process's PLC, enabled flag and identity settings, with the new tag.
func TestCounterRoute_RepointsKeepingThePLCAndTheFlags(t *testing.T) {
	h, r := newTestHandlers(t)
	r.Post("/api/processes/{id}/counter", h.apiRepointProcessCounter)
	testDB := dbOf(h)
	pid, err := testDB.CreateProcess("SLOT-REPOINT", "", "plc-w", "MES_W.Prod_Counter_01", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	t.Cleanup(func() { _ = testDB.DeleteProcess(pid) })
	path := "/api/processes/" + itoa(pid) + "/counter"
	if code := counterRepoint(t, h, r, path, `{"tag_name":"MES_W.Prod_Counter_02"}`); code != http.StatusOK {
		t.Fatalf("re-point = %d, want 200", code)
	}
	p, err := testDB.GetProcess(pid)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if p.CounterPLCName != "plc-w" || p.CounterTagName != "MES_W.Prod_Counter_02" || p.CounterEnabled ||
		p.IdentityTagName != "" || p.IdentityNone {
		t.Errorf("process after = %+v, want plc-w/MES_W.Prod_Counter_02, disabled as it was, identity derived", p)
	}
	if code := counterRepoint(t, h, r, path, `{"tag_name":"  "}`); code != http.StatusBadRequest {
		t.Errorf("a blank tag = %d, want 400", code)
	}
	bare, err := testDB.CreateProcess("SLOT-NO-PLC", "", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	t.Cleanup(func() { _ = testDB.DeleteProcess(bare) })
	if code := counterRepoint(t, h, r, "/api/processes/"+itoa(bare)+"/counter", `{"tag_name":"T"}`); code != http.StatusBadRequest {
		t.Errorf("a process with no counter PLC = %d, want 400", code)
	}
	if code := counterRepoint(t, h, r, "/api/processes/999999/counter", `{"tag_name":"T"}`); code != http.StatusNotFound {
		t.Errorf("no such process = %d, want 404", code)
	}
}
