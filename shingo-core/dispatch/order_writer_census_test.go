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

// door is one way an order can come to exist, described by what a person or a
// system does rather than by where the code lives.
//
// The file list this replaced counted eight "doors" and was wrong in both
// directions, because a file is not a door. service/order_service.go is a
// delegate that three different surfaces call, so one file was three doors.
// engine/orders.go is reached only from the /test-orders page, so one file was
// a test harness rather than an operator action. And the buried-reshuffle
// branch was counted as its own door until it turned out to be building the
// same row complex intake builds — one door that looked like two.
//
// Counting files answered "how many places call the writer". What anyone
// actually needs to know is "how many ways can an order appear, and what does
// each of them check" — because that is the question behind every gate
// decision, every projection scope, and every blank origin_class.
type door struct {
	name string // what the door IS, in the words someone would use out loud
	site string // where it starts, module-root-relative
	who  string // who or what opens it
}

// TestCensus_OrderCreationDoors pins every way an order can come to exist.
//
// The Core-native loader design was written on the premise that
// CreateInboundOrder is "the admission path", with a Core originator added
// beside it as the second entry point. That premise does not survive a census:
// most of the doors below never go near it. Extracting a shared admitOrder body
// is still right for the wire path and the new originator — but it is one door
// among several, not THE one, and anything hung off admitOrder covers only what
// routes through it.
//
// The immediate consequence is the Edge order projection: orders from the other
// doors do not pass through admitOrder, so a projection emitted there will not
// carry them and the Edge board stays blind to them, exactly as it is today.
// That may be the right first scope. It has to be a decision rather than an
// oversight, which is what this test exists to force.
func TestCensus_OrderCreationDoors(t *testing.T) {
	t.Parallel()
	doors := []door{
		{"Edge wire intake", "dispatch/lifecycle_service.go", "an Edge station sends an order request"},
		{"complex intake", "dispatch/complex_intake.go", "an Edge station sends a multi-leg order; the buried branch is this same door"},
		{"compound children", "dispatch/compound.go", "a reshuffle plan, written as child rows in one transaction"},
		// The "restore synthetic" door that stood here is gone: the restore-blockers
		// subsystem that minted those parents is retired (no code creates them; the
		// one-shot boot sweep only cancels leftovers). No order-creation site, no door.
		// One door, two screens. It was two doors — the operator's orders page
		// and the engineers' /test-orders page — which had drifted twelve ways
		// between them, and each difference was a bug waiting its turn. They
		// are one function now; the screens differ only in whether a person
		// names the bin or names the node to take one off.
		//
		// The three questions this list exists to ask, answered:
		//  1. Projects to the Edge? No. It writes the row itself rather than
		//     routing through admitOrder, so it is outside the projection scope
		//     — the same answer both doors gave separately.
		//  2. Needs the dropoff-capacity gate? Yes, and it consults it, before
		//     the row and before any claim so a refusal leaves the bin alone.
		//  3. What origin_class? no_demand, stamped at creation. Somebody
		//     moving a bin from A to B is not a place asking for material, so
		//     there is no episode; blank would land it in the bucket that means
		//     "we lost a demand link".
		{"bin move", "engine/bin_move.go", "a person moving one bin from where it is to somewhere else — the operator names the bin, the engineer names the node"},
		// The one door that is about a ROBOT rather than about material. A bin
		// riding a deck has nothing coming to fetch it — the order that was
		// carrying it is terminal — so this asks that robot, and only that
		// robot, to set the bin down somewhere.
		//
		// The three questions:
		//  1. Projects to the Edge? No. It writes the row itself, like the bin-move
		//     door beside it, so it is outside the projection scope. That is the
		//     right answer here for a second reason as well: no Edge station asked
		//     for this order and none of them owns the bin. It is Core reconciling
		//     its own bookkeeping with the floor.
		//  2. Needs the dropoff-capacity gate? Yes, and it takes the real slot
		//     reservation: ReserveStorageDropoff before ConfirmForDispatch, which
		//     also settles a group destination to a concrete child. Its own
		//     three-tier destination search additionally refuses any node that is
		//     occupied or claimed before it gets that far.
		//  3. What origin_class? no_demand, by omission — and deliberately. No
		//     place asked for material; a bin is in the wrong state and Core is
		//     putting it right. An episode here would count a recovery as demand
		//     and read every plant's demand history high by however many bins got
		//     dropped that month.
		{"carried-bin recovery", "engine/carried_bin_recovery.go", "nobody — Core asks the robot holding a stranded bin to put it down"},
		// THE LANE SELF-HEAL DOOR IS DELETED (§R.104), and it is not merely moved:
		// nothing takes its place, because the order it used to create does not
		// exist. Its entry read "nobody — the lane gate finds a robot dwelling
		// behind a bin no one is coming for, and digs it out", and its comment
		// explained that it "mints the parent that OWNS the excavation, because the
		// dweller cannot: {staged → reshuffling} is not a legal transition and
		// should not become one, so the demand keeps dwelling and something else
		// does the digging."
		//
		// The dweller can. It owns its excavation without moving at all — no
		// transition is needed because its resume is the splice-append, not the
		// queue round-trip. So the excavation's children are written by the
		// compound door like every other dig's, and this list is one door shorter
		// rather than one door renamed. The three questions it answered are now
		// the compound door's, unchanged.
	}

	// Each named door has to still be there. A door whose site stops writing
	// orders has either moved or been merged, and either way this list is now
	// describing a system that does not exist.
	sources := scanCoreSources(t)
	writes := func(src string) bool {
		return strings.Contains(src, "db.CreateOrder(") ||
			strings.Contains(src, "orders.Create(") ||
			strings.Contains(src, "CreateCompoundChildren(")
	}
	for _, d := range doors {
		src, ok := sources[d.site]
		if !ok {
			t.Errorf("door %q names %s, which no longer exists", d.name, d.site)
			continue
		}
		if !writes(src) {
			t.Errorf("door %q (%s) no longer creates orders. If it moved, say where; if it merged into another door, delete the entry and widen that one's description.",
				d.name, d.site)
		}
	}

	// Plumbing a door reaches THROUGH, named so that "not a door" is a stated
	// judgement rather than an omission. This is the distinction the old
	// file-counting version could not make, and the reason it counted eight.
	delegates := map[string]string{
		"service/order_service.go": "delegate; the doors are the surfaces that call it, counted there",
	}

	// And no site may start writing orders without being named as a door. This
	// is the direction that catches a new way in.
	named := map[string]bool{}
	for _, d := range doors {
		named[d.site] = true
	}
	for site := range delegates {
		named[site] = true
	}
	var unnamed []string
	for p, src := range sources {
		if strings.Contains(src, "db.CreateOrder(") && !named[p] {
			unnamed = append(unnamed, p)
		}
	}
	slices.Sort(unnamed)
	if len(unnamed) > 0 {
		t.Errorf("these create orders and are not named as a door: %s\n"+
			"Add one, in the words someone would use out loud, and answer three questions in the same commit:\n"+
			"  1. Does it project to the Edge? (only what routes through admitOrder does)\n"+
			"  2. Does it need the dropoff-capacity gate?\n"+
			"  3. What origin_class do its rows carry? A blank is a row nothing can explain later.",
			strings.Join(unnamed, ", "))
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
