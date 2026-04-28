package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// releaseRequest is the parsed body shape shared by the disposition-carrying
// release endpoints (apiReleaseOrder, apiReleaseNodeStagedOrders).
//
// The fields mirror what the operator-station release prompt and kanbans.js
// post; older clients that pre-date the Phase 8 disposition addition send
// only Disposition-empty bodies with called_by + qty_by_part, which the
// engine handles via buildReleaseDisposition's empty-Mode branch.
type releaseRequest struct {
	Disposition string         `json:"disposition"`
	QtyByPart   map[string]int `json:"qty_by_part"`
	CalledBy    string         `json:"called_by"`
}

// parseReleaseRequest reads and validates the JSON body for any release
// endpoint that carries a disposition. Returns the parsed body or a 400-
// suitable error.
//
// Post-2026-04-27 contract (commit c56ceb9):
//   - Empty body            → 400 "release requires a JSON body with called_by"
//   - Empty/whitespace called_by → 400 "release requires called_by ..."
//
// Both rules collapse the disposition-bypass fingerprint observed in the
// 04-27 plant test: a bare-body POST to a release endpoint produces
// called_by="" + remaining_uop=<nil> at Core, which silently skips the
// manifest sync. Every legitimate first-party caller (operator.js,
// kanbans.js) sets called_by; anything that doesn't is either an external
// script or a stale browser and should fail loudly so the caller is visible
// rather than the symptom.
//
// Used by apiReleaseOrder and apiReleaseNodeStagedOrders. The third
// release endpoint, apiReleaseChangeoverWait, has a slimmer body
// (called_by only — no disposition or qty_by_part) and applies the same
// guard inline rather than via this helper.
func parseReleaseRequest(r *http.Request) (releaseRequest, error) {
	var req releaseRequest
	if r.ContentLength == 0 {
		return req, fmt.Errorf("release requires a JSON body with called_by")
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return req, err
	}
	if strings.TrimSpace(req.CalledBy) == "" {
		return req, fmt.Errorf("release requires called_by to identify the caller")
	}
	return req, nil
}
