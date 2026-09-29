package engine

// curtain_gate.go - the FG light-curtain release interlock.
//
// A light curtain stands at each finished-goods pickup location. An operator
// button writes a BOOL tag that mutes the curtain. With the interlock enabled
// on a process, a PRODUCE release is only allowed when that tag reads the
// configured safe value - checked by a DIRECT WarLink read at the moment of
// the release (plc.Manager.ReadTagDirect), never the poll cache, because a
// safety decision must not be older than the request that made it.
//
// FAIL-CLOSED, on everything. This is a safety interlock, and every refusal
// here is a click the operator repeats a minute later, while every wrong pass
// is a release the curtain was there to prevent:
//
//   - the interlock's configuration cannot be read - refuse (we cannot know
//     whether the interlock is armed, so treat it as armed);
//   - the tag cannot be read (WarLink down, PLC unknown, tag unpublished) -
//     refuse, with the reason named;
//   - the tag reads something that is not recognisably a BOOL - refuse, not
//     guess;
//   - the value disagrees with the configured safe value - refuse.
//
// The gate is keyed on the CLAIM'S ROLE: a consume claim's release and a
// changeover release are never gated, whatever the process says. The curtain
// stands at the FG pickup; the produce release is the only release that puts
// a robot into that conversation.
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
	proc, err := e.db.GetProcess(node.ProcessID)
	if err != nil || proc == nil {
		return fmt.Errorf("release held: the light-curtain interlock's configuration could not be read - refusing until it can")
	}
	if !proc.CurtainEnabled {
		return nil
	}
	if proc.CurtainPLCName == "" || proc.CurtainTagName == "" {
		return fmt.Errorf("release held: the curtain interlock is enabled but its PLC/tag pointers are missing - fix the process settings")
	}
	ctx, cancel := context.WithTimeout(context.Background(), curtainReadTimeout)
	defer cancel()
	raw, err := e.plcMgr.ReadTagDirect(ctx, proc.CurtainPLCName, proc.CurtainTagName)
	if err != nil {
		return fmt.Errorf("release held: the curtain tag %s/%s could not be read (%s) - the interlock refuses on an unreadable curtain",
			proc.CurtainPLCName, proc.CurtainTagName, err)
	}
	val, ok := plc.CurtainBool(raw)
	if !ok {
		return fmt.Errorf("release held: the curtain tag %s/%s did not read as a BOOL (%v) - the interlock refuses on a value it cannot interpret",
			proc.CurtainPLCName, proc.CurtainTagName, raw)
	}
	if val != proc.CurtainSafeValue {
		want := "TRUE"
		if !proc.CurtainSafeValue {
			want = "FALSE"
		}
		return fmt.Errorf("release held: the light curtain is not in its release state (tag reads %v, release requires %s)",
			val, want)
	}
	return nil
}
