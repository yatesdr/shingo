package orders_test

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// deliberatelyNotWritten lists every orders column that Create does NOT bind,
// each with the reason it is somebody else's to write.
//
// This list is the point of the test. A column missing from the INSERT is not
// automatically a bug — most of these are genuinely owned by a later transition
// — but a column missing from the INSERT *and* from this list is a column
// somebody forgot, and that is exactly how queue_reason came to be assigned in
// complex intake and silently dropped on the floor for as long as anyone can
// tell. The cost of adding an entry here is one line and one sentence; the cost
// of the alternative was a column that read blank in production and an
// investigation to find out why.
//
// Adding a column to the orders DDL therefore forces a decision: write it at
// creation, or say here who writes it instead.
var deliberatelyNotWritten = map[string]string{
	"id":              "serial primary key, assigned by RETURNING id",
	"vendor_order_id": "fleet identifier, minted at dispatch (dispatcher.go)",
	"vendor_state":    "vendor lifecycle mirror, written on vendor callbacks",
	"robot_id":        "assigned when the fleet picks a robot",
	"error_detail":    "written on failure transitions",
	"completed_at":    "written at the terminal transition",
	"wait_index":      "queue-position bookkeeping, maintained after create",
	"queue_reason":    "owned by the transition that queues, via SetOrderQueueDetail",
	"queue_code":      "same; read back at lifecycle.go:291 to build the history reason",
	"queue_cause":     "same",
	"remaining_uop":   "operator-declared release correction, carried to the bin claim",
	"orphan_aged_at":  "stamped by the orphan sweep long after creation; an order is never born aged",
	// Deliberately NOT bound, and the omission is the mechanism rather than an
	// oversight. Every order is born sealed -- the column's DEFAULT false says
	// so for the whole table at once, which is also the true statement about
	// every row that predates it. Binding it here would give Create a say in a
	// fact it does not decide, and would put a second writer next to
	// SetCompoundOpen. Openness is written on purpose, by one place, or not at
	// all.
	"open_for_children": "born sealed by DEFAULT false; only SetCompoundOpen ever changes it (store/orders.go)",
}

// TestWriter_CoversEveryOrdersColumn pins that every column on the orders table
// is either written by Create or listed in deliberatelyNotWritten with a reason.
//
// It reads the two sources as text rather than querying a database, so it runs
// in the plain gate with no container: the DDL constant in store/schema and the
// INSERT in this package are both the literal strings that ship.
func TestWriter_CoversEveryOrdersColumn(t *testing.T) {
	t.Parallel()
	ddl := ordersDDLColumns(t)
	written := insertColumns(t)

	if len(ddl) == 0 {
		t.Fatal("parsed zero columns out of the orders DDL; the parser is broken, not the schema")
	}

	var unaccounted []string
	for _, col := range ddl {
		if slices.Contains(written, col) {
			continue
		}
		if _, ok := deliberatelyNotWritten[col]; ok {
			continue
		}
		unaccounted = append(unaccounted, col)
	}
	if len(unaccounted) > 0 {
		t.Errorf("orders columns neither written by Create nor listed as deliberately skipped: %s\n"+
			"Either add the column to the INSERT in Create, or add it to deliberatelyNotWritten with the reason somebody else writes it.\n"+
			"A column in neither place is written by nobody, and reads blank forever.",
			strings.Join(unaccounted, ", "))
	}

	// The allowlist has to decay when the writer grows, or it becomes a list of
	// claims about the past. Both directions are checked: an entry naming a
	// column that no longer exists, and an entry for a column the writer has
	// since started binding.
	for col := range deliberatelyNotWritten {
		if !slices.Contains(ddl, col) {
			t.Errorf("deliberatelyNotWritten names %q, which is not a column on the orders table — drop the entry", col)
		}
		if slices.Contains(written, col) {
			t.Errorf("deliberatelyNotWritten names %q, but Create now writes it — drop the entry", col)
		}
	}
}

// ordersDDLColumns returns the column names declared in the CREATE TABLE for
// orders, in declaration order.
func ordersDDLColumns(t *testing.T) []string {
	t.Helper()
	const ddlPath = "../schema/postgres_ddl.go"
	b, err := os.ReadFile(ddlPath)
	if err != nil {
		t.Fatalf("read %s: %v", ddlPath, err)
	}
	_, rest, found := strings.Cut(string(b), "CREATE TABLE IF NOT EXISTS orders (")
	if !found {
		t.Fatalf("no CREATE TABLE for orders in %s — did the table move, or the DDL style change?", ddlPath)
	}
	body, _, found := strings.Cut(rest, "\n);")
	if !found {
		t.Fatalf("unterminated CREATE TABLE for orders in %s", ddlPath)
	}

	var cols []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		name, _, _ := strings.Cut(line, " ")
		name = strings.TrimSuffix(name, ",")
		// Table-level constraints are not columns.
		switch strings.ToUpper(name) {
		case "PRIMARY", "FOREIGN", "UNIQUE", "CHECK", "CONSTRAINT", "EXCLUDE":
			continue
		}
		if name != "" {
			cols = append(cols, name)
		}
	}
	return cols
}

// insertColumns returns the column names bound by Create's INSERT, read out of
// this package's own source so the test compares the shipping string.
func insertColumns(t *testing.T) []string {
	t.Helper()
	const srcPath = "orders.go"
	b, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read %s: %v", srcPath, err)
	}
	_, rest, found := strings.Cut(string(b), "INSERT INTO orders (")
	if !found {
		t.Fatalf("no INSERT INTO orders in %s", srcPath)
	}
	list, _, found := strings.Cut(rest, ")")
	if !found {
		t.Fatalf("unterminated column list in %s", srcPath)
	}

	var cols []string
	for _, c := range strings.Split(list, ",") {
		if c = strings.TrimSpace(c); c != "" {
			cols = append(cols, c)
		}
	}
	return cols
}
