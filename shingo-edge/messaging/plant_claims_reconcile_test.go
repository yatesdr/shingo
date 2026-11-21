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

// ClaimsMembers is the claims family as the engine caches it: every process
// with styles at its report's digest, the digest the report on the wire
// carries; a process with no styles is no member. The reads are a full
// snapshot's, and nothing is published.
func TestClaimsMembers_EveryStyledProcessAtItsReportsDigest(t *testing.T) {
	t.Parallel()
	db, _ := countingPublisherDB(t)
	p := NewPlantClaimsPublisher(db, "edge.test", 0)
	pidSame, _ := seedClaimsProcess(t, db, "SAME", 2, 1)
	pidStale, _ := seedClaimsProcess(t, db, "STALE", 1, 1)
	if _, err := db.CreateProcess("BARE", "", "", "", false); err != nil {
		t.Fatalf("create bare process: %v", err)
	}
	members, unread, err := p.ClaimsMembers()
	if err != nil {
		t.Fatalf("claims members: %v", err)
	}
	want := map[string]string{"SAME": digestOf(t, p, db, pidSame), "STALE": digestOf(t, p, db, pidStale)}
	if !reflect.DeepEqual(members, want) || len(unread) != 0 {
		t.Errorf("members = %v, unread = %v; want %v and none", members, unread, want)
	}
	if got := sentReports(t, db); len(got) != 0 {
		t.Errorf("reading the members published %v", got)
	}
}

// SendClaims publishes one process's report when due says so, and the empty
// report for a name with no styles here or no process at all, so Core drops
// its mirror; it says the digest of what it read either way.
func TestSendClaims_TheReportOrTheEmptyOne(t *testing.T) {
	t.Parallel()
	db, _ := countingPublisherDB(t)
	p := NewPlantClaimsPublisher(db, "edge.test", 0)
	pid, _ := seedClaimsProcess(t, db, "STALE", 1, 1)
	if _, err := db.CreateProcess("BARE", "", "", "", false); err != nil {
		t.Fatalf("create bare process: %v", err)
	}
	empty, err := withDigest(protocol.PlantClaimsReport{ProcessID: "GONE"})
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	always := func(string) bool { return true }
	for _, tc := range []struct {
		name, digest string
		styled       bool
	}{
		{"STALE", digestOf(t, p, db, pid), true},
		{"BARE", "", false},
		{"GONE", empty.Digest, false},
	} {
		digest, styled, err := p.SendClaims(tc.name, always)
		if err != nil {
			t.Fatalf("send %s: %v", tc.name, err)
		}
		if styled != tc.styled || (tc.digest != "" && digest != tc.digest) {
			t.Errorf("send %s = %s, styled %v; want %s, styled %v", tc.name, digest, styled, tc.digest, tc.styled)
		}
	}
	if got, want := sentReports(t, db), []string{"BARE:0", "GONE:0", "STALE:1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("published = %v, want %v", got, want)
	}
	if _, _, err := p.SendClaims("STALE", func(string) bool { return false }); err != nil {
		t.Fatalf("send not due: %v", err)
	}
	if got := sentReports(t, db); len(got) != 3 {
		t.Errorf("a send that was not due published: %v", got)
	}
}

// A failed read of this Edge's own process list knows nothing: ClaimsMembers
// returns the error, and SendClaims publishes nothing — not even the empty
// report, since an unread spec is not evidence a process is gone.
func TestClaimsMembers_AFailedListReadKnowsNothing(t *testing.T) {
	t.Parallel()
	db, _ := countingPublisherDB(t)
	p := NewPlantClaimsPublisher(db, "edge.test", 0)
	seedClaimsProcess(t, db, "STALE", 1, 1)
	if _, err := db.DB.Exec(`ALTER TABLE processes RENAME TO processes_hidden`); err != nil {
		t.Fatalf("hide processes: %v", err)
	}
	if members, _, err := p.ClaimsMembers(); err == nil {
		t.Errorf("members = %v with no process list, want the error", members)
	}
	asked := false
	if _, _, err := p.SendClaims("GONE", func(string) bool { asked = true; return true }); err == nil || asked {
		t.Errorf("send with no process list: err %v, asked %v; want the error and no ask", err, asked)
	}
	if got := sentReports(t, db); len(got) != 0 {
		t.Errorf("published = %v, want nothing", got)
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
