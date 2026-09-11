package engine

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shingo/protocol"
)

// dry_source_guard_test.go — census 11: guardSourceKnownDry, three answers.
//
// The guard stops a consume REQUEST arming a swap pair into a source Core has no
// stock for, so two legs that cannot possibly source are never created (the
// Springfield 2026-07-21 churn). It is a refusal, and a refusal is only
// wait-not-fail when there is genuinely nothing to wait FOR:
//
//	(a) Core has no bin of the payload anywhere     → refuse; create nothing.
//	(b) Core cannot be asked                         → arm; "could not find out"
//	                                                   is not knowing.
//	(c) Core has stock, all of it spoken for         → arm; that is congestion,
//	                                                   and the pair waits for it.
//
// Core's preflight answers two different questions and says so in two fields:
// `missing` (no FREE bin right now — the changeover preflight's advisory question)
// and `absent` (no bin at all). The guard asks the second.
//
// (a) and (b) are COVERAGE: they pass at bcbde0d2. (c) is a DEFECT PIN: it fails
// at bcbde0d2, because the guard refuses on `missing`, which counts a busy market
// as a dry one (ochre-marten F6).

// preflightStub answers node-bins with "occupied" (no node-empty downgrade, so the
// REQUEST really builds a pair) and preflight with the given body, or 500 when
// body is empty. A failed write is reported with Errorf: the handler runs on
// the server's goroutine, where Fatalf is not allowed.
func preflightStub(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/telemetry/node-bins":
			var rows []string
			for _, n := range strings.Split(r.URL.Query().Get("nodes"), ",") {
				if n != "" {
					rows = append(rows, `{"node_name":"`+n+`","occupied":true}`)
				}
			}
			if _, err := w.Write([]byte("[" + strings.Join(rows, ",") + "]")); err != nil {
				t.Errorf("preflight stub: write the node-bins answer: %v", err)
			}
		case "/api/inventory/preflight":
			if body == "" {
				http.Error(w, "core is having a bad day", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write([]byte(body)); err != nil {
				t.Errorf("preflight stub: write the preflight answer: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDrySourceGuard_RefusesOnlyTrueAbsence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		prefix  string
		body    string
		refused bool
	}{
		{"no bin of the payload anywhere", "DRYA",
			`{"missing":["PART-C"],"absent":["PART-C"],"available":[{"payload_code":"PART-C","bin_count":0}]}`, true},
		{"core cannot be asked", "DRYB", "", false},
		{"stock present, all of it spoken for", "DRYC",
			`{"missing":["PART-C"],"absent":[],"available":[{"payload_code":"PART-C","bin_count":0}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			nodeID := seedLineSwapClaim(t, db, tc.prefix, protocol.SwapModeTwoRobot)
			eng := testEngine(t, db)
			eng.coreClient = NewCoreClient(preflightStub(t, tc.body).URL)

			res, err := eng.RequestNodeMaterial(nodeID, 1)
			reqs := complexRequestsOnTheWire(t, db)
			if tc.refused {
				if err == nil {
					t.Fatalf("the REQUEST armed a pair into a payload Core holds no bin of (%d legs on the wire)", len(reqs))
				}
				if len(reqs) != 0 {
					t.Errorf("the refusal left %d leg(s) on the wire — refusing creates nothing, or it churns", len(reqs))
				}
				return
			}
			if err != nil {
				t.Fatalf("the REQUEST was refused (%v). %s is a wait, not a refusal — the pair should be "+
					"created and park until material is free", err, tc.name)
			}
			if res.OrderA == nil || res.OrderB == nil || len(reqs) != 2 {
				t.Fatalf("the REQUEST did not arm the pair (legs on wire: %d)", len(reqs))
			}
		})
	}
}
