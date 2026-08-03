package dispatch

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// scanCoreSources returns every non-test .go file in shingo-core, as
// (path relative to the module root, contents).
//
// This used to list six directories by hand. That made the census answer a
// narrower question than it claimed to: not "who creates orders" but "who
// creates orders in the six places we already knew about." A writer added
// anywhere else — fulfillment, messaging, material, a new package — was
// invisible, and the test would have gone on passing while reporting
// completeness. Widening it to the whole module found a real file the hand
// list had missed on the first run.
//
// Package dir is the test's CWD, so ".." is the module root regardless of
// where `go test` ran from.
func scanCoreSources(t *testing.T) map[string]string {
	t.Helper()
	const root = ".."
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Nothing under these creates an order, and walking them costs
			// more than the rest of the tree put together.
			switch d.Name() {
			case ".git", "vendor", "node_modules", "testdata", "build", "deploy", "docs", "scripts":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		// Shared test fixtures live in ordinary .go files so several packages
		// can import them, so the _test.go suffix does not catch them. They are
		// not doors: nothing in production reaches them. internal/testdb is the
		// one today, and widening the walk is what surfaced it. Importing
		// "testing" is the reliable tell — production code never does.
		if strings.Contains(string(b), "\"testing\"") {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// TestCensus_OrdersTableInsertStatements pins that exactly ONE SQL statement
// inserts into the orders table.
//
// There used to be two. The shared writer carried 21 data columns and a second
// statement inside CreateCompoundChildren carried 16, so five columns were
// silently dropped on the path that writes compound children — the rows the
// demand grain counts. Nothing enforced the two lists staying in step; it
// depended on whoever added the next column having read a comment.
//
// Now there is one statement, so a column added to it cannot be missed by
// another. If this test fails because you added a second INSERT, the question to
// answer first is whether it can go through orders.Create instead — it takes a
// QueryRower, so it works inside a transaction.
func TestCensus_OrdersTableInsertStatements(t *testing.T) {
	t.Parallel()
	const want = 1
	var sites []string
	for p, src := range scanCoreSources(t) {
		for i, line := range strings.Split(src, "\n") {
			// Skip comments: store/orders.go carries a prose warning about the
			// second INSERT, and counting it would report a hazard that is
			// actually the documentation of the hazard.
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strings.Contains(line, "INSERT INTO orders ") {
				sites = append(sites, p+":"+itoa(i+1))
			}
		}
	}
	slices.Sort(sites)
	if len(sites) != want {
		t.Errorf("INSERT INTO orders statements = %d, want %d.\nEvery column on the orders table must be added to ALL of them or deliberately omitted from all.\nSites:\n  %s",
			len(sites), want, strings.Join(sites, "\n  "))
	}
}

// TestCensus_OrderCreationPaths pins which files create rows in the orders table.
//
// The Core-native loader design was written on the premise that CreateInboundOrder
// is "the admission path," with a Core originator added beside it as the second
// entry point. That premise does not survive a census: seven other places create
// orders without going anywhere near it, including a plain admin bin-move
// (engine/orders.go) and a core-spot send_to raised straight from a web handler.
// Extracting a shared admitOrder body is still the right move for the wire path
// and the new originator — but it is one path among several, not THE one, and
// anything that hangs off admitOrder covers only what routes through it.
//
// The immediate consequence is for the Edge order projection: orders created by
// the paths below do not pass through admitOrder, so a projection emitted there
// will not carry them, and the Edge board stays blind to them exactly as it is
// today. That may be the right v1 scope. It has to be a decision rather than an
// oversight, which is what this test exists to force.
//
// A new file here means a new way for an order to exist. Decide whether it should
// project to the Edge and whether it needs the capacity gate, then update this
// list in the same commit.
func TestCensus_OrderCreationPaths(t *testing.T) {
	t.Parallel()
	want := []string{
		"dispatch/complex_intake.go",    // complex / multi-leg intake
		"dispatch/complex_reshuffle.go", // reshuffle leg
		"dispatch/lifecycle_service.go", // CreateInboundOrder — the Edge wire intake
		"dispatch/restore_listeners.go", // synthetic order raised during restore
		"engine/orders.go",              // CreateDirectOrder — admin moves a bin A->B
		"service/order_service.go",      // OrderService.Create — delegate used by web handlers
	}
	var got []string
	for p, src := range scanCoreSources(t) {
		if strings.Contains(src, "db.CreateOrder(") {
			got = append(got, p)
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("files creating orders rows changed.\n got: %s\nwant: %s\nA new entry is a new way for an order to exist. Answer three questions, then update this list in the same commit:\n  1. Does it project to the Edge? (only what routes through admitOrder does)\n  2. Does it need the dropoff-capacity gate?\n  3. What origin_class do its rows carry? A blank is a row nothing can explain later.",
			strings.Join(got, ", "), strings.Join(want, ", "))
	}
}

// itoa avoids pulling strconv in for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
