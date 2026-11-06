// process_counter_edit_test.go — the counter and identity edit: the reporting
// point found by tag, the refusals, the identity tag's three states, the
// monitor's re-prime and its published snapshot.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/config"
	"shingoedge/domain"
	"shingoedge/plc"
	"shingoedge/store"
)

// recordingWarlink is a WarLink that lists the tags it is given and records
// every publish toggle.
type recordingWarlink struct {
	curtainStubClient
	mu        sync.Mutex
	tags      []plc.WarlinkTagInfo
	published []string // "on PLC.TAG" / "off PLC.TAG"
}

func (w *recordingWarlink) ListAllTags(context.Context, string) ([]plc.WarlinkTagInfo, error) {
	return w.tags, nil
}

func (w *recordingWarlink) SetTagPublishing(_ context.Context, plcName, tagName string, enabled bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	word := "off"
	if enabled {
		word = "on"
	}
	w.published = append(w.published, word+" "+plcName+"."+tagName)
	return nil
}

func (w *recordingWarlink) toggles() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.published...)
}

// counterRig is a process counting on plc/MES_Cell.Prod_Counter_01 under one
// active style, its point created, with a recording WarLink that declares the
// two cells' counters as DINT and one REAL.
func counterRig(t *testing.T) (eng *Engine, db *store.DB, pid, style int64, wl *recordingWarlink) {
	t.Helper()
	db = testEngineDB(t)
	eng = testEngine(t, db)
	wl = &recordingWarlink{tags: []plc.WarlinkTagInfo{
		{Name: "MES_Cell.Prod_Counter_01", Type: "DINT"},
		{Name: "MES_Other.Prod_Counter_01", Type: "DINT"},
		{Name: "MES_Cell.Cycle_Time_Average", Type: "REAL"},
	}}
	eng.plcMgr = plc.NewManager(nil, config.WarLinkConfig{}, 0, 0, nil, wl)
	var err error
	pid, err = db.CreateProcess("CELL", "", "plc", "MES_Cell.Prod_Counter_01", true)
	testutil.MustNoErr(t, err, "create process")
	style, err = db.CreateStyle("RUN", "", pid)
	testutil.MustNoErr(t, err, "create style")
	testutil.MustNoErr(t, db.SetActiveStyle(pid, &style), "active style")
	testutil.MustNoErr(t, eng.syncPrimaryPoint(pid, "", ""), "first sync")
	wl.published = nil // the counter tag's own publish is the rig's, not the case's
	return eng, db, pid, style, wl
}

// TestSync_TagEditNeverRepointsTheOrphan: a point left on the same style by an
// older re-point, at the LOWER id (a LIMIT 1 by style scans rowid order and
// finds it first), stays where it is; the process's own point follows the tag.
func TestSync_TagEditNeverRepointsTheOrphan(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	eng.plcMgr = seedRigManager()
	pid, err := db.CreateProcess("CELL", "", "plc", "TAG_A", true)
	testutil.MustNoErr(t, err, "create process")
	style, err := db.CreateStyle("RUN", "", pid)
	testutil.MustNoErr(t, err, "create style")
	testutil.MustNoErr(t, db.SetActiveStyle(pid, &style), "active style")
	orphan, err := db.CreateReportingPoint("plc", "TAG_GHOST", style)
	testutil.MustNoErr(t, err, "seed orphan")
	testutil.MustNoErr(t, eng.syncPrimaryPoint(pid, "", ""), "first sync")
	own, err := db.GetReportingPointByTag("plc", "TAG_A")
	testutil.MustNoErr(t, err, "own point")
	if own.ID < orphan {
		t.Fatalf("fixture: own point %d must sit above the orphan %d", own.ID, orphan)
	}

	testutil.MustNoErr(t, db.SetProcessCounter(pid, "plc", "TAG_B", true, "", false), "edit tag")
	testutil.MustNoErr(t, eng.syncPrimaryPoint(pid, "plc", "TAG_A"), "sync")
	got, err := db.GetReportingPoint(own.ID)
	testutil.MustNoErr(t, err, "own point after")
	if got.TagName != "TAG_B" {
		t.Errorf("the process's point reads %q, want TAG_B", got.TagName)
	}
	ghost, err := db.GetReportingPoint(orphan)
	testutil.MustNoErr(t, err, "orphan after")
	if ghost.TagName != "TAG_GHOST" {
		t.Errorf("the orphan was re-pointed to %q", ghost.TagName)
	}
}

// TestSync_BlankCounterDisablesThePoint: clearing the counter fields stops the
// counting — the page says "not wired", and the point must agree.
func TestSync_BlankCounterDisablesThePoint(t *testing.T) {
	t.Parallel()
	eng, db, pid, _, _ := counterRig(t)
	testutil.MustNoErr(t, db.SetProcessCounter(pid, "", "", true, "", false), "blank the counter")
	testutil.MustNoErr(t, eng.syncPrimaryPoint(pid, "plc", "MES_Cell.Prod_Counter_01"), "sync")
	rp, err := db.GetReportingPointByTag("plc", "MES_Cell.Prod_Counter_01")
	testutil.MustNoErr(t, err, "point")
	if rp.Enabled {
		t.Error("the point still counts after the counter was blanked")
	}
}

// TestSync_RecreatedProcessAdoptsTheDeletedProcessesPoint: a process made again
// on a deleted process's tag counts on the point that tag already has.
// Characterization: green before and after.
func TestSync_RecreatedProcessAdoptsTheDeletedProcessesPoint(t *testing.T) {
	t.Parallel()
	eng, db, pid, _, _ := counterRig(t)
	before, err := db.GetReportingPointByTag("plc", "MES_Cell.Prod_Counter_01")
	testutil.MustNoErr(t, err, "point")
	testutil.MustNoErr(t, eng.DeleteProcess(pid), "delete process")

	again, err := db.CreateProcess("CELL-2", "", "plc", "MES_Cell.Prod_Counter_01", true)
	testutil.MustNoErr(t, err, "create process again")
	style, err := db.CreateStyle("RUN-2", "", again)
	testutil.MustNoErr(t, err, "create style")
	testutil.MustNoErr(t, db.SetActiveStyle(again, &style), "active style")
	testutil.MustNoErr(t, eng.syncPrimaryPoint(again, "", ""), "sync")
	after, err := db.GetReportingPointByTag("plc", "MES_Cell.Prod_Counter_01")
	testutil.MustNoErr(t, err, "point after")
	if after.ID != before.ID || after.StyleID != style {
		t.Errorf("point = id %d style %d, want the deleted process's point %d on style %d", after.ID, after.StyleID, before.ID, style)
	}
}

// TestCounterEdit_RefusedWhileChangeoverOpen: a re-point while a changeover is
// open would move the count the cutover is about to attribute.
func TestCounterEdit_RefusedWhileChangeoverOpen(t *testing.T) {
	t.Parallel()
	eng, db, pid, style, _ := counterRig(t)
	target, err := db.CreateStyle("NEXT", "", pid)
	testutil.MustNoErr(t, err, "create style")
	_, err = eng.changeoverService.Create(pid, &style, target, "test", "", nil, nil, nil, nil)
	testutil.MustNoErr(t, err, "open a changeover")

	err = eng.EditProcessCounter(pid, domain.ProcessCounterEdit{PLCName: "plc", TagName: "MES_Other.Prod_Counter_01", Enabled: true})
	if !errors.Is(err, ErrChangeoverActive) {
		t.Fatalf("edit with a changeover open = %v, want ErrChangeoverActive", err)
	}
	if p := mustProcess(t, db, pid); p.CounterTagName != "MES_Cell.Prod_Counter_01" {
		t.Errorf("a refused edit wrote the tag: %q", p.CounterTagName)
	}
}

// TestSave_RealTagRefused: the poll reads a number and truncates it, so a REAL
// tag (a cycle-time average) would count its truncated differences.
func TestSave_RealTagRefused(t *testing.T) {
	t.Parallel()
	eng, db, pid, _, _ := counterRig(t)
	err := eng.EditProcessCounter(pid, domain.ProcessCounterEdit{PLCName: "plc", TagName: "MES_Cell.Cycle_Time_Average", Enabled: true})
	if err == nil || !strings.Contains(err.Error(), "REAL") {
		t.Fatalf("a REAL counter tag = %v, want a refusal naming its type", err)
	}
	if p := mustProcess(t, db, pid); p.CounterTagName != "MES_Cell.Prod_Counter_01" {
		t.Errorf("a refused edit wrote the tag: %q", p.CounterTagName)
	}
}

// primedRig is counterRig with the monitor primed on it.
func primedRig(t *testing.T) (eng *Engine, db *store.DB, pid int64, cm *catidMonitor, wl *recordingWarlink) {
	t.Helper()
	eng, db, pid, _, wl = counterRig(t)
	cm = &catidMonitor{eng: eng, states: map[int64]*catidState{}}
	cm.prime()
	eng.catidMon = cm
	return eng, db, pid, cm, wl
}

// TestPrime_RepointTakesEffectWithoutRestart: a counter moved to another struct
// moves the monitor to that struct's identity tag on the save. The new identity
// is published and recorded as the Edge's; the old one, enabled only by prime,
// is left published, and a save's own is unpublished when the next save moves.
func TestPrime_RepointTakesEffectWithoutRestart(t *testing.T) {
	t.Parallel()
	eng, _, pid, cm, wl := primedRig(t)
	testutil.MustNoErr(t, eng.EditProcessCounter(pid, domain.ProcessCounterEdit{
		PLCName: "plc", TagName: "MES_Other.Prod_Counter_01", Enabled: true}), "re-point")
	st, ok := cm.states[pid]
	if !ok || st.tagName != "MES_Other.CATID_01" {
		t.Fatalf("monitor state = %+v, want MES_Other.CATID_01", st)
	}
	toggles := strings.Join(wl.toggles(), ",")
	if strings.Contains(toggles, "off plc.MES_Cell.CATID_01") || !strings.Contains(toggles, "on plc.MES_Other.CATID_01") {
		t.Errorf("publish toggles = %s, want the new identity on and prime's left alone", toggles)
	}
	testutil.MustNoErr(t, eng.EditProcessCounter(pid, domain.ProcessCounterEdit{
		PLCName: "plc", TagName: "MES_Cell.Prod_Counter_01", Enabled: true}), "re-point back")
	if toggles := strings.Join(wl.toggles(), ","); !strings.Contains(toggles, "off plc.MES_Other.CATID_01") {
		t.Errorf("publish toggles = %s, want the identity the first save published off", toggles)
	}
}

// TestReprime_UnrelatedSaveKeepsTheArm: a save that moves neither tag (here the
// enabled box) leaves the monitor's tracker — its settle window, its decision —
// as it was.
func TestReprime_UnrelatedSaveKeepsTheArm(t *testing.T) {
	t.Parallel()
	eng, _, pid, cm, _ := primedRig(t)
	before := cm.states[pid]
	before.armCandidate, before.armCount = "1002", 2
	testutil.MustNoErr(t, eng.EditProcessCounter(pid, domain.ProcessCounterEdit{
		PLCName: "plc", TagName: "MES_Cell.Prod_Counter_01", Enabled: false}), "toggle enabled")
	if cm.states[pid] != before || before.armCount != 2 {
		t.Errorf("an unrelated save replaced the tracker: %+v", cm.states[pid])
	}
}

// TestReprime_ConcurrentWithTick: a re-prime on the HTTP goroutine and the
// monitor's tick touch the same maps; under -race they must not interleave.
func TestReprime_ConcurrentWithTick(t *testing.T) {
	t.Parallel()
	eng, _, pid, cm, _ := primedRig(t)
	eng.plcMgr.SetTagValueForTest("plc", "MES_Cell.CATID_01", float64(1001))
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		now := time.Unix(0, 0)
		for {
			select {
			case <-stop:
				return
			default:
				now = now.Add(time.Second)
				cm.tick(now)
			}
		}
	}()
	for i := 0; i < 50; i++ {
		cm.reprime(pid)
		_, _ = cm.snapshot(pid)
	}
	close(stop)
	<-done
	if _, ok := cm.states[pid]; !ok {
		t.Error("the process is no longer monitored after the re-primes")
	}
}

// TestIdentity_OmittedFieldsDerive: blank identity fields read CATID_0N from
// the counter tag, as before the fields existed.
func TestIdentity_OmittedFieldsDerive(t *testing.T) {
	t.Parallel()
	_, _, pid, cm, wl := primedRig(t)
	if st := cm.states[pid]; st == nil || st.tagName != "MES_Cell.CATID_01" {
		t.Fatalf("state = %+v, want the derived MES_Cell.CATID_01", st)
	}
	if got := strings.Join(wl.toggles(), ","); got != "on plc.MES_Cell.CATID_01" {
		t.Errorf("toggles = %s", got)
	}
}

// TestIdentity_NoneEnablesNothing: a process with identity "none" is not
// published, not monitored, and has no snapshot.
func TestIdentity_NoneEnablesNothing(t *testing.T) {
	t.Parallel()
	eng, db, pid, _, wl := counterRig(t)
	testutil.MustNoErr(t, db.SetProcessCounter(pid, "plc", "MES_Cell.Prod_Counter_01", true, "", true), "identity none")
	cm := &catidMonitor{eng: eng, states: map[int64]*catidState{}}
	cm.prime()
	if _, ok := cm.states[pid]; ok {
		t.Error("a process with identity none is monitored")
	}
	if _, ok := cm.snapshot(pid); ok {
		t.Error("a process with identity none has a snapshot")
	}
	if got := wl.toggles(); len(got) != 0 {
		t.Errorf("identity none published %v", got)
	}
}

// TestIdentity_OverrideWins: a typed identity tag is read instead of the
// derived one.
func TestIdentity_OverrideWins(t *testing.T) {
	t.Parallel()
	eng, db, pid, _, _ := counterRig(t)
	testutil.MustNoErr(t, db.SetProcessCounter(pid, "plc", "MES_Cell.Prod_Counter_01", true, "MES_Cell.Part_ID", false), "override")
	cm := &catidMonitor{eng: eng, states: map[int64]*catidState{}}
	cm.prime()
	if st := cm.states[pid]; st == nil || st.tagName != "MES_Cell.Part_ID" {
		t.Errorf("state = %+v, want the override MES_Cell.Part_ID", st)
	}
}

// TestSnapshot_PublishedOnConfirmAndSwappedOnReprime: the snapshot carries the
// confirmed value and the last good read, grows its observed set on a confirm,
// and starts empty again after a re-prime. Read with no lock while the monitor
// ticks (-race).
func TestSnapshot_PublishedOnConfirmAndSwappedOnReprime(t *testing.T) {
	t.Parallel()
	eng, _, pid, cm, _ := primedRig(t)
	eng.plcMgr.SetTagValueForTest("plc", "MES_Cell.CATID_01", float64(1001))
	var readers sync.WaitGroup
	stop := make(chan struct{})
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if s, ok := cm.snapshot(pid); ok {
					_ = s.value
					_ = len(s.observed)
				}
			}
		}
	}()
	t0 := time.Unix(1000, 0)
	cm.tick(t0)
	snap, ok := cm.snapshot(pid)
	if !ok || snap.value != "1001" || !snap.lastOK.Equal(t0) || !catidSetHas(snap.observed, "1001") {
		t.Fatalf("after the first read: %+v, want 1001 read at t0 and observed", snap)
	}
	cm.tick(t0.Add(time.Second))
	if s, _ := cm.snapshot(pid); !s.lastOK.Equal(t0.Add(time.Second)) {
		t.Errorf("a good read did not refresh lastOK: %v", s.lastOK)
	}
	cm.reprime(pid)
	close(stop)
	readers.Wait()
	after, ok := cm.snapshot(pid)
	if !ok || after.value != "" || !after.lastOK.IsZero() || !catidSetHas(after.observed, "1001") {
		t.Errorf("after the re-prime: %+v, want no value, no read, the observed values kept", after)
	}
}

// TestSave_UnlistedMemberAcceptedByItsRead: WarLink's tag list carries the
// structs, not their members, so a dotted counter member is not on it. Such a
// tag is enabled and read, and a whole-number read is accepted (the type is
// inferred from the read, not declared).
func TestSave_UnlistedMemberAcceptedByItsRead(t *testing.T) {
	t.Parallel()
	eng, db, pid, _, wl := counterRig(t)
	wl.value = float64(1234)
	err := eng.EditProcessCounter(pid, domain.ProcessCounterEdit{PLCName: "plc", TagName: "MES_Cell.Prod_Counter_02", Enabled: true})
	if err != nil {
		t.Fatalf("an unlisted member reading a whole number was refused: %v", err)
	}
	if p := mustProcess(t, db, pid); p.CounterTagName != "MES_Cell.Prod_Counter_02" {
		t.Errorf("tag = %q, want the member", p.CounterTagName)
	}
	if got := strings.Join(wl.toggles(), ","); !strings.Contains(got, "on plc.MES_Cell.Prod_Counter_02") {
		t.Errorf("toggles = %s, want the member enabled before its read", got)
	}
}

// TestSave_NullTypedMemberIsCheckedByItsRead: WarLink lists a member that is
// configured but disabled with "type": null. Its listing declares nothing, so
// it is treated as unlisted: enabled and read, and a whole number is accepted.
func TestSave_NullTypedMemberIsCheckedByItsRead(t *testing.T) {
	t.Parallel()
	eng, db, pid, _, wl := counterRig(t)
	var listed plc.WarlinkTagInfo
	testutil.MustNoErr(t, json.Unmarshal([]byte(`{"name":"Sta01.Counter.ACC","type":null,"configured":true,"enabled":false}`), &listed), "decode the listing")
	wl.tags = append(wl.tags, listed)
	wl.value = float64(1599)
	if err := eng.EditProcessCounter(pid, domain.ProcessCounterEdit{PLCName: "plc", TagName: "Sta01.Counter.ACC", Enabled: true}); err != nil {
		t.Fatalf("a null-typed member reading a whole number was refused: %v", err)
	}
	if p := mustProcess(t, db, pid); p.CounterTagName != "Sta01.Counter.ACC" {
		t.Errorf("tag = %q, want the member", p.CounterTagName)
	}
}

// TestSave_RefusedReadLeavesNoTagPublished: the check enables an unlisted tag
// to read it; when the read refuses the tag, the check takes the publishing
// back, so a refused save leaves WarLink as it found it.
func TestSave_RefusedReadLeavesNoTagPublished(t *testing.T) {
	t.Parallel()
	eng, _, pid, _, wl := counterRig(t)
	wl.value = 80.28
	if err := eng.EditProcessCounter(pid, domain.ProcessCounterEdit{PLCName: "plc", TagName: "MES_Cell.Prod_Counter_02", Enabled: true}); err == nil {
		t.Fatal("an unlisted tag reading 80.28 was saved")
	}
	if got := strings.Join(wl.toggles(), ","); got != "on plc.MES_Cell.Prod_Counter_02,off plc.MES_Cell.Prod_Counter_02" {
		t.Errorf("toggles = %s, want the tag enabled for its read and disabled after the refusal", got)
	}
}

// TestPrime_LeavesTheIdentityManagedBitAsItFoundIt: at boot the manager's
// cache cannot say whether WarLink already published the identity tag, so
// prime enables it to read it but records nothing; only a save, which asks
// IsTagPublished first, marks the tag shingo's.
func TestPrime_LeavesTheIdentityManagedBitAsItFoundIt(t *testing.T) {
	t.Parallel()
	_, db, pid, _, wl := primedRig(t)
	if got := strings.Join(wl.toggles(), ","); got != "on plc.MES_Cell.CATID_01" {
		t.Fatalf("fixture: toggles = %s, want prime's enable", got)
	}
	if p := mustProcess(t, db, pid); p.IdentityWarlinkManaged {
		t.Error("prime marked the identity tag shingo's")
	}
}

// hangingWarlink is a recordingWarlink whose tag list answers only once
// released, signalling when a caller is inside it.
type hangingWarlink struct {
	recordingWarlink
	inside  chan struct{}
	release chan struct{}
}

func (w *hangingWarlink) ListAllTags(ctx context.Context, plcName string) ([]plc.WarlinkTagInfo, error) {
	close(w.inside)
	<-w.release
	return w.recordingWarlink.ListAllTags(ctx, plcName)
}

// TestCounterEdit_AHungWarLinkNeverHoldsTheDoor: the save's type check is a
// WarLink round trip of up to 8 s. It runs before the door's lock, so the door
// (the monitor's fire, the operator's CUTOVER) is not held while WarLink hangs.
func TestCounterEdit_AHungWarLinkNeverHoldsTheDoor(t *testing.T) {
	t.Parallel()
	eng, db, pid, _, rec := counterRig(t)
	wl := &hangingWarlink{inside: make(chan struct{}), release: make(chan struct{})}
	wl.tags = rec.tags
	eng.plcMgr = plc.NewManager(nil, config.WarLinkConfig{}, 0, 0, nil, wl)
	saved := make(chan error, 1)
	go func() {
		saved <- eng.EditProcessCounter(pid, domain.ProcessCounterEdit{PLCName: "plc", TagName: "MES_Other.Prod_Counter_01", Enabled: true})
	}()
	<-wl.inside
	door := make(chan error, 1)
	go func() {
		_, err := eng.completeCutover(pid, 1, "test", CutoverGated)
		door <- err
	}()
	select {
	case err := <-door:
		if !errors.Is(err, errNotTheOpenChangeover) {
			t.Errorf("door = %v, want the no-op", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("the door still waited after 2s: held by the counter save's WarLink call")
	}
	close(wl.release)
	testutil.MustNoErr(t, <-saved, "the save, once WarLink answered")
	if p := mustProcess(t, db, pid); p.CounterTagName != "MES_Other.Prod_Counter_01" {
		t.Errorf("tag = %q, want the save written once WarLink answered", p.CounterTagName)
	}
}

// TestSave_UnlistedRealValueRefused: an unlisted tag whose read is not a whole
// number is a REAL in all but name.
func TestSave_UnlistedRealValueRefused(t *testing.T) {
	t.Parallel()
	eng, db, pid, _, wl := counterRig(t)
	wl.value = 80.28
	err := eng.EditProcessCounter(pid, domain.ProcessCounterEdit{PLCName: "plc", TagName: "MES_Cell.Prod_Counter_02", Enabled: true})
	if err == nil || !strings.Contains(err.Error(), "80.28") {
		t.Fatalf("an unlisted tag reading 80.28 = %v, want a refusal naming the value", err)
	}
	if p := mustProcess(t, db, pid); p.CounterTagName != "MES_Cell.Prod_Counter_01" {
		t.Errorf("a refused edit wrote the tag: %q", p.CounterTagName)
	}
}
