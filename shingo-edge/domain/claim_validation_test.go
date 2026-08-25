package domain

import (
	"strings"
	"testing"

	"shingo/protocol"
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

		// Per-position tooling evacuation. A marked position the layout does not have
		// is not an unlikely config — it is a reference to nothing, and the
		// evacuation it asks for silently never happens.
		{"third position marked on a 2-position press", func(c *NodeClaimInput) {
			c.SecondPairedCoreNode = ""
			c.ChangeoverEvacPositions = Ptr([]string{EvacPositionSecond})
		}, "changeover_evac_positions"},
		{"back position marked with no back node", func(c *NodeClaimInput) {
			c.PairedCoreNode = ""
			c.ChangeoverEvacPositions = Ptr([]string{EvacPositionPaired})
		}, "changeover_evac_positions"},
		{"positions marked on a non-press-index mode", func(c *NodeClaimInput) {
			c.SwapMode = SwapModeForTest
			c.InboundStaging = "IN"
			c.ChangeoverEvacPositions = Ptr([]string{EvacPositionFront})
		}, "changeover_evac_positions"},
		{"an unknown position name", func(c *NodeClaimInput) {
			c.ChangeoverEvacPositions = Ptr([]string{"middle-ish"})
		}, "changeover_evac_positions"},

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
func TestValidateNodeClaim_ValidPositionSelectionsAccepted(t *testing.T) {
	t.Parallel()
	for _, positions := range [][]string{
		nil,
		{EvacPositionFront},
		{EvacPositionPaired},
		{EvacPositionFront, EvacPositionPaired},
		{EvacPositionFront, EvacPositionPaired, EvacPositionSecond},
	} {
		c := validClaim()
		c.SecondPairedCoreNode = "INDEX-C"
		c.ChangeoverEvacPositions = &positions
		if got := ValidateNodeClaim(c, ClaimNodeContext{}); HasErrors(got) {
			t.Errorf("positions %v must be accepted on a 3-position press; findings = %+v", positions, got)
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

func TestValidateNodeClaim_ManualSwapNeedsNoPayload(t *testing.T) {
	t.Parallel()
	c := validClaim()
	c.SwapMode = protocol.SwapModeManualSwap
	c.PayloadCode = ""
	if got := ValidateNodeClaim(c, ClaimNodeContext{}); HasErrors(got) {
		t.Fatalf("manual_swap with no payload must be accepted; findings = %+v", got)
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
	positions := []string{EvacPositionFront}

	c := validClaim()
	c.SwapMode = protocol.SwapModeTwoRobotPressIndex
	c.PairedCoreNode = "INDEX-B"
	c.ChangeoverEvacPositions = &positions
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
	positions := []string{EvacPositionFront}
	c := validClaim()
	c.SwapMode = protocol.SwapModeTwoRobotPressIndex
	c.PairedCoreNode = "INDEX-B"
	c.ChangeoverEvacPositions = &positions
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
