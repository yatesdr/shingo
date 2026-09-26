//go:build docker

package dispatch

import (
	"regexp"
	"strings"
	"sync"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/dispatch/plan"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// THE VACATED-SLOT RULE WRITES NOTHING, on the pair path. The planner golden's
// zero-write test (cmd/seeddev) judges every golden case, and no golden case is
// a pair whose second leg is handed its partner's lift, so this runs P4 — a
// two-robot pair into its own full market, the evac granted the child its
// supply empties (case b) — against a statement-counting store and judges every
// statement issued inside a planner call. The planner's entry and exit come
// through plan.SetObserver, keyed by the store, as the golden's do.
//
// Not parallel: the observer is process-wide, and keying it by store keeps
// other tests' calls out of this book, but a parallel neighbour would still pay
// for the observer it did not ask for.

var vzwWrite = regexp.MustCompile(`(?i)\b(INSERT\s+INTO|UPDATE\s+[\w.]+(\s+\w+)?\s+SET|DELETE\s+FROM|TRUNCATE|MERGE\s+INTO)\b|\bFOR\s+(UPDATE|SHARE|NO\s+KEY\s+UPDATE|KEY\s+SHARE)\b|\bpg_(try_)?advisory`)

func vzwIsRead(stmt string) bool {
	s := strings.ToUpper(strings.TrimSpace(stmt))
	return (strings.HasPrefix(s, "SELECT") || strings.HasPrefix(s, "WITH")) && !vzwWrite.MatchString(stmt)
}

func TestVacatedSlot_ZeroWrite_PairPath(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	sd := testdb.SetupStandardData(t, db)
	grp := vsGroup(t, db, "VZW-MKT")
	c1, c2 := vsChildOf(t, db, grp, "VZW-C1"), vsChildOf(t, db, grp, "VZW-C2")
	stage := prNode(t, db, "VZW-STAGE")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, c1.ID, "VZW-FRESH")
	makeEmptyBin(t, db, c2.ID, "VZW-OTHER")
	prResident(t, db, sd.LineNode, sd.BinType.ID, sd.Payload.Code, "VZW-RESIDENT")

	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "open counting store")
	t.Cleanup(func() { cdb.Close() })
	d, _ := newTestDispatcherWithResolver(t, cdb)

	var (
		mu         sync.Mutex
		depth      int
		start      int
		parts      = map[string]int{}
		judged     int
		violations []string
	)
	plan.SetObserver(func(r any, part string, enter bool) {
		if r != plan.Reader(cdb) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if enter {
			if depth == 0 {
				start = len(counter.Statements())
			}
			depth++
			parts[part]++
			return
		}
		depth--
		if depth == 0 {
			for _, s := range counter.Statements()[start:] {
				judged++
				if !vzwIsRead(s) {
					violations = append(violations, part+": "+strings.Join(strings.Fields(s), " "))
				}
			}
		}
	})
	t.Cleanup(func() { plan.SetObserver(nil) })

	vsSubmitPair(d, "vzw", sd.Payload.Code, sd.LineNode.Name, stage.Name, grp.Name, grp.Name, false)
	prScanPass(t, d, cdb, "vzw-return", "vzw-supply")

	supply, evac := prReloadUUID(t, db, "vzw-supply"), prReloadUUID(t, db, "vzw-return")
	if evac.DeliveryNode != c1.Name {
		t.Fatalf("fixture: evac lands %s (%s, %q), want %s — the pair path did not run the partner grant",
			evac.DeliveryNode, evac.Status, evac.QueueCause, c1.Name)
	}
	steps, _ := decodeSteps(evac.StepsJSON)
	granted := false
	for _, s := range steps {
		if st := plan.PartnerStamp(s); st != nil && st.Partner == supply.ID {
			granted = true
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !granted || parts["vacated-group"] == 0 {
		t.Fatalf("fixture: no partner grant through the planner (vacated-group calls %d, granted %v) — "+
			"the pair's second leg was not handed its partner", parts["vacated-group"], granted)
	}
	t.Logf("planner calls on the pair path: %v; %d statements judged", parts, judged)
	if judged == 0 {
		t.Fatal("no statement was judged inside a planner call, so the assertion below is vacuous")
	}
	for _, v := range violations {
		t.Errorf("a planner call wrote: %s", v)
	}
}
