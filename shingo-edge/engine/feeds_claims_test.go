package engine

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/config"
	"shingoedge/messaging"
	"shingoedge/store"
)

// The ack hands Claims to the publisher only when Core sent a map: nil is an
// older Core, or one that could not read its table, and nothing is read or
// sent for it. {} is a newer Core holding none, which still runs (a styled
// process this Edge has is then missing and is re-sent).
func TestOnCoreAck_ClaimsReachThePublisherOnlyWhenSent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		ack    *protocol.EdgeHeartbeatAck
		called bool
	}{
		{"older Core: no feeds, no claims", &protocol.EdgeHeartbeatAck{}, false},
		{"newer Core, claims unread", &protocol.EdgeHeartbeatAck{Feeds: map[string]string{}}, false},
		{"newer Core holding none", &protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Claims: map[string]string{}}, true},
		{"newer Core holding one", &protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Claims: map[string]string{"P1": "d1"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := testEngine(t, testEngineDB(t))
			var got map[string]string
			called := false
			e.SetClaimsAckFunc(func(claims map[string]string) { called, got = true, claims })
			e.OnCoreAck(tc.ack)
			if called != tc.called {
				t.Fatalf("publisher called = %v, want %v", called, tc.called)
			}
			if called && !reflect.DeepEqual(got, tc.ack.Claims) {
				t.Errorf("claims handed over = %v, want %v", got, tc.ack.Claims)
			}
		})
	}
}

// The claims keys share the feeds' resend guard: the same digest inside the
// guard is held back, a new digest goes, the third send in a row flags the
// process on /status, and settling it (Core quoted it back) clears the flag
// while a still-pending process keeps its own.
func TestClaimSendDue_GuardAndFlag(t *testing.T) {
	t.Parallel()
	e := testEngine(t, testEngineDB(t))
	steps := []struct {
		process, digest string
		want            bool
	}{
		{"P1", "d1", true},
		{"P1", "d1", false}, // in flight
		{"P1", "d2", true},
		{"P1", "d3", true}, // third send in a row
		{"P2", "e1", true},
		{"P2", "e2", true},
		{"P2", "e3", true},
	}
	for i, s := range steps {
		if got := e.ClaimSendDue(s.process, s.digest, ""); got != s.want {
			t.Fatalf("step %d: ClaimSendDue(%s, %s) = %v, want %v", i, s.process, s.digest, got, s.want)
		}
	}
	want := []string{"claims:P1 not converging: sent 3 times", "claims:P2 not converging: sent 3 times"}
	if got := e.FeedFlags(); !reflect.DeepEqual(got, want) {
		t.Fatalf("flags = %v, want %v", got, want)
	}
	e.ClaimsSettled(map[string]bool{"P2": true})
	if got := e.FeedFlags(); !reflect.DeepEqual(got, want[1:]) {
		t.Errorf("flags after P1 settled = %v, want %v", got, want[1:])
	}
}

// An ack that changes nothing — every feed confirmed, every process quoted at
// the digest this Edge would send, the bins watch list quoted back — still
// reads the plant spec on the inbound read loop: the publisher's reconcile
// (answerClaims → ReconcileClaims, wired as cmd/shingoedge/main.go wires it)
// lists the processes and reads each one's styles and claims, 1 + 2P
// statements for P processes, and writes and sends nothing. Counted through
// the store's counting driver (testdb.OpenCounting), at three process counts
// and a style count that differs per process, so the count is a function of P
// and not of the styles.
//
// Lane D1 caches the member digests, and a quiet ack then runs 0 statements:
// this pin fails on that change and is replaced by that assertion.
//
// Not parallel: the stations' bin lists the watch digest reads are held under
// processes.NodeGeneration, which is process-wide, so a parallel test writing
// process_nodes would force a rebuild (two more reads) inside the count.
func TestOnCoreAck_AQuietAckReadsOnePlusTwoPerProcess(t *testing.T) {
	for _, p := range []int{1, 3, 6} {
		t.Run(fmt.Sprintf("P=%d", p), func(t *testing.T) {
			db, counter := testEngineDBCounting(t)
			e := New(Config{AppConfig: &config.Config{Namespace: "test", LineID: "test-line"}, DB: db})
			// Process i has i%3+1 live styles of two produce claims each.
			for i := 0; i < p; i++ {
				pid, err := db.CreateProcess(fmt.Sprintf("QA-PROC-%02d", i), "", "", "", false)
				if err != nil {
					t.Fatalf("create process: %v", err)
				}
				for s := 0; s <= i%3; s++ {
					sid, err := db.CreateStyle(fmt.Sprintf("QA-%02d-S%d", i, s), "", pid)
					if err != nil {
						t.Fatalf("create style: %v", err)
					}
					seedProduceClaim(t, db, sid, "QA-N0", fmt.Sprintf("SYN-QA-%02d-%d-0", i, s))
					seedProduceClaim(t, db, sid, "QA-N1", fmt.Sprintf("SYN-QA-%02d-%d-1", i, s))
				}
			}
			pub := messaging.NewPlantClaimsPublisher(db, "edge.quiet", 0)
			e.SetClaimsAckFunc(func(claims map[string]string) { pub.ReconcileClaims(claims, e) })

			// Core's quote is each report's own digest: learned off the reports
			// an ack from a Core holding none makes this Edge send.
			e.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, Claims: map[string]string{}})
			quoted := drainClaimsReports(t, db)
			if len(quoted) != p {
				t.Fatalf("learned %d process digests, want %d: the fixture no longer publishes one report per process", len(quoted), p)
			}
			feeds := map[string]string{}
			for _, key := range protocol.AllFeeds() {
				d := "held-" + key
				e.holdFeed(key, d, "")
				feeds[key] = d
			}
			_, watch, ok := e.binWatchList()
			if !ok {
				t.Fatal("no bins watch digest")
			}
			ack := &protocol.EdgeHeartbeatAck{Feeds: feeds, Claims: quoted, BinWatch: &watch}
			e.OnCoreAck(ack) // the first quoting ack settles the guard and builds the station bin lists

			want := int64(1 + 2*p)
			for i := 1; i <= 2; i++ {
				counter.Reset()
				e.OnCoreAck(ack)
				if n, w := counter.Count(), counter.Writes(); n != want || w != 0 {
					t.Errorf("quiet ack %d at P=%d ran %d statements (%d writes), want 1 + 2P = %d reads and no write "+
						"(if lane D1 has landed, a quiet ack runs 0: replace this pin with that assertion); statements:\n%s",
						i, p, n, w, want, strings.Join(counter.Statements(), "\n---\n"))
				}
			}
			if left := drainClaimsReports(t, db); len(left) != 0 {
				t.Errorf("a quiet ack sent %d plant-claims reports, want none: the ack was not quiet and the count above measured something else", len(left))
			}
		})
	}
}

// drainClaimsReports acks every unsent plant-claims report in the outbox and
// returns process → digest of the reports it found.
func drainClaimsReports(t *testing.T, db *store.DB) map[string]string {
	t.Helper()
	msgs, err := db.ListUnsentOutboxByType([]string{protocol.SubjectPlantClaims})
	if err != nil {
		t.Fatalf("list plant-claims outbox: %v", err)
	}
	out := map[string]string{}
	for _, m := range msgs {
		r := decodePlantClaimsReport(t, m.Payload)
		out[r.ProcessID] = r.Digest
		if err := db.AckOutbox(m.ID); err != nil {
			t.Fatalf("ack outbox row: %v", err)
		}
	}
	return out
}
