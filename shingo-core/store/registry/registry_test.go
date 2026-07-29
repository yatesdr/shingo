//go:build docker

package registry_test

import (
	"errors"
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/registry"
)

// enrolled is the fixture every register test now needs, and needing it IS the
// change: before v66 a register brought a station into existence by asserting
// a name, so no test could distinguish "enrolled" from "mentioned".
func enrolled(t *testing.T, db *store.DB, uid string) {
	t.Helper()
	if _, err := registry.Enroll(db.DB, uid, "", uid); err != nil {
		t.Fatalf("Enroll %s: %v", uid, err)
	}
}

// ── GUARD 2: no statement both creates and mutates ──────────────────────────

// TestCoverage_Register_UnknownStationIsRefusedAndWritesNothing is the guard.
//
// The defect it closes is not "a register overwrote a row". It is that ONE
// STATEMENT could both create a station and mutate one, driven by a string the
// Edge chose — so a second Pi configured with the same name did not collide,
// it took turns owning the row, and `hostname = excluded.hostname` deleted the
// evidence there had been two.
//
// The assertion is deliberately in two parts. The error alone would be
// satisfied by a statement that inserted and then complained; the row count is
// what proves the statement is INCAPABLE of creating one.
func TestCoverage_Register_UnknownStationIsRefusedAndWritesNothing(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	_, err := registry.Register(db.DB, "stn-never-enrolled", "some-pi", "inst-1", "v1")
	if !errors.Is(err, registry.ErrUnknownStation) {
		t.Fatalf("Register on an unenrolled uid: err = %v, want ErrUnknownStation", err)
	}
	edges, err := registry.List(db.DB)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(edges) != 0 {
		t.Fatalf("a refused register created %d row(s) — the whole guard is that it CANNOT", len(edges))
	}
}

// TestCoverage_UpdateHeartbeat_UnknownStationIsRefusedAndWritesNothing is the
// OTHER half of guard 2, and leaving it out would have made the first half
// theatre.
//
// The old UpdateHeartbeat also upserted, and it set status='active'. So an
// unknown machine refused at Register would have appeared sixty seconds later
// anyway — with no hostname and no version on the row, which is strictly worse
// evidence than the register it was refused. found=false drives the same
// edge.register_request the old isNew flag drove; what changed is that it is
// now the only outcome.
func TestCoverage_UpdateHeartbeat_UnknownStationIsRefusedAndWritesNothing(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	found, err := registry.UpdateHeartbeat(db.DB, "stn-never-enrolled-hb")
	if err != nil {
		t.Fatalf("UpdateHeartbeat: %v", err)
	}
	if found {
		t.Error("UpdateHeartbeat reported found=true for a station that was never enrolled")
	}
	edges, _ := registry.List(db.DB)
	if len(edges) != 0 {
		t.Fatalf("a heartbeat from an unenrolled station created %d row(s), want 0", len(edges))
	}
}

// TestCoverage_Enroll_MintsOnceAndRefusesTheSecond.
//
// Enrolling is MINTING. Making it idempotent would quietly re-mint an identity
// that already has history hanging off it, which is precisely the operation
// this model exists to make impossible.
func TestCoverage_Enroll_MintsOnceAndRefusesTheSecond(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	uid, err := registry.NewStationUID()
	if err != nil {
		t.Fatalf("NewStationUID: %v", err)
	}
	e, err := registry.Enroll(db.DB, uid, "SPRINGFIELD / EDGE-2", "")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if e.StationUID != uid {
		t.Errorf("station_uid = %q, want %q", e.StationUID, uid)
	}
	if e.DisplayName != "SPRINGFIELD / EDGE-2" {
		t.Errorf("display_name = %q, want the operator's string", e.DisplayName)
	}
	if e.StationID != uid {
		t.Errorf("station_id = %q, want the uid — Address.Station's VALUE is the identity", e.StationID)
	}
	if e.Status != "enrolled" {
		t.Errorf("status = %q, want enrolled — an enrolled station has not claimed to be up yet", e.Status)
	}

	if _, err := registry.Enroll(db.DB, uid, "someone else", ""); !errors.Is(err, registry.ErrAlreadyEnrolled) {
		t.Fatalf("second Enroll of the same uid: err = %v, want ErrAlreadyEnrolled", err)
	}
	got, err := registry.GetByUID(db.DB, uid)
	if err != nil {
		t.Fatalf("GetByUID: %v", err)
	}
	if got.DisplayName != "SPRINGFIELD / EDGE-2" {
		t.Errorf("display_name = %q — a refused enroll must not have edited the incumbent", got.DisplayName)
	}
}

// TestCoverage_SetDisplayName_RenamesNothingThatIsKeyed.
//
// The rename case is the one the owner raised and the one the old model could
// not survive: display name and identity were one column, so relabelling a
// station rewrote a key under orders, mission_telemetry, outbox, node_stations,
// cell_targets and the Edge's own backup manifest. This pins that the two are
// now independent — the name moves, the uid and the routing address do not.
func TestCoverage_SetDisplayName_RenamesNothingThatIsKeyed(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	enrolled(t, db, "stn-rename")

	ok, err := registry.SetDisplayName(db.DB, "stn-rename", "LINE 4 / WELD")
	if err != nil {
		t.Fatalf("SetDisplayName: %v", err)
	}
	if !ok {
		t.Fatal("SetDisplayName reported no matching station")
	}
	e, err := registry.GetByUID(db.DB, "stn-rename")
	if err != nil {
		t.Fatalf("GetByUID: %v", err)
	}
	if e.DisplayName != "LINE 4 / WELD" {
		t.Errorf("display_name = %q, want LINE 4 / WELD", e.DisplayName)
	}
	if e.StationUID != "stn-rename" || e.StationID != "stn-rename" {
		t.Errorf("rename moved the identity: uid=%q station_id=%q, want both stn-rename",
			e.StationUID, e.StationID)
	}
}

// ── The v64 hostname detector, carried forward onto the new statement ────────

func TestCoverage_RegisterEdge_UpdatesAndKeepsTheFirstBinding(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	enrolled(t, db, "stn-line-1")

	conflict, err := registry.Register(db.DB, "stn-line-1", "host-a", "inst-a", "v1.0.0")
	if err != nil {
		t.Fatalf("Register initial: %v", err)
	}
	if conflict != nil {
		t.Fatalf("first register reported a conflict: %s", conflict)
	}
	e, err := registry.GetByUID(db.DB, "stn-line-1")
	if err != nil {
		t.Fatalf("GetByUID: %v", err)
	}
	if e.Hostname != "host-a" {
		t.Errorf("hostname = %q, want host-a", e.Hostname)
	}
	if e.Version != "v1.0.0" {
		t.Errorf("version = %q, want v1.0.0", e.Version)
	}
	if e.Status != "active" {
		t.Errorf("status = %q, want active", e.Status)
	}

	// A DIFFERENT HOSTNAME ON THE SAME STATION IS THE DUPLICATE-EDGE CASE. The
	// update still lands (see Register's comment on why it must); what is new
	// since v64 is that the register reports it and the first hostname survives.
	conflict, err = registry.Register(db.DB, "stn-line-1", "host-b", "inst-b", "v2.0.0")
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
	e2, _ := registry.GetByUID(db.DB, "stn-line-1")
	if e2.Hostname != "host-b" {
		t.Errorf("hostname after update = %q, want host-b", e2.Hostname)
	}
	if e2.Version != "v2.0.0" {
		t.Errorf("version after update = %q, want v2.0.0", e2.Version)
	}
	// THE EVIDENCE THE UPSERT USED TO DESTROY. hostname is last-seen and is
	// still overwritten; bound_hostname is not.
	if e2.BoundHostname != "host-a" {
		t.Errorf("bound_hostname = %q, want host-a — the first claimant must survive the overwrite",
			e2.BoundHostname)
	}
	if e2.ConflictHostname != "host-b" || e2.ConflictCount != 1 {
		t.Errorf("conflict row = {%q, %d}, want {host-b, 1}", e2.ConflictHostname, e2.ConflictCount)
	}
	if e2.ConflictAt == nil {
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
	enrolled(t, db, "stn-same")

	for i := range 5 {
		c, err := registry.Register(db.DB, "stn-same", "the-pi", "the-instance", "v1")
		if err != nil {
			t.Fatalf("Register %d: %v", i, err)
		}
		if c != nil {
			t.Fatalf("register %d from the same hostname reported a conflict: %s", i, c)
		}
	}
	e, _ := registry.GetByUID(db.DB, "stn-same")
	if e.BoundHostname != "the-pi" || e.ConflictCount != 0 {
		t.Errorf("after 5 clean registers: bound=%q count=%d, want the-pi/0",
			e.BoundHostname, e.ConflictCount)
	}
	if e.ConflictAt != nil {
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
	enrolled(t, db, "stn-anon")

	if _, err := registry.Register(db.DB, "stn-anon", "", "", "v1"); err != nil {
		t.Fatalf("Register with empty hostname: %v", err)
	}
	e, _ := registry.GetByUID(db.DB, "stn-anon")
	if e.BoundHostname != "" {
		t.Errorf("bound_hostname = %q after an empty-hostname register, want empty", e.BoundHostname)
	}

	// The real hostname arrives and claims the unbound station — cleanly.
	c, err := registry.Register(db.DB, "stn-anon", "real-pi", "", "v1")
	if err != nil {
		t.Fatalf("Register with real hostname: %v", err)
	}
	if c != nil {
		t.Fatalf("claiming an unbound station reported a conflict: %s", c)
	}

	// And a later empty-hostname register does not accuse the bound machine.
	c, err = registry.Register(db.DB, "stn-anon", "", "", "v1")
	if err != nil {
		t.Fatalf("Register with empty hostname after binding: %v", err)
	}
	if c != nil {
		t.Fatalf("empty hostname reported as a conflicting machine: %s", c)
	}
	e, _ = registry.GetByUID(db.DB, "stn-anon")
	if e.BoundHostname != "real-pi" || e.ConflictCount != 0 {
		t.Errorf("bound=%q count=%d, want real-pi/0", e.BoundHostname, e.ConflictCount)
	}
}

// TestCoverage_Register_ConflictCountDistinguishesFlapFromMove is the reason
// there is a COUNT and not a boolean.
//
// Two live Pis and one replaced Pi both produce "a different hostname
// registered". What separates them is what happens NEXT: two live machines keep
// taking turns, so the count keeps climbing; a replaced machine registers as
// itself from then on, so the count stops.
func TestCoverage_Register_ConflictCountDistinguishesFlapFromMove(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	enrolled(t, db, "stn-dup")

	if _, err := registry.Register(db.DB, "stn-dup", "pi-one", "inst-one", "v1"); err != nil {
		t.Fatalf("bind pi-one: %v", err)
	}
	// Two machines alternating, exactly as two edges sharing one uid do.
	for i := range 3 {
		c, err := registry.Register(db.DB, "stn-dup", "pi-two", "inst-two", "v1")
		if err != nil {
			t.Fatalf("pi-two register %d: %v", i, err)
		}
		if c == nil {
			t.Fatalf("pi-two register %d reported no conflict", i)
		}
		if _, err := registry.Register(db.DB, "stn-dup", "pi-one", "inst-one", "v1"); err != nil {
			t.Fatalf("pi-one register %d: %v", i, err)
		}
	}
	e, _ := registry.GetByUID(db.DB, "stn-dup")
	// SIX, not three, and the number is worth stating rather than fitting to.
	// After the first alternation every register is BOTH a hostname mismatch
	// and an instance recurrence; the two arms of the OR are one increment, so
	// each of the six registers after the initial bind counts once. Six is the
	// honest count of "registers that were evidence of two machines".
	if e.ConflictCount != 6 {
		t.Errorf("conflict_count = %d after 3 alternations, want 6", e.ConflictCount)
	}
	if e.BoundHostname != "pi-one" {
		t.Errorf("bound_hostname = %q, want pi-one — the binding must not drift", e.BoundHostname)
	}
}

// ── GUARD 3: the binding lease, and the case the hostname check cannot see ───

// TestCoverage_Register_InstanceRecurrenceCatchesTheSDCardClone is the reason
// guard 3 exists at all.
//
// v64's hostname detector has a false NEGATIVE on the expansion path this
// plant actually plans to use: TWO PIS FLASHED FROM ONE SD IMAGE SHARE A
// HOSTNAME, and after v66 they share the station_uid baked into that image
// too. Every field the old detector compares is identical. It sees one machine.
//
// The instance is drawn fresh per PROCESS, so the three cases separate:
//
//	one Pi restarting     → X, Y, Z — every value brand new
//	one Pi re-registering → X, X, X — reconnects reuse the value it holds
//	two clones alive      → A, B, A — a displaced instance COMES BACK
//
// Only the third recurs, which is why recurrence and not mere difference is
// the trigger. This test is that third case with the hostname held constant,
// so nothing v64 could see is different.
func TestCoverage_Register_InstanceRecurrenceCatchesTheSDCardClone(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	enrolled(t, db, "stn-clone")

	const sameHost = "shingo-edge" // one image, one hostname, two machines

	if c, err := registry.Register(db.DB, "stn-clone", sameHost, "inst-A", "v1"); err != nil || c != nil {
		t.Fatalf("clone A first register: c=%v err=%v", c, err)
	}
	// Clone B comes up. Its instance has never been seen, which is
	// indistinguishable from A having restarted — so this must NOT alarm.
	if c, err := registry.Register(db.DB, "stn-clone", sameHost, "inst-B", "v1"); err != nil || c != nil {
		t.Fatalf("clone B first register alarmed, but a fresh instance is also what a "+
			"restart looks like: c=%v err=%v", c, err)
	}
	// A registers again — from a process B already displaced. A single machine
	// cannot produce this: a reboot draws a value it has never used, and a live
	// process reuses the one it holds.
	c, err := registry.Register(db.DB, "stn-clone", sameHost, "inst-A", "v1")
	if err != nil {
		t.Fatalf("clone A second register: %v", err)
	}
	if c == nil {
		t.Fatal("a displaced instance came back and nothing fired — this is the SD-card clone, " +
			"and the hostname check is blind to it by construction")
	}
	if c.Kind != "instance" {
		t.Errorf("conflict kind = %q, want instance (the hostnames are identical here)", c.Kind)
	}
	e, _ := registry.GetByUID(db.DB, "stn-clone")
	if e.ConflictCount != 1 || e.ConflictAt == nil {
		t.Errorf("persisted conflict = {count:%d at:%v}, want {1, set} — a journal rotates, "+
			"'has this ever happened here' must be answerable in SQL", e.ConflictCount, e.ConflictAt)
	}
}

// TestCoverage_Register_RestartsNeverRecur is the false-positive guard for
// guard 3, and it is the assertion that decides whether the signal is usable.
//
// Every Edge restart draws a new instance. If a new instance alarmed, the
// alarm would fire on every deploy at every plant — and an alarm that fires on
// the routine case is one people turn off. The lease MOVES silently on a new
// instance; only a return alarms.
func TestCoverage_Register_RestartsNeverRecur(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	enrolled(t, db, "stn-restart")

	for _, inst := range []string{"boot-1", "boot-2", "boot-3", "boot-4"} {
		c, err := registry.Register(db.DB, "stn-restart", "the-pi", inst, "v1")
		if err != nil {
			t.Fatalf("register %s: %v", inst, err)
		}
		if c != nil {
			t.Fatalf("restart %s reported a conflict: %s", inst, c)
		}
		// A reconnect inside the same process reuses the instance — also clean.
		if c, err := registry.Register(db.DB, "stn-restart", "the-pi", inst, "v1"); err != nil || c != nil {
			t.Fatalf("reconnect within %s: c=%v err=%v", inst, c, err)
		}
	}
	e, _ := registry.GetByUID(db.DB, "stn-restart")
	if e.ConflictCount != 0 {
		t.Errorf("conflict_count = %d after four clean restarts, want 0", e.ConflictCount)
	}
	if e.BoundInstance != "boot-4" {
		t.Errorf("bound_instance = %q, want boot-4 — the lease follows the live process", e.BoundInstance)
	}
	if e.BoundAt == nil {
		t.Error("bound_at is NULL after the lease moved")
	}
}

// TestCoverage_Register_EmptyInstanceIsNeverJudged.
//
// An old Edge sends no instance (the field is additive/omitempty), and a
// rand.Read failure also yields "". Both mean "cannot judge", and neither may
// take the lease — taking it would make the REAL instance look like the
// intruder on the next register, which is the same trap the empty-hostname
// case sets.
func TestCoverage_Register_EmptyInstanceIsNeverJudged(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	enrolled(t, db, "stn-legacy")

	if c, err := registry.Register(db.DB, "stn-legacy", "old-pi", "inst-real", "v1"); err != nil || c != nil {
		t.Fatalf("bind: c=%v err=%v", c, err)
	}
	// An old edge (or a failed draw) registers with no instance.
	if c, err := registry.Register(db.DB, "stn-legacy", "old-pi", "", "v1"); err != nil || c != nil {
		t.Fatalf("empty instance judged: c=%v err=%v", c, err)
	}
	e, _ := registry.GetByUID(db.DB, "stn-legacy")
	if e.BoundInstance != "inst-real" {
		t.Errorf("bound_instance = %q — an empty instance must not take the lease", e.BoundInstance)
	}
	// And the real one comes back without looking like an intruder.
	if c, err := registry.Register(db.DB, "stn-legacy", "old-pi", "inst-real", "v1"); err != nil || c != nil {
		t.Fatalf("the real instance was accused after an empty one: c=%v err=%v", c, err)
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
	enrolled(t, db, "stn-swap")

	registry.Register(db.DB, "stn-swap", "old-pi", "inst-old", "v1") //nolint:errcheck // fixture
	if _, err := registry.Register(db.DB, "stn-swap", "new-pi", "inst-new", "v1"); err != nil {
		t.Fatalf("register new-pi: %v", err)
	}

	ok, err := registry.Rebind(db.DB, "stn-swap", "new-pi")
	if err != nil {
		t.Fatalf("Rebind: %v", err)
	}
	if !ok {
		t.Fatal("Rebind reported no matching station")
	}

	e, _ := registry.GetByUID(db.DB, "stn-swap")
	if e.BoundHostname != "new-pi" {
		t.Errorf("bound_hostname = %q after rebind, want new-pi", e.BoundHostname)
	}
	if e.ConflictCount != 0 || e.ConflictHostname != "" || e.ConflictAt != nil {
		t.Errorf("conflict record not cleared: {%q, %d, %v}",
			e.ConflictHostname, e.ConflictCount, e.ConflictAt)
	}
	// prev_instance is cleared too: leaving the displaced instance behind would
	// arm the recurrence detector against a machine that is now the only one
	// there, which is the latching failure Rebind exists to prevent.
	if e.PrevInstance != "" {
		t.Errorf("prev_instance = %q after rebind, want empty", e.PrevInstance)
	}

	// And the alarm stays quiet from here — the whole point of clearing it.
	c, err := registry.Register(db.DB, "stn-swap", "new-pi", "inst-new", "v1")
	if err != nil {
		t.Fatalf("register after rebind: %v", err)
	}
	if c != nil {
		t.Fatalf("register from the rebound host still conflicts: %s", c)
	}

	// Rebinding a station that does not exist reports so rather than creating
	// one: this is a correction to an existing binding, never an enrollment.
	ok, err = registry.Rebind(db.DB, "stn-nonexistent", "whatever")
	if err != nil {
		t.Fatalf("Rebind unknown station: %v", err)
	}
	if ok {
		t.Error("Rebind reported success for a station that does not exist")
	}
}

func TestCoverage_UpdateHeartbeat_FoundThenNewer(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	enrolled(t, db, "stn-fresh")

	found, err := registry.UpdateHeartbeat(db.DB, "stn-fresh")
	if err != nil {
		t.Fatalf("UpdateHeartbeat first: %v", err)
	}
	if !found {
		t.Fatal("heartbeat for an ENROLLED station reported found=false")
	}
	e, _ := registry.GetByUID(db.DB, "stn-fresh")
	if e.LastHeartbeat == nil {
		t.Fatal("last_heartbeat should be set")
	}
	firstBeat := e.LastHeartbeat
	// KEEP: timestamp separation — second heartbeat must record a later timestamp.
	time.Sleep(10 * time.Millisecond)
	if _, err := registry.UpdateHeartbeat(db.DB, "stn-fresh"); err != nil {
		t.Fatalf("UpdateHeartbeat second: %v", err)
	}
	e2, _ := registry.GetByUID(db.DB, "stn-fresh")
	if e2.LastHeartbeat != nil && firstBeat != nil && !e2.LastHeartbeat.After(*firstBeat) {
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
	enrolled(t, db, "stn-stale-1")
	enrolled(t, db, "stn-stale-2")
	registry.Register(db.DB, "stn-stale-1", "h1", "i1", "v") //nolint:errcheck // fixture
	registry.UpdateHeartbeat(db.DB, "stn-stale-1")           //nolint:errcheck // fixture
	registry.Register(db.DB, "stn-stale-2", "h2", "i2", "v") //nolint:errcheck // fixture
	registry.UpdateHeartbeat(db.DB, "stn-stale-2")           //nolint:errcheck // fixture
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
	if !seen["stn-stale-1"] || !seen["stn-stale-2"] {
		t.Errorf("stale ids = %+v, want both", stale)
	}
	for _, uid := range []string{"stn-stale-1", "stn-stale-2"} {
		e, _ := registry.GetByUID(db.DB, uid)
		if e.Status != "stale" {
			t.Errorf("edge %s status = %q, want stale", uid, e.Status)
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

// TestCoverage_MarkStaleEdges_EnrolledButNeverStartedIsNotStale.
//
// A station that has been enrolled and not yet stood up carries status
// 'enrolled' and a NULL heartbeat. Sweeping it would mark stale a station that
// has correctly never claimed to be up — and since the stale sweep also reaps
// that station's demand_registry rows, "stale" is not a cosmetic label.
func TestCoverage_MarkStaleEdges_EnrolledButNeverStartedIsNotStale(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	enrolled(t, db, "stn-not-yet")

	stale, err := registry.MarkStale(db.DB, 0)
	if err != nil {
		t.Fatalf("MarkStale: %v", err)
	}
	for _, s := range stale {
		if s == "stn-not-yet" {
			t.Fatal("an enrolled-but-never-started station was swept stale")
		}
	}
	e, _ := registry.GetByUID(db.DB, "stn-not-yet")
	if e.Status != "enrolled" {
		t.Errorf("status = %q, want enrolled", e.Status)
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
	enrolled(t, db, "stn-recent")
	enrolled(t, db, "stn-old")
	registry.Register(db.DB, "stn-recent", "h", "i", "v") //nolint:errcheck // fixture
	registry.Register(db.DB, "stn-old", "h", "i", "v")    //nolint:errcheck // fixture
	if _, err := db.DB.Exec(
		`UPDATE edge_registry SET last_heartbeat = NOW() - interval '10 seconds' WHERE station_uid = 'stn-recent'`,
	); err != nil {
		t.Fatalf("backdate stn-recent: %v", err)
	}
	if _, err := db.DB.Exec(
		`UPDATE edge_registry SET last_heartbeat = NOW() - interval '10 minutes' WHERE station_uid = 'stn-old'`,
	); err != nil {
		t.Fatalf("backdate stn-old: %v", err)
	}

	stale, err := registry.MarkStale(db.DB, 5*time.Minute)
	if err != nil {
		t.Fatalf("MarkStale: %v", err)
	}
	if len(stale) != 1 || stale[0] != "stn-old" {
		t.Fatalf("stale = %+v, want exactly [stn-old]", stale)
	}
	for uid, want := range map[string]string{"stn-recent": "active", "stn-old": "stale"} {
		e, err := registry.GetByUID(db.DB, uid)
		if err != nil {
			t.Fatalf("GetByUID %s: %v", uid, err)
		}
		if e.Status != want {
			t.Errorf("edge %s status = %q, want %q", uid, e.Status, want)
		}
	}
}
