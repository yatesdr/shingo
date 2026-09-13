package domain

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/domain/flowspec"
)

// validClaim is a claim with nothing wrong with it. Each test breaks exactly
// one thing, so a finding can only be about the thing that was broken.
func validClaim() NodeClaimInput {
	return NodeClaimInput{
		StyleID:             1,
		CoreNodeName:        "PRESS",
		Role:                protocol.ClaimRoleProduce,
		SwapMode:            protocol.SwapModeTwoRobotPressIndex,
		PayloadCode:         "PART-A",
		InboundSource:       "EMPTIES",
		OutboundDestination: "MARKET",
		PairedCoreNode:      "INDEX-B",
	}
}

func fieldsOf(findings []FieldError, severity string) []string {
	var out []string
	for _, f := range findings {
		if f.Severity == severity {
			out = append(out, f.Field)
		}
	}
	return out
}

func hasField(findings []FieldError, field string) bool {
	for _, f := range findings {
		if f.Field == field {
			return true
		}
	}
	return false
}

func TestValidateNodeClaim_ValidClaimHasNoFindings(t *testing.T) {
	t.Parallel()
	if got := ValidateNodeClaim(validClaim(), ClaimNodeContext{}); len(got) != 0 {
		t.Fatalf("a valid claim produced findings: %+v", got)
	}
}

func TestValidateNodeClaim_Invariants(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		mut   func(*NodeClaimInput)
		field string
	}{
		{"missing style", func(c *NodeClaimInput) { c.StyleID = 0 }, "style_id"},
		{"missing core node", func(c *NodeClaimInput) { c.CoreNodeName = "" }, "core_node_name"},
		{"missing swap mode", func(c *NodeClaimInput) { c.SwapMode = "" }, "swap_mode"},
		{"unconfigurable swap mode", func(c *NodeClaimInput) { c.SwapMode = "simple" }, "swap_mode"},
		{"missing payload", func(c *NodeClaimInput) { c.PayloadCode = "" }, "payload_code"},
		{"negative board order", func(c *NodeClaimInput) { c.Sequence = Ptr(-1) }, "sequence"},

		// Per-node changeover clearance. A marked node this claim does not hold
		// is not an unlikely config — it is a reference to nothing, and the
		// clearance it asks for silently never happens.
		{"a node this claim dropped from its layout", func(c *NodeClaimInput) {
			c.SecondPairedCoreNode = ""
			c.ChangeoverEvacNodes = Ptr([]string{"INDEX-C"})
		}, "changeover_evac_nodes"},
		{"the back node marked after it was unset", func(c *NodeClaimInput) {
			c.PairedCoreNode = ""
			c.ChangeoverEvacNodes = Ptr([]string{"INDEX-B"})
		}, "changeover_evac_nodes"},
		{"nodes marked on a single-node claim", func(c *NodeClaimInput) {
			c.SwapMode = SwapModeForTest
			c.InboundStaging = "IN"
			c.ChangeoverEvacNodes = Ptr([]string{"PRESS"})
		}, "changeover_evac_nodes"},
		{"a node belonging to nobody", func(c *NodeClaimInput) {
			c.ChangeoverEvacNodes = Ptr([]string{"middle-ish"})
		}, "changeover_evac_nodes"},

		{"press-index without back position", func(c *NodeClaimInput) { c.PairedCoreNode = "" }, "paired_core_node"},
		{"press-index without outbound", func(c *NodeClaimInput) { c.OutboundDestination = "" }, "outbound_destination"},
		{"back position same as front", func(c *NodeClaimInput) { c.PairedCoreNode = "PRESS" }, "paired_core_node"},
		{"third position same as front", func(c *NodeClaimInput) { c.SecondPairedCoreNode = "PRESS" }, "second_paired_core_node"},
		{"third position same as back", func(c *NodeClaimInput) { c.SecondPairedCoreNode = "INDEX-B" }, "second_paired_core_node"},

		{"single_robot without inbound staging", func(c *NodeClaimInput) {
			c.SwapMode, c.OutboundStaging = protocol.SwapModeSingleRobot, "OUT"
		}, "inbound_staging"},
		{"single_robot without outbound staging", func(c *NodeClaimInput) {
			c.SwapMode, c.InboundStaging = protocol.SwapModeSingleRobot, "IN"
		}, "outbound_staging"},
		{"two_robot without inbound staging", func(c *NodeClaimInput) {
			c.SwapMode = protocol.SwapModeTwoRobot
		}, "inbound_staging"},
		{"manual_swap without outbound destination", func(c *NodeClaimInput) {
			c.SwapMode, c.OutboundDestination, c.PayloadCode = protocol.SwapModeManualSwap, "", ""
		}, "outbound_destination"},

		// SEQUENTIAL HAD NO ARM AT ALL. Every other mode's required routing is
		// refused here, at the moment the operator saves the claim; sequential
		// fell through the switch, so a claim missing its partner or its routing
		// saved clean and failed at runtime as an empty dispatch — a changeover
		// that plans, creates nothing, and leaves the node task in error with a
		// message about the builder rather than about the field.
		{"sequential without a paired position", func(c *NodeClaimInput) {
			c.SwapMode, c.PairedCoreNode = protocol.SwapModeSequential, ""
		}, "paired_core_node"},
		{"sequential paired to itself", func(c *NodeClaimInput) {
			c.SwapMode, c.PairedCoreNode = protocol.SwapModeSequential, "PRESS"
		}, "paired_core_node"},
		{"sequential without outbound destination", func(c *NodeClaimInput) {
			c.SwapMode, c.OutboundDestination = protocol.SwapModeSequential, ""
		}, "outbound_destination"},
		{"sequential without inbound source", func(c *NodeClaimInput) {
			c.SwapMode, c.InboundSource = protocol.SwapModeSequential, ""
		}, "inbound_source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := validClaim()
			tc.mut(&c)
			got := ValidateNodeClaim(c, ClaimNodeContext{})
			if !HasErrors(got) {
				t.Fatalf("no error raised; findings = %+v", got)
			}
			if !hasField(got, tc.field) {
				t.Errorf("error is not tagged %q — fields raised: %v", tc.field, fieldsOf(got, SeverityError))
			}
		})
	}
}

// manual_swap loaders carry no edge-side payload — Core owns the loader's
// payload set — so the payload requirement must not fire for them.
// Absent and zero are both fine: absent means "no opinion" and the store
// assigns the next free slot, and 0 is what an untouched number input reads.
// Only a negative is a refusal — a test that only checked nil would pass with
// the guard written as `*in.Sequence <= 0` and break every new claim.
func TestValidateNodeClaim_SequenceAbsentOrZeroIsFine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		seq  *int
	}{
		{"absent", nil},
		{"zero", Ptr(0)},
		{"positive", Ptr(7)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := validClaim()
			c.Sequence = tc.seq
			if got := ValidateNodeClaim(c, ClaimNodeContext{}); HasErrors(got) {
				t.Fatalf("sequence %v must be accepted; findings = %+v", tc.seq, got)
			}
		})
	}
}

// SwapModeForTest is two_robot — a mode with no positions, used to pin that positions
// are press-index-only. Named rather than inlined so the intent survives
// someone changing which mode the row uses.
var SwapModeForTest = protocol.SwapModeTwoRobot

// A position selection the layout DOES have is accepted, including the whole set
// on a 3-position press. Without this the rows above would pass with the check
// written as "any position selection is an error".
func TestValidateNodeClaim_MarkedNodesMustBeThisClaimsNodes(t *testing.T) {
	t.Parallel()
	// validClaim() is PRESS + INDEX-B; the third node is added per case.
	for _, marked := range [][]string{
		nil,
		{"PRESS"},
		{"INDEX-B"},
		{"PRESS", "INDEX-B"},
		{"PRESS", "INDEX-B", "INDEX-C"},
	} {
		c := validClaim()
		c.SecondPairedCoreNode = "INDEX-C"
		c.ChangeoverEvacNodes = &marked
		if got := ValidateNodeClaim(c, ClaimNodeContext{}); HasErrors(got) {
			t.Errorf("nodes %v are all this claim's own; findings = %+v", marked, got)
		}
	}
}

// A node the claim does not occupy is refused BY NAME. This is what replaces
// the old positional indirection: marks used to follow a re-pairing silently,
// which re-targeted a physical clearance onto a different node with nobody
// told. Now the same edit is a save-time message.
func TestValidateNodeClaim_MarkedNodeNotOnThisClaimIsRefused(t *testing.T) {
	t.Parallel()
	for _, marked := range [][]string{
		{"SOMEONE-ELSES-NODE"},
		{"PRESS", "INDEX-C"}, // INDEX-C is not set on this claim
	} {
		c := validClaim()
		c.ChangeoverEvacNodes = &marked
		findings := ValidateNodeClaim(c, ClaimNodeContext{})
		if !HasErrors(findings) {
			t.Errorf("marks %v were accepted on a claim that does not hold them", marked)
			continue
		}
		if !findingOnField(findings, "changeover_evac_nodes") {
			t.Errorf("the refusal does not name the field: %+v", findings)
		}
	}
}

// A free-form evac destination is never refused — node OR group, and blank is
// the ordinary default.
func TestValidateNodeClaim_EvacDestinationIsFreeForm(t *testing.T) {
	t.Parallel()
	for _, dest := range []string{"", "TOOLING-BAY", "SMG_01", "some.group.name"} {
		c := validClaim()
		c.ChangeoverEvacDestination = &dest
		if got := ValidateNodeClaim(c, ClaimNodeContext{}); HasErrors(got) {
			t.Errorf("evac destination %q must be accepted; findings = %+v", dest, got)
		}
	}
}

// TestValidateNodeClaim_ManualSwapIsNotAConfigurableInput replaces
// TestValidateNodeClaim_ManualSwapNeedsNoPayload, which asserted the opposite:
// that a manual_swap claim with a blank payload was ACCEPTED, because Core owns
// a loader's payload set and there was nothing for the Edge to require.
//
// The ownership move finished that argument in the other direction. Core owns
// the whole loader, not only its payloads, and SynthClaim serves all six facts
// from the aggregate — so there is no such thing as a loader claim to validate.
// manual_swap has left protocol.ConfigurableSwapModes() and the input is now
// refused outright.
//
// WHAT THE ASSERTION IS ABOUT IS WHICH FIELD THE REFUSAL NAMES. A person who
// types this into the claim editor must be told the mode is not configurable,
// not that they forgot a payload — the payload arm still exempts loaders, so
// silence there is what leaves swap_mode as the only thing said.
func TestValidateNodeClaim_ManualSwapIsNotAConfigurableInput(t *testing.T) {
	t.Parallel()
	c := validClaim()
	c.SwapMode = protocol.SwapModeManualSwap
	c.PayloadCode = ""
	got := ValidateNodeClaim(c, ClaimNodeContext{})
	if !HasErrors(got) {
		t.Fatalf("manual_swap must be refused as an input; findings = %+v", got)
	}
	for _, f := range got {
		if f.Severity != SeverityError {
			continue
		}
		if f.Field != "swap_mode" {
			t.Errorf("the refusal must name swap_mode, not %q (%q)", f.Field, f.Message)
		}
	}
}

// ── membership ──────────────────────────────────────────────────────────

// The membership finding is ADVICE. One physical slot is legitimately named by
// several processes — a shared loader window is the ordinary case — so a
// refusal here would block a working configuration to catch a likely typo.
func TestValidateNodeClaim_ForeignNodeWarnsButDoesNotRefuse(t *testing.T) {
	t.Parallel()
	got := ValidateNodeClaim(validClaim(), ClaimNodeContext{
		Checked:        true,
		StyleProcessID: 1,
		NodeProcessIDs: []int64{2, 3},
	})
	if HasErrors(got) {
		t.Fatalf("membership must never refuse; findings = %+v", got)
	}
	if len(got) != 1 || got[0].Severity != SeverityWarning || got[0].Field != "core_node_name" {
		t.Fatalf("want one core_node_name warning; got %+v", got)
	}
	if !strings.Contains(got[0].Message, "PRESS") {
		t.Errorf("warning should name the node; got %q", got[0].Message)
	}
}

func TestValidateNodeClaim_NodeOnOwnProcessIsSilent(t *testing.T) {
	t.Parallel()
	got := ValidateNodeClaim(validClaim(), ClaimNodeContext{
		Checked:        true,
		StyleProcessID: 1,
		NodeProcessIDs: []int64{1},
	})
	if len(got) != 0 {
		t.Fatalf("a node on the style's own process is unremarkable; got %+v", got)
	}
}

// A shared slot: the node serves this process AND others. No warning — that is
// the configuration the warning is deliberately not refusing.
func TestValidateNodeClaim_SharedNodeIsSilent(t *testing.T) {
	t.Parallel()
	got := ValidateNodeClaim(validClaim(), ClaimNodeContext{
		Checked:        true,
		StyleProcessID: 1,
		NodeProcessIDs: []int64{1, 2},
	})
	if len(got) != 0 {
		t.Fatalf("a node shared with this style's process must not warn; got %+v", got)
	}
}

// A CHECK MUST KNOW WHETHER IT HAD THE INPUT TO CHECK. An unresolved lookup and
// "this node belongs to someone else" are different sentences, and only one of
// them belongs in front of an engineer.
func TestValidateNodeClaim_UncheckedContextSaysNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		ctx  ClaimNodeContext
	}{
		{"lookup_failed", ClaimNodeContext{Checked: false, StyleProcessID: 1, NodeProcessIDs: []int64{9}}},
		{"no_process_node_anywhere", ClaimNodeContext{Checked: true, StyleProcessID: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ValidateNodeClaim(validClaim(), tc.ctx); len(got) != 0 {
				t.Fatalf("absence of data must not render as a finding; got %+v", got)
			}
		})
	}
}

func TestHasErrors_WarningsAloneAreNotErrors(t *testing.T) {
	t.Parallel()
	if HasErrors([]FieldError{{Field: "x", Severity: SeverityWarning}}) {
		t.Error("a warning-only finding set must not read as an error")
	}
	if !HasErrors([]FieldError{{Field: "x", Severity: SeverityWarning}, {Field: "y", Severity: SeverityError}}) {
		t.Error("an error mixed in with warnings must still read as an error")
	}
}

// ── carry-over dispositions ─────────────────────────────────────────────

// A cell that asks to park a carried-over part at outbound staging and has no
// outbound staging node is refused AT SAVE TIME, by name.
//
// The two alternatives are both worse. Falling back to clearing means the
// operator asked for a short hop, got a supermarket round-trip, and was never
// told. Refusing at changeover time means finding out with the press down and
// people standing around. The arm-gate doctrine says a configuration that
// cannot work is refused where it is written.
func TestValidateNodeClaim_OutboundStagingCarryoverNeedsAStagingNode(t *testing.T) {
	t.Parallel()
	disp := CarryoverOutboundStaging
	marked := []string{"PRESS"}

	c := validClaim()
	c.SwapMode = protocol.SwapModeTwoRobotPressIndex
	c.PairedCoreNode = "INDEX-B"
	c.ChangeoverEvacNodes = &marked
	c.ChangeoverCarryoverDisposition = &disp
	c.OutboundStaging = ""
	findings := ValidateNodeClaim(c, ClaimNodeContext{})
	if !HasErrors(findings) {
		t.Fatal("outbound_staging carry-over accepted with no outbound staging node")
	}
	if !findingOnField(findings, "changeover_carryover_disposition") {
		t.Errorf("the refusal does not name the field the operator has to fix: %+v", findings)
	}

	// With a staging node it is accepted.
	c.OutboundStaging = "OUT-STAGE"
	if got := ValidateNodeClaim(c, ClaimNodeContext{}); HasErrors(got) {
		t.Errorf("outbound_staging carry-over refused despite a staging node; findings = %+v", got)
	}
}

// A disposition on a claim that marks no positions is configuration that can never
// be read — the disposition is only ever consulted for a marked position.
func TestValidateNodeClaim_CarryoverNeedsAMarkedPosition(t *testing.T) {
	t.Parallel()
	disp := CarryoverKeepLineside
	c := validClaim()
	c.ChangeoverCarryoverDisposition = &disp
	if got := ValidateNodeClaim(c, ClaimNodeContext{}); !HasErrors(got) {
		t.Error("a carry-over disposition was accepted on a claim that marks no positions — " +
			"nothing would ever read it, which is a setting the operator believes is doing something")
	}
}

// replace is the default and must be accepted anywhere, including on a claim
// with no marks: it is what every row says after the column arrives.
func TestValidateNodeClaim_ReplaceIsAlwaysAccepted(t *testing.T) {
	t.Parallel()
	disp := CarryoverReplace
	c := validClaim()
	c.ChangeoverCarryoverDisposition = &disp
	if got := ValidateNodeClaim(c, ClaimNodeContext{}); HasErrors(got) {
		t.Errorf("the default disposition was refused; findings = %+v", got)
	}
}

// An unknown value is refused rather than silently read as replace.
func TestValidateNodeClaim_UnknownCarryoverRefused(t *testing.T) {
	t.Parallel()
	disp := CarryoverDisposition("send_to_mars")
	marked := []string{"PRESS"}
	c := validClaim()
	c.SwapMode = protocol.SwapModeTwoRobotPressIndex
	c.PairedCoreNode = "INDEX-B"
	c.ChangeoverEvacNodes = &marked
	c.ChangeoverCarryoverDisposition = &disp
	if got := ValidateNodeClaim(c, ClaimNodeContext{}); !HasErrors(got) {
		t.Error("an unknown carry-over disposition was accepted")
	}
}

// findingOnField reports whether any finding names this wire field — the thing
// an operator's form highlights, so a refusal that names the wrong field sends
// them to the wrong control.
func findingOnField(findings []FieldError, field string) bool {
	for _, f := range findings {
		if f.Field == field {
			return true
		}
	}
	return false
}

// TestValidateNodeClaim_KeepStagedIsWithheld: keep-staged is withheld from plant
// configuration, so a claim asking for it is refused at ingress with the one
// message every door gives; a claim that says false, or nothing, is not.
func TestValidateNodeClaim_KeepStagedIsWithheld(t *testing.T) {
	t.Parallel()
	c := validClaim()
	c.KeepStaged = Ptr(true)
	var got string
	for _, f := range ValidateNodeClaim(c, ClaimNodeContext{}) {
		if f.Field == "keep_staged" {
			got = f.Message
		}
	}
	if got != KeepStagedWithheld {
		t.Fatalf("keep_staged=true: finding %q, want %q", got, KeepStagedWithheld)
	}
	for _, v := range []*bool{Ptr(false), nil} {
		c.KeepStaged = v
		if hasField(ValidateNodeClaim(c, ClaimNodeContext{}), "keep_staged") {
			t.Errorf("keep_staged=%v produced a finding; only asking for it is refused", v)
		}
	}
}

// completeClaimFor is a claim that satisfies every Required entry of
// Steady(role, mode) and nothing else, so a finding can only be about the one
// thing a test then changes.
func completeClaimFor(role protocol.ClaimRole, mode protocol.SwapMode) NodeClaimInput {
	in := NodeClaimInput{StyleID: 1, CoreNodeName: "NODE", Role: role, SwapMode: mode}
	for _, f := range flowspec.RequiredFields(flowspec.Steady(role, mode)) {
		populateClaimInputField(&in, f)
	}
	return in
}

// errorFieldSet is the set of fields ValidateNodeClaim refused, restricted to
// the ones flowspec knows, sorted.
func errorFieldSet(findings []FieldError) []string {
	known := map[string]bool{}
	for _, f := range flowspec.Fields() {
		known[string(f)] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range findings {
		if f.Severity == SeverityError && known[f.Field] && !seen[f.Field] {
			seen[f.Field] = true
			out = append(out, f.Field)
		}
	}
	sort.Strings(out)
	return out
}

// serverEnforcedForbids are the Forbidden entries ValidateNodeClaim refuses
// when populated IN EVERY MODE. For the strict modes (StrictSteadyModes:
// single_robot and sequential, D4) every Forbidden entry is refused. For the
// other modes every remaining Forbidden entry is the EDITOR's answer only: the
// admin page clears the value at save and the server accepts it if it arrives
// another way. That gap is pinned below as D7, mode by mode; a mode joins
// StrictSteadyModes in the commit that gives the server the rule.
var serverEnforcedForbids = map[flowspec.Field]bool{
	flowspec.ChangeoverEvacNodes:            true,
	flowspec.IndexRobotSupplies:             true,
	flowspec.KeyRoute:                       true,
	flowspec.ChangeoverCarryoverDisposition: true,
}

func serverEnforcesForbid(mode protocol.SwapMode, f flowspec.Field) bool {
	if serverEnforcedForbids[f] {
		return true
	}
	for _, m := range StrictSteadyModes() {
		if m == mode {
			return true
		}
	}
	return false
}

// TestFlowspecMatchesValidateNodeClaim: the save-time validator and
// flowspec.Steady agree, modulo the pinned disagreements.
//
// Required: a claim with every field blank is refused on exactly Steady's
// Required fields, for every (role, configurable mode). No exceptions at U1 —
// D1 and D2 are disagreements between Steady and Changeover, not between
// Steady and this validator.
//
// Forbidden: a complete claim with one Forbidden field populated is refused on
// that field when the server has the rule (serverEnforcedForbids) and ACCEPTED
// otherwise (D7, the editor-only forbids). A case moving from the second list
// to the first is a behaviour change and must be named in its commit.
func TestFlowspecMatchesValidateNodeClaim(t *testing.T) {
	t.Parallel()
	for _, role := range flowspec.Roles() {
		for _, mode := range protocol.ConfigurableSwapModes() {
			spec := flowspec.Steady(role, mode)
			blank := NodeClaimInput{StyleID: 1, CoreNodeName: "NODE", Role: role, SwapMode: mode}
			got := errorFieldSet(ValidateNodeClaim(blank, ClaimNodeContext{}))
			var want []string
			for _, f := range flowspec.RequiredFields(spec) {
				want = append(want, string(f))
			}
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Required(%s, %s): validator refuses %v, Steady requires %v", role, mode, got, want)
			}

			complete := completeClaimFor(role, mode)
			if got := errorFieldSet(ValidateNodeClaim(complete, ClaimNodeContext{})); len(got) != 0 {
				t.Errorf("complete(%s, %s) still refused on %v", role, mode, got)
				continue
			}
			for _, f := range flowspec.Fields() {
				if spec[f] != flowspec.Forbidden {
					continue
				}
				in := complete
				populateClaimInputField(&in, f)
				refused := hasField(ValidateNodeClaim(in, ClaimNodeContext{}), string(f))
				if serverEnforcesForbid(mode, f) && !refused {
					t.Errorf("Forbidden(%s, %s, %s): server has the rule and did not refuse", role, mode, f)
				}
				if !serverEnforcesForbid(mode, f) && refused {
					t.Errorf("D7 resolved for (%s, %s, %s): the server now refuses what only the editor cleared — add the mode to StrictSteadyModes or the field to serverEnforcedForbids in the commit that did it", role, mode, f)
				}
			}
		}
	}
}

// TestRoutingRequiredMessagesAreTotal: every Required routing entry in Steady
// has wording, for every configurable mode, and no wording exists for an entry
// the table does not require. The fallback in validateSwapModeRouting keeps
// the refusal if this drifts; this keeps the sentence.
func TestRoutingRequiredMessagesAreTotal(t *testing.T) {
	t.Parallel()
	for _, mode := range protocol.ConfigurableSwapModes() {
		spec := flowspec.Steady(protocol.ClaimRoleConsume, mode)
		for _, f := range flowspec.RoutingFields() {
			_, worded := routingRequiredMessages[mode][f]
			if spec[f] == flowspec.Required && !worded {
				t.Errorf("%s requires %s and routingRequiredMessages has no sentence for it", mode, f)
			}
			if spec[f] != flowspec.Required && worded {
				t.Errorf("routingRequiredMessages words %s for %s, which Steady does not require", f, mode)
			}
		}
	}
}

// TestValidateNodeClaim_SingleRobotRequiresOutboundDestination — D1 resolved.
// The single_robot builder sends the old bin to the outgoing claim's outbound
// destination and the planner has always refused a blank one; until this the
// save path did not, so the refusal arrived after the operator pressed START.
func TestValidateNodeClaim_SingleRobotRequiresOutboundDestination(t *testing.T) {
	t.Parallel()
	c := NodeClaimInput{
		StyleID: 1, CoreNodeName: "LINE", Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeSingleRobot, PayloadCode: "PART",
		InboundStaging: "IN", OutboundStaging: "OUT",
	}
	got := ValidateNodeClaim(c, ClaimNodeContext{})
	if !hasField(got, "outbound_destination") {
		t.Fatalf("single_robot with no outbound destination was accepted: %+v", got)
	}
	for _, f := range got {
		if f.Field == "outbound_destination" && !strings.Contains(f.Message, "outbound destination") {
			t.Errorf("message does not name the field: %q", f.Message)
		}
	}
	c.OutboundDestination = "SMN"
	if got := ValidateNodeClaim(c, ClaimNodeContext{}); len(got) != 0 {
		t.Fatalf("complete single_robot claim refused: %+v", got)
	}
}

// TestValidateNodeClaim_TwoRobotRequiresOutboundDestination — D2 resolved.
// Robot B takes the old bin straight to the outgoing claim's outbound
// destination; the planner refused a blank one and the dispatcher refuses to
// build the leg, both after START. The save path refuses it now, on the field.
func TestValidateNodeClaim_TwoRobotRequiresOutboundDestination(t *testing.T) {
	t.Parallel()
	c := NodeClaimInput{
		StyleID: 1, CoreNodeName: "LINE", Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeTwoRobot, PayloadCode: "PART",
		InboundStaging: "IN",
	}
	got := ValidateNodeClaim(c, ClaimNodeContext{})
	if !hasField(got, "outbound_destination") {
		t.Fatalf("two_robot with no outbound destination was accepted: %+v", got)
	}
	c.OutboundDestination = "SMN"
	if got := ValidateNodeClaim(c, ClaimNodeContext{}); len(got) != 0 {
		t.Fatalf("complete two_robot claim refused: %+v", got)
	}
}

// TestValidateNodeClaim_StrictModesRefuseFieldsTheyDoNotUse — D4 resolved
// at the API for single_robot: every field the table calls Forbidden is
// refused when populated, not only the four the server always had rules for;
// the store reads the same answer, so the two cannot drift.
func TestValidateNodeClaim_StrictModesRefuseFieldsTheyDoNotUse(t *testing.T) {
	t.Parallel()
	sr := completeClaimFor(protocol.ClaimRoleConsume, protocol.SwapModeSingleRobot)
	sr.PairedCoreNode = "B"
	if got := ValidateNodeClaim(sr, ClaimNodeContext{}); !hasField(got, "paired_core_node") {
		t.Errorf("single_robot with a paired node accepted: %+v", got)
	}
	// The wording names the mode and the field, and the finding is an error.
	for _, f := range ValidateNodeClaim(sr, ClaimNodeContext{}) {
		if f.Field == "paired_core_node" {
			if f.Severity != SeverityError || !strings.Contains(f.Message, "Paired Core Node") {
				t.Errorf("finding = %+v", f)
			}
		}
	}
	// sequential was meant to be strict too and is held (see
	// StrictSteadyModes); populated staging on one is still accepted here, and
	// that is D7 for sequential, pinned above.
	sq := completeClaimFor(protocol.ClaimRoleConsume, protocol.SwapModeSequential)
	sq.InboundStaging = "IN"
	if got := ValidateNodeClaim(sq, ClaimNodeContext{}); hasField(got, "inbound_staging") {
		t.Errorf("sequential is not strict (held); inbound staging refused: %+v", got)
	}
	// A two_robot claim carrying outbound staging is NOT refused: two_robot is
	// not a strict mode, and that is D7 for it, pinned above.
	tr := completeClaimFor(protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot)
	tr.OutboundStaging = "OUT"
	if got := ValidateNodeClaim(tr, ClaimNodeContext{}); hasField(got, "outbound_staging") {
		t.Errorf("two_robot is not strict yet; outbound staging refused: %+v", got)
	}
}
