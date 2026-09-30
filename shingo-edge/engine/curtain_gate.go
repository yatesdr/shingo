package engine

// curtain_gate.go - the FG light-curtain release interlock.
//
// A light curtain stands at each finished-goods pickup location. An operator
// button writes a BOOL tag that mutes the curtain. With the interlock enabled
// on a node, a PRODUCE release there is only allowed when that node's tag
// reads the node's configured safe value - checked by a DIRECT WarLink read at the moment of
// the release (plc.Manager.ReadTagDirect), never the poll cache, because a
// safety decision must not be older than the request that made it.
//
// FAIL-CLOSED, on everything. This is a safety interlock, and every refusal
// here is a click the operator repeats a minute later, while every wrong pass
// is a release the curtain was there to prevent:
//
//   - the interlock is on but its polarity was never chosen - refuse (a
//     guessed polarity is an inverted gate wherever the guess is wrong);
//   - the tag cannot be read (WarLink down, PLC unknown, tag unpublished) -
//     refuse, with the reason named;
//   - the tag reads something that is not recognisably a BOOL - refuse, not
//     guess;
//   - the value disagrees with the configured safe value - refuse.
//
// The gate is keyed on the CLAIM'S ROLE: a consume claim's release is never
// gated, whatever the node says. The curtain stands at the FG pickup; the
// produce release is the only release that puts a robot into that
// conversation.
//
// The render half is elsewhere: the station view folds the tag's cache value
// into the node entry so the RELEASE button greys out before the click (see
// the view enrichment). That is UX. This gate is the interlock, and the
// button never being shown is not what stands between a pair and a release.

import (
	"context"
	"fmt"
	"time"

	"shingo/protocol"
	"shingoedge/plc"
	"shingoedge/store/processes"
)

// curtainReadTimeout bounds the direct WarLink read. A release click waits
// at most this long for the curtain's answer; past it the release refuses.
// Short on purpose - WarLink answers in milliseconds on a healthy plant, and
// a gate that hangs the operator's button is its own failure.
const curtainReadTimeout = 3 * time.Second

// curtainGate returns nil when the release may proceed, and the operator-
// readable refusal when it may not. Call it in the GATES half of every
// release surface, before any side effect.
func (e *Engine) curtainGate(node *processes.Node, claim *processes.NodeClaim) error {
	if claim == nil || claim.Role != protocol.ClaimRoleProduce {
		return nil // a consume release, or no claim to speak of
	}
	if node == nil || !node.CurtainEnabled {
		return nil
	}
	if node.CurtainPLCName == "" || node.CurtainTagName == "" {
		return fmt.Errorf("release held: the curtain interlock at %s is enabled but its PLC/tag pointers are missing - fix the node's curtain settings", node.Name)
	}
	if node.CurtainSafeValue == nil {
		return fmt.Errorf("release held: the curtain interlock at %s is enabled but nobody has chosen which tag value allows the release - set it in the node's curtain settings", node.Name)
	}
	plcName, tagName, safe := node.CurtainPLCName, node.CurtainTagName, *node.CurtainSafeValue
	ctx, cancel := context.WithTimeout(context.Background(), curtainReadTimeout)
	defer cancel()
	raw, err := e.plcMgr.ReadTagDirect(ctx, plcName, tagName)
	if err != nil {
		return fmt.Errorf("release held: the curtain tag %s/%s could not be read (%s) - the interlock refuses on an unreadable curtain",
			plcName, tagName, err)
	}
	val, ok := plc.CurtainBool(raw)
	if !ok {
		return fmt.Errorf("release held: the curtain tag %s/%s did not read as a BOOL (%v) - the interlock refuses on a value it cannot interpret",
			plcName, tagName, raw)
	}
	if val != safe {
		want := "TRUE"
		if !safe {
			want = "FALSE"
		}
		return fmt.Errorf("release held: the light curtain at %s is not in its release state (tag reads %v, release requires %s)",
			node.Name, val, want)
	}
	return nil
}
