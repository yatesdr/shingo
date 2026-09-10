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

// writerSymbols is every call that brings an order row into existence, in the
// spelling a reader would grep for.
//
// ONE LIST, BOTH ARMS. The census asks two questions — "is this door still a
// door" and "has a new door opened" — and they used to be asked with different
// predicates: the existence arm checked all three writer symbols while the
// completeness arm greped db.CreateOrder( alone. So the direction that exists to
// catch a NEW way in was blind to a writer reaching the table through
// orders.Create or CreateCompoundChildren, and the census reported a
// completeness it had not checked.
//
// AdmitCoreAsk belongs on this list even though it is not the store call. From a
// caller's side it IS creation: it builds the order, admits it through the same
// admitOrder body the wire path uses, and hands back the created row. The two
// Core timer doors reach the table only through it and carry no store symbol of
// their own — which is exactly how both of them stood uncounted while both
// census tests passed green.
var writerSymbols = []string{
	"db.CreateOrder(",
	"orders.Create(",
	"CreateCompoundChildren(",
	"AdmitCoreAsk(",
}

// writesOrders reports whether a source file brings orders into existence.
func writesOrders(src string) bool {
	for _, sym := range writerSymbols {
		if strings.Contains(src, sym) {
			return true
		}
	}
	return false
}

// unnamedWriters is the completeness arm's whole judgement, lifted out so it can
// be run against a synthetic tree as well as the real one. A census that can
// only be exercised against the code it already describes cannot demonstrate
// that it would catch anything.
func unnamedWriters(sources map[string]string, named map[string]bool) []string {
	var unnamed []string
	for p, src := range sources {
		if writesOrders(src) && !named[p] {
			unnamed = append(unnamed, p)
		}
	}
	slices.Sort(unnamed)
	return unnamed
}

// door is one way an order can come to exist, described by what a person or a
// system does rather than by where the code lives.
//
// The file list this replaced counted eight "doors" and was wrong in both
// directions, because a file is not a door. service/order_service.go was
// counted as a delegate three surfaces called; the census then found that no
// surface called it at all, and the delegate was deleted rather than described
// (see the note where its Create used to be). engine/orders.go is reached only
// from the /test-orders page, so one file was a test harness rather than an
// operator action. And the buried-reshuffle branch was counted as its own door
// until it turned out to be building the same row complex intake builds — one
// door that looked like two. Meanwhile the two Core timer doors, which no file
// list ever named, were writing orders the whole time.
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
		{"carried-bin recovery", "engine/carried_bin_recovery.go", "an operator pressing Recover on the bins page — Core then asks the robot holding the stranded bin to put it down"},
		// THE TWO CORE TIMER DOORS. Both mint retrieve_empty orders nobody on the
		// Edge requested, both reach the orders table through AdmitCoreAsk and
		// admitOrder rather than writing a row, and both stood on no list at all
		// until the completeness arm was widened to see them. The two doors that
		// fire with nobody watching were the two nothing counted.
		//
		// They are TWO doors and not one, for the reason this list exists: a door
		// is a way an order comes to exist, and a shared body is not that. Two
		// different systems ask for two different reasons under two different
		// bounds — the same distinction that made one delegate file three doors
		// before the delegate turned out to be dead.
		//
		// The three questions, loader replenish:
		//  1. Projects to the Edge? YES, and this is THE case the projection
		//     exists for. It routes through admitOrder, so it is inside the
		//     scope; and no Edge station asked for the order, so without the
		//     projection an operator watches a robot arrive at a window with
		//     nothing on the board to say why.
		//  2. Needs the dropoff-capacity gate? Yes, and it consults it per window
		//     at decision time, with no order of its own to exclude. It then asks
		//     a SECOND question the gate cannot answer — is a carrier already on
		//     order for this window — because the gate does not count `queued`,
		//     and that blindness is what let one dry loader stack 241 identical
		//     asks at a single window, roughly one a minute.
		//  3. What origin_class? Whatever the replenish request carries, and never
		//     blank: AdmitCoreAsk stamps `attached` when an episode id is in hand
		//     and `orphan` when neither is, rather than defaulting a
		//     correctly-attributed order into the bucket that exists to find lost
		//     attributions.
		{"loader replenish", "dispatch/loader_replenish.go", "nobody — a loader's windows are below level and the replenishment loop asks for empty carriers"},
		// The three questions, maintained-group level keeper:
		//  1. Projects to the Edge? Yes, through the same body and for the same
		//     reason — nobody on the Edge asked, so nobody there has a row.
		//  2. Needs the dropoff-capacity gate? It does not consult it, and that is
		//     a decision rather than an omission. The ask is bounded twice before
		//     it is made: the keeper's own arithmetic subtracts what it already
		//     asked for and what is already coming (want − resident − asked −
		//     coming), and ResolveStore refuses a group already at its declared
		//     level and hands back one concrete free slot. The destination is
		//     pre-resolved for exactly this reason — one ask per free typed slot,
		//     so there is no second ask to gate.
		//  3. What origin_class? `attached`, stated at the call site rather than
		//     defaulted. The keeper opened the episode itself and knows what it is.
		{"maintained-group level keeper", "engine/maintainer.go", "nobody — a maintained group holds fewer empty carriers than its declared level and the maintainer's timer tops it up"},
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
	for _, d := range doors {
		src, ok := sources[d.site]
		if !ok {
			t.Errorf("door %q names %s, which no longer exists", d.name, d.site)
			continue
		}
		if !writesOrders(src) {
			t.Errorf("door %q (%s) no longer creates orders. If it moved, say where; if it merged into another door, delete the entry and widen that one's description.",
				d.name, d.site)
		}
	}

	// Plumbing a door reaches THROUGH, named so that "not a door" is a stated
	// judgement rather than an omission. This is the distinction the old
	// file-counting version could not make, and the reason it counted eight.
	delegates := map[string]string{
		"store/orders.go": "the writer itself; every door reaches the orders table through this file, which is why there is one INSERT and not several",
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
	unnamed := unnamedWriters(sources, named)
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

// TestCensus_CompletenessArmSeesEveryWriterSymbol is the census pointed at
// itself: a new production file that creates orders must fail the census until
// somebody names it as a door.
//
// It exists because the arm was silently half-blind. The existence arm checked
// all the writer symbols; the completeness arm — the ONE direction that can
// catch a way in nobody has thought of yet — greped db.CreateOrder( alone. A new
// file reaching the table through orders.Create, CreateCompoundChildren or
// AdmitCoreAsk passed straight through it, which is not a hypothetical: two Core
// timer doors did exactly that, and both census tests stayed green while they
// did. Against the real tree that blindness is invisible, because the tree's own
// db.CreateOrder( sites all happen to be named. So the arm is run here against a
// synthetic tree, one symbol at a time.
func TestCensus_CompletenessArmSeesEveryWriterSymbol(t *testing.T) {
	t.Parallel()
	// The doors named at the time the new file appears. Deliberately not the
	// real list: this test is about the ARM, not about today's census.
	named := map[string]bool{"dispatch/existing_door.go": true}

	for _, sym := range writerSymbols {
		t.Run(sym, func(t *testing.T) {
			const newDoor = "dispatch/brand_new_door.go"
			sources := map[string]string{
				"dispatch/existing_door.go": "package dispatch\n\nfunc old() { " + sym + "o) }\n",
				newDoor:                     "package dispatch\n\nfunc brandNew() { " + sym + "o) }\n",
			}
			got := unnamedWriters(sources, named)
			if len(got) != 1 || got[0] != newDoor {
				t.Errorf("a new file calling %s was reported as %v, want [%s].\n"+
					"The completeness arm has to check every symbol the existence arm checks, or a door can open through the ones it skips.",
					sym, got, newDoor)
			}
		})
	}
}

// TestCensus_CompletenessArmIgnoresNonWriters keeps the arm from crying wolf:
// a file that does not create orders is not an unnamed door, and an arm that
// flags everything gets whitelisted into uselessness.
func TestCensus_CompletenessArmIgnoresNonWriters(t *testing.T) {
	t.Parallel()
	sources := map[string]string{
		"dispatch/reader.go": "package dispatch\n\nfunc r() { db.GetOrder(1) }\n",
	}
	if got := unnamedWriters(sources, map[string]bool{}); len(got) != 0 {
		t.Errorf("unnamedWriters flagged non-writers: %v", got)
	}
}
