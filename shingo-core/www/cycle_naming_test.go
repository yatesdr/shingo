package www

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// cycle_naming_test.go — "cycle" names ONE measurement in this codebase.
//
// ── THE PROBLEM, AND WHERE IT ACTUALLY WAS ───────────────────────────────────
//
// Round 3 shipped /cycle-time (5.10) and flagged a risk that a second Phase 6
// surface would land at /cycle-* and put two grains behind one word — the thing
// Agent C refused to worsen when it declined /demand. That risk did not
// materialise: 5.12 landed at /demand-episodes/{originID}.
//
// THE COLLISION WAS ALREADY THERE, with an endpoint nobody in round 3 looked at.
// /api/parts/cycle-time averaged mission_telemetry.duration_ms — how long a
// ROBOT took to carry a payload, attributed to every part in that payload's
// manifest. Unit: one order's journey. /cycle-time reads consecutive PLC ticks
// out of bin_uop_audit. Unit: one part crossing one station. Same word, two
// grains, two tables, no shared key — and the misnamed one was the pre-existing
// endpoint, not the new page. So the page kept the word and the endpoint became
// /api/parts/mission-duration.
//
// ── WHY THIS TEST AND NOT JUST A COMMENT ─────────────────────────────────────
//
// Because the way this regresses is somebody adding a route. "cycle" is the
// obvious word for anything periodic, so the next duration, the next takt
// surface and the next per-order timing endpoint all reach for it, and each one
// individually looks harmless. The rule is checkable, so it is checked.
//
// ASSERTED AGAINST router.go's SOURCE rather than a live chi router: chi exposes
// its route tree only through Walk on a built router, and building one needs a
// full engine. The source is where a route is added, which is where the rule
// needs to bite.
func TestCycleNamesOneMeasurement(t *testing.T) {
	src, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatalf("read router.go: %v", err)
	}

	// r.Get("/path", ...), r.Post(...), and so on — path literals only.
	routePat := regexp.MustCompile(`r\.(?:Get|Post|Put|Patch|Delete|Head|Handle)\("([^"]+)"`)
	var cycleRoutes []string
	for _, m := range routePat.FindAllStringSubmatch(string(src), -1) {
		if strings.Contains(strings.ToLower(m[1]), "cycle") {
			cycleRoutes = append(cycleRoutes, m[1])
		}
	}

	// EXACTLY ONE, and it is named here so the failure says which one is new.
	const theOne = "/cycle-time"
	if len(cycleRoutes) != 1 || cycleRoutes[0] != theOne {
		t.Errorf("routes matching \"cycle\" = %v, want exactly [%s].\n\n"+
			"\"Cycle time\" in this codebase means ONE measurement: the interval between "+
			"consecutive units at a station, from consecutive PLC ticks in bin_uop_audit. "+
			"It does not mean a mission duration, an order's end-to-end time, or a takt — "+
			"/api/parts/cycle-time used to mean the first of those and was renamed to "+
			"/api/parts/mission-duration for exactly this reason.\n\n"+
			"If the new route measures the same thing at a different grain, put the grain "+
			"in the path (/cycle-time/{station}). If it measures something else, name it "+
			"after what it measures and update this test with the reason.",
			cycleRoutes, theOne)
	}

	// The old spelling must not come back, in a route or as a Go symbol on the
	// parts path. A reinstated alias is the same defect wearing a compatibility
	// argument: two live names for one endpoint is what makes the word ambiguous
	// again, and it would be permanent.
	if strings.Contains(string(src), "/parts/cycle-time") {
		t.Error("router.go registers /parts/cycle-time again. That endpoint averages " +
			"mission_telemetry.duration_ms — a carrying mission's duration, not a cycle " +
			"time. It is /parts/mission-duration now, and an alias would restore the " +
			"ambiguity rather than ease a migration.")
	}
}

// TestPartsDurationSymbolsDoNotSayCycle holds the same rule one layer down.
//
// The route could be right while the handler, service method and store type all
// still said Cycle — which is how the name would leak back the next time
// somebody reads the code to find out what the endpoint measures and names their
// new route after what they found.
func TestPartsDurationSymbolsDoNotSayCycle(t *testing.T) {
	src, err := os.ReadFile("handlers_parts.go")
	if err != nil {
		t.Fatalf("read handlers_parts.go: %v", err)
	}
	// stripGoComments so the explanation of the rename does not fail the rule it
	// explains — the same trap TestPhase6PagesUseNoChips documents for templates.
	if code := stripGoComments(string(src)); strings.Contains(code, "CycleTime") {
		t.Error("handlers_parts.go still names a CycleTime symbol. The parts endpoints " +
			"measure carrying-mission durations (mission_telemetry.duration_ms); cycle " +
			"time is /cycle-time, off bin_uop_audit. Two names for two things, not one " +
			"name for both.")
	}
}

// stripGoComments removes // line comments and /* */ blocks.
func stripGoComments(s string) string {
	s = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(s, "")
	return regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(s, "")
}
