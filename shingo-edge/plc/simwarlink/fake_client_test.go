//go:build sim

package simwarlink

import (
	"context"
	"testing"
	"time"

	"shingo/shared/clock"
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
