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

// supply_refusal_plant_claims_feedspin_test.go — behaviour pins for two
// Edge → Core state handlers: the supply refusal router (HandleSupplyRefusal)
// and the plant-claims mirror (HandlePlantClaims). They were written before
// the versioned-feeds change, each case carrying the value it predicted beside
// the value it asserted; those changes have landed, and each case asserts the
// behaviour as it now stands.

// ── HandleSupplyRefusal ────────────────────────────────────────────────────

// TestFeedsPin_HandleSupplyRefusal pins store-then-broadcast: a stored refusal
// goes to every Edge (StationBroadcast) as the same state, with the station
// taken from the envelope; a store failure broadcasts nothing; a message with
// no loader/payload is dropped before the store. "fix(core,edge): a lost supply
// refusal message heals from Core's open set" left all of that as it was (it
// added only the FeedRefusals memo drop after the store, not asserted here);
// the repair it added is on the heartbeat path (buildRefusalsFeed).
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
	}{
		{
			name:      "stored, then broadcast to every Edge",
			state:     &protocol.SupplyRefusalState{Action: protocol.SupplyRefusalOpened, LoaderNode: "LN-1", PayloadCode: "PC-A", RefusedAt: refusedAt, RefusedBy: "op-1"},
			wantTrace: []string{broadcast}, wantStored: 1,
		},
		{
			name: "store fails: nothing broadcast", breakStore: true,
			state:     &protocol.SupplyRefusalState{Action: protocol.SupplyRefusalOpened, LoaderNode: "LN-1", PayloadCode: "PC-A", RefusedAt: refusedAt, RefusedBy: "op-1"},
			wantTrace: []string{}, wantStored: -1, // table hidden; not read
		},
		{
			name:      "no loader node: dropped before the store",
			state:     &protocol.SupplyRefusalState{Action: protocol.SupplyRefusalOpened, PayloadCode: "PC-A", RefusedAt: refusedAt},
			wantTrace: []string{}, wantStored: 0,
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
				t.Fatalf("trace = %v, want %v", got, tc.wantTrace)
			}
			if len(resp.events) == 1 && resp.events[0].payload != tc.state {
				t.Errorf("broadcast payload = %+v, want the received state unchanged", resp.events[0].payload)
			}
			if tc.wantStored < 0 {
				return
			}
			var n int
			testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM supply_refusals
				WHERE loader_node = 'LN-1' AND payload_code = 'PC-A' AND closed_at IS NULL`).Scan(&n), "count open refusals")
			if n != tc.wantStored {
				t.Errorf("open rows = %d, want %d", n, tc.wantStored)
			}
			if n == 1 {
				var station, by string
				testutil.MustNoErr(t, db.QueryRow(`SELECT station_id, refused_by FROM supply_refusals
					WHERE loader_node = 'LN-1' AND payload_code = 'PC-A'`).Scan(&station, &by), "read refusal row")
				if station != st || by != "op-1" {
					t.Errorf("stored row station %q refused_by %q, want %q / op-1 — the station comes from the envelope", station, by, st)
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
// a report with no styles clears the process. Each report also leaves a
// plant_claims_reports row for its process (station_id from the envelope),
// deleted with the process on an empty report; TestHandlePlantClaims_ReportRow
// pins the row's digest.
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
	}{
		{
			name: "per-process replace drops a removed style, leaves other processes",
			reports: []*protocol.PlantClaimsReport{
				plantClaimsReport("P2", 1, one("X", "PC-X")),
				plantClaimsReport("P1", 1, two),
				plantClaimsReport("P1", 2, one("A", "PC-A")),
			},
			wantP1: []string{"A@2"}, wantP2: []string{"X@1"},
			wantRows: []string{"P1@edge.test", "P2@edge.test"},
		},
		{
			name: "older ConfigGen is ignored",
			reports: []*protocol.PlantClaimsReport{
				plantClaimsReport("P1", 5, one("NEW", "PC-N")),
				plantClaimsReport("P1", 3, one("OLD", "PC-O")),
			},
			wantP1: []string{"NEW@5"}, wantP2: []string{},
		},
		{
			name: "zero ConfigGen always applies",
			reports: []*protocol.PlantClaimsReport{
				plantClaimsReport("P1", 5, one("NEW", "PC-N")),
				plantClaimsReport("P1", 0, one("ZERO", "PC-Z")),
			},
			wantP1: []string{"ZERO@0"}, wantP2: []string{},
		},
		{
			name: "empty styles clears that process's mirror only",
			reports: []*protocol.PlantClaimsReport{
				plantClaimsReport("P2", 1, one("X", "PC-X")),
				plantClaimsReport("P1", 1, two),
				plantClaimsReport("P1", 2, nil),
			},
			wantP1: []string{}, wantP2: []string{"X@1"},
			wantRows: []string{"P2@edge.test"}, // P1's row deleted with its mirror
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
				t.Errorf("P1 mirror = %v, want %v", got, tc.wantP1)
			}
			if got := mirrorStyles(t, db, "P2"); !reflect.DeepEqual(got, tc.wantP2) {
				t.Errorf("P2 mirror = %v, want %v", got, tc.wantP2)
			}
			if tc.wantRows != nil {
				if got := reportRows(t, db); !reflect.DeepEqual(got, tc.wantRows) {
					t.Errorf("report rows = %v, want %v", got, tc.wantRows)
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
