package dispatch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"shingo/protocol"
)

// declaredQueueCauses reads the CONSTANT DECLARATIONS out of queue_cause.go.
//
// Go cannot enumerate a package's constants, so a totality test needs a list —
// and a hand-written list is the thing being guarded against, one level up. The
// declarations are the only enumeration that cannot drift from itself.
type declaredCause struct {
	name  string // the Go constant identifier
	value QueueCause
}

func declaredQueueCauses(t *testing.T) []declaredCause {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(repoRootFor(t), "shingo-core", "dispatch", "queue_cause.go"))
	if err != nil {
		t.Fatalf("read queue_cause.go: %v", err)
	}
	re := regexp.MustCompile(`(?m)^\s*(Cause\w+)\s+QueueCause\s*=\s*"([^"]+)"`)
	matches := re.FindAllStringSubmatch(string(src), -1)
	if len(matches) < 20 {
		t.Fatalf("found only %d cause declarations — the regex no longer matches the constant "+
			"block's shape, so every test built on it is passing vacuously", len(matches))
	}
	var out []declaredCause
	for _, m := range matches {
		out = append(out, declaredCause{name: m[1], value: QueueCause(m[2])})
	}
	return out
}

// TestEveryQueueCauseHasAReleaser is TOTALITY — the doctrine's (a) and (b) made
// checkable.
//
// A cause is a wait Core can record. If the table has no row for one, then a
// wait exists that nobody has said how to end, and the omission is invisible:
// the order sits carrying a cause, the board renders it, and the question "what
// clears this" has no written answer. That is F-22's shape exactly — the
// evaluator's doc assumed a next firing existed and nothing checked.
//
// A row whose releaser cannot be described truthfully carries a `finding`
// instead, which SATISFIES this test on purpose. The failure being guarded
// against is a blank, not an ugly truth: two rows are findings today (the
// fleet-refusal absence class and one declared-but-never-set cause) and both are
// data the batch reports rather than gaps it hides.
//
// MUTATION: delete any causeReleasers row — this names the cause and says a wait
// exists with no documented way out.
func TestEveryQueueCauseHasAReleaser(t *testing.T) {
	t.Parallel()
	declared := declaredQueueCauses(t)

	rows := make(map[QueueCause]causeReleaser, len(causeReleasers))
	for _, r := range causeReleasers {
		if _, dup := rows[r.cause]; dup {
			t.Errorf("cause %q has TWO causeReleasers rows — the floor's record would print whichever "+
				"came first and the histogram would double-count it", r.cause)
		}
		rows[r.cause] = r
	}

	for _, dc := range declared {
		c := dc.value
		r, ok := rows[c]
		if !ok {
			t.Errorf("cause %q is declared but has NO causeReleasers row. An order can be parked "+
				"under it with nothing on record about what ends the wait — write the row, and if "+
				"the honest answer is 'nothing', write that as a finding", c)
			continue
		}
		if r.what == "" && r.finding == "" {
			t.Errorf("cause %q has a row with neither a releaser nor a finding — a blank is the one "+
				"thing the table may not contain, because it reads as covered", c)
		}
		if len(r.populations) == 0 {
			t.Errorf("cause %q names no population, so nothing says which floor backstops it", c)
		}
	}

	// The other direction: a row for a cause that no longer exists is a releaser
	// describing a wait nothing can enter, and it makes the table look wider than
	// the vocabulary it covers.
	declaredSet := make(map[QueueCause]bool, len(declared))
	for _, dc := range declared {
		declaredSet[dc.value] = true
	}
	for _, r := range causeReleasers {
		if !declaredSet[r.cause] {
			t.Errorf("causeReleasers has a row for %q, which is not a declared QueueCause constant", r.cause)
		}
	}
}

// TestEveryWaitPopulationHasBothPaths is the doctrine stated as an assertion:
// every population an order can wait in has an EVENT path and a FLOOR.
//
// This is the F-22 guard proper. The gate-staged and compound-leg populations
// each had a complete, generous, correct event trigger set and no floor, and the
// gap was invisible precisely because the event side looked so thorough. Nothing
// compared the two columns.
//
// MUTATION: blank the floor on any waitPopulations row — this names it and says
// a quiesced plant never wakes it, which is the sentence F-22 was written in.
func TestEveryWaitPopulationHasBothPaths(t *testing.T) {
	t.Parallel()
	seen := map[WaitPopulation]bool{}
	for _, p := range waitPopulations {
		seen[p.population] = true
		if len(p.events) == 0 {
			t.Errorf("population %q has no event releasers — every wait in it depends entirely on a "+
				"periodic pass, which makes the floor the mechanism rather than the backstop", p.population)
		}
		if p.floor == "" {
			t.Errorf("population %q has NO FLOOR. Its waits end only when an event fires, and a "+
				"quiesced plant emits none — the orders that could produce one are exactly the set "+
				"waiting to be released. This is F-22", p.population)
		}
		if p.redriver == "" {
			t.Errorf("population %q names no re-driver, so nothing says the floor and the events go "+
				"through the same machinery", p.population)
		}
	}

	// Every population a cause references must be one the mechanism table
	// describes — otherwise a cause claims a backstop that does not exist.
	for _, r := range causeReleasers {
		for _, pop := range r.populations {
			if pop == PopNone {
				if r.finding == "" {
					t.Errorf("cause %q is in no population but records no finding — PopNone is the "+
						"honest answer for a cause nothing produces, and it has to say so", r.cause)
				}
				continue
			}
			if !seen[pop] {
				t.Errorf("cause %q names population %q, which has no waitPopulations row and "+
					"therefore no declared floor", r.cause, pop)
			}
		}
	}
}

// ── The status partition, raised from soakstat to the protocol level ──────
//
// D4 partitioned the non-terminal statuses across soakstat's three stall
// THRESHOLDS. That caught the checker's blind spot but says nothing about
// liveness: a status can be perfectly well watched by a stall report and still
// have nothing that ends its waits.
//
// This is the same technique aimed at the doctrine. Every non-terminal status is
// classified once, and the classification says WHO ends a wait in it. A new
// status fails the test until somebody answers that question — which is the
// property nothing had when `faulted` slipped between three defensible
// exclusions and blocked changeovers until a person found it.

type statusWaitClass string

const (
	// classCoreWait — Core owes this order a decision. These are the populations
	// the doctrine governs, and each must map to a waitPopulations row.
	classCoreWait statusWaitClass = "core-wait"
	// classVendorHeld — the fleet has it. The RDS poller drives it and the stuck
	// sweep bounds it; Core is not deciding anything.
	classVendorHeld statusWaitClass = "vendor-held"
	// classHumanOwned — a person owes the decision. Bounded by a longer timeout,
	// never by a lane event, and deliberately not floored on a machine cadence.
	classHumanOwned statusWaitClass = "human-owned"
	// classTransient — no wait decision has been made; the order is mid-creation
	// or mid-grace. A row stuck here is a crash artifact rather than a wait, so
	// its safety net is the runtime-stuck ANOMALY, not a releaser.
	classTransient statusWaitClass = "transient"
)

// statusWaitClasses classifies every non-terminal status. `population` is set
// only for classCoreWait and must name a waitPopulations row.
var statusWaitClasses = map[protocol.Status]struct {
	class      statusWaitClass
	population WaitPopulation
	why        string
}{
	protocol.StatusQueued:   {classCoreWait, PopAcquiring, "the scanner re-resolves it on events and on its own 60s sweep"},
	protocol.StatusSourcing: {classCoreWait, PopAcquiring, "same as queued — IsAcquiring is the scanner's set"},
	protocol.StatusStaged: {classCoreWait, PopGateStaged,
		"a robot parked at a wait point. A LANE wait is Core's and is floored here; an OPERATOR " +
			"wait on the same status is human-owned and bounded by AbandonStuckOrders' longer timeout. " +
			"The status cannot tell them apart — the wait step can (WaitKind), which is why the " +
			"population is derived from the step and not from this column"},
	protocol.StatusReshuffling: {classCoreWait, PopCompoundParent,
		"a compound parent waiting on its children; AdvanceStuckReshuffleParents is its floor"},

	protocol.StatusSubmitted:    {classVendorHeld, "", "handed to the vendor, awaiting acknowledgement; the poller drives it"},
	protocol.StatusAcknowledged: {classVendorHeld, "", "the vendor has it; the poller drives it"},
	protocol.StatusDispatched:   {classVendorHeld, "", "the fleet has the job; poller-driven, and the stuck sweep bounds it"},
	protocol.StatusInTransit:    {classVendorHeld, "", "a robot is moving; not a wait"},

	protocol.StatusDelivered: {classHumanOwned, "", "awaiting confirmation; AutoConfirmStuckDeliveredOrders bounds it"},

	protocol.StatusPending: {classTransient, "",
		"pre-intake. Nothing decided to hold it, so nothing releases it; a row stuck here is a " +
			"crash between create and route, and IsRuntimeStuckCandidate is what surfaces that"},
	protocol.StatusFaulted: {classTransient, "",
		"a grace period, ended by its own timer or by the operator. Deliberately outside both the " +
			"anomaly set and the sweep — and IsVendorTracked is what stops it becoming the hole it " +
			"was before 2026-08-03"},
}

// TestEveryNonTerminalStatusIsClassifiedOnce is the PARTITION.
//
// MUTATION: delete the StatusStaged row — the test names it and says nothing on
// record says who ends a wait in it, which is the state F-22 shipped in.
func TestEveryNonTerminalStatusIsClassifiedOnce(t *testing.T) {
	t.Parallel()
	pops := map[WaitPopulation]bool{}
	for _, p := range waitPopulations {
		pops[p.population] = true
	}

	for _, s := range protocol.AllStatuses() {
		if protocol.IsTerminal(s) {
			if _, ok := statusWaitClasses[s]; ok {
				t.Errorf("terminal status %q is classified as a wait — nothing waits after a "+
					"terminal transition", s)
			}
			continue
		}
		c, ok := statusWaitClasses[s]
		if !ok {
			t.Errorf("status %q is non-terminal and UNCLASSIFIED — nothing on record says who ends "+
				"a wait in it or what backstops that. Classify it: Core's, the vendor's, a human's, "+
				"or transient", s)
			continue
		}
		if c.why == "" {
			t.Errorf("status %q is classified %q with no reason — the classification is the claim, "+
				"and an unexplained claim is the one that gets inherited wrongly", s, c.class)
		}
		switch c.class {
		case classCoreWait:
			if c.population == "" {
				t.Errorf("status %q is a CORE wait but names no population, so no floor covers it", s)
			} else if !pops[c.population] {
				t.Errorf("status %q names population %q, which has no waitPopulations row", s, c.population)
			}
		default:
			if c.population != "" {
				t.Errorf("status %q is %q but names population %q — only Core waits have Core floors",
					s, c.class, c.population)
			}
		}
	}
}

// TestCoreWaitPopulationsAreAllReachable closes the loop from the other end: a
// population declared in waitPopulations that no status and no cause can reach
// is a floor running over an empty set, which reads as "covered" forever.
func TestCoreWaitPopulationsAreAllReachable(t *testing.T) {
	t.Parallel()
	reached := map[WaitPopulation]bool{}
	for _, c := range statusWaitClasses {
		if c.population != "" {
			reached[c.population] = true
		}
	}
	for _, r := range causeReleasers {
		for _, p := range r.populations {
			reached[p] = true
		}
	}
	for _, p := range waitPopulations {
		if !reached[p.population] {
			t.Errorf("population %q is declared with an event set and a floor, and NOTHING can be "+
				"in it — no status classifies to it and no cause names it. A floor over an empty "+
				"set passes every test and protects nobody", p.population)
		}
	}
}

// TestQueueCauseCollisionsAreDeclared pins the two known one-string-two-facts
// pairs as FINDINGS rather than letting them quietly become normal.
//
// Both are kept on purpose (re-spelling either rewrites what existing rows in
// the plant's orders table mean), so the guard is not "make them unique" — it is
// "if two constants share a value, the table must say so". A future third
// collision added silently is the failure.
func TestQueueCauseCollisionsAreDeclared(t *testing.T) {
	t.Parallel()
	byValue := map[QueueCause][]string{}
	for _, dc := range declaredQueueCauses(t) {
		byValue[dc.value] = append(byValue[dc.value], dc.name)
	}
	for value, names := range byValue {
		if len(names) < 2 {
			continue
		}
		r, ok := releaserFor(value)
		if !ok || r.finding == "" {
			sort.Strings(names)
			t.Errorf("%d constants share the value %q (%s) and its causeReleasers row records no "+
				"finding. A queue_cause histogram cannot separate them, so the row describes two "+
				"waits and is exact about neither — say so in the table",
				len(names), value, strings.Join(names, ", "))
		}
	}
}
