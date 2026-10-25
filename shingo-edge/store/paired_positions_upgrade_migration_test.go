package store

import (
	"path/filepath"
	"testing"
)

// A PAIRED POSITION ON A MODE THAT NAMES NONE IS CLEARED ON UPGRADE.
//
// The claim save stores a paired and a second paired position only on a mode
// whose flowspec row names the paired position (domain
// NodeClaimInput.SavedPairs: two_robot_press_index and sequential). A row
// stored before that rule may carry one on any other mode, and Core's
// FedClaimSQL reads such a row as a press fed from behind while the Edge's
// chain (NodeClaim.DependentPositions) finds no dependent position. The upgrade
// applies the save rule once to the rows already stored and leaves every
// press-index and sequential row as it was; a later boot does not run it again.
func TestUpgradeClearsThePairsOfAModeThatNamesNone(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	res, err := db.Exec(`INSERT INTO styles (name) VALUES ('UPG-PAIRS')`)
	if err != nil {
		t.Fatalf("style: %v", err)
	}
	styleID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("style id: %v", err)
	}
	// The rows as a build before the rule could leave them: written raw, as
	// the save no longer would.
	rows := []struct {
		node, mode, paired, second string
		keep                       bool
	}{
		{"UPG-TR", "two_robot", "UPG-TR-B", "UPG-TR-C", false},
		{"UPG-SR", "single_robot", "UPG-SR-B", "", false},
		{"UPG-LEGACY", "simple", "UPG-LEGACY-B", "", false},
		{"UPG-PI", "two_robot_press_index", "UPG-PI-B", "UPG-PI-C", true},
		{"UPG-SQ-A", "sequential", "UPG-SQ-B", "", true},
		{"UPG-SQ-B", "sequential", "UPG-SQ-A", "", true},
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO style_node_claims (style_id, core_node_name, swap_mode, paired_core_node, second_paired_core_node)
			VALUES (?, ?, ?, ?, ?)`, styleID, r.node, r.mode, r.paired, r.second); err != nil {
			t.Fatalf("claim %s: %v", r.node, err)
		}
	}
	version := 0
	for _, m := range edgeMigrations() {
		if m.Name == "clear_paired_positions_a_mode_names_none" {
			version = m.Version
		}
	}
	if version == 0 {
		t.Fatal("no Edge migration named clear_paired_positions_a_mode_names_none")
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version >= ?`, version); err != nil {
		t.Fatalf("roll the version table back: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The upgrade.
	db, err = Open(path)
	if err != nil {
		t.Fatalf("reopen (the upgrade): %v", err)
	}
	defer db.Close()
	read := func(node string) (paired, second string) {
		t.Helper()
		if err := db.QueryRow(`SELECT paired_core_node, second_paired_core_node FROM style_node_claims
			WHERE style_id = ? AND core_node_name = ?`, styleID, node).Scan(&paired, &second); err != nil {
			t.Fatalf("read %s: %v", node, err)
		}
		return paired, second
	}
	for _, r := range rows {
		paired, second := read(r.node)
		wantPaired, wantSecond := "", ""
		if r.keep {
			wantPaired, wantSecond = r.paired, r.second
		}
		if paired != wantPaired || second != wantSecond {
			t.Errorf("%s (%s) after the upgrade: %q/%q, want %q/%q", r.node, r.mode, paired, second, wantPaired, wantSecond)
		}
	}

	// A later boot leaves the table alone: from here the save keeps it.
	if _, err := db.Exec(`UPDATE style_node_claims SET paired_core_node = 'UPG-TR-B' WHERE core_node_name = 'UPG-TR'`); err != nil {
		t.Fatalf("write a pair after the upgrade: %v", err)
	}
	if err := db.migrate(); err != nil {
		t.Fatalf("second boot: %v", err)
	}
	if paired, _ := read("UPG-TR"); paired != "UPG-TR-B" {
		t.Errorf("a later boot cleared UPG-TR again (%q): the upgrade runs once", paired)
	}
}
