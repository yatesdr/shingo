package messaging

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"

	"shingo/protocol"
	"shingoedge/store/catalog"
)

// plant_claims_feedspin_test.go — the plant-claims report bytes and the hourly
// snapshot loop, pinned before the versioned feeds change them.

// reportBody unwraps one built plant.claims payload to the report's raw JSON
// body, the bytes Core decodes.
func reportBody(t *testing.T, payload []byte) string {
	t.Helper()
	var env protocol.Envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	var data protocol.Data
	if err := env.DecodePayload(&data); err != nil {
		t.Fatalf("decode data wrapper: %v", err)
	}
	return string(data.Body)
}

// TestFeedsPin_PlantClaimsReportBytes pins the report for a synthetic process
// with two styles, one claim each, the second style running, through both
// publish paths. The capacity is resolved from the catalog on read, so the
// catalog carries both payloads.
func TestFeedsPin_PlantClaimsReportBytes(t *testing.T) {
	t.Parallel()
	const base = `{"process_id":"PCL","styles":[` +
		`{"style_id":"PCL-S-000","claims":[{"core_node_name":"N-0","role":"produce","swap_mode":"sequential","payload_code":"PCL-P-000-0","allowed_payload_codes":["PCL-P-000-0"],"uop_capacity":100,"reorder_point":0}]},` +
		`{"style_id":"PCL-S-001","claims":[{"core_node_name":"N-0","role":"produce","swap_mode":"sequential","payload_code":"PCL-P-001-0","allowed_payload_codes":["PCL-P-001-0"],"uop_capacity":100,"reorder_point":0}],"active":true}]}`
	// F5: the same bytes with the digest appended — protocol.Digest of the
	// report without it, i.e. the first 16 hex of sha256(base), set where the
	// report is built. At the base the body was exactly `base`.
	want := base[:len(base)-1] + `,"digest":"86d786a5a3800350"}`

	cases := []struct {
		name    string
		publish func(p *PlantClaimsPublisher, pid int64) error
		label   string
	}{
		{"PublishAll", func(p *PlantClaimsPublisher, _ int64) error { return p.PublishAll() }, "F5"},
		{"PublishChanged", func(p *PlantClaimsPublisher, pid int64) error { return p.PublishChanged(pid) }, "F5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, _ := countingPublisherDB(t)
			for i, code := range []string{"PCL-P-000-0", "PCL-P-001-0"} {
				if err := db.UpsertPayloadCatalog(&catalog.CatalogEntry{ID: int64(i + 1), Name: code, Code: code, UOPCapacity: 100}); err != nil {
					t.Fatalf("catalog %s: %v", code, err)
				}
			}
			pid, ids := seedClaimsProcess(t, db, "PCL", 2, 1)
			if err := db.SetActiveStyle(pid, &ids[1]); err != nil {
				t.Fatalf("set active: %v", err)
			}
			p := NewPlantClaimsPublisher(db, "edge.test", 0)
			if err := tc.publish(p, pid); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			_, payloads := unsentClaimsPayloads(t, db)
			if len(payloads) != 1 {
				t.Fatalf("plant.claims rows = %d, want 1", len(payloads))
			}
			if got := reportBody(t, payloads[0]); got != want {
				t.Errorf("report bytes:\n got  %s\n want %s\n(%s)", got, want, tc.label)
			}
		})
	}
}

// TestFeedsPin_PlantClaimsHourlyLoop is a source-shape pin of the snapshot
// loop: a ticker on snapshotInterval (60 m by default, pinned in
// TestPlantClaimsPublisher_SnapshotIntervalIsTheSafetyNet) that calls
// PublishAll on every tick with no condition.
//
// F5 (flipped): the tick publishes only while !CoreSpeaksFeeds, read through a
// func hook set in main.go; the unconditional pattern stops matching.
func TestFeedsPin_PlantClaimsHourlyLoop(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("plant_claims_publisher.go")
	if err != nil {
		t.Fatalf("read plant_claims_publisher.go: %v", err)
	}
	cases := []struct {
		name  string
		re    *regexp.Regexp
		want  bool
		after bool
		label string
	}{
		{"ticker on snapshotInterval", regexp.MustCompile(`time\.NewTicker\(p\.snapshotInterval\)`), true, true, "same"},
		{"unconditional PublishAll on each tick",
			regexp.MustCompile(`case <-ticker\.C:\s*if err := p\.PublishAll\(\); err != nil \{`), false, false, "F5"}, // F5: gated on CoreSpeaksFeeds
	}
	for _, tc := range cases {
		if got := tc.re.Match(src); got != tc.want {
			t.Errorf("%s: matched = %v, want %v (after %s: %v)", tc.name, got, tc.want, tc.label, tc.after)
		}
	}
}
