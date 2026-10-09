//go:build docker

package messaging

import (
	"reflect"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
)

// reportRow is one plant_claims_reports row as "station/digest", "" when the
// process has none.
func reportRow(t *testing.T, db *store.DB, process string) string {
	t.Helper()
	var station, digest string
	err := db.QueryRow(`SELECT station_id, digest FROM plant_claims_reports WHERE process_id = $1`, process).
		Scan(&station, &digest)
	if err != nil {
		return ""
	}
	return station + "/" + digest
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

// A process reported by a second station is taken over (last writer wins) and
// flagged on both stations' rows; the flag clears when one station reports it
// twice running, or when it is emptied.
func TestHandlePlantClaims_SecondStationFlagged(t *testing.T) {
	t.Parallel()
	const conflict = "process P1 reported by both edge.a and edge.b"
	cases := []struct {
		name  string
		final *protocol.PlantClaimsReport
		from  string
	}{
		{"one station reports it again", digestedReport("P1", "b2", oneStyle), "edge.b"},
		{"it is emptied", digestedReport("P1", "", nil), "edge.a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testdb.Open(t)
			svc := NewCoreDataService(db, &feedsPinResponder{}, service.EpochAnnounce{})
			svc.HandlePlantClaims(feedsPinEnv("edge.a"), digestedReport("P1", "a1", oneStyle))
			if got := svc.EdgeFeedFlags("edge.a"); len(got) != 0 {
				t.Fatalf("flags after one station = %v, want none", got)
			}
			svc.HandlePlantClaims(feedsPinEnv("edge.b"), digestedReport("P1", "b1", oneStyle))
			if got := reportRow(t, db, "P1"); got != "edge.b/b1" {
				t.Errorf("row after the second station = %q, want edge.b/b1 (last writer wins)", got)
			}
			for _, st := range []string{"edge.a", "edge.b"} {
				if got := svc.EdgeFeedFlags(st); !reflect.DeepEqual(got, []string{conflict}) {
					t.Errorf("%s flags = %v, want [%s]", st, got, conflict)
				}
			}
			svc.HandlePlantClaims(feedsPinEnv(tc.from), tc.final)
			for _, st := range []string{"edge.a", "edge.b"} {
				if got := svc.EdgeFeedFlags(st); len(got) != 0 {
					t.Errorf("%s flags after %s = %v, want none", st, tc.name, got)
				}
			}
		})
	}
}

// The ack's Claims: this station's rows only for a newer Edge, nil for an
// older one, and nil — with the ack still sent — when the read fails.
func TestHandleEdgeHeartbeat_Claims(t *testing.T) {
	t.Parallel()
	const st = "edge.test"
	cases := []struct {
		name      string
		feeds     map[string]string
		breakRead bool
		want      map[string]string
	}{
		{"newer Edge gets its own processes", map[string]string{}, false, map[string]string{"P1": "d1", "P2": "e1"}},
		{"older Edge gets nil", nil, false, nil},
		{"read fails: nil", map[string]string{}, true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testdb.Open(t)
			_, err := db.EnrollEdge(st, "", st)
			testutil.MustNoErr(t, err, "enroll")
			resp := &feedsPinResponder{}
			svc := NewCoreDataService(db, resp, service.EpochAnnounce{})
			svc.HandlePlantClaims(feedsPinEnv(st), digestedReport("P1", "d1", oneStyle))
			svc.HandlePlantClaims(feedsPinEnv(st), digestedReport("P2", "e1", oneStyle))
			svc.HandlePlantClaims(feedsPinEnv("edge.other"), digestedReport("P3", "f1", oneStyle))
			if tc.breakRead {
				hideTable(t, db, "plant_claims_reports")
			}
			svc.HandleEdgeHeartbeat(feedsPinEnv(st), &protocol.EdgeHeartbeat{StationID: st, Feeds: tc.feeds})

			if got := resp.trace(); len(got) != 1 || got[0] != "reply "+protocol.SubjectEdgeHeartbeatAck {
				t.Fatalf("trace = %v, want the ack alone", got)
			}
			ack := resp.events[0].payload.(*protocol.EdgeHeartbeatAck)
			if !reflect.DeepEqual(ack.Claims, tc.want) {
				t.Errorf("ack claims = %#v, want %#v", ack.Claims, tc.want)
			}
		})
	}
}
