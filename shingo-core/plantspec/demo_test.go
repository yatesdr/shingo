package plantspec

import "testing"

// The shipped demo plant must always load + validate — guards plants/demo.yaml
// against drift (a dangling ref or missing staging would break `make dev-seed`).
func TestShippedDemoPlantValid(t *testing.T) {
	p, err := Load("../../plants/demo.yaml")
	if err != nil {
		t.Fatalf("load plants/demo.yaml: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("plants/demo.yaml invalid: %v", err)
	}
}

// The demo plant must seed both shared loader TYPES so each is exercisable end to end:
// MULTI-WINDOW (a shared_window loader keyed on a synthetic id, ≥2 windows) and SINGLE-
// WINDOW (a shared_window loader anchored at its own real node — the demo has ≥1). The
// dedicated-positions loader is covered by the stable seed fixture
// (cmd/seeddev/testdata/seed-fixture.yaml), not the demo. Guards the demo against losing
// loader coverage. Assertions are SHAPE-based (each type present), not count-based, so
// tuning the demo (window counts, adding/dropping a leg) doesn't trip it.
func TestShippedDemoPlantLoaderTypes(t *testing.T) {
	p, err := Load("../../plants/demo.yaml")
	if err != nil {
		t.Fatalf("load plants/demo.yaml: %v", err)
	}

	byNode := map[string]Claim{}
	for _, c := range p.Claims {
		byNode[c.CoreNode] = c
	}

	// MULTI-WINDOW: a synthetic shared_window loader with ≥2 window_of claims.
	var multiLoader string
	windows := 0
	for _, c := range p.Claims {
		if c.WindowOf != "" {
			multiLoader = c.WindowOf
			windows++
		}
	}
	if windows < 2 {
		t.Fatalf("multi-window loader %q: want ≥2 windows, got %d", multiLoader, windows)
	}

	// SINGLE-WINDOW: ≥1 manual_swap produce loader that is its own anchor (no window_of /
	// home_of). The demo may carry several (one per single-payload component leg); assert
	// at least one exists.
	var singles []Claim
	for _, c := range p.Claims {
		if c.IsManualSwap() && c.Role == "produce" && c.WindowOf == "" && c.HomeOf == "" {
			singles = append(singles, c)
		}
	}
	if len(singles) < 1 {
		t.Fatalf("single-window loaders: want ≥1 (a manual_swap produce loader with no window_of/home_of), got %d", len(singles))
	}
}

// THE FIXTURE MUST KEEP BOTH TOOLING SHAPES REACHABLE.
//
// This is the guard the N1 defect went uncaught for want of. The staged tooling
// evacuation shipped, was configured on the demo press, and never executed —
// because the only marked press's changeover was ALSO a different-bin-type one,
// and the bin-type fan-out disqualified the tooling pass before it ran. The
// feature looked configured and did nothing, on the fixture written to exercise
// it, and no test could tell.
//
// So the demo now pins the two shapes deliberately, and this asserts the
// CONFIGURATION that makes each reachable:
//
//   - PRESS-1-RUN -> PRESS-1-ALT: same nodes, DIFFERENT bin type. Keeping the
//     bin types different is the point — that combination is exactly what used
//     to be unreachable, so equalizing them to "make it work" would delete the
//     coverage.
//   - PRESS-1-RUN -> PRESS-1-MOVED: same press, DISJOINT nodes. The marked
//     seats leave the style entirely and the new seats arrive empty.
//
// Shape-based, not value-based: it does not care WHICH bin types or WHICH
// nodes, only that the relationships hold.
func TestShippedDemoPlantKeepsBothToolingShapesReachable(t *testing.T) {
	p, err := Load("../../plants/demo.yaml")
	if err != nil {
		t.Fatalf("load plants/demo.yaml: %v", err)
	}

	binTypeOf := map[string]string{}
	for _, pl := range p.Payloads {
		binTypeOf[pl.Code] = pl.BinType
	}
	claimFor := func(style string) *Claim {
		for i := range p.Claims {
			if p.Claims[i].Style == style {
				return &p.Claims[i]
			}
		}
		return nil
	}

	run := claimFor("PRESS-1-RUN")
	if run == nil {
		t.Fatal("no PRESS-1-RUN claim")
	}
	if len(run.ChangeoverEvacSeats) == 0 {
		t.Error("PRESS-1-RUN marks no seats — the outgoing claim owns the tooling decision, " +
			"and with no marks NEITHER shape is a tooling changeover any more")
	}
	// THE DEFAULT PATH IS THE ONE THAT MUST SHIP EXERCISED. Clearance is normal
	// routing; changeover_evac_destination is an optional override. If the only
	// marked press in the fixture set the override, every sim run would exercise
	// the exception and nothing would exercise what a plant actually gets.
	if run.ChangeoverEvacDestination != "" {
		t.Errorf("PRESS-1-RUN sets changeover_evac_destination=%q.\n"+
			"Leave it empty: the marked seats are cleared by NORMAL ROUTING, and this is the "+
			"scenario that covers the default. The override has its own claim — see the "+
			"single-override assertion below.", run.ChangeoverEvacDestination)
	}

	// ...and the override must ship exercised too, by exactly one claim, and it
	// must name a node GROUP. A one-slot station is what the old "tooling bay"
	// was, and a two-seat press sends two bins at it.
	var overriding []*Claim
	for i := range p.Claims {
		if p.Claims[i].ChangeoverEvacDestination != "" {
			overriding = append(overriding, &p.Claims[i])
		}
	}
	if len(overriding) != 1 {
		t.Fatalf("want exactly one claim demonstrating the clearance override, found %d.\n"+
			"None means the override ships unexercised — which is how N1 hid in the first "+
			"place; more than one and the default path starts losing coverage.", len(overriding))
	}
	dest := overriding[0].ChangeoverEvacDestination
	isGroup := false
	for _, z := range p.Zones {
		if z.Name == dest {
			isGroup = true
			break
		}
	}
	if !isGroup {
		t.Errorf("the clearance override names %q, which is not a node group in this plant.\n"+
			"Point it at a group: an override is an ordinary destination and must get ordinary "+
			"capacity behaviour. The single-station version of this is what left robots dwelling "+
			"on an occupied bay holding bins nothing would take away.", dest)
	}
	if len(overriding[0].ChangeoverEvacSeats) == 0 {
		t.Errorf("claim on %s names a clearance override but marks no seats — the override is "+
			"only ever read for a marked seat, so nothing would exercise it",
			overriding[0].CoreNode)
	}

	// Shape 1: same node, different bin type.
	alt := claimFor("PRESS-1-ALT")
	if alt == nil {
		t.Fatal("no PRESS-1-ALT claim")
	}
	if alt.InboundStaging == "" {
		t.Error("PRESS-1-ALT names no inbound_staging — the changeover cannot even arm")
	}
	if alt.CoreNode != run.CoreNode {
		t.Errorf("PRESS-1-ALT is on %s and PRESS-1-RUN on %s — shape 1 is the SAME-node case",
			alt.CoreNode, run.CoreNode)
	}
	if a, b := binTypeOf[run.Payload], binTypeOf[alt.Payload]; a == b {
		t.Errorf("PRESS-1-RUN (%s) and PRESS-1-ALT (%s) now ride the SAME bin type %q.\n"+
			"Do not equalize them. Marked-AND-different-bin-type is the combination that was "+
			"silently unreachable (N1); making the bin types match is how you delete the only "+
			"organic coverage of it.", run.Payload, alt.Payload, a)
	}

	// Shape 2: same press, disjoint nodes.
	moved := claimFor("PRESS-1-MOVED")
	if moved == nil {
		t.Fatal("no PRESS-1-MOVED claim — the disjoint-node tooling shape has no fixture")
	}
	if moved.InboundStaging == "" {
		t.Error("PRESS-1-MOVED names no inbound_staging — its Adds would deliver into a cell " +
			"mid tool-change with no hold")
	}
	outgoing := map[string]bool{run.CoreNode: true, run.PairedCoreNode: true}
	for _, n := range []string{moved.CoreNode, moved.PairedCoreNode} {
		if n != "" && outgoing[n] {
			t.Errorf("PRESS-1-MOVED shares node %s with PRESS-1-RUN — shape 2 is the DISJOINT "+
				"case, and a shared node turns it back into one the old fan-out could already see", n)
		}
	}
}
