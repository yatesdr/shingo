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

func knownNodes(names ...string) ClaimNodeContext {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	return ClaimNodeContext{Checked: true, KnownCoreNodes: set}
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
	findings := ValidateNodeClaim(in, knownNodes("PRESS_A", "AISLE_1", "STG_A", "STG_B", "MARKET"))
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

// ── AND IT MUST KNOW WHETHER IT HAD THE INPUT TO CHECK ────────────────────
//
// An empty node set means Core has not been heard from — a fresh Edge, a
// restart, a Kafka gap. Refusing every route on that basis would brick setup
// exactly when someone is most likely to be doing it, and absence of data must
// never render as a finding.
func TestValidateNodeClaim_KeyRouteSkipsWhenCoreIsUnknown(t *testing.T) {
	t.Parallel()
	in := keyRouteClaim([]string{"ANYTHING", "AT_ALL"}, "")
	for _, tc := range []struct {
		name string
		ctx  ClaimNodeContext
	}{
		{"never looked", ClaimNodeContext{}},
		{"looked, list empty", ClaimNodeContext{Checked: true, KnownCoreNodes: map[string]bool{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := findingsFor(ValidateNodeClaim(in, tc.ctx), "key_route"); len(got) != 0 {
				t.Errorf("no node list means no evidence, so no finding; got %v", got)
			}
		})
	}
}

// Group children arrive as "Group.CHILD" and the runtime keys on the bare
// name, so a bare child name resolves — the same fallback the process-node
// guard uses. Without it, every legitimate group-member route would be refused.
func TestValidateNodeClaim_KeyRouteAcceptsBareGroupChild(t *testing.T) {
	t.Parallel()
	in := keyRouteClaim([]string{"L1"}, "")
	if got := findingsFor(ValidateNodeClaim(in, knownNodes("SMG_01.L1")), "key_route"); len(got) != 0 {
		t.Errorf("a bare group-child name resolves; got %v", got)
	}
}

func TestValidateNodeClaim_KeyRouteRejections(t *testing.T) {
	t.Parallel()
	known := knownNodes("PRESS_A", "AISLE_1", "AISLE_2", "STG_A", "STG_B", "MARKET")
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
		// Not a vendor rule — a repeat is how a mis-click renders.
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
	if got := findingsFor(ValidateNodeClaim(in, knownNodes("AISLE_1")), "key_route"); len(got) == 0 {
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
			findings := ValidateNodeClaim(tc.in, knownNodes("PRESS_A", "AISLE_1", "AISLE_2", "STG_A", "STG_B", "MARKET"))
			if got := append(findingsFor(findings, "key_route"), findingsFor(findings, "key_task")...); len(got) != 0 {
				t.Errorf("want silence; got %v", got)
			}
		})
	}
}
