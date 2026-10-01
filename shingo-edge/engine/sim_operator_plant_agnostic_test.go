//go:build sim

package engine

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestSimOperator_NamesNoPlantsNodes is a tripwire for the defect class that
// killed a two-and-a-half-hour soak.
//
// `const negBinMarket = "SYN_MARKET"`, since removed, lived in sim_operator.go with a comment
// admitting what it was: "hardcoded to the demo's combined market name. If the
// plant renames/splits its market group, update this... Untested." A second plant
// then arrived with SYN_STAMP and SYN_COMP, the negative-bin sweep looked up a
// group that does not exist, found nothing, and returned at its own length check
// on every tick for the whole run. It logs only when it clears something, so a
// sweep that could not find its own subject was indistinguishable from a sweep
// with nothing to do — and six of the plant's twelve carriers stayed stranded.
//
// THE FAILURE MODE IS SILENCE, which is why a test rather than a comment. Nothing
// about a hardcoded plant name fails loudly: it degrades to a no-op, and a no-op
// sweep looks exactly like a healthy one from outside.
//
// So: the sim operator may not name a plant's nodes. Everything it needs it
// derives — markets from the active claims' inbound/outbound, manual_swap nodes
// from the process-node table. A future helper that wants to know a node name has
// to ask the plant, and this is where it finds that out.
//
// The pattern is the house node-naming convention (a 3+ letter uppercase prefix
// then an underscore: SYN_MARKET, PLN_001, ALN_003, LSD_027, FGN_001). It
// deliberately does not try to be a general "is this a node name" oracle — it
// catches the shape every plant in this repo actually uses, which is the shape
// the bug had.
func TestSimOperator_NamesNoPlantsNodes(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("sim_operator.go")
	if err != nil {
		t.Fatalf("read sim_operator.go: %v", err)
	}
	// Node-shaped identifiers inside double-quoted string literals only. A prose
	// comment may name SYN_MARKET all it likes — the tombstone above the deleted
	// constant does, and it should.
	literal := regexp.MustCompile(`"[^"]*"`)
	nodeish := regexp.MustCompile(`\b[A-Z]{3,}_[A-Z0-9]+\b`)

	var found []string
	for i, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		for _, lit := range literal.FindAllString(line, -1) {
			for _, hit := range nodeish.FindAllString(lit, -1) {
				found = append(found, hit+" (line "+itoaLocal(i+1)+")")
			}
		}
	}
	if len(found) > 0 {
		t.Errorf("sim_operator.go names plant nodes in string literals: %s\n"+
			"The sim operator runs against whatever plant is seeded, so a node name written down here "+
			"is a name that can disagree with the plant — and it fails SILENTLY when it does, because "+
			"every lookup degrades to an empty result that reads as 'nothing to do'. That is exactly "+
			"how negBinMarket=\"SYN_MARKET\" stranded half the carrier pool for a whole soak.\n"+
			"Derive it instead: markets from the active claims' InboundSource/OutboundDestination, "+
			"manual_swap nodes from ListProcessNodes.", strings.Join(found, ", "))
	}
}

// TestOperatorHasWorkAt pins the sweep's decision, including both boundaries.
//
// The two roles are exact opposites, so an inverted predicate does not fail
// visibly — it produces an operator that loads full bins and clears empty ones,
// a plant that looks busy and moves nothing.
//
// THE EMPTY-NODE ROWS ARE HERE BECAUSE THE FIRST LIVE RUN NEEDED THEM. This
// predicate originally took only the UOP, and FetchNodeBins returns a row for
// every node it was asked about — so a loader window with nothing on it came back
// as UOP 0 and read as an empty carrier. Four idle loaders were scheduled, each
// failed eight times with "no bin at node — request an empty bin first", and the
// whole thing repeated every reconcile tick. The bin id separates "a carrier is
// here and it is empty" from "nothing is here"; those two cases differ by one
// field and by the entire meaning.
//
// The negative-UOP rows matter for the other reason: an over-consumed carrier is
// still a carrier waiting to be filled, and a `== 0` test would skip precisely
// the bins the negative sweep exists to rescue — six of twelve, on this rig.
func TestOperatorHasWorkAt(t *testing.T) {
	t.Parallel()
	const someBin = int64(7)
	cases := []struct {
		name       string
		wantsEmpty bool
		binID      int64
		uop        int
		want       bool
	}{
		{"loader holding an empty carrier", true, someBin, 0, true},
		{"loader holding an over-consumed carrier", true, someBin, -2, true},
		{"loader holding a full bin", true, someBin, 40, false},
		{"unloader holding a full bin", false, someBin, 20, true},
		{"unloader holding a part-drained bin", false, someBin, 3, true},
		{"unloader holding an empty carrier", false, someBin, 0, false},
		{"unloader holding an over-consumed carrier", false, someBin, -1, false},
		// The rows the live run added.
		{"loader standing empty is NOT an empty carrier", true, 0, 0, false},
		{"unloader standing empty", false, 0, 0, false},
	}
	for _, c := range cases {
		if got := operatorHasWorkAt(c.wantsEmpty, c.binID, c.uop); got != c.want {
			t.Errorf("%s: operatorHasWorkAt(%v, bin=%d, %d) = %v, want %v",
				c.name, c.wantsEmpty, c.binID, c.uop, got, c.want)
		}
	}
}

// itoaLocal keeps the tripwire free of an strconv import for one call.
func itoaLocal(n int) string {
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
