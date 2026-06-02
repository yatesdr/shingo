package engine

import "testing"

// isPickupBlock is the BinTask classifier used by handleBlockCompleted.
// Pure function, no DB — table-driven test verifies every classification.
//
// The vendor's BinTask vocabulary is roboshop-configurable, so the
// classifier mixes exact-match (the common shapes) with substring
// fallback (containing "load" or "pick" but not "unload"/"drop"/
// "release"). Locking down both branches here means a future
// classification change has to change the test, not just slip through.
func TestIsPickupBlock(t *testing.T) {
	t.Parallel()
	cases := []struct {
		binTask string
		want    bool
		reason  string
	}{
		{"", false, "empty binTask is navigation/wait, not a pickup"},
		{"Load", true, "common JackLoad short form"},
		{"load", true, "lowercase variant"},
		{"pickup", true, "explicit pickup"},
		{"pick", true, "shorthand"},
		{"JackLoad", true, "vendor jacking-style"},
		{"jack_load", true, "snake-case variant"},
		{"fork_load", true, "forklift variant"},
		{"RollerLoad", true, "roller-conveyor variant"},
		{"unload", false, "unload is dropoff, not pickup"},
		{"JackUnload", false, "even with 'load' substring, unload wins"},
		{"drop", false, "drop is dropoff"},
		{"release", false, "release is end-of-cycle, not pickup"},
		{"Wait", false, "wait blocks aren't pickups"},
		{"Script", false, "script blocks aren't pickups"},
		{"Navigate", false, "pure navigation isn't a pickup"},
		// Substring fallback cases. These exist because vendor configs
		// can introduce new BinTask values; we want a sane default
		// rather than silent no-ops.
		{"customLoadSpecial", true, "substring match on 'load'"},
		{"specialPickStation", true, "substring match on 'pick'"},
		{"unloadCustomA", false, "unload substring beats load"},
	}

	for _, tc := range cases {
		got := isPickupBlock(tc.binTask)
		if got != tc.want {
			t.Errorf("isPickupBlock(%q) = %v, want %v — %s", tc.binTask, got, tc.want, tc.reason)
		}
	}
}

// isDropoffBlock is the store/deliver dual classifier used by
// handleBlockCompleted to route intermediate storage dropoffs through
// handleStoreBlockCompleted. Pickup and dropoff classifiers must be
// disjoint — a block routed to both paths would double-move a bin — so the
// cases below mirror TestIsPickupBlock and assert the boundary.
func TestIsDropoffBlock(t *testing.T) {
	t.Parallel()
	cases := []struct {
		binTask string
		want    bool
		reason  string
	}{
		{"", false, "empty binTask is navigation/wait, not a dropoff"},
		{"Unload", true, "common JackUnload short form"},
		{"unload", true, "lowercase variant"},
		{"dropoff", true, "explicit dropoff"},
		{"drop", true, "shorthand"},
		{"JackUnload", true, "vendor jacking-style"},
		{"jack_unload", true, "snake-case variant"},
		{"fork_unload", true, "forklift variant"},
		{"RollerUnload", true, "roller-conveyor variant"},
		{"release", true, "release is an end-of-cycle drop"},
		{"Load", false, "load is a pickup, not a dropoff"},
		{"JackLoad", false, "pickup-shaped, even though substring fallback never sees 'unload'"},
		{"pick", false, "pickup shorthand"},
		{"Wait", false, "wait blocks aren't dropoffs"},
		{"Script", false, "script blocks aren't dropoffs"},
		{"Navigate", false, "pure navigation isn't a dropoff"},
		{"customUnloadSpecial", true, "substring match on 'unload'"},
		{"specialDropStation", true, "substring match on 'drop'"},
		{"jackLoadCustomA", false, "load substring is not a dropoff"},
	}

	for _, tc := range cases {
		got := isDropoffBlock(tc.binTask)
		if got != tc.want {
			t.Errorf("isDropoffBlock(%q) = %v, want %v — %s", tc.binTask, got, tc.want, tc.reason)
		}
		// Disjointness: no BinTask may classify as both pickup and dropoff.
		if got && isPickupBlock(tc.binTask) {
			t.Errorf("binTask %q classified as BOTH pickup and dropoff — routing would double-move the bin", tc.binTask)
		}
	}
}
