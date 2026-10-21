package plc

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"shingoedge/config"
	"shingoedge/internal/testdb"
)

// TestRace_ApplyWarLinkLeavesTheConfigAlone pins the shape the 2026-10-20
// stop installed: the WarLink settings are a VALUE the save hands the manager,
// and the poll loop reads only that value.
//
// The writer does what apiSaveConfig → Config.Adopt → Engine.ApplyWarLinkConfig
// does — write the live config under its lock, then hand the manager a copy —
// while the poll loop ticks on the same leaves. Under -race this fires if the
// manager goes back to reading the live config: the loop holds no lock, so its
// read and the save's write would share no happens-before edge.
//
// Run: go test -race -run TestRace_ ./plc/
func TestRace_ApplyWarLinkLeavesTheConfigAlone(t *testing.T) {
	if !raceEnabled {
		t.Skip("race detector not enabled; this test is meaningful only under -race")
	}
	db := testdb.Open(t)
	cfg := config.Defaults()
	cfg.WarLink.Mode = "poll" // the loop that reads the settings on every tick
	cfg.WarLink.PollRate = time.Millisecond

	mgr := newTestManager(db, cfg, &mockEmitter{}, &mockWarlinkClient{})
	mgr.StartWarLinkPoller()
	defer mgr.StopWarLinkPoller()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			cfg.Lock()
			cfg.WarLink.Host = fmt.Sprintf("wl%d.test", i)
			cfg.WarLink.PollRate = time.Duration(1+i%5) * time.Millisecond
			cfg.WarLink.Enabled = i%2 == 0
			cfg.Unlock()
			mgr.ApplyWarLink(cfg.WarLinkSettings())
		}
	}()
	wg.Wait()
}

// TestApplyWarLinkTakesEffect: the handoff is what the poll loop reads, so a
// new host reaches baseURL and a new mode decides the next start's loop.
func TestApplyWarLinkTakesEffect(t *testing.T) {
	cfg := config.Defaults()
	mgr := newTestManager(nil, cfg, &mockEmitter{}, nil)

	mgr.ApplyWarLink(config.WarLinkConfig{Host: "warlink.test", Port: 9090, Mode: "sse"})
	if got, want := mgr.baseURL(), "http://warlink.test:9090/api"; got != want {
		t.Errorf("baseURL after apply = %q, want %q", got, want)
	}
	if got := mgr.warLinkSettings().Mode; got != "sse" {
		t.Errorf("mode after apply = %q, want sse", got)
	}
}
