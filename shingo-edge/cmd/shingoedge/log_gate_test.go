package main

import "testing"

// log_gate_test.go — what the Edge mirrors to stderr, which under systemd is
// the journal (memory close-out addendum A, 2026-09-24).

// edgeSubsystems is every debuglog subsystem the Edge logs under.
var edgeSubsystems = []string{
	"edge_handler", "engine", "heartbeat", "inventory_delta", "kafka", "orders",
	"outbox", "plant_claims", "plc", "production_ticks", "protocol", "release", "reporter",
}

// EVERY SUBSYSTEM REACHES THE JOURNAL. PIN: the Edge never restricts the
// stderr mirror, so at Hopkinsville 13,092 lines an hour reached journald,
// 82% of them outbox, inventory_delta, kafka and reporter.
func TestPin_A_EveryEdgeSubsystemReachesStderr(t *testing.T) {
	dbg := mustInitDebugLog(nil)
	defer dbg.Close()
	for _, s := range edgeSubsystems {
		if !dbg.StderrMirrors(s) {
			t.Errorf("%s does not reach stderr; at the base every subsystem does", s)
		}
	}
}
