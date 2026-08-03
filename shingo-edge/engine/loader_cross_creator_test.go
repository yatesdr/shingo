package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
)

// TestCrossCreator_UnloaderBudget_OperatorAndAutomatic is the never-2N property
// stated across CREATORS rather than within one. Two independent paths can order
// a full into the same unloader window — the operator's RequestFullBin button and
// the automatic createUnloaderFullInViaSeam — and the invariant is a property of
// the window, not of either caller. Whichever fires first, the second must find
// the budget spent.
//
// The automatic_then_operator row is the one that was red before RequestFullBin
// was routed through the reservation seam: it created its order directly, with no
// in-flight count, so an operator tap while a U1 was already inbound produced two
// fulls for a one-bin window. The reverse row already passed, because the
// automatic path has always counted in-flight orders and would see the
// operator's.
func TestCrossCreator_UnloaderBudget_OperatorAndAutomatic(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		first, second string
	}{
		{name: "automatic_then_operator", first: "automatic", second: "operator"},
		{name: "operator_then_automatic", first: "operator", second: "automatic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			eng := testEngine(t, db)
			nodeID := seedCapManualSwap(t, db, "XC-"+tc.name, "XC-ULD", protocol.ClaimRoleConsume, []string{"PART-A"}, 0, false)
			seedCoreLoader(t, eng, sharedLoaderInfo("XC-ULD", "consume", "operator", "PART-A", 0, 0))

			uld, err := eng.loaders().LoaderAt(domain.NodeID("XC-ULD"), domain.RoleConsume)
			if err != nil || uld == nil {
				t.Fatalf("resolve unloader: err=%v nil=%v", err, uld == nil)
			}

			fire := map[string]func(){
				"automatic": func() { eng.createUnloaderFullInViaSeam(uld, "PART-A") },
				"operator":  func() { _, _ = eng.RequestFullBin(nodeID, "PART-A") },
			}

			fire[tc.first]()
			if got := inFlightFulls(t, db, []string{"XC-ULD"}); got != 1 {
				t.Fatalf("%s creator made %d fulls, want 1 — the fixture is not exercising it, so the second half proves nothing", tc.first, got)
			}
			fire[tc.second]()
			if got := inFlightFulls(t, db, []string{"XC-ULD"}); got != 1 {
				t.Fatalf("%s after %s: %d in-flight fulls at a one-window unloader, want 1. "+
					"Both creators share one never-2N budget; this is a second full ordered into a window that already has one inbound.",
					tc.second, tc.first, got)
			}
		})
	}
}

// retrieveCreatorSites lists every non-test call site of the two exported
// retrieve-order constructors. Both wrap one unexported body, so counting a
// single name undercounts: fireThresholdL1 and the unloader U1 reach it through
// CreateRetrieveOrderWithOrigin, and neither appears in a census that greps only
// CreateRetrieveOrder.
func retrieveCreatorSites(t *testing.T) []string {
	t.Helper()
	// Package dir is the test's CWD, so these are stable regardless of where
	// `go test` was invoked from.
	dirs := []string{".", "../www", "../orders"}
	needles := []string{".CreateRetrieveOrder(", ".CreateRetrieveOrderWithOrigin("}
	var sites []string
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			t.Fatalf("read %s: %v", d, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			p := filepath.Join(d, e.Name())
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("read %s: %v", p, err)
			}
			for i, line := range strings.Split(string(b), "\n") {
				for _, n := range needles {
					if strings.Contains(line, n) {
						sites = append(sites, fmt.Sprintf("%s:%d", filepath.ToSlash(p), i+1))
					}
				}
			}
		}
	}
	return sites
}

// TestCensus_RetrieveOrderCreatorSites is a tripwire, not a behavior test. The
// never-2N invariant is only as good as the list of writers it covers, and that
// list has been wrong before: a comment claiming every empty-firing writer routed
// through the reservation seam survived two review rounds while three paths
// bypassed it.
//
// A change to this count means a retrieve-order creator was added or removed.
// That is not a failure — it is a prompt. Re-run the census, decide whether the
// new site belongs behind the seam, then update the expected count here and the
// scope comment on withLoaderBudget in the same commit.
func TestCensus_RetrieveOrderCreatorSites(t *testing.T) {
	t.Parallel()
	const want = 9
	sites := retrieveCreatorSites(t)
	if len(sites) != want {
		t.Errorf("retrieve-order creator sites = %d, expected %d.\nA creator was added or removed. Re-run the census and update this count WITH the seam's scope comment.\nSites:\n  %s",
			len(sites), want, strings.Join(sites, "\n  "))
	}
}
