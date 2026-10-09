package messaging

import (
	"reflect"
	"sort"
	"strconv"
	"testing"

	"shingo/protocol"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// fakeClaimsGuard holds back a repeat of the same digest for the same process,
// as the engine's guard does inside its two minutes, and records what it saw.
type fakeClaimsGuard struct {
	last    map[string]string
	settled map[string]bool
	calls   int
}

func (g *fakeClaimsGuard) ClaimSendDue(process, digest, _ string) bool {
	g.calls++
	if g.last == nil {
		g.last = map[string]string{}
	}
	if g.last[process] == digest {
		return false
	}
	g.last[process] = digest
	return true
}

func (g *fakeClaimsGuard) ClaimsSettled(pending map[string]bool) { g.settled = pending }

// sentReports decodes every unsent plant.claims row to "process:<n styles>".
func sentReports(t *testing.T, db *store.DB) []string {
	t.Helper()
	_, payloads := unsentClaimsPayloads(t, db)
	out := make([]string, 0, len(payloads))
	for _, pl := range payloads {
		r := decodeReport(t, pl)
		out = append(out, r.ProcessID+":"+strconv.Itoa(len(r.Styles)))
	}
	sort.Strings(out)
	return out
}

func digestOf(t *testing.T, p *PlantClaimsPublisher, db *store.DB, pid int64) string {
	t.Helper()
	proc, err := processes.Get(db.DB, pid)
	if err != nil {
		t.Fatalf("get process: %v", err)
	}
	r, err := p.reportFor(*proc)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	return r.Digest
}

// The digest on the wire is ClaimsDigest of the report as sent, through the
// builder both publish paths use.
func TestPlantClaimsPublisher_DigestIsTheReportsOwn(t *testing.T) {
	t.Parallel()
	db, _ := countingPublisherDB(t)
	p := NewPlantClaimsPublisher(db, "edge.test", 0)
	pid, _ := seedClaimsProcess(t, db, "DIG", 3, 2)
	if err := p.PublishChanged(pid); err != nil {
		t.Fatalf("publish: %v", err)
	}
	_, payloads := unsentClaimsPayloads(t, db)
	if len(payloads) != 1 {
		t.Fatalf("rows = %d, want 1", len(payloads))
	}
	got := decodeReport(t, payloads[0])
	want, err := protocol.ClaimsDigest(got)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if got.Digest == "" || got.Digest != want {
		t.Errorf("wire digest = %q, want ClaimsDigest of the report %q", got.Digest, want)
	}
}

// On an ack quoting this Edge's processes: a styled process Core holds at the
// same digest is left alone; one at a different digest, or missing, is
// re-published; a name Core holds that this Edge has with no styles, or does
// not have at all, gets an empty report. Every one of those is pending; the
// converged one is not.
func TestReconcileClaims_RepublishesWhatDiffers(t *testing.T) {
	t.Parallel()
	db, _ := countingPublisherDB(t)
	p := NewPlantClaimsPublisher(db, "edge.test", 0)
	pidSame, _ := seedClaimsProcess(t, db, "SAME", 2, 1)
	seedClaimsProcess(t, db, "STALE", 1, 1)
	seedClaimsProcess(t, db, "MISSING", 1, 1)
	if _, err := db.CreateProcess("BARE", "", "", "", false); err != nil {
		t.Fatalf("create bare process: %v", err)
	}
	held := map[string]string{
		"SAME":  digestOf(t, p, db, pidSame),
		"STALE": "0000000000000000",
		"BARE":  "1111111111111111",
		"GONE":  "2222222222222222",
	}
	guard := &fakeClaimsGuard{}
	p.ReconcileClaims(held, guard)

	if got, want := sentReports(t, db), []string{"BARE:0", "GONE:0", "MISSING:1", "STALE:1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("published = %v, want %v", got, want)
	}
	if want := map[string]bool{"BARE": true, "GONE": true, "MISSING": true, "STALE": true}; !reflect.DeepEqual(guard.settled, want) {
		t.Errorf("pending = %v, want %v", guard.settled, want)
	}
}

// The guard holds a repeat: the same differing digest on the next ack is in
// flight, not re-sent.
func TestReconcileClaims_GuardHoldsARepeat(t *testing.T) {
	t.Parallel()
	db, _ := countingPublisherDB(t)
	p := NewPlantClaimsPublisher(db, "edge.test", 0)
	seedClaimsProcess(t, db, "STALE", 1, 1)
	guard := &fakeClaimsGuard{}
	held := map[string]string{"STALE": "0000000000000000", "GONE": "2222222222222222"}
	p.ReconcileClaims(held, guard)
	p.ReconcileClaims(held, guard)
	if got := sentReports(t, db); !reflect.DeepEqual(got, []string{"GONE:0", "STALE:1"}) {
		t.Errorf("published = %v, want each once", got)
	}
}

// A failed read of this Edge's own processes publishes nothing — not even the
// empty reports, since an unread spec is not evidence a process is gone — and
// settles nothing.
func TestReconcileClaims_FailedReadPublishesNothing(t *testing.T) {
	t.Parallel()
	db, _ := countingPublisherDB(t)
	p := NewPlantClaimsPublisher(db, "edge.test", 0)
	seedClaimsProcess(t, db, "STALE", 1, 1)
	if _, err := db.DB.Exec(`ALTER TABLE processes RENAME TO processes_hidden`); err != nil {
		t.Fatalf("hide processes: %v", err)
	}
	guard := &fakeClaimsGuard{}
	p.ReconcileClaims(map[string]string{"STALE": "0", "GONE": "1"}, guard)
	if got := sentReports(t, db); len(got) != 0 {
		t.Errorf("published = %v, want nothing", got)
	}
	if guard.calls != 0 || guard.settled != nil {
		t.Errorf("guard calls = %d, settled = %v; want untouched", guard.calls, guard.settled)
	}
}

// The hourly snapshot runs for an older Core (or an unwired hook) and not for
// one that quotes digests back.
func TestPlantClaimsPublisher_SnapshotTickGatedOnFeeds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		speak func() bool
		want  int
	}{
		{"hook unwired", nil, 1},
		{"older Core", func() bool { return false }, 1},
		{"Core speaks feeds", func() bool { return true }, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, _ := countingPublisherDB(t)
			p := NewPlantClaimsPublisher(db, "edge.test", 0)
			p.CoreSpeaksFeeds = tc.speak
			seedClaimsProcess(t, db, "TICK", 1, 1)
			p.snapshotTick()
			if got := len(sentReports(t, db)); got != tc.want {
				t.Errorf("rows after a tick = %d, want %d", got, tc.want)
			}
		})
	}
}
