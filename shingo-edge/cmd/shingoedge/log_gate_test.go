package main

import (
	"testing"

	"shingoedge/config"
)

// log_gate_test.go — what the Edge mirrors to stderr, which under systemd is
// the journal (memory close-out addendum A, 2026-09-24).

// edgeSubsystems is every debuglog subsystem the Edge logs under.
var edgeSubsystems = []string{
	"edge_handler", "engine", "heartbeat", "inventory_delta", "kafka", "orders",
	"outbox", "plant_claims", "plc", "production_ticks", "protocol", "release", "reporter",
}

// THE DEFAULT KEEPS THE PER-TICK CHATTER OUT OF THE JOURNAL. outbox,
// inventory_delta, kafka and reporter were 82% of Hopkinsville's 13,092
// journal lines an hour; every other subsystem still reaches it. Inverts the
// pin that every subsystem did.
func TestLogGate_DefaultMutesThePerTickSubsystems(t *testing.T) {
	dbg := mustInitDebugLog(nil)
	defer dbg.Close()
	applyLogGate(dbg, config.Defaults())
	muted := map[string]bool{"outbox": true, "inventory_delta": true, "kafka": true, "reporter": true}
	for _, s := range edgeSubsystems {
		if got := dbg.StderrMirrors(s); got == muted[s] {
			t.Errorf("%s reaches stderr = %t, want %t", s, got, !muted[s])
		}
	}
}
