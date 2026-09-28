package www

import (
	"fmt"
	"net/http"
	"testing"
)

// handlers_styles_delete_guard_test.go — a style a cell is running, or an open
// changeover is taking a cell to, cannot be retired out from under it (I3,
// CW#15).
//
// Retiring drops the style's claims from Core's demand registry (the publisher
// walks live styles only), so retiring the ACTIVE style silently ended every
// demand the running cell had, and retiring a changeover's TARGET left the
// changeover heading for a style with no claims. Both are 409 with the reason.
// An idle style retires as before.
func TestApiDeleteStyle_RefusesActiveAndChangeoverTarget(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)
	pid := seedProcess(t, "StyleDeleteGuardLine")

	mk := func(name string) int64 {
		id, err := testDB.CreateStyle(name, "", pid)
		if err != nil {
			t.Fatalf("CreateStyle %s: %v", name, err)
		}
		return id
	}
	active, target, idle := mk("SDG-Active"), mk("SDG-Target"), mk("SDG-Idle")
	if err := testDB.SetActiveStyle(pid, &active); err != nil {
		t.Fatalf("SetActiveStyle: %v", err)
	}
	if _, err := testDB.Exec(`INSERT INTO process_changeovers (process_id, from_style_id, to_style_id, state, called_by)
		VALUES (?, ?, ?, 'in_progress', 'test')`, pid, active, target); err != nil {
		t.Fatalf("seed changeover: %v", err)
	}

	del := func(id int64) *http.Response {
		return doRequest(t, router, "DELETE", fmt.Sprintf("/api/styles/%d", id), nil, cookie)
	}
	assertStatus(t, del(active), http.StatusConflict)
	assertStatus(t, del(target), http.StatusConflict)
	assertStatus(t, del(idle), http.StatusOK)

	for _, id := range []int64{active, target} {
		s, err := testDB.GetStyle(id)
		if err != nil {
			t.Fatalf("GetStyle %d: %v", id, err)
		}
		if s.DeletedAt != nil {
			t.Errorf("style %d was retired by a refused delete", id)
		}
	}
	if s, _ := testDB.GetStyle(idle); s == nil || s.DeletedAt == nil {
		t.Errorf("idle style was not retired")
	}
}
