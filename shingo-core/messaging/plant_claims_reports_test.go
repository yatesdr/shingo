//go:build docker

package messaging

import (
	"reflect"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
)

// reportRow is a process's plant_claims_reports rows as "station/digest",
// comma-joined in station order: "" when the process has none, one row when one
// station reports it.
func reportRow(t *testing.T, db *store.DB, process string) string {
	t.Helper()
	return strings.Join(processRows(t, db, process), ",")
}

// processRows is every station's plant_claims_reports row for one process, as
// "station/digest", sorted by station; empty when it has none.
func processRows(t *testing.T, db *store.DB, process string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT station_uid || '/' || digest FROM plant_claims_reports WHERE process_id = $1 ORDER BY station_uid`, process)
	testutil.MustNoErr(t, err, "read plant_claims_reports")
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var row string
		testutil.MustNoErr(t, rows.Scan(&row), "scan plant_claims_reports")
		out = append(out, row)
	}
	testutil.MustNoErr(t, rows.Err(), "iterate plant_claims_reports")
	return out
}

func digestedReport(process, digest string, styles []styleSpec) *protocol.PlantClaimsReport {
	r := plantClaimsReport(process, 0, styles)
	r.Digest = digest
	return r
}

var oneStyle = []styleSpec{{name: "A", claims: []claimSpec{{node: "LN-1", payload: "PC-A", allowed: []string{"PC-A"}}}}}

// Each report writes its process's row with the sending station and the digest
// it carried; a later report replaces it; a report with no styles deletes it
// with the mirror, and leaves other processes' rows alone.
func TestHandlePlantClaims_ReportRow(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewCoreDataService(db, &feedsPinResponder{}, service.EpochAnnounce{})
	env := feedsPinEnv("edge.test")

	steps := []struct {
		report *protocol.PlantClaimsReport
		wantP1 string
		wantP2 string
	}{
		{digestedReport("P1", "d1", oneStyle), "edge.test/d1", ""},
		{digestedReport("P2", "e1", oneStyle), "edge.test/d1", "edge.test/e1"},
		{digestedReport("P1", "d2", oneStyle), "edge.test/d2", "edge.test/e1"},
		{digestedReport("P1", "d3", nil), "", "edge.test/e1"},
	}
	for i, s := range steps {
		svc.HandlePlantClaims(env, s.report)
		if got := reportRow(t, db, "P1"); got != s.wantP1 {
			t.Errorf("step %d: P1 row = %q, want %q", i, got, s.wantP1)
		}
		if got := reportRow(t, db, "P2"); got != s.wantP2 {
			t.Errorf("step %d: P2 row = %q, want %q", i, got, s.wantP2)
		}
	}
}

// A process a second station reports is taken over by the mirror (last writer
// wins), and each station keeps its own report row, so each is quoted its own
// digest. Both stations' rows on the Edges page carry the reported-by-both
// flag, read from those rows: it stays while both report the process, through
// another report from either and through a Core restart, and goes on both when
// either empties it, which deletes every row of the process with its mirror.
func TestHandlePlantClaims_SecondStationFlagged(t *testing.T) {
	t.Parallel()
	const conflict = "process P1 reported by both edge.a and edge.b"
	flags := func(t *testing.T, svc *CoreDataService, when string, want []string) {
		t.Helper()
		for _, st := range []string{"edge.a", "edge.b"} {
			if got := svc.EdgeFeedFlags(st); !reflect.DeepEqual(got, want) {
				t.Errorf("%s: %s flags = %v, want %v", when, st, got, want)
			}
		}
	}
	db := testdb.Open(t)
	svc := NewCoreDataService(db, &feedsPinResponder{}, service.EpochAnnounce{})
	svc.HandlePlantClaims(feedsPinEnv("edge.a"), digestedReport("P1", "a1", oneStyle))
	flags(t, svc, "one station", []string{})
	svc.HandlePlantClaims(feedsPinEnv("edge.b"), digestedReport("P1", "b1", oneStyle))
	if got, want := processRows(t, db, "P1"), []string{"edge.a/a1", "edge.b/b1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("rows after the second station = %v, want %v (one per station)", got, want)
	}
	if got := mirrorStyles(t, db, "P1"); !reflect.DeepEqual(got, []string{"A@0"}) {
		t.Errorf("mirror after the second station = %v, want its report's", got)
	}
	var mirrorStation string
	testutil.MustNoErr(t, db.QueryRow(`SELECT station_uid FROM process_styles WHERE process_id = 'P1'`).Scan(&mirrorStation), "mirror station")
	if mirrorStation != "edge.b" {
		t.Errorf("mirror names %q, want edge.b (last writer wins)", mirrorStation)
	}
	for st, own := range map[string]string{"edge.a": "a1", "edge.b": "b1"} {
		got, err := db.PlantClaimsReportDigests(st)
		testutil.MustNoErr(t, err, "digests")
		if want := map[string]string{"P1": own}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s is quoted %v, want its own digest %v", st, got, want)
		}
	}
	flags(t, svc, "the second station", []string{conflict})

	svc.HandlePlantClaims(feedsPinEnv("edge.b"), digestedReport("P1", "b2", oneStyle))
	flags(t, svc, "one station reports it again", []string{conflict})
	restarted := NewCoreDataService(db, &feedsPinResponder{}, service.EpochAnnounce{})
	flags(t, restarted, "after a Core restart", []string{conflict})

	svc.HandlePlantClaims(feedsPinEnv("edge.a"), digestedReport("P1", "", nil))
	if got := processRows(t, db, "P1"); len(got) != 0 {
		t.Errorf("rows after the empty report = %v, want none: the mirror they were quoted against is gone", got)
	}
	flags(t, svc, "it is emptied", []string{})
}

// The ack's owned map answers a quoted claims family with this station's own
// processes at the digests they arrived with, and leaves the family off, the
// ack still sent, when the read fails.
func TestHandleEdgeHeartbeat_Claims(t *testing.T) {
	t.Parallel()
	const st = "edge.test"
	cases := []struct {
		name      string
		breakRead bool
		want      map[string]string // nil: the family is left off
	}{
		{"newer Edge gets its own processes", false, map[string]string{"P1": "d1", "P2": "e1"}},
		{"read fails: nil", true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testdb.Open(t)
			_, err := db.EnrollEdge(st, "")
			testutil.MustNoErr(t, err, "enroll")
			resp := &feedsPinResponder{}
			svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
			svc.HandlePlantClaims(feedsPinEnv(st), digestedReport("P1", "d1", oneStyle))
			svc.HandlePlantClaims(feedsPinEnv(st), digestedReport("P2", "e1", oneStyle))
			svc.HandlePlantClaims(feedsPinEnv("edge.other"), digestedReport("P3", "f1", oneStyle))
			if tc.breakRead {
				hideTable(t, db, "plant_claims_reports")
			}
			svc.HandleEdgeHeartbeat(feedsPinEnv(st), &protocol.EdgeHeartbeat{Feeds: map[string]string{},
				Owned: map[string]string{protocol.OwnedClaims: "stale"}})

			if got := resp.trace(); len(got) != 1 || got[0] != "reply "+protocol.SubjectEdgeHeartbeatAck {
				t.Fatalf("trace = %v, want the ack alone", got)
			}
			ack := resp.events[0].payload.(*protocol.EdgeHeartbeatAck)
			answer, answered := ack.Owned[protocol.OwnedClaims]
			if answered != (tc.want != nil) || !reflect.DeepEqual(answer.Members, tc.want) {
				t.Errorf("ack owned claims = %#v (answered %v), want %#v", answer.Members, answered, tc.want)
			}
		})
	}
}

// A mirrored process with no report row is quoted with an empty digest, so the
// Edge answers it: rows written before the station was recorded (NULL station)
// and rows naming this station. Without the quote such a mirror was never
// reconciled — a process deleted on its Edge kept its claims in Core for good
// (SPR "Test Route"). Another station's mirror is not quoted here.
func TestReportDigests_QuotesAnUnreportedMirror(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewCoreDataService(db, &feedsPinResponder{}, service.EpochAnnounce{})
	svc.HandlePlantClaims(feedsPinEnv("edge.test"), digestedReport("LIVE", "d1", oneStyle))
	svc.HandlePlantClaims(feedsPinEnv("edge.other"), digestedReport("OTHERS", "o1", oneStyle))
	for _, q := range []string{
		`INSERT INTO process_styles (process_id, style_id, station_uid) VALUES ('LEGACY', 'S1', NULL)`,
		`INSERT INTO process_styles (process_id, style_id, station_uid) VALUES ('MINE', 'S1', 'edge.test')`,
		`INSERT INTO process_styles (process_id, style_id, station_uid) VALUES ('THEIRS', 'S1', 'edge.other')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed mirror: %v", err)
		}
	}

	got, err := db.PlantClaimsReportDigests("edge.test")
	testutil.MustNoErr(t, err, "digests")
	want := map[string]string{"LIVE": "d1", "LEGACY": "", "MINE": ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("quote for edge.test = %v, want %v", got, want)
	}

	// The Edge does not have LEGACY: its empty report clears the mirror.
	svc.HandlePlantClaims(feedsPinEnv("edge.test"), digestedReport("LEGACY", "", nil))
	var n int
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM process_styles WHERE process_id = 'LEGACY'`).Scan(&n), "count")
	if n != 0 {
		t.Errorf("LEGACY mirror rows after the empty report = %d, want 0", n)
	}
}
