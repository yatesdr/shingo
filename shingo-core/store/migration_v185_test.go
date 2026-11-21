package store

import (
	"strings"
	"testing"

	"shingocore/store/schema"
)

// TestV185_EveryOpHasItsInverse: down is generated from the same ops table as
// up, so each kind of op inverts by construction (a rename is renamed back, an
// add dropped, a check dropped). What construction cannot give is a move's
// inverse and a drop's restore: each is written beside its op, and each must
// be there. The values test (internal/schemadump, docker) proves they invert
// the rows.
func TestV185_EveryOpHasItsInverse(t *testing.T) {
	t.Parallel()
	for _, m := range v185Moves {
		if strings.TrimSpace(m.up) == "" || strings.TrimSpace(m.down) == "" {
			t.Errorf("move %q has no %s", m.name, map[bool]string{true: "up", false: "down"}[strings.TrimSpace(m.up) == ""])
		}
	}
	for _, d := range v185Ops.drops {
		if strings.TrimSpace(d.restore) == "" {
			t.Errorf("dropped column %s.%s has no restore", d.table, d.column)
		}
	}
	for _, d := range v185Ops.dropTables {
		if len(d.restore) == 0 || !strings.HasPrefix(strings.TrimSpace(d.restore[0]), "CREATE TABLE public."+d.table+" (") {
			t.Errorf("dropped table %s's restore does not start by creating it", d.table)
		}
	}
	seen := map[string]bool{}
	for _, r := range v185Ops.renames {
		k := r.table + "." + r.to
		if seen[k] {
			t.Errorf("two renames land on %s: down could not tell them apart", k)
		}
		seen[k] = true
	}
}

// TestV185CommentTable_ReadsTheTableOfEveryStatement: v185CommentsFor drops a
// comment whose table is gone (v189 dropped cell_config) by the table
// v185CommentTable reads off the statement, so that read must name a real
// table for every COMMENT ON TABLE, COLUMN and CONSTRAINT in comments.sql (one
// this build ships, or cell_config), and none for an INDEX or SEQUENCE.
func TestV185CommentTable_ReadsTheTableOfEveryStatement(t *testing.T) {
	t.Parallel()
	read := 0
	for _, line := range strings.Split(schema.Comments, "\n") {
		if !strings.HasPrefix(line, "COMMENT ON ") {
			continue
		}
		kind := strings.Fields(line)[2]
		got := v185CommentTable(line)
		switch kind {
		case "TABLE", "COLUMN", "CONSTRAINT":
			read++
			if got != "cell_config" && !strings.Contains(schema.Snapshot, "CREATE TABLE public."+got+" (") {
				t.Errorf("v185CommentTable read %q off %.80s…: not a table this build ships", got, line)
			}
		default:
			if got != "" {
				t.Errorf("v185CommentTable read %q off a COMMENT ON %s; want none", got, kind)
			}
		}
	}
	if read == 0 {
		t.Fatal("comments.sql has no COMMENT ON TABLE, COLUMN or CONSTRAINT: the test read nothing")
	}
}
