package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// Lane 0 item 6 (C2c DECISIONS D9, D30): a report's positions ride the claims
// digest at +0 heartbeat bytes. A report with no positions must encode, and
// digest, exactly as it did before the field existed (TestClaimsDigest_Golden
// holds the number); a report with positions must digest differently, or a
// lost positions publish would never be re-sent.
func TestPlantClaimsReport_PositionsRideTheDigest(t *testing.T) {
	t.Parallel()
	d := digester(t)
	claim := PlantClaim{CoreNodeName: "LN-1", Role: ClaimRoleConsume, SwapMode: "simple", PayloadCode: "P1", UOPCapacity: 10}
	plain := PlantClaimsReport{ProcessID: "PR1", Styles: []PlantClaimsStyle{{StyleID: "S1", Claims: []PlantClaim{claim}}}}

	b, err := json.Marshal(plain)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	before := string(b)
	if strings.Contains(before, "positions") {
		t.Errorf("a report with no positions carries the key: %s", before)
	}

	// Set through the wire, so the pin reads the same at a base without the field.
	var withPositions PlantClaimsReport
	if err := json.Unmarshal([]byte(`{"process_id":"PR1","styles":[{"style_id":"S1","claims":[{"core_node_name":"LN-1","role":"consume","swap_mode":"simple","payload_code":"P1","uop_capacity":10}]}],"positions":["PLK-W1"]}`), &withPositions); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if a, b := d(ClaimsDigest(plain)), d(ClaimsDigest(withPositions)); a == b {
		t.Errorf("a report with positions digests as %s, the same as one without: a lost positions publish "+
			"would never be noticed and re-sent", a)
	}
	again, err := json.Marshal(withPositions)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := before[:len(before)-1] + `,"positions":["PLK-W1"]}`; string(again) != want {
		t.Errorf("positions encode as\n%s\nwant\n%s", again, want)
	}
}
