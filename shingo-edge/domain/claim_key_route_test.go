package domain

import (
	"strings"
	"testing"

	"shingo/protocol"
)

func keyRouteClaim(route []string, task string) NodeClaimInput {
	return NodeClaimInput{
		StyleID:             1,
		CoreNodeName:        "PRESS_A",
		Role:                protocol.ClaimRoleProduce,
		SwapMode:            protocol.SwapModeTwoRobot,
		PayloadCode:         "WIDGET-A",
		InboundStaging:      "STG_A",
		OutboundStaging:     "STG_B",
		InboundSource:       "MARKET",
		OutboundDestination: "MARKET",
		KeyRoute:            &route,
		KeyTask:             &task,
	}
}

// mappedPoints is the context a key route is validated against: the VENDOR
// MAP's point set, not Core's node list.
//
// Shingo works in APs, so the node list is only the subset of map points Shingo
// gave a job to — and a corridor waypoint, the feature's primary use, is not in
// it. These tests used to build KnownCoreNodes, which is precisely why they were
// green while the check refused correct routes.
func mappedPoints(names ...string) ClaimNodeContext {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	return ClaimNodeContext{Checked: true, KnownScenePoints: set}
}

// theUsualMap is the fixture most cases want: the claim's own nodes plus a
// couple of aisle points, all mapped.
func theUsualMap() ClaimNodeContext {
	return mappedPoints("PRESS_A", "AISLE_1", "AISLE_2", "STG_A", "STG_B", "MARKET")
}

func findingsFor(findings []FieldError, field string) []string {
	var out []string
	for _, f := range findings {
		if f.Field == field {
			out = append(out, f.Severity+": "+f.Message)
		}
	}
	return out
}

// A ROUTE POINT THAT DOES NOT RESOLVE IS NOT A SOFT FAILURE. Per the vendor,
// a keyRoute point that does not exist or is unreachable terminates the
// robot's waybill the moment the order is issued. Stored quietly it becomes an
// order that dies at dispatch with nothing on the Edge side to explain it.
func TestValidateNodeClaim_KeyRoutePointMustResolve(t *testing.T) {
	t.Parallel()
	in := keyRouteClaim([]string{"AISLE_1", "NOPE"}, "")
	findings := ValidateNodeClaim(in, theUsualMap())
	got := findingsFor(findings, "key_route")
	if len(got) != 1 {
		t.Fatalf("want exactly one key_route finding (the unresolvable point); got %v", got)
	}
	if !strings.Contains(got[0], "NOPE") {
		t.Errorf("the finding must name the point that failed; got %q", got[0])
	}
	if !HasErrors(findings) {
		t.Error("an unresolvable point must REFUSE the save — a warning here is a robot that stops on issue")
	}
}

// THE POINT OF THE WHOLE UNIT: a plain waypoint saves.
//
// "Go via the north aisle" is the primary use of a key route. A waypoint is not
// a node, has never been a node, and never will be — Shingo gives nodes jobs
// and nobody gives a corridor a job. Validated against Core's node list it was
// refused with a sentence that read like a configuration error, which is the
// worst kind of wrong: confident and unactionable.
func TestValidateNodeClaim_KeyRouteAcceptsAPlainWaypoint(t *testing.T) {
	t.Parallel()
	in := keyRouteClaim([]string{"WP_AISLE_N"}, "")
	ctx := mappedPoints("PRESS_A", "WP_AISLE_N", "STG_A", "STG_B", "MARKET")
	// Core's node list does NOT contain the waypoint, and must not need to.
	ctx.KnownCoreNodes = map[string]bool{
		"PRESS_A": true, "STG_A": true, "STG_B": true, "MARKET": true,
	}
	findings := ValidateNodeClaim(in, ctx)
	if got := findingsFor(findings, "key_route"); len(got) != 0 {
		t.Errorf("a mapped waypoint is a valid route point; got %v", got)
	}
	if HasErrors(findings) {
		t.Error("refusing a real map point is the defect this unit exists to fix")
	}
}

// ── AND IT MUST KNOW WHETHER IT HAD THE INPUT TO CHECK ────────────────────
//
// An empty point set means the map has not been received — a fresh Edge, a
// restart, a Kafka gap, or a Core that predates the scene sync. Refusing every
// route on that basis would brick setup exactly when someone is most likely to
// be doing it.
//
// SAVE WITH A WARNING: not a refusal, and not silence either. This is the
// CheckLocationTasks posture — the write lands and says out loud that nobody
// checked it. Silence would be indistinguishable from a route that WAS
// verified, which is the one thing an engineer would want to know.
func TestValidateNodeClaim_KeyRouteDegradesWhenTheMapIsUnknown(t *testing.T) {
	t.Parallel()
	in := keyRouteClaim([]string{"ANYTHING", "AT_ALL"}, "")
	for _, tc := range []struct {
		name string
		ctx  ClaimNodeContext
	}{
		{"never looked", ClaimNodeContext{}},
		{"looked, map empty", ClaimNodeContext{Checked: true, KnownScenePoints: map[string]bool{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			findings := ValidateNodeClaim(in, tc.ctx)
			got := findingsFor(findings, "key_route")
			if len(got) != 2 {
				t.Fatalf("want one unverified warning per point; got %v", got)
			}
			for _, f := range got {
				if !strings.HasPrefix(f, SeverityWarning+":") {
					t.Errorf("finding %q must be a warning — the save proceeds", f)
				}
			}
			if HasErrors(findings) {
				t.Error("a map we could not read must not refuse the save")
			}
		})
	}
}

// ── THE MATCH IS EXACT, AND THE LOOSENING IS GONE ────────────────────────
//
// The node-list check matched after the last dot too, so a bare child name
// resolved against "Group.CHILD" — the same fallback the process-node guard
// uses, and correct there, because Edge keys on bare child names.
//
// It is wrong for map points. A key route is handed to the fleet verbatim and
// the scene stores instance names as SEER holds them, so a suffix match made
// "001" resolve against "SMN.001": loose and narrow at once. It accepted a name
// the fleet will not recognise while still refusing the waypoints the feature
// exists for.
func TestValidateNodeClaim_KeyRouteMatchesMapPointsExactly(t *testing.T) {
	t.Parallel()
	in := keyRouteClaim([]string{"L1"}, "")
	if got := findingsFor(ValidateNodeClaim(in, mappedPoints("SMG_01.L1")), "key_route"); len(got) != 1 {
		t.Fatalf("a suffix of a mapped point is not that point; got %v", got)
	}
	exact := keyRouteClaim([]string{"SMG_01.L1"}, "")
	if got := findingsFor(ValidateNodeClaim(exact, mappedPoints("SMG_01.L1")), "key_route"); len(got) != 0 {
		t.Errorf("the point's own instance name must resolve; got %v", got)
	}
}

func TestValidateNodeClaim_KeyRouteRejections(t *testing.T) {
	t.Parallel()
	known := theUsualMap()
	for _, tc := range []struct {
		name  string
		in    NodeClaimInput
		field string
		want  string
	}{
		// The robot's own current position. The vendor forbids it in keyRoute
		// specifically, and it is the one value an operator might reasonably
		// type meaning "start where you are".
		// Asserted on the SELF_POSITION sentence, not on the point's name: the
		// resolution check below would also quote "SELF_POSITION" back, so a
		// name-only assertion passes whether or not this rule exists.
		{"self position", keyRouteClaim([]string{"SELF_POSITION"}, ""), "key_route", "never valid in a key route"},
		// Not a vendor rule — a repeat is how a mis-click renders. Kept as a
		// refusal: keyRoute constrains the path between a source and a
		// destination, so a revisit is a routing RESULT, not an input.
		{"duplicate point", keyRouteClaim([]string{"AISLE_1", "AISLE_1"}, ""), "key_route", "more than once"},
		{"blank point", keyRouteClaim([]string{"AISLE_1", "  "}, ""), "key_route", "blank"},
		// SEER silently ignores anything else, which is worse than being told.
		{"bad key task", keyRouteClaim(nil, "sideways"), "key_task", "load"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			findings := ValidateNodeClaim(tc.in, known)
			got := findingsFor(findings, tc.field)
			if len(got) == 0 {
				t.Fatalf("want a %s finding; got none", tc.field)
			}
			if !strings.Contains(got[0], tc.want) {
				t.Errorf("finding %q does not mention %q", got[0], tc.want)
			}
			if !HasErrors(findings) {
				t.Error("must refuse, not warn")
			}
		})
	}
}

// A manual_swap loader does not drive, so it has nothing to route. The editor
// hides the fieldset for it; this is the same rule on the server, where the
// HTTP API and an import also arrive.
func TestValidateNodeClaim_KeyRouteRefusedForManualSwap(t *testing.T) {
	t.Parallel()
	in := keyRouteClaim([]string{"AISLE_1"}, "")
	in.SwapMode = protocol.SwapModeManualSwap
	in.InboundStaging, in.OutboundStaging = "", ""
	if got := findingsFor(ValidateNodeClaim(in, mappedPoints("AISLE_1")), "key_route"); len(got) == 0 {
		t.Error("a loader has no route to configure")
	}
}

// THE ORDINARY CASE MUST STAY SILENT. Every claim in the plant today has no
// route at all, and a validator that found something to say about them would
// make the whole editor unusable.
func TestValidateNodeClaim_NoRouteIsSilent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   NodeClaimInput
	}{
		{"nil route, no task", keyRouteClaim(nil, "")},
		{"empty route", keyRouteClaim([]string{}, "")},
		{"a valid route", keyRouteClaim([]string{"AISLE_1", "AISLE_2"}, "load")},
		{"unload", keyRouteClaim(nil, "unload")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			findings := ValidateNodeClaim(tc.in, theUsualMap())
			if got := append(findingsFor(findings, "key_route"), findingsFor(findings, "key_task")...); len(got) != 0 {
				t.Errorf("want silence; got %v", got)
			}
		})
	}
}
