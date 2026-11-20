package engine

import (
	"testing"

	"shingo/protocol"
)

// legacyBlockAction is the name rule the block subscriber read every finished
// block by before Core recorded what it sent (orders.block_actions). It now
// reads only an order sent before the record and a leg row written before legs
// carried their action, so it must read those exactly as the handler did: the
// exact names, then the substring fallbacks (a name holding "unload", "drop" or
// "release" is a dropoff; one holding "load" or "pick" and none of those is a
// pickup), Wait, and nothing else. Pure function, no DB.
//
// The cases are the two tables the rule's halves were pinned by, folded: each
// name has one action, so a block is never read as both.
func TestLegacyBlockAction(t *testing.T) {
	t.Parallel()
	const (
		pickup  = protocol.ActionPickup
		dropoff = protocol.ActionDropoff
		wait    = protocol.ActionWait
	)
	cases := []struct {
		binTask string
		want    string
		reason  string
	}{
		{"", "", "empty binTask is navigation, no step's kind"},
		{"Load", pickup, "common JackLoad short form"},
		{"load", pickup, "lowercase variant"},
		{"pickup", pickup, "explicit pickup"},
		{"pick", pickup, "shorthand"},
		{"JackLoad", pickup, "vendor jacking-style"},
		{"jack_load", pickup, "snake-case variant"},
		{"fork_load", pickup, "forklift variant"},
		{"RollerLoad", pickup, "roller-conveyor variant"},
		{"Unload", dropoff, "common JackUnload short form"},
		{"unload", dropoff, "lowercase variant"},
		{"dropoff", dropoff, "explicit dropoff"},
		{"drop", dropoff, "shorthand"},
		{"JackUnload", dropoff, "even with 'load' substring, unload wins"},
		{"jack_unload", dropoff, "snake-case variant"},
		{"fork_unload", dropoff, "forklift variant"},
		{"RollerUnload", dropoff, "roller-conveyor variant"},
		{"release", dropoff, "release is an end-of-cycle drop"},
		{"Wait", wait, "the wait a node's wait step is sent as"},
		{"wait", wait, "case is ignored"},
		{"Script", "", "script blocks are no step's kind"},
		{"Navigate", "", "pure navigation is no step's kind"},
		{"Go_AP1", "", "the seeded load sequence's turns are no step's kind"},
		{"Spin_inverse_90", "", "the seeded load sequence's turns are no step's kind"},
		{"customLoadSpecial", pickup, "substring match on 'load'"},
		{"specialPickStation", pickup, "substring match on 'pick'"},
		{"unloadCustomA", dropoff, "unload substring beats load"},
		{"customUnloadSpecial", dropoff, "substring match on 'unload'"},
		{"specialDropStation", dropoff, "substring match on 'drop'"},
		{"jackLoadCustomA", pickup, "load substring is a pickup, not a dropoff"},
		{"wait_4548", "", "a sized wait was never Wait to the name rule"},
	}
	for _, tc := range cases {
		if got := legacyBlockAction(tc.binTask); got != tc.want {
			t.Errorf("legacyBlockAction(%q) = %q, want %q — %s", tc.binTask, got, tc.want, tc.reason)
		}
	}
}
