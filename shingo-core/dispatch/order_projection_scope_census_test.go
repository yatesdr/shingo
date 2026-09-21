package dispatch

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// order_projection_scope_census_test.go — which orders can reach an Edge's
// orders table, and whether a compound child is one of them.
//
// WHY THIS IS WORTH A TEST. The Edge stores a projected row with whatever
// origin_id the projection carries, and compound.go copies the parent's
// origin_id AND its class onto every child it writes. So if a child can be
// projected, an Edge orders row carrying an origin_id is not necessarily the
// order that episode was opened for — it may be a dig leg created in service of
// one. Any reader that counts Edge rows per episode, or treats "has an origin"
// as "is a root", has to know which it is, and that cannot be recovered after
// the fact: the wire type carries no parent marker (protocol.OrderProjection has
// fourteen fields and none of them is parent_order_id), so the Edge cannot tell
// a leg from a root by looking at the row.
//
// THERE ARE TWO DOORS, and they answer differently. The census below names both
// and pins the answer each gives, because the difference is the whole point:
//
//   - THE PUSH, dispatch/lifecycle_service.go. Its scope is a function —
//     admitOrder — and compound children do not route through it. No child is
//     ever pushed.
//
//   - THE RECONCILE, messaging/core_data_service.go. Its scope is a QUERY:
//     ListActiveOrdersByStation, which filters on station_id and status and
//     nothing else. A child inherits its parent's station_id (compound.go), the
//     Edge has never had a row for it and so never names it, and the reconcile
//     therefore sends it down. checkOwnership's comment in dispatcher.go already
//     records this — "legs are projected down and Edge creates rows for them" —
//     and the comment is correct.
//
// So "compound children are never projected" is TRUE of the push and FALSE of
// the reconcile, and this census exists so that stays a stated fact rather than
// something rediscovered by whoever next reasons about the Edge's rows. Nothing
// here changes the reconcile: a parent filter on that query would take legs off
// the Edge board, which is a behaviour change about what an operator sees, not
// a bookkeeping tidy-up.

// projectionDoor is one production site that turns a Core order row into a
// protocol.OrderProjection bound for an Edge.
type projectionDoor struct {
	site          string // module-root-relative path, the call's own file
	name          string // what the door IS
	carriesChild  bool   // can a compound child (ParentOrderID != nil) go through it
	whyThatAnswer string
}

// projectionDoors is the whole list. A site not on it fails the census.
var projectionDoors = []projectionDoor{
	{
		site:         "dispatch/lifecycle_service.go",
		name:         "the push — an order Core just admitted, projected to the station that owns it",
		carriesChild: false,
		whyThatAnswer: "its scope is admitOrder, and CreateCompoundChildren does not route through admitOrder; " +
			"see projectOrder's doc comment, which ties the scope to the function on purpose",
	},
	{
		site:         "messaging/core_data_service.go",
		name:         "the reconcile — every active order for the asking station that the Edge did not name",
		carriesChild: true,
		whyThatAnswer: "ListActiveOrdersByStation filters on station_id and status only; a child inherits its " +
			"parent's station_id and the Edge never names a row it has never had",
	},
}

// projectionSites returns every production line that builds a projection, as
// "path:line". The declaration of ProjectionFor itself is not a site.
func projectionSites(sources map[string]string) []string {
	var out []string
	for path, src := range sources {
		for i, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.HasPrefix(trimmed, "func ProjectionFor(") {
				continue
			}
			if strings.Contains(line, "ProjectionFor(") {
				out = append(out, path+":"+strconv.Itoa(i+1))
			}
		}
	}
	slices.Sort(out)
	return out
}

// projectOrderSites returns every production line that calls the push helper.
// Its own declaration is not a call site.
func projectOrderSites(sources map[string]string) []string {
	var out []string
	for path, src := range sources {
		for i, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.HasPrefix(trimmed, "func (s *LifecycleService) projectOrder(") {
				continue
			}
			if strings.Contains(line, "projectOrder(") {
				out = append(out, path+":"+strconv.Itoa(i+1))
			}
		}
	}
	slices.Sort(out)
	return out
}

// fileOf strips the ":line" a site carries.
func fileOf(site string) string {
	if i := strings.LastIndex(site, ":"); i >= 0 {
		return site[:i]
	}
	return site
}

// TestCensus_OrderProjectionDoors pins that every way an order reaches an Edge's
// orders table is on the list above, with its child answer recorded.
func TestCensus_OrderProjectionDoors(t *testing.T) {
	t.Parallel()
	named := map[string]bool{}
	for _, d := range projectionDoors {
		named[d.site] = true
	}

	sites := projectionSites(scanCoreSources(t))
	if len(sites) == 0 {
		t.Fatal("no projection sites found at all; the scanner is broken, not the code")
	}

	var unnamed []string
	seen := map[string]bool{}
	for _, s := range sites {
		f := fileOf(s)
		seen[f] = true
		if !named[f] {
			unnamed = append(unnamed, s)
		}
	}
	if len(unnamed) > 0 {
		t.Errorf(`projection site(s) not on the door list: %v

A new way for a Core order to reach an Edge's orders table has opened. Add it to
projectionDoors WITH its answer to "can a compound child go through this", and
say why. The answer is what a reader of Edge rows needs and cannot recover
afterwards — the wire type carries no parent marker.`, unnamed)
	}
	for _, d := range projectionDoors {
		if !seen[d.site] {
			t.Errorf("door %q (%s) no longer builds a projection; delete it from the list or fix the scanner", d.site, d.name)
		}
	}
}

// TestCompoundChildren_AreNeverPushed is the half of the invariant that holds,
// and the half S3's Edge close is entitled to rely on for the PUSH path.
//
// The guard is structural rather than a check: projectOrder is called from
// exactly two places, both of them admitted orders, and compound.go is neither.
// projectOrder itself would happily project a child — a child carries its
// parent's station_id, so the StationID != "" guard inside it does not stop one
// — which is exactly why the call-site set is the thing worth pinning.
func TestCompoundChildren_AreNeverPushed(t *testing.T) {
	t.Parallel()
	want := []string{
		"dispatch/lifecycle_service.go:161", // the Edge wire intake's own echo
		"dispatch/loader_replenish.go:479",  // AdmitCoreAsk: both Core timer doors
	}
	got := projectOrderSites(scanCoreSources(t))
	if !slices.Equal(got, want) {
		t.Errorf(`projectOrder call sites = %v, want %v.

Every caller must be a door that admits a ROOT order. A call added anywhere that
writes child rows — dispatch/compound.go above all — would put legs on an Edge
board carrying their parent's origin_id, and nothing downstream could tell them
from the order the episode was opened for.`, got, want)
	}
	for _, s := range got {
		if fileOf(s) == "dispatch/compound.go" {
			t.Errorf("dispatch/compound.go pushes a projection at %s; compound children must not be pushed", s)
		}
	}
}

// TestCensus_ProjectionScannerCatchesAPlantedCall demonstrates the census can
// fail. A census that has only ever been run against the code it describes
// cannot show it would notice anything; this runs both scanners over a synthetic
// tree with the call each is looking for planted in a compound-shaped file.
func TestCensus_ProjectionScannerCatchesAPlantedCall(t *testing.T) {
	t.Parallel()
	synthetic := map[string]string{
		"dispatch/compound.go": "package dispatch\n" +
			"// d.lifecycle.projectOrder(child) — a comment must not count\n" +
			"func write() {\n\td.lifecycle.projectOrder(child)\n}\n",
		"messaging/other.go": "package messaging\n\tout = append(out, dispatch.ProjectionFor(o))\n",
	}

	calls := projectOrderSites(synthetic)
	if !slices.Equal(calls, []string{"dispatch/compound.go:4"}) {
		t.Errorf("projectOrder scanner found %v, want the planted call at dispatch/compound.go:4 and nothing from the comment", calls)
	}
	builds := projectionSites(synthetic)
	if !slices.Equal(builds, []string{"messaging/other.go:2"}) {
		t.Errorf("projection scanner found %v, want the planted build at messaging/other.go:2", builds)
	}
}
