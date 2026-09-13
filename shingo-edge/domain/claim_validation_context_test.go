package domain

import (
	"reflect"
	"sort"
	"testing"

	"shingo/protocol"
	"shingoedge/domain/flowspec"
)

// claim_validation_context_test.go — the refusal matrix does not depend on
// whether the lookups resolved.
//
// TestFlowspecMatchesValidateNodeClaim walks every (role, mode) with an EMPTY
// ClaimNodeContext — Checked false, which is what a validator gets when the
// process could not be read, Core has not been heard from, or the plant map
// has not arrived. That is the right context to prove the FIELD rules in,
// because it isolates them.
//
// It is the wrong context to prove them in ALONE. "Checked stays false on any
// lookup failure" is the rule the context exists for, and a matrix run only
// against the failure case cannot tell a rule that is about fields from one
// that quietly turns on when a lookup succeeds. A validator whose refusals
// change with the weather is one whose answer depends on whether Core was
// reachable, which is precisely the thing the Checked flag is there to stop.
//
// So the same matrix runs against a POPULATED context, and the two sets must
// be identical. The context is built FROM the claim — every node it names is
// a node the process has, every point it routes through is on the map — so it
// can only ever add information, never withhold it. A difference is either a
// field rule that needs a lookup (it should not) or a membership rule leaking
// into the field matrix (it should not).

// contextFor is the friendliest possible context for one claim: the process
// owns every node the claim names, Core knows them, and the map has every
// point of its key route.
func contextFor(in NodeClaimInput) ClaimNodeContext {
	names := []string{in.CoreNodeName, in.PairedCoreNode, in.SecondPairedCoreNode,
		in.InboundStaging, in.OutboundStaging, in.InboundSource, in.OutboundDestination}
	names = append(names, OptValue(in.ChangeoverEvacNodes)...)
	if d := OptValue(in.ChangeoverEvacDestination); d != "" {
		names = append(names, d)
	}
	core := map[string]bool{}
	for _, n := range names {
		if n != "" {
			core[n] = true
		}
	}
	points := map[string]bool{}
	for _, w := range OptValue(in.KeyRoute) {
		if w != "" {
			points[w] = true
		}
	}
	// A key route with no points still needs a non-empty point set, or the
	// validator reads it as "could not look" rather than "checked and fine".
	if len(points) == 0 {
		points["LM-ANY"] = true
	}
	for n := range core {
		points[n] = true
	}
	return ClaimNodeContext{
		Checked:          true,
		StyleProcessID:   1,
		NodeProcessIDs:   []int64{1},
		KnownCoreNodes:   core,
		KnownScenePoints: points,
	}
}

func TestValidateNodeClaim_RefusalsDoNotDependOnTheLookups(t *testing.T) {
	t.Parallel()
	for _, role := range flowspec.Roles() {
		for _, mode := range protocol.ConfigurableSwapModes() {
			for _, tc := range []struct {
				what string
				in   NodeClaimInput
			}{
				{"blank", NodeClaimInput{StyleID: 1, CoreNodeName: "NODE", Role: role, SwapMode: mode}},
				{"complete", completeClaimFor(role, mode)},
			} {
				unchecked := errorFieldSet(ValidateNodeClaim(tc.in, ClaimNodeContext{}))
				checked := errorFieldSet(ValidateNodeClaim(tc.in, contextFor(tc.in)))
				if !reflect.DeepEqual(unchecked, checked) {
					t.Errorf("%s/%s %s: refused %v with no context and %v with one.\n"+
						"  A field rule must not need a lookup, and a membership rule must not "+
						"reach the field matrix — see ClaimNodeContext.Checked.",
						role, mode, tc.what, unchecked, checked)
				}
			}
		}
	}
}

// TestValidateNodeClaim_AnUnresolvedLookupRefusesNothing is the other half of
// the same rule, stated directly: absence of data is never a finding.
//
// A claim naming a node no process has and a waypoint no map has is REFUSED
// when the lookups resolved and is not when they did not — because "this node
// is not on your process" and "I could not find out" are different sentences,
// and only one of them belongs in front of an engineer.
func TestValidateNodeClaim_AnUnresolvedLookupRefusesNothing(t *testing.T) {
	t.Parallel()
	in := completeClaimFor(protocol.ClaimRoleConsume, protocol.SwapModeSingleRobot)
	in.KeyRoute = Ptr([]string{"LM-NOT-ON-THE-MAP"})

	if got := errorFieldSet(ValidateNodeClaim(in, ClaimNodeContext{})); len(got) != 0 {
		t.Errorf("with nothing resolved the validator refused %v — absence of data is never a finding", got)
	}

	// The same claim, with a map that does not have the point: now it is a
	// refusal, and on the key_route field.
	known := ClaimNodeContext{
		Checked: true, StyleProcessID: 1, NodeProcessIDs: []int64{1},
		KnownScenePoints: map[string]bool{"LM-SOMETHING-ELSE": true},
	}
	got := errorFieldSet(ValidateNodeClaim(in, known))
	if !reflect.DeepEqual(got, []string{string(flowspec.KeyRoute)}) {
		t.Errorf("with a map that lacks the point the validator refused %v, want [key_route] — "+
			"an unresolvable waypoint terminates the robot's waybill the moment the order is issued", got)
	}
	sort.Strings(got)
}
