package domain

import (
	"fmt"

	"shingo/protocol"
)

// reorder_hold_words.go — a held position's chip on the station board.
//
// THREE CONTROLS SHARE ONE NAME on the Edge's screens: a claim's arm
// (AutoReorder, asking at its reorder point), the line's switch (a person's
// off at every position, store/processes.FlipAutoReorder) and the station's
// button for that switch. The board names each in the repo's terms
// (docs/terminology.md): the arm by the reorder point it asks at, the switch
// as the line's ("LINE AUTO-REORDER: ON", operator-render.js). A held tile's
// chip is the arm's: its point and why it is not asking, in two or three
// words. The decider's sentence (ReorderHeld.Sentence) is the tile's panel.

// ReorderHoldCause is why an armed position's automatic ask is held, in the
// chip's words. note is the runtime row's (RuntimeState.ReorderHoldNote): an
// off is the idle rule's (IsIdleOff) or an older Core's, written when it gave
// up waiting on an order (orders.holdOnCoreCancel); the note is what tells
// them apart.
func ReorderHoldCause(hold ReorderHold, note string) string {
	switch {
	case hold == ReorderHoldPaused:
		return "PAUSED"
	case IsIdleOff(hold, note):
		return "LINE IDLE"
	case hold == ReorderHoldOff:
		return "CORE GAVE UP"
	case hold == ReorderHoldPersonOff:
		return "SWITCHED OFF"
	case hold == ReorderHoldCount:
		return "CHECK COUNT"
	}
	return "HELD"
}

// ReorderHoldChip is a held tile's chip, "REORDER AT 110: LINE IDLE": the
// level the claim asks at (DemandLevel, the sweep's) and the cause. Empty when
// nothing holds, and for a person's off on a board whose header carries the
// line's switch (switchOnBoard, Process.AutoReorderSwitchHMI): the header
// already reads off, and a tile's chip marks a position that differs from its
// line. Where the header carries no switch, the chip is the board's only word
// that a person switched the line off.
//
// The level is the claim's count (parts for a part alone); above its part's
// ratio of 1 it reads in UoP, the parts in brackets: "REORDER AT 50 UoP (400
// parts): LINE IDLE" (owner, 2026-11-19: UOP big, the parts beside it). At 1
// the chip reads as it always did.
func ReorderHoldChip(c *NodeClaim, hold ReorderHold, note string, switchOnBoard bool) string {
	if hold == ReorderHoldNone || (hold == ReorderHoldPersonOff && switchOnBoard) {
		return ""
	}
	level := c.DemandLevel()
	if c.PiecesPerCycle > 1 {
		u, _ := protocol.SplitUOP(level, c.PiecesPerCycle)
		// OWNER-WORDS
		return fmt.Sprintf("REORDER AT %d UoP (%d parts): %s", u, level, ReorderHoldCause(hold, note))
	}
	return fmt.Sprintf("REORDER AT %d: %s", level, ReorderHoldCause(hold, note))
}
