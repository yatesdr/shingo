package engine

// release_creation.go — S7: an order the Edge creates that would lift a bin
// off a curtained node without a station wait gets one in front of the pickup,
// and its intent is written at creation. The robot drives in, the operator
// clears the curtain from the cell, and the bin crossing the curtain on the
// way out is what the gate is for. A dropoff gets no wait: the curtain was
// cleared to let the robot in, and the robot finishes. The intent goes when
// Core parks the robot at the wait and the act lets it: through a safe curtain
// at once; at a live one, an operator's press is remembered and goes when it
// clears, and a system-created hold asks for a RELEASE press (Q8).

import (
	"shingo/protocol"
	"shingoedge/release"
)

// curtainedCoreNodes is the set of core nodes with the curtain interlock on. A
// failed read answers an empty set: the creation proceeds without a wait, and
// the act's own curtain gate (G6) still reads the configuration at the release.
func (e *Engine) curtainedCoreNodes() map[string]bool {
	nodes, err := e.db.ListCurtainedProcessNodes()
	if err != nil {
		e.logRelease("curtained nodes unreadable at creation (%v) - no station wait added; the release's own curtain gate still applies", err)
		return nil
	}
	out := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		out[n.CoreNodeName] = true
	}
	return out
}

// withCurtainWaits puts wait(node) in front of each pickup at a curtained node
// that no wait at that node already precedes, and reports the ordinal (among
// the result's station waits) of the first wait it added, or -1.
// wait names the purpose at the call, as the purpose census requires.
func withCurtainWaits(steps []protocol.ComplexOrderStep, curtained map[string]bool,
	wait func(node string) protocol.ComplexOrderStep) ([]protocol.ComplexOrderStep, int) {
	if len(curtained) == 0 {
		return steps, -1
	}
	out := make([]protocol.ComplexOrderStep, 0, len(steps)+2)
	first, waits := -1, 0
	for _, s := range steps {
		crossing := s.Action == protocol.ActionPickup && curtained[s.Node]
		guarded := len(out) > 0 && out[len(out)-1].Action == protocol.ActionWait && out[len(out)-1].Node == s.Node
		if crossing && !guarded {
			if first < 0 {
				first = waits
			}
			out = append(out, wait(s.Node))
			waits++
		} else if s.Action == protocol.ActionWait && protocol.IsStationWaitKind(s.WaitKind) {
			waits++
		}
		out = append(out, s)
	}
	return out, first
}

// writeCreationIntent records the creation's intent for the first wait
// withCurtainWaits added. calledBy is the operator whose button created the
// order ("" for one the Edge created on its own, which is then System).
func (e *Engine) writeCreationIntent(orderID int64, firstWait int, purpose release.Purpose, calledBy string, choices release.Choices) {
	if firstWait < 0 {
		return
	}
	in := release.Intent{
		StationWait: firstWait, Purpose: purpose, Origin: release.OriginCreation,
		CalledBy: calledBy, Choices: choices, Standing: true, System: calledBy == "",
	}
	raw, err := in.Encode()
	if err == nil {
		err = e.db.SetOrderReleaseIntent(orderID, raw)
	}
	if err != nil {
		e.logRelease("order=%d: creation intent not written (%v) - the robot will hold at its curtain wait for a RELEASE press", orderID, err)
	}
}
