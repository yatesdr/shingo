package www

import (
	"encoding/json"
	"net/http"
	"testing"

	"shingoedge/store/processes"
)

// decodeBody reads a response body into a generic map so a test can assert on
// keys that are ADDITIVE — present sometimes, absent otherwise.
func decodeBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

// TestUpsertClaim_FieldTaggedErrors pins the refusal contract: `error` still
// carries a plain message for every existing consumer, and `field_errors` is
// additive — the part that lets round 2 render each message on the field it is
// about instead of as one toast that does not say where to look.
func TestUpsertClaim_FieldTaggedErrors(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)
	pid := seedProcess(t, "FieldErrLine")
	sid := seedStyle(t, "FieldErrStyle", pid)

	// press-index missing BOTH the back position and the outbound destination.
	body := processes.NodeClaimInput{
		StyleID:      sid,
		CoreNodeName: "FE-PRESS",
		Role:         "produce",
		SwapMode:     "two_robot_press_index",
		PayloadCode:  "PART-FE",
	}
	resp := doRequest(t, router, "POST", "/api/style-node-claims", body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)

	got := decodeBody(t, resp)
	if _, ok := got["error"].(string); !ok {
		t.Fatalf("`error` must stay a plain string for existing consumers; body = %+v", got)
	}
	raw, ok := got["field_errors"].([]any)
	if !ok {
		t.Fatalf("`field_errors` missing from the refusal; body = %+v", got)
	}
	fields := map[string]string{}
	for _, r := range raw {
		m := r.(map[string]any)
		fields[m["field"].(string)] = m["severity"].(string)
	}
	for _, want := range []string{"paired_core_node", "outbound_destination"} {
		if fields[want] != "error" {
			t.Errorf("want an error tagged %q; got fields %+v", want, fields)
		}
	}

	// And nothing was written.
	claims, err := testDB.ListStyleNodeClaims(sid)
	if err != nil {
		t.Fatalf("list claims: %v", err)
	}
	if len(claims) != 0 {
		t.Errorf("a refused claim must not be stored; got %+v", claims)
	}
}

// TestUpsertClaim_ForeignNodeWarnsAndStillSaves is the advisory half. A node
// that belongs to another process is very often a mis-pick and sometimes a
// perfectly good shared slot, so it is reported and the write proceeds.
func TestUpsertClaim_ForeignNodeWarnsAndStillSaves(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	ownPID := seedProcess(t, "WarnOwnLine")
	otherPID := seedProcess(t, "WarnOtherLine")
	sid := seedStyle(t, "WarnStyle", ownPID)
	// The node exists — on the OTHER process.
	seedProcessNode(t, otherPID, 0, "WARN-NODE")

	// A COMPLETE sequential claim. It is complete because sequential's routing
	// is now validated like every other mode's (2026-08-28) — before that arm
	// existed this fixture saved with none of it, which is exactly the defect,
	// not a property this test wanted. What it is about is the MEMBERSHIP
	// warning, so the rest of the claim has to be clean or the warning is buried
	// under errors about fields nobody is testing.
	body := processes.NodeClaimInput{
		StyleID:             sid,
		CoreNodeName:        "WARN-NODE",
		Role:                "consume",
		SwapMode:            "sequential",
		PayloadCode:         "PART-WARN",
		PairedCoreNode:      "WARN-NODE-B",
		InboundSource:       "MARKET",
		OutboundDestination: "DEST",
	}
	resp := doRequest(t, router, "POST", "/api/style-node-claims", body, cookie)
	assertStatus(t, resp, http.StatusOK)

	got := decodeBody(t, resp)
	if _, ok := got["id"]; !ok {
		t.Fatalf("`id` must be present and unchanged for existing consumers; body = %+v", got)
	}
	raw, ok := got["warnings"].([]any)
	if !ok || len(raw) == 0 {
		t.Fatalf("want a membership warning; body = %+v", got)
	}
	w0 := raw[0].(map[string]any)
	if w0["severity"] != "warning" || w0["field"] != "core_node_name" {
		t.Errorf("warning = %+v, want a core_node_name warning", w0)
	}

	// The claim really was written — advisory means advisory.
	claims, err := testDB.ListStyleNodeClaims(sid)
	if err != nil {
		t.Fatalf("list claims: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("a warned claim must still be stored; got %+v", claims)
	}
}

// The ordinary case carries no `warnings` key at all, so a consumer that does
// not know about the field sees exactly the body it saw before.
func TestUpsertClaim_CleanSaveHasNoWarningsKey(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)
	pid := seedProcess(t, "CleanLine")
	sid := seedStyle(t, "CleanStyle", pid)
	seedProcessNode(t, pid, 0, "CLEAN-NODE")

	// Complete sequential routing — see the note in the warning test above.
	body := processes.NodeClaimInput{
		StyleID:             sid,
		CoreNodeName:        "CLEAN-NODE",
		Role:                "consume",
		SwapMode:            "sequential",
		PayloadCode:         "PART-CLEAN",
		PairedCoreNode:      "CLEAN-NODE-B",
		InboundSource:       "MARKET",
		OutboundDestination: "DEST",
	}
	resp := doRequest(t, router, "POST", "/api/style-node-claims", body, cookie)
	assertStatus(t, resp, http.StatusOK)

	got := decodeBody(t, resp)
	if _, present := got["warnings"]; present {
		t.Errorf("a clean save must not carry a warnings key; body = %+v", got)
	}
	if _, ok := got["id"]; !ok {
		t.Errorf("`id` missing; body = %+v", got)
	}
}
