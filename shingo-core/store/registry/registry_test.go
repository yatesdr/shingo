//go:build docker

package registry_test

import (
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store/registry"
)

func TestCoverage_RegisterEdge_InsertAndUpdate(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	conflict, err := registry.Register(db.DB, "line-1", "host-a", "v1.0.0", []string{"L1", "L2"})
	if err != nil {
		t.Fatalf("Register initial: %v", err)
	}
	if conflict != nil {
		t.Fatalf("first register reported a conflict: %s", conflict)
	}
	edges, err := registry.List(db.DB)
	if err != nil {
		t.Fatalf("List initial: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("initial len = %d, want 1", len(edges))
	}
	if edges[0].StationID != "line-1" {
		t.Errorf("station = %q, want line-1", edges[0].StationID)
	}
	if edges[0].Hostname != "host-a" {
		t.Errorf("hostname = %q, want host-a", edges[0].Hostname)
	}
	if edges[0].Version != "v1.0.0" {
		t.Errorf("version = %q, want v1.0.0", edges[0].Version)
	}
	if len(edges[0].LineIDs) != 2 || edges[0].LineIDs[0] != "L1" || edges[0].LineIDs[1] != "L2" {
		t.Errorf("line_ids = %+v, want [L1 L2]", edges[0].LineIDs)
	}
	if edges[0].Status != "active" {
		t.Errorf("status = %q, want active", edges[0].Status)
	}
	// A DIFFERENT HOSTNAME ON THE SAME STATION ID IS THE DUPLICATE-EDGE CASE,
	// and this test has always exercised it — it just had no way to say so. The
	// upsert still lands (see Register's comment on why it must), so every
	// assertion below is unchanged; what is new is that the register reports the
	// conflict and the first hostname survives it.
	conflict, err = registry.Register(db.DB, "line-1", "host-b", "v2.0.0", []string{"L9"})
	if err != nil {
		t.Fatalf("Register update: %v", err)
	}
	if conflict == nil {
		t.Fatal("second register from host-b on host-a's station reported NO conflict — " +
			"this is the shape that has to be impossible to miss")
	}
	if conflict.Bound != "host-a" || conflict.Claimant != "host-b" || conflict.Count != 1 {
		t.Errorf("conflict = {bound:%q claimant:%q count:%d}, want {host-a host-b 1}",
			conflict.Bound, conflict.Claimant, conflict.Count)
	}
	edges2, _ := registry.List(db.DB)
	if len(edges2) != 1 {
		t.Fatalf("after update len = %d, want 1 (upsert)", len(edges2))
	}
	if edges2[0].Hostname != "host-b" {
		t.Errorf("hostname after update = %q, want host-b", edges2[0].Hostname)
	}
	if edges2[0].Version != "v2.0.0" {
		t.Errorf("version after update = %q, want v2.0.0", edges2[0].Version)
	}
	if len(edges2[0].LineIDs) != 1 || edges2[0].LineIDs[0] != "L9" {
		t.Errorf("line_ids after update = %+v, want [L9]", edges2[0].LineIDs)
	}
	// THE EVIDENCE THE UPSERT USED TO DESTROY. hostname is last-seen and is
	// still overwritten; bound_hostname is not.
	if edges2[0].BoundHostname != "host-a" {
		t.Errorf("bound_hostname = %q, want host-a — the first claimant must survive the overwrite",
			edges2[0].BoundHostname)
	}
	if edges2[0].ConflictHostname != "host-b" || edges2[0].ConflictCount != 1 {
		t.Errorf("conflict row = {%q, %d}, want {host-b, 1}",
			edges2[0].ConflictHostname, edges2[0].ConflictCount)
	}
	if edges2[0].ConflictAt == nil {
		t.Error("conflict_at is NULL after a conflicting register")
	}
}

// TestCoverage_Register_SameHostnameNeverConflicts is the false-positive guard,
// and it is the one that catches an inverted predicate.
//
// The normal life of a station is many registers from one machine — every Edge
// restart, plus every core-requested re-registration. If those fired the alarm
// the alarm would be worthless within an hour of deploy, and the conflict
// columns would climb at both live plants on day one.
func TestCoverage_Register_SameHostnameNeverConflicts(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	for i := range 5 {
		c, err := registry.Register(db.DB, "line-same", "the-pi", "v1", []string{"L1"})
		if err != nil {
			t.Fatalf("Register %d: %v", i, err)
		}
		if c != nil {
			t.Fatalf("register %d from the same hostname reported a conflict: %s", i, c)
		}
	}
	edges, _ := registry.List(db.DB)
	if edges[0].BoundHostname != "the-pi" || edges[0].ConflictCount != 0 {
		t.Errorf("after 5 clean registers: bound=%q count=%d, want the-pi/0",
			edges[0].BoundHostname, edges[0].ConflictCount)
	}
	if edges[0].ConflictAt != nil {
		t.Error("conflict_at set with no conflict")
	}
}

// TestCoverage_Register_EmptyHostnameNeitherClaimsNorConflicts pins the
// unknown-hostname case.
//
// The Edge builds its register payload with `hostname, _ := os.Hostname()`
// (messaging/heartbeat.go), discarding the error — so a failure there sends "".
// An empty hostname is "I could not tell you", and it must not read either as a
// second machine (false alarm) or as a binding (which would then make the REAL
// hostname look like the intruder on the next register).
func TestCoverage_Register_EmptyHostnameNeitherClaimsNorConflicts(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	if _, err := registry.Register(db.DB, "line-anon", "", "v1", nil); err != nil {
		t.Fatalf("Register with empty hostname: %v", err)
	}
	edges, _ := registry.List(db.DB)
	if edges[0].BoundHostname != "" {
		t.Errorf("bound_hostname = %q after an empty-hostname register, want empty", edges[0].BoundHostname)
	}

	// The real hostname arrives and claims the unbound station — cleanly.
	c, err := registry.Register(db.DB, "line-anon", "real-pi", "v1", nil)
	if err != nil {
		t.Fatalf("Register with real hostname: %v", err)
	}
	if c != nil {
		t.Fatalf("claiming an unbound station reported a conflict: %s", c)
	}

	// And a later empty-hostname register does not accuse the bound machine.
	c, err = registry.Register(db.DB, "line-anon", "", "v1", nil)
	if err != nil {
		t.Fatalf("Register with empty hostname after binding: %v", err)
	}
	if c != nil {
		t.Fatalf("empty hostname reported as a conflicting machine: %s", c)
	}
	edges, _ = registry.List(db.DB)
	if edges[0].BoundHostname != "real-pi" || edges[0].ConflictCount != 0 {
		t.Errorf("bound=%q count=%d, want real-pi/0", edges[0].BoundHostname, edges[0].ConflictCount)
	}
}

// TestCoverage_Register_ConflictCountDistinguishesFlapFromMove is the reason
// there is a COUNT and not a boolean.
//
// Two live Pis and one replaced Pi both produce "a different hostname
// registered". What separates them is what happens NEXT: two live machines keep
// taking turns, so the count keeps climbing; a replaced machine registers as
// itself from then on, so the count stops. Nothing else in the system can tell
// these apart, which is why Register records a rate rather than a flag.
func TestCoverage_Register_ConflictCountDistinguishesFlapFromMove(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	if _, err := registry.Register(db.DB, "line-dup", "pi-one", "v1", nil); err != nil {
		t.Fatalf("bind pi-one: %v", err)
	}
	// Two machines alternating, exactly as two edges sharing plant-a.line-1 do.
	for i := range 3 {
		c, err := registry.Register(db.DB, "line-dup", "pi-two", "v1", nil)
		if err != nil {
			t.Fatalf("pi-two register %d: %v", i, err)
		}
		if c == nil {
			t.Fatalf("pi-two register %d reported no conflict", i)
		}
		if _, err := registry.Register(db.DB, "line-dup", "pi-one", "v1", nil); err != nil {
			t.Fatalf("pi-one register %d: %v", i, err)
		}
	}
	edges, _ := registry.List(db.DB)
	if edges[0].ConflictCount != 3 {
		t.Errorf("conflict_count = %d after 3 alternations, want 3", edges[0].ConflictCount)
	}
	if edges[0].BoundHostname != "pi-one" {
		t.Errorf("bound_hostname = %q, want pi-one — the binding must not drift", edges[0].BoundHostname)
	}
}

// TestCoverage_Rebind_ClearsTheAlarmAndMovesTheBinding.
//
// A signal that cannot be cleared is a signal people learn to ignore. After a
// legitimate box replacement bound_hostname still names the dead machine, so
// EVERY subsequent register mismatches and conflict_at stays permanently fresh.
// Rebind is the sanctioned "this station lives here now"; without it the guard
// would have to be either wrong or annoying, and annoying is how it dies.
func TestCoverage_Rebind_ClearsTheAlarmAndMovesTheBinding(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	registry.Register(db.DB, "line-swap", "old-pi", "v1", nil)
	if _, err := registry.Register(db.DB, "line-swap", "new-pi", "v1", nil); err != nil {
		t.Fatalf("register new-pi: %v", err)
	}

	ok, err := registry.Rebind(db.DB, "line-swap", "new-pi")
	if err != nil {
		t.Fatalf("Rebind: %v", err)
	}
	if !ok {
		t.Fatal("Rebind reported no matching station")
	}

	edges, _ := registry.List(db.DB)
	if edges[0].BoundHostname != "new-pi" {
		t.Errorf("bound_hostname = %q after rebind, want new-pi", edges[0].BoundHostname)
	}
	if edges[0].ConflictCount != 0 || edges[0].ConflictHostname != "" || edges[0].ConflictAt != nil {
		t.Errorf("conflict record not cleared: {%q, %d, %v}",
			edges[0].ConflictHostname, edges[0].ConflictCount, edges[0].ConflictAt)
	}

	// And the alarm stays quiet from here — the whole point of clearing it.
	c, err := registry.Register(db.DB, "line-swap", "new-pi", "v1", nil)
	if err != nil {
		t.Fatalf("register after rebind: %v", err)
	}
	if c != nil {
		t.Fatalf("register from the rebound host still conflicts: %s", c)
	}

	// Rebinding a station that does not exist reports so rather than creating
	// one: this is a correction to an existing binding, never an enrollment.
	ok, err = registry.Rebind(db.DB, "line-nonexistent", "whatever")
	if err != nil {
		t.Fatalf("Rebind unknown station: %v", err)
	}
	if ok {
		t.Error("Rebind reported success for a station that does not exist")
	}
}

// TestCoverage_Register_ClaimsAHeartbeatOnlyRow.
//
// UpdateHeartbeat inserts a minimal row for a station Core has never had a
// register from — an empty hostname, so an empty bound_hostname too. That row must be
// CLAIMABLE rather than treated as bound-to-nothing-and-therefore-conflicting,
// because it is a real state at a plant: heartbeats can land before the
// register, and Core answers one with an EdgeRegisterRequest.
func TestCoverage_Register_ClaimsAHeartbeatOnlyRow(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	if _, err := registry.UpdateHeartbeat(db.DB, "line-hb-first"); err != nil {
		t.Fatalf("UpdateHeartbeat: %v", err)
	}
	edges, _ := registry.List(db.DB)
	if edges[0].BoundHostname != "" {
		t.Fatalf("heartbeat-created row bound to %q, want unbound", edges[0].BoundHostname)
	}

	c, err := registry.Register(db.DB, "line-hb-first", "the-pi", "v1", nil)
	if err != nil {
		t.Fatalf("Register after heartbeat: %v", err)
	}
	if c != nil {
		t.Fatalf("first register against a heartbeat-created row conflicted: %s", c)
	}
	edges, _ = registry.List(db.DB)
	if edges[0].BoundHostname != "the-pi" || edges[0].ConflictCount != 0 {
		t.Errorf("bound=%q count=%d, want the-pi/0", edges[0].BoundHostname, edges[0].ConflictCount)
	}
}

func TestCoverage_UpdateHeartbeat_IsNewThenNot(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	isNew1, err := registry.UpdateHeartbeat(db.DB, "line-fresh")
	if err != nil {
		t.Fatalf("UpdateHeartbeat first: %v", err)
	}
	if !isNew1 {
		t.Error("first heartbeat for unknown station should report isNew=true")
	}
	edges, _ := registry.List(db.DB)
	if len(edges) != 1 {
		t.Fatalf("after first heartbeat len = %d, want 1", len(edges))
	}
	if edges[0].LastHeartbeat == nil {
		t.Error("last_heartbeat should be set")
	}
	firstBeat := edges[0].LastHeartbeat
	// KEEP: timestamp separation — second heartbeat must record a later timestamp.
	time.Sleep(10 * time.Millisecond)
	isNew2, err := registry.UpdateHeartbeat(db.DB, "line-fresh")
	if err != nil {
		t.Fatalf("UpdateHeartbeat second: %v", err)
	}
	if isNew2 {
		t.Error("second heartbeat should report isNew=false")
	}
	edges2, _ := registry.List(db.DB)
	if len(edges2) != 1 {
		t.Errorf("after second heartbeat len = %d, want 1", len(edges2))
	}
	if edges2[0].LastHeartbeat != nil && firstBeat != nil && !edges2[0].LastHeartbeat.After(*firstBeat) {
		t.Errorf("second heartbeat should be newer")
	}
}

// TestCoverage_MarkStaleEdges takes NO sleep and a ZERO threshold,
// deliberately.
//
// The version this replaces slept 5 ms and passed a 1 ns threshold. Those
// two numbers were the entire margin protecting a cutoff computed from the
// HOST clock against a last_heartbeat written by the DATABASE clock — so
// the test was a race between two clocks, and the 5 ms was how much skew
// it tolerated. That is I6: a container running a few milliseconds ahead
// of its host failed it, which reads as a flaky test and is a real defect
// in MarkStale (see its doc comment).
//
// MarkStale now computes the cutoff in Postgres, so this assertion rests
// on transaction ordering instead: the heartbeats above were written by
// earlier transactions, so their NOW() is strictly less than the UPDATE's
// NOW(), for every threshold down to and including zero. There is no
// margin left to lose, which is why there is no sleep left to tune.
func TestCoverage_MarkStaleEdges(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	registry.Register(db.DB, "line-stale-1", "h1", "v", nil) //nolint:errcheck // fixture
	registry.UpdateHeartbeat(db.DB, "line-stale-1")
	registry.Register(db.DB, "line-stale-2", "h2", "v", nil)
	registry.UpdateHeartbeat(db.DB, "line-stale-2")
	stale, err := registry.MarkStale(db.DB, 0)
	if err != nil {
		t.Fatalf("MarkStale: %v", err)
	}
	if len(stale) != 2 {
		t.Fatalf("stale ids len = %d, want 2", len(stale))
	}
	seen := map[string]bool{}
	for _, s := range stale {
		seen[s] = true
	}
	if !seen["line-stale-1"] || !seen["line-stale-2"] {
		t.Errorf("stale ids = %+v, want both", stale)
	}
	edges, _ := registry.List(db.DB)
	for _, e := range edges {
		if e.Status != "stale" {
			t.Errorf("edge %s status = %q, want stale", e.StationID, e.Status)
		}
	}
	again, err := registry.MarkStale(db.DB, 0)
	if err != nil {
		t.Fatalf("MarkStale second: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second MarkStale len = %d, want 0", len(again))
	}
}

// TestCoverage_MarkStaleEdges_ThresholdIsHonoured pins that the threshold
// is a bound and not decoration.
//
// The zero-threshold test above would still pass if MarkStale ignored its
// argument entirely, or if pgInterval rendered the wrong unit — a
// threshold of 15 minutes sent as "900 seconds" reads the same, but sent
// as "900 microseconds" would mark every edge at every plant on the first
// tick. This is the test that would catch that, and both timestamps are
// set BY THE DATABASE so it carries no host clock either.
func TestCoverage_MarkStaleEdges_ThresholdIsHonoured(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	registry.Register(db.DB, "line-recent", "h", "v", nil)
	registry.Register(db.DB, "line-old", "h", "v", nil)
	if _, err := db.DB.Exec(
		`UPDATE edge_registry SET last_heartbeat = NOW() - interval '10 seconds' WHERE station_id = 'line-recent'`,
	); err != nil {
		t.Fatalf("backdate line-recent: %v", err)
	}
	if _, err := db.DB.Exec(
		`UPDATE edge_registry SET last_heartbeat = NOW() - interval '10 minutes' WHERE station_id = 'line-old'`,
	); err != nil {
		t.Fatalf("backdate line-old: %v", err)
	}

	stale, err := registry.MarkStale(db.DB, 5*time.Minute)
	if err != nil {
		t.Fatalf("MarkStale: %v", err)
	}
	if len(stale) != 1 || stale[0] != "line-old" {
		t.Fatalf("stale = %+v, want exactly [line-old]", stale)
	}

	edges, err := registry.List(db.DB)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, e := range edges {
		want := "active"
		if e.StationID == "line-old" {
			want = "stale"
		}
		if e.Status != want {
			t.Errorf("edge %s status = %q, want %q", e.StationID, e.Status, want)
		}
	}
}
