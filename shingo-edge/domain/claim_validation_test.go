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
