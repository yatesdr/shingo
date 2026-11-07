//go:build sim

package simwarlink

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"shingo/protocol/clock"
	"shingoedge/config"
	"shingoedge/plc"
)

var fakeStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func twoProcessCfg() config.SimConfig {
	return config.SimConfig{
		Enabled: true,
		Processes: []config.SimProcessConfig{
			{PLCName: "PRESS-1", TagName: "PRESS-1_COUNTER", TickInterval: time.Second, UOPPerTick: 2},
			{PLCName: "LINE1-IN", TagName: "LINE1-IN_COUNTER", TickInterval: time.Second, UOPPerTick: 1},
		},
	}
}

// waitForCounter polls ReadTagValue until it reaches want (the ticker goroutine
// applies the increment asynchronously after the manual clock fires).
func waitForCounter(t *testing.T, f *FakeClient, plcName, tagName string, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		v, err := f.ReadTagValue(context.Background(), plcName, tagName)
		if err == nil {
			if n, ok := v.(int64); ok && n == want {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("counter %s/%s never reached %d", plcName, tagName, want)
}

func TestFakeListsEveryPLCConnected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := NewFakeClient(ctx, twoProcessCfg(), clock.NewManual(fakeStart))

	plcs, err := f.ListPLCs(ctx)
	if err != nil {
		t.Fatalf("ListPLCs: %v", err)
	}
	if len(plcs) != 2 {
		t.Fatalf("want 2 PLCs, got %d", len(plcs))
	}
	for _, p := range plcs {
		if p.Status != "Connected" {
			t.Fatalf("PLC %s status %q, want Connected", p.Name, p.Status)
		}
	}
}

// ListTags must key by "plcName.tagName" (the real WarLink shape Manager.applyTags
// strips) and carry an int64 value (Manager.toInt64 accepts it).
func TestFakeListTagsShape(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := NewFakeClient(ctx, twoProcessCfg(), clock.NewManual(fakeStart))

	tags, err := f.ListTags(ctx, "PRESS-1")
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	tag, ok := tags["PRESS-1.PRESS-1_COUNTER"]
	if !ok {
		t.Fatalf("want key PRESS-1.PRESS-1_COUNTER, got keys %v", keysOf(tags))
	}
	if tag.PLC != "PRESS-1" || tag.Name != "PRESS-1_COUNTER" {
		t.Fatalf("tag identity wrong: %+v", tag)
	}
	if _, ok := tag.Value.(int64); !ok {
		t.Fatalf("tag value should be int64, got %T", tag.Value)
	}
	if tag.Error != "" {
		t.Fatalf("tag error should be empty (else read as disconnect): %q", tag.Error)
	}
}

// Counters climb by uop_per_tick each tick_interval. Advance one interval at a
// time and confirm each increment lands before the next (the cap-1 ticker
// channel coalesces if we outrun the goroutine).
func TestFakeCountersClimbOnClock(t *testing.T) {
	m := clock.NewManual(fakeStart)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := NewFakeClient(ctx, twoProcessCfg(), m)

	waitForCounter(t, f, "PRESS-1", "PRESS-1_COUNTER", 0)
	for i := int64(1); i <= 3; i++ {
		m.Advance(time.Second)
		waitForCounter(t, f, "PRESS-1", "PRESS-1_COUNTER", i*2) // UOPPerTick=2
	}
}

func TestFakeReadWriteAndStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := NewFakeClient(ctx, twoProcessCfg(), clock.NewManual(fakeStart))

	if _, err := f.ReadTagValue(ctx, "NOPE", "x"); err == nil {
		t.Fatal("ReadTagValue should error for unknown PLC")
	}
	if _, err := f.ReadTagValue(ctx, "PRESS-1", "nope"); err == nil {
		t.Fatal("ReadTagValue should error for unknown tag")
	}
	if err := f.WriteTagValue(ctx, "PRESS-1", "PRESS-1_COUNTER", 999); err != nil {
		t.Fatalf("WriteTagValue should be discarded with nil, got %v", err)
	}
	// The write must be discarded — value unchanged.
	if v, _ := f.ReadTagValue(ctx, "PRESS-1", "PRESS-1_COUNTER"); v.(int64) != 0 {
		t.Fatalf("WriteTagValue must not change the counter, got %v", v)
	}
	rc, err := f.OpenEventStream(ctx)
	if err != nil || rc == nil {
		t.Fatalf("OpenEventStream should return a live reader, got rc=%v err=%v", rc, err)
	}
	_ = rc.Close()
}

func keysOf(m map[string]plc.WarlinkTag) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// A curtain is a writable BOOL tag: the sim's stand-in for the cell's bypass
// button (S7). A bool write creates or flips it, its PLC is listed, the poll
// sees it as a BOOL, and a direct read returns it; a counter stays unwritable.
func TestFakeCurtainIsAWritableBool(t *testing.T) {
	ctx := context.Background()
	f := NewFakeClient(ctx, twoProcessCfg(), clock.NewManual(time.Unix(0, 0)))
	if err := f.WriteTagValue(ctx, "CURTAIN-PLC", "FG_CURTAIN", true); err != nil {
		t.Fatalf("write curtain: %v", err)
	}
	if v, err := f.ReadTagValue(ctx, "CURTAIN-PLC", "FG_CURTAIN"); err != nil || v != true {
		t.Fatalf("read curtain = %v, %v; want true", v, err)
	}
	_ = f.WriteTagValue(ctx, "CURTAIN-PLC", "FG_CURTAIN", false)
	if v, _ := f.ReadTagValue(ctx, "CURTAIN-PLC", "FG_CURTAIN"); v != false {
		t.Fatalf("the bypass flip did not take: %v", v)
	}
	plcs, _ := f.ListPLCs(ctx)
	found := false
	for _, p := range plcs {
		found = found || p.Name == "CURTAIN-PLC"
	}
	if !found {
		t.Error("the curtain's PLC is not listed, so the poll never caches it")
	}
	tags, _ := f.ListTags(ctx, "CURTAIN-PLC")
	if tg := tags["CURTAIN-PLC.FG_CURTAIN"]; tg.Type != "BOOL" || tg.Value != false {
		t.Errorf("polled curtain = %+v, want a BOOL false", tg)
	}
	_ = f.WriteTagValue(ctx, "PRESS-1", "PRESS-1_COUNTER", true)
	if v, _ := f.ReadTagValue(ctx, "PRESS-1", "PRESS-1_COUNTER"); v.(int64) != 0 {
		t.Errorf("a bool write changed a counter: %v", v)
	}
}

// TestFakeIdentityIsAWritableDINT: a part-identity tag (a process's CATID)
// is set with an Identity and polled as a DINT beside the counter, so the
// CATID monitor reads it as it reads a plant's. A plain integer write stays
// discarded, and a counter's name is refused.
func TestFakeIdentityIsAWritableDINT(t *testing.T) {
	ctx := context.Background()
	f := NewFakeClient(ctx, twoProcessCfg(), clock.NewManual(time.Unix(0, 0)))
	if err := f.WriteTagValue(ctx, "PRESS-1", "MES_P1.CATID_01", Identity(11111111)); err != nil {
		t.Fatalf("set identity: %v", err)
	}
	tags, _ := f.ListTags(ctx, "PRESS-1")
	if tg := tags["PRESS-1.MES_P1.CATID_01"]; tg.Type != "DINT" || tg.Value != int64(11111111) {
		t.Errorf("polled identity = %+v, want a DINT 11111111", tg)
	}
	_ = f.WriteTagValue(ctx, "PRESS-1", "MES_P1.CATID_01", Identity(22222222))
	if v, _ := f.ReadTagValue(ctx, "PRESS-1", "MES_P1.CATID_01"); v != int64(22222222) {
		t.Errorf("the tool change did not take: %v", v)
	}
	_ = f.WriteTagValue(ctx, "PRESS-1", "ZONE_LIGHT", int64(3))
	if _, err := f.ReadTagValue(ctx, "PRESS-1", "ZONE_LIGHT"); err == nil {
		t.Error("a plain integer write was kept; it is discarded")
	}
	if err := f.WriteTagValue(ctx, "PRESS-1", "PRESS-1_COUNTER", Identity(5)); err == nil {
		t.Error("an identity write to a counter was accepted")
	}
}

// TestFakeOverrunIsAPerPLCSwitch: the overrun switch is set and cleared per PLC
// with an Overrun on OverrunTag, read back through ReadTagValue, never published
// to the poll, and refused on a PLC with no counter, on another tag name, and
// as a plain bool.
func TestFakeOverrunIsAPerPLCSwitch(t *testing.T) {
	ctx := context.Background()
	f := NewFakeClient(ctx, twoProcessCfg(), clock.NewManual(time.Unix(0, 0)))
	if f.Overrun("PRESS-1") {
		t.Fatal("a fresh PLC starts overrun")
	}
	if err := f.WriteTagValue(ctx, "PRESS-1", OverrunTag, Overrun(true)); err != nil {
		t.Fatalf("set overrun: %v", err)
	}
	if !f.Overrun("PRESS-1") || f.Overrun("LINE1-IN") {
		t.Fatalf("overrun PRESS-1=%v LINE1-IN=%v; want true, false", f.Overrun("PRESS-1"), f.Overrun("LINE1-IN"))
	}
	if v, err := f.ReadTagValue(ctx, "PRESS-1", OverrunTag); err != nil || v != true {
		t.Errorf("read overrun = %v, %v; want true", v, err)
	}
	tags, _ := f.ListTags(ctx, "PRESS-1")
	if _, ok := tags["PRESS-1."+OverrunTag]; ok || len(tags) != 1 {
		t.Errorf("the poll sees %v; want the counter alone", tags)
	}
	_ = f.WriteTagValue(ctx, "PRESS-1", OverrunTag, Overrun(false))
	if f.Overrun("PRESS-1") {
		t.Error("the switch did not clear")
	}
	if err := f.WriteTagValue(ctx, "NO-SUCH-PLC", OverrunTag, Overrun(true)); err == nil {
		t.Error("overrun on a PLC with no counter was accepted")
	}
	if err := f.WriteTagValue(ctx, "PRESS-1", "FG_CURTAIN", Overrun(true)); err == nil {
		t.Error("an Overrun on another tag name was accepted")
	}
	if err := f.WriteTagValue(ctx, "PRESS-1", OverrunTag, true); err == nil || f.Overrun("PRESS-1") {
		t.Error("a plain bool set the overrun switch")
	}
}

// lineCfg is one process with a line of the given depth: a station tag that
// clicks per load and an identity tag a program change writes.
func lineCfg(depth int) config.SimConfig {
	return config.SimConfig{Enabled: true, Processes: []config.SimProcessConfig{{
		PLCName: "WELD-6", TagName: "WELD-6_COUNTER", TickInterval: time.Second, UOPPerTick: 1,
		StationTag: "WELD-6_STATION", LineDepth: depth, IdentityTag: "WELD-6_CATID",
	}}}
}

// readInt reads a published integer tag; -1 when it is not there.
func readInt(f *FakeClient, plcName, tagName string) int64 {
	v, err := f.ReadTagValue(context.Background(), plcName, tagName)
	if n, ok := v.(int64); ok && err == nil {
		return n
	}
	return -1
}

// lineOf reads the first line of a PLC through LineTag.
func lineOf(t *testing.T, f *FakeClient, plcName string) ProcessLine {
	t.Helper()
	v, err := f.ReadTagValue(context.Background(), plcName, LineTag)
	lines, ok := v.([]ProcessLine)
	if err != nil || !ok || len(lines) == 0 {
		t.Fatalf("read %s line = %v, %v", plcName, v, err)
	}
	return lines[0]
}

// TestFakeLineCountsALoadLClicksAfterItsStation: the counter's n-th tick comes
// exactly L ticks after the station's n-th click.
func TestFakeLineCountsALoadLClicksAfterItsStation(t *testing.T) {
	const depth = 3
	m := clock.NewManual(fakeStart)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := NewFakeClient(ctx, lineCfg(depth), m)
	for k := int64(1); k <= 6; k++ {
		m.Advance(time.Second)
		waitForCounter(t, f, "WELD-6", "WELD-6_STATION", k)
		if got, want := readInt(f, "WELD-6", "WELD-6_COUNTER"), max(0, k-depth); got != want {
			t.Fatalf("after click %d the counter reads %d; want %d", k, got, want)
		}
	}
}

// TestFakeProgramChangeDrainsThenResets: after the verb nothing loads, the
// counter keeps rising until the line is empty, and on the next tick (a stalled
// one here) the identity changes and the counter reads 0; loading then resumes
// under the new identity.
func TestFakeProgramChangeDrainsThenResets(t *testing.T) {
	m := clock.NewManual(fakeStart)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := NewFakeClient(ctx, lineCfg(2), m)
	var stopped atomic.Bool
	f.SetReadinessFunc(func(string) bool { return !stopped.Load() })
	_ = f.WriteTagValue(ctx, "WELD-6", "WELD-6_CATID", Identity(1001))
	for k := int64(1); k <= 4; k++ {
		m.Advance(time.Second)
		waitForCounter(t, f, "WELD-6", "WELD-6_STATION", k)
	}
	if err := f.WriteTagValue(ctx, "WELD-6", "WELD-6_COUNTER", ProgramChange(1002)); err != nil {
		t.Fatalf("program change: %v", err)
	}
	for c := int64(3); c <= 4; c++ {
		m.Advance(time.Second)
		waitForCounter(t, f, "WELD-6", "WELD-6_COUNTER", c)
		if s, id := readInt(f, "WELD-6", "WELD-6_STATION"), readInt(f, "WELD-6", "WELD-6_CATID"); s != 4 || id != 1001 {
			t.Fatalf("draining: station %d, identity %d; want 4 (no loading) and 1001", s, id)
		}
	}
	stopped.Store(true)
	m.Advance(time.Second)
	waitForCounter(t, f, "WELD-6", "WELD-6_CATID", 1002)
	if c := readInt(f, "WELD-6", "WELD-6_COUNTER"); c != 0 {
		t.Fatalf("the identity changed with the counter at %d; want 0", c)
	}
	stopped.Store(false)
	m.Advance(time.Second)
	waitForCounter(t, f, "WELD-6", "WELD-6_STATION", 5)
	if st := lineOf(t, f, "WELD-6"); st.ByIdentity[1001] != 4 || st.ByIdentity[1002] != 1 || st.InFlight != 1 || st.Counter != 0 {
		t.Errorf("line after the change = %+v; want loads 1001:4 1002:1, one in flight, counter 0", st.LineState)
	}
}

// TestFakeForcedChangeLeavesTheLineInFlight: the identity and the reset land at
// the write, and the L loads still in the line count out on the next L ticks
// (booked to the identity they were loaded under).
func TestFakeForcedChangeLeavesTheLineInFlight(t *testing.T) {
	m := clock.NewManual(fakeStart)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := NewFakeClient(ctx, lineCfg(2), m)
	_ = f.WriteTagValue(ctx, "WELD-6", "WELD-6_CATID", Identity(1001))
	for k := int64(1); k <= 4; k++ {
		m.Advance(time.Second)
		waitForCounter(t, f, "WELD-6", "WELD-6_STATION", k)
	}
	if err := f.WriteTagValue(ctx, "WELD-6", "WELD-6_COUNTER", ForcedChange(1002)); err != nil {
		t.Fatalf("forced change: %v", err)
	}
	if id, c := readInt(f, "WELD-6", "WELD-6_CATID"), readInt(f, "WELD-6", "WELD-6_COUNTER"); id != 1002 || c != 0 {
		t.Fatalf("at the write: identity %d, counter %d; want 1002 and 0", id, c)
	}
	for k := int64(5); k <= 6; k++ {
		m.Advance(time.Second)
		waitForCounter(t, f, "WELD-6", "WELD-6_STATION", k)
		if c := readInt(f, "WELD-6", "WELD-6_COUNTER"); c != k-4 {
			t.Fatalf("tick %d after the forced change: counter %d; want %d", k-4, c, k-4)
		}
	}
	if st := lineOf(t, f, "WELD-6"); st.ByIdentity[1001] != 4 || st.ByIdentity[1002] != 2 || st.InFlight != 2 {
		t.Errorf("line = %+v; want loads 1001:4 1002:2, two in flight", st.LineState)
	}
}

// TestFakeLineDepthZeroIsTodaysCounter: no station tag and depth 0 is the
// counter of before lines: it ticks on every load, the poll sees it alone, and
// the line read books one load per tick with nothing in flight.
func TestFakeLineDepthZeroIsTodaysCounter(t *testing.T) {
	m := clock.NewManual(fakeStart)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := NewFakeClient(ctx, twoProcessCfg(), m)
	for i := int64(1); i <= 3; i++ {
		m.Advance(time.Second)
		waitForCounter(t, f, "PRESS-1", "PRESS-1_COUNTER", i*2)
	}
	if tags, _ := f.ListTags(ctx, "PRESS-1"); len(tags) != 1 {
		t.Errorf("the poll sees %v; want the counter alone", keysOf(tags))
	}
	if st := lineOf(t, f, "PRESS-1"); st.Loads != 3 || st.ByIdentity[0] != 3 || st.InFlight != 0 {
		t.Errorf("line = %+v; want 3 loads, none in flight", st.LineState)
	}
}

// TestFakeIntegerWriteIsNoProgramChange: a plain integer on the counter or the
// identity tag changes no program; the line keeps loading and counting.
func TestFakeIntegerWriteIsNoProgramChange(t *testing.T) {
	m := clock.NewManual(fakeStart)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := NewFakeClient(ctx, lineCfg(1), m)
	_ = f.WriteTagValue(ctx, "WELD-6", "WELD-6_CATID", Identity(1001))
	m.Advance(time.Second)
	waitForCounter(t, f, "WELD-6", "WELD-6_STATION", 1)
	for _, v := range []any{int64(1002), 1002, int32(1002)} {
		_ = f.WriteTagValue(ctx, "WELD-6", "WELD-6_COUNTER", v)
		_ = f.WriteTagValue(ctx, "WELD-6", "WELD-6_CATID", v)
	}
	m.Advance(time.Second)
	waitForCounter(t, f, "WELD-6", "WELD-6_STATION", 2)
	if c, id := readInt(f, "WELD-6", "WELD-6_COUNTER"), readInt(f, "WELD-6", "WELD-6_CATID"); c != 1 || id != 1001 {
		t.Errorf("after integer writes: counter %d, identity %d; want 1 and 1001", c, id)
	}
}

// TestFakePauseLoading_TheLineDrainsUntilResumed: a PauseLoading written on a
// process's counter tag stops its loads (the station stands) while the
// counter ticks the line out, and the line read says paused; false loads
// again. A plain bool on the counter pauses nothing, and a tag no process
// counts on is refused.
func TestFakePauseLoading_TheLineDrainsUntilResumed(t *testing.T) {
	m := clock.NewManual(fakeStart)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := NewFakeClient(ctx, lineCfg(2), m)
	_ = f.WriteTagValue(ctx, "WELD-6", "WELD-6_CATID", Identity(1001))
	_ = f.WriteTagValue(ctx, "WELD-6", "WELD-6_COUNTER", true)
	for k := int64(1); k <= 3; k++ {
		m.Advance(time.Second)
		waitForCounter(t, f, "WELD-6", "WELD-6_STATION", k)
	}
	if err := f.WriteTagValue(ctx, "WELD-6", "WELD-6_COUNTER", PauseLoading(true)); err != nil {
		t.Fatalf("pause: %v", err)
	}
	for c := int64(2); c <= 3; c++ {
		m.Advance(time.Second)
		waitForCounter(t, f, "WELD-6", "WELD-6_COUNTER", c)
		if s := readInt(f, "WELD-6", "WELD-6_STATION"); s != 3 {
			t.Fatalf("paused: the station clicked to %d; want it standing at 3", s)
		}
	}
	if st := lineOf(t, f, "WELD-6"); !st.Paused || st.InFlight != 0 || st.Loads != 3 || st.Identity != 1001 {
		t.Fatalf("the line read, drained = %+v; want paused, empty, 3 loads, identity 1001", st.LineState)
	}
	if err := f.WriteTagValue(ctx, "WELD-6", "WELD-6_COUNTER", PauseLoading(false)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	m.Advance(time.Second)
	waitForCounter(t, f, "WELD-6", "WELD-6_STATION", 4)
	if st := lineOf(t, f, "WELD-6"); st.Paused || st.Counter != 3 || st.InFlight != 1 {
		t.Errorf("resumed = %+v; want loading, counter 3, one in flight", st.LineState)
	}
	if err := f.WriteTagValue(ctx, "WELD-6", "NO_SUCH_COUNTER", PauseLoading(true)); err == nil {
		t.Error("a pause on a tag no process counts on was taken; want refused")
	}
}

// mesCfg is a line counting on slot 1 of an MES struct with that slot's CATID
// as its identity, and a plain counter beside it.
func mesCfg() config.SimConfig {
	return config.SimConfig{Enabled: true, Processes: []config.SimProcessConfig{
		{PLCName: "WELD-6", TagName: "MES_WELD_6.Prod_Counter_01", TickInterval: time.Second, UOPPerTick: 1,
			StationTag: "STA_W6_LOAD.Load_Count", IdentityTag: "MES_WELD_6.CATID_01"},
		{PLCName: "PRESS-1", TagName: "PRESS-1_COUNTER", TickInterval: time.Second, UOPPerTick: 1},
	}}
}

// TestFakeCountingSlot_TheSiblingTicksReadByTheStructUntilEnabled: the poll
// sees the struct's configured members only; after the count moves to slot 2,
// Prod_Counter_01 stands, Prod_Counter_02 ticks and CATID_02 reads the part,
// both read through a read of the struct (or of the member) and published once
// the Edge enables them. A process with no MES struct, a slot past the
// struct's five and an integer write move nothing.
func TestFakeCountingSlot_TheSiblingTicksReadByTheStructUntilEnabled(t *testing.T) {
	m := clock.NewManual(fakeStart)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := NewFakeClient(ctx, mesCfg(), m)
	const c1, c2, id1, id2 = "MES_WELD_6.Prod_Counter_01", "MES_WELD_6.Prod_Counter_02", "MES_WELD_6.CATID_01", "MES_WELD_6.CATID_02"
	_ = f.WriteTagValue(ctx, "WELD-6", id1, Identity(1001))
	for k := int64(1); k <= 3; k++ {
		m.Advance(time.Second)
		waitForCounter(t, f, "WELD-6", c1, k)
	}
	published := func() map[string]plc.WarlinkTag {
		tags, _ := f.ListTags(ctx, "WELD-6")
		return tags
	}
	if tags := published(); len(tags) != 3 {
		t.Fatalf("the poll sees %v; want the counter, the CATID and the station only", keysOf(tags))
	}
	for _, v := range []any{int64(2), 2} {
		_ = f.WriteTagValue(ctx, "WELD-6", c1, v)
	}
	if err := f.WriteTagValue(ctx, "WELD-6", c1, CountingSlot(2)); err != nil {
		t.Fatalf("count on slot 2: %v", err)
	}
	for k := int64(4); k <= 5; k++ {
		m.Advance(time.Second)
		waitForCounter(t, f, "WELD-6", "STA_W6_LOAD.Load_Count", k)
	}
	if n := readInt(f, "WELD-6", c1); n != 3 {
		t.Errorf("Prod_Counter_01 after the move = %d; want it stopped at 3", n)
	}
	raw, err := f.ReadTagValue(ctx, "WELD-6", "MES_WELD_6")
	members, ok := raw.(map[string]any)
	if err != nil || !ok || members["Prod_Counter_02"] != int64(2) || members["CATID_02"] != int64(1001) ||
		members["Prod_Counter_01"] != int64(3) || members["CATID_01"] != int64(1001) || members["CATID_03"] != int64(0) {
		t.Fatalf("the struct read = %v, %v; want slot 2 {1001 2} counting, slot 1 left {1001 3}, slot 3 unused", raw, err)
	}
	if n := readInt(f, "WELD-6", c2); n != 2 {
		t.Errorf("a direct read of Prod_Counter_02 = %d; want 2", n)
	}
	if tags := published(); len(tags) != 3 {
		t.Fatalf("before the Edge enables them the poll sees %v; want slot 2's members unpublished", keysOf(tags))
	}
	for _, tag := range []string{c2, id2} {
		if err := f.SetTagPublishing(ctx, "WELD-6", tag, true); err != nil {
			t.Fatalf("enable %s: %v", tag, err)
		}
	}
	if tags := published(); tags["WELD-6."+c2].Value != int64(2) || tags["WELD-6."+id2].Value != int64(1001) {
		t.Fatalf("enabled, the poll sees %v; want Prod_Counter_02 at 2 and CATID_02 at 1001", tags)
	}
	if st := lineOf(t, f, "WELD-6"); st.Slot != 2 || st.Counter != 2 || st.Slots[0] != (Slot{1001, 3}) {
		t.Errorf("the line read = %+v; want counting on slot 2 at 2, slot 1 {1001 3}", st.LineState)
	}
	if err := f.WriteTagValue(ctx, "PRESS-1", "PRESS-1_COUNTER", CountingSlot(2)); err == nil {
		t.Error("a counter with no MES struct moved its count; want refused")
	}
	if err := f.WriteTagValue(ctx, "WELD-6", c1, CountingSlot(6)); err == nil {
		t.Error("a move to slot 6 of a five-slot struct was taken; want refused")
	}
}
