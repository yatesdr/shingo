//go:build docker

package messaging

import (
	"reflect"
	"strconv"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
)

// supply_refusal_plant_claims_feedspin_test.go — behaviour pins for the two
// Edge → Core state handlers the versioned-feeds change touches: the supply
// refusal router and the plant-claims mirror. Base value asserted; the
// predicted post-change value and its label sit beside it.

// ── HandleSupplyRefusal ────────────────────────────────────────────────────

// TestFeedsPin_HandleSupplyRefusal pins store-then-broadcast: a stored refusal
// goes to every Edge (StationBroadcast) as the same state; a store failure
// broadcasts nothing; a message with no loader/payload is dropped before the
// store. F3 leaves this handler alone (it adds a digest and a snapshot on the
// heartbeat path, and fixes a comment), so every case is "after: same".
func TestFeedsPin_HandleSupplyRefusal(t *testing.T) {
	t.Parallel()

	const st = "edge.test"
	refusedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	broadcast := "send " + protocol.SubjectSupplyRefusalState + " -> " + protocol.StationBroadcast

	cases := []struct {
		name       string
		breakStore bool
		state      *protocol.SupplyRefusalState

		wantTrace  []string
		wantStored int // open rows for LN-1/PC-A after the call

		after string
		label string
	}{
		{
			name:      "stored, then broadcast to every Edge",
			state:     &protocol.SupplyRefusalState{Action: protocol.SupplyRefusalOpened, LoaderNode: "LN-1", PayloadCode: "PC-A", RefusedAt: refusedAt, RefusedBy: "op-1"},
			wantTrace: []string{broadcast}, wantStored: 1,
			after: "same", label: "-",
		},
		{
			name: "store fails: nothing broadcast", breakStore: true,
			state:     &protocol.SupplyRefusalState{Action: protocol.SupplyRefusalOpened, LoaderNode: "LN-1", PayloadCode: "PC-A", RefusedAt: refusedAt, RefusedBy: "op-1"},
			wantTrace: []string{}, wantStored: -1, // table hidden; not read
			after: "same", label: "-",
		},
		{
			name:      "no loader node: dropped before the store",
			state:     &protocol.SupplyRefusalState{Action: protocol.SupplyRefusalOpened, PayloadCode: "PC-A", RefusedAt: refusedAt},
			wantTrace: []string{}, wantStored: 0,
			after: "same", label: "-",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testdb.Open(t)
			if tc.breakStore {
				hideTable(t, db, "supply_refusals")
			}
			resp := &feedsPinResponder{}
			svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
			svc.HandleSupplyRefusal(feedsPinEnv(st), tc.state)

			if got := resp.trace(); !reflect.DeepEqual(got, tc.wantTrace) {
				t.Fatalf("trace = %v, want %v (after: %s)", got, tc.wantTrace, tc.after)
			}
			if len(resp.events) == 1 && resp.events[0].payload != tc.state {
				t.Errorf("broadcast payload = %+v, want the received state unchanged (after: same)", resp.events[0].payload)
			}
			if tc.wantStored < 0 {
				return
			}
			var n int
			testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM supply_refusals
				WHERE loader_node = 'LN-1' AND payload_code = 'PC-A' AND closed_at IS NULL`).Scan(&n), "count open refusals")
			if n != tc.wantStored {
				t.Errorf("open rows = %d, want %d (after: same)", n, tc.wantStored)
			}
			if n == 1 {
				var station, by string
				testutil.MustNoErr(t, db.QueryRow(`SELECT station_id, refused_by FROM supply_refusals
					WHERE loader_node = 'LN-1' AND payload_code = 'PC-A'`).Scan(&station, &by), "read refusal row")
				if station != st || by != "op-1" {
					t.Errorf("stored row station %q refused_by %q, want %q / op-1 — the station comes from the envelope (after: same)", station, by, st)
				}
			}
		})
	}
}

// ── HandlePlantClaims ──────────────────────────────────────────────────────

// mirrorStyles is the process's mirror as "style@config_gen", sorted by style.
func mirrorStyles(t *testing.T, db *store.DB, process string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT style_id, config_gen FROM process_styles WHERE process_id = $1 ORDER BY style_id`, process)
	testutil.MustNoErr(t, err, "read process_styles")
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var style string
		var gen int64
		testutil.MustNoErr(t, rows.Scan(&style, &gen), "scan process_styles")
		out = append(out, style+"@"+strconv.FormatInt(gen, 10))
	}
	testutil.MustNoErr(t, rows.Err(), "iterate process_styles")
	return out
}

// TestFeedsPin_HandlePlantClaims pins the mirror's write rules: each report
// replaces its process wholesale and leaves other processes alone; an older
// ConfigGen than the mirror holds is ignored; zero ConfigGen always applies;
// a report with no styles clears the process. F5 keeps all of that and adds a
// plant_claims_reports row per process (station_id from the envelope, the
// report's digest), deleted with the process on an empty report.
func TestFeedsPin_HandlePlantClaims(t *testing.T) {
	t.Parallel()

	one := func(style, payload string) []styleSpec {
		return []styleSpec{{name: style, claims: []claimSpec{{node: "LN-1", payload: payload, allowed: []string{payload}}}}}
	}
	two := []styleSpec{
		{name: "A", claims: []claimSpec{{node: "LN-1", payload: "PC-A", allowed: []string{"PC-A"}}}},
		{name: "B", claims: []claimSpec{{node: "LN-2", payload: "PC-B", allowed: []string{"PC-B"}}}},
	}

	cases := []struct {
		name    string
		reports []*protocol.PlantClaimsReport // applied in order, all from edge.test

		wantP1 []string // mirror of process P1 after the reports
		wantP2 []string // mirror of process P2 (untouched bystander)
		// wantRows is plant_claims_reports as "process@station", sorted; nil
		// where the case does not assert it.
		wantRows []string

		after string
		label string
	}{
		{
			name: "per-process replace drops a removed style, leaves other processes",
			reports: []*protocol.PlantClaimsReport{
				plantClaimsReport("P2", 1, one("X", "PC-X")),
				plantClaimsReport("P1", 1, two),
				plantClaimsReport("P1", 2, one("A", "PC-A")),
			},
			wantP1: []string{"A@2"}, wantP2: []string{"X@1"},
			wantRows: []string{"P1@edge.test", "P2@edge.test"}, // F5
			after:    "same, plus a report row per process (F5)", label: "F5",
		},
		{
			name: "older ConfigGen is ignored",
			reports: []*protocol.PlantClaimsReport{
				plantClaimsReport("P1", 5, one("NEW", "PC-N")),
				plantClaimsReport("P1", 3, one("OLD", "PC-O")),
			},
			wantP1: []string{"NEW@5"}, wantP2: []string{},
			after: "same", label: "-",
		},
		{
			name: "zero ConfigGen always applies",
			reports: []*protocol.PlantClaimsReport{
				plantClaimsReport("P1", 5, one("NEW", "PC-N")),
				plantClaimsReport("P1", 0, one("ZERO", "PC-Z")),
			},
			wantP1: []string{"ZERO@0"}, wantP2: []string{},
			after: "same", label: "-",
		},
		{
			name: "empty styles clears that process's mirror only",
			reports: []*protocol.PlantClaimsReport{
				plantClaimsReport("P2", 1, one("X", "PC-X")),
				plantClaimsReport("P1", 1, two),
				plantClaimsReport("P1", 2, nil),
			},
			wantP1: []string{}, wantP2: []string{"X@1"},
			wantRows: []string{"P2@edge.test"}, // F5
			after:    "same, and P1's plant_claims_reports row is deleted too", label: "F5",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testdb.Open(t)
			svc := NewCoreDataService(db, &feedsPinResponder{}, service.EpochAnnounce{})
			for _, r := range tc.reports {
				svc.HandlePlantClaims(feedsPinEnv("edge.test"), r)
			}
			if got := mirrorStyles(t, db, "P1"); !reflect.DeepEqual(got, tc.wantP1) {
				t.Errorf("P1 mirror = %v, want %v (after %s: %s)", got, tc.wantP1, tc.label, tc.after)
			}
			if got := mirrorStyles(t, db, "P2"); !reflect.DeepEqual(got, tc.wantP2) {
				t.Errorf("P2 mirror = %v, want %v (after: same)", got, tc.wantP2)
			}
			if tc.wantRows != nil {
				if got := reportRows(t, db); !reflect.DeepEqual(got, tc.wantRows) {
					t.Errorf("report rows = %v, want %v (%s)", got, tc.wantRows, tc.label)
				}
			}
		})
	}
}

// reportRows is plant_claims_reports as "process@station", sorted by process.
func reportRows(t *testing.T, db *store.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT process_id, station_id FROM plant_claims_reports ORDER BY process_id`)
	testutil.MustNoErr(t, err, "read plant_claims_reports")
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var process, station string
		testutil.MustNoErr(t, rows.Scan(&process, &station), "scan plant_claims_reports")
		out = append(out, process+"@"+station)
	}
	testutil.MustNoErr(t, rows.Err(), "iterate plant_claims_reports")
	return out
}

// TestFeedsPin_HandlePlantClaims_ReportRecord pins the per-process record F5
// adds. At the base there was no plant_claims_reports table (Core kept no
// record of who reported a process or what digest it carried); after F5 one
// report from edge.test for P1 leaves exactly one row: process_id P1,
// station_id edge.test, the report's Digest, received_at set. The ack's
// Claims is read from it.
func TestFeedsPin_HandlePlantClaims_ReportRecord(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewCoreDataService(db, &feedsPinResponder{}, service.EpochAnnounce{})
	report := plantClaimsReport("P1", 1, []styleSpec{
		{name: "A", claims: []claimSpec{{node: "LN-1", payload: "PC-A", allowed: []string{"PC-A"}}}},
	})
	report.Digest = "0123456789abcdef"
	svc.HandlePlantClaims(feedsPinEnv("edge.test"), report)

	// F5: the table exists and holds the one row (base: no table).
	var n int
	var station, digest string
	var stamped bool
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) OVER (), station_id, digest, received_at IS NOT NULL
		FROM plant_claims_reports WHERE process_id = 'P1'`).Scan(&n, &station, &digest, &stamped), "read plant_claims_reports")
	if n != 1 || station != "edge.test" || digest != report.Digest || !stamped {
		t.Errorf("report row = (%d rows, station %q, digest %q, stamped %v), want 1 row from edge.test with digest %q, stamped (F5)",
			n, station, digest, stamped, report.Digest)
	}
}
