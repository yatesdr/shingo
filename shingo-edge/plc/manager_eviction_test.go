package plc

import (
	"testing"

	"shingoedge/config"
)

// A PLC removed upstream must disappear from the manager's view, not just be
// restatused. Until this was fixed the entry survived for the life of the
// process, so a removed PLC kept showing in PLCNames() (the Processes-page
// dropdown) and PLCStatuses() (GET /api/plcs) until the edge restarted — which
// is why removing a PLC appeared to require a restart to take effect.
func TestWarlinkPollEvictsPLCsRemovedUpstream(t *testing.T) {
	t.Parallel()
	cfg := config.Defaults()
	emitter := &mockEmitter{}
	mgr := NewManager(nil, cfg, emitter, nil)
	client := &mockWarlinkClient{
		plcs: []WarlinkPLC{
			{Name: "keep_PLC", Status: "Connected"},
			{Name: "remove_PLC", Status: "Connected"},
		},
	}
	mgr.wl = client

	mgr.warlinkPollTick()
	if got := mgr.PLCNames(); len(got) != 2 {
		t.Fatalf("after discovery PLCNames() = %v, want both PLCs", got)
	}

	// Upstream drops one.
	client.plcs = []WarlinkPLC{{Name: "keep_PLC", Status: "Connected"}}
	mgr.warlinkPollTick()

	if got := mgr.PLCNames(); len(got) != 1 || got[0] != "keep_PLC" {
		t.Errorf("PLCNames() = %v, want only [keep_PLC] — a removed PLC must not linger in the dropdown", got)
	}
	if statuses := mgr.PLCStatuses(); len(statuses) != 1 {
		t.Errorf("PLCStatuses() = %v, want only keep_PLC", statuses)
	}
	if mgr.GetPLC("remove_PLC") != nil {
		t.Error("GetPLC still returns the evicted PLC")
	}
	if mgr.IsConnected("remove_PLC") {
		t.Error("evicted PLC still reports connected")
	}
	if !mgr.IsConnected("keep_PLC") {
		t.Error("the surviving PLC was disturbed by the eviction")
	}
}

// A PLC that has already gone Disconnected and is THEN removed upstream must
// still be evicted. Gating eviction on "was connected" — the shape of the
// original disconnect check — would leave exactly this one behind for good.
func TestWarlinkPollEvictsAlreadyDisconnectedPLC(t *testing.T) {
	t.Parallel()
	cfg := config.Defaults()
	mgr := NewManager(nil, cfg, &mockEmitter{}, nil)
	client := &mockWarlinkClient{
		plcs: []WarlinkPLC{{Name: "flaky_PLC", Status: "Disconnected"}},
	}
	mgr.wl = client

	mgr.warlinkPollTick()
	if mgr.GetPLC("flaky_PLC") == nil {
		t.Fatal("PLC was not discovered")
	}
	if mgr.IsConnected("flaky_PLC") {
		t.Fatal("a PLC reported Disconnected should not read as connected")
	}

	client.plcs = nil
	mgr.warlinkPollTick()

	if mgr.GetPLC("flaky_PLC") != nil {
		t.Error("an already-disconnected PLC was not evicted when it left the WarLink list")
	}
	if got := mgr.PLCNames(); len(got) != 0 {
		t.Errorf("PLCNames() = %v, want empty", got)
	}
}

// Eviction must fire the disconnect event exactly once, and only for a PLC that
// was actually connected — a re-poll with the PLC still absent has no further
// transition to report.
func TestWarlinkPollEvictionEmitsDisconnectOnce(t *testing.T) {
	t.Parallel()
	cfg := config.Defaults()
	emitter := &mockEmitter{}
	mgr := NewManager(nil, cfg, emitter, nil)
	client := &mockWarlinkClient{
		plcs: []WarlinkPLC{{Name: "gone_PLC", Status: "Connected"}},
	}
	mgr.wl = client
	mgr.warlinkPollTick()

	before := countEvents(emitter, "plc_disconnected:gone_PLC")
	client.plcs = nil
	mgr.warlinkPollTick()
	afterFirst := countEvents(emitter, "plc_disconnected:gone_PLC")
	mgr.warlinkPollTick()
	afterSecond := countEvents(emitter, "plc_disconnected:gone_PLC")

	if afterFirst != before+1 {
		t.Errorf("disconnect events for gone_PLC: %d → %d, want exactly one more", before, afterFirst)
	}
	if afterSecond != afterFirst {
		t.Errorf("a second poll with the PLC still absent emitted another disconnect (%d → %d)", afterFirst, afterSecond)
	}
}

// countEvents counts exact matches in mockEmitter's recorded event log.
func countEvents(e *mockEmitter, want string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, ev := range e.events {
		if ev == want {
			n++
		}
	}
	return n
}
