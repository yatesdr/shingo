package main

import (
	"database/sql"
	"testing"
)

// ---------------------------------------------------------------------------
// The round-3/4 claim config must survive the seeder.
//
// changeover_evac_nodes, changeover_evac_destination,
// changeover_load_directive, key_route and key_task were added to
// style_node_claims and to the claim editor, and left out of plantspec and out
// of this seeder — so staged tooling evacuation, evacuation-destination
// precedence, the loader card and key routes could not be SET by any scenario.
// Not "were untested": unreachable. Those features had never executed on a sim
// and there was no way to make them.
//
// allowed_payload_codes is here for a related reason: the seeder wrote it
// comma-joined while the store reads it with json.Unmarshal and discards the
// error, so every allowed-payload list a scenario declared decoded to nothing
// and the claim came up unrestricted. Silent, because nil is a legal value
// meaning "no list".
// ---------------------------------------------------------------------------

func TestSeedEdge_ClaimConfigRoundTrips(t *testing.T) {
	t.Parallel()
	db, _, _ := openSeededEdge(t)

	var positions, dest, route, task, allowed string
	var directive int
	err := db.QueryRow(`
		SELECT changeover_evac_nodes, changeover_evac_destination,
		       changeover_load_directive, key_route, key_task, allowed_payload_codes
		  FROM style_node_claims c
		  JOIN styles s ON s.id = c.style_id
		 WHERE c.core_node_name = 'PLN_001' AND s.name = 'PRESS-1-RUN'`).
		Scan(&positions, &dest, &directive, &route, &task, &allowed)
	if err == sql.ErrNoRows {
		t.Fatal("the press-index claim was not seeded at all")
	}
	if err != nil {
		t.Fatalf("read seeded claim: %v", err)
	}

	// JSON ARRAYS, because that is what the edge store reads back. A comma join
	// decodes to nothing and reads as "not configured".
	if want := `["PLN_001","PLN_002"]`; positions != want {
		t.Errorf("changeover_evac_nodes = %q, want %q — the store reads this with "+
			"json.Unmarshal and discards the error, so a wrong encoding is silent", positions, want)
	}
	if want := `["WP_AISLE_S","WP_AISLE_N"]`; route != want {
		t.Errorf("key_route = %q, want %q — ORDER IS MEANINGFUL to the fleet, so a "+
			"round trip that sorts or re-orders is a different route", route, want)
	}
	if want := `["PANEL-LH","PANEL-RH"]`; allowed != want {
		t.Errorf("allowed_payload_codes = %q, want %q", allowed, want)
	}
	// Distinctive on purpose: NOT the claim's outbound_destination, so a
	// fallback silently standing in for the real column would show.
	if dest != "SYN_MT_Return" {
		t.Errorf("changeover_evac_destination = %q, want SYN_MT_Return", dest)
	}
	if task != "unload" {
		t.Errorf("key_task = %q, want unload", task)
	}
	if directive != 0 {
		t.Errorf("changeover_load_directive = %d on a press claim, want 0", directive)
	}
}
