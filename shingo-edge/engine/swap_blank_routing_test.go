package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// A SWAP'S BLANK ROUTING AT REQUEST (round 11). Two blanks a builder can be
// handed, every mode pinned:
//
//   - A blank outbound destination is where the old carrier goes. Every swap
//     mode's steps drop it there, and buildStep makes a dropoff with NO NODE
//     of a blank, which Core reads as a deliberate deferred destination (the
//     dedicated-loader form) and cannot refuse. 2-Robot Swap refused it since
//     Springfield 2026-06-23 and Index Swap since it was written; Sequential
//     A/B and 1-Robot Swap sent the robot a dropoff with no node. Now every
//     mode refuses it with the same words.
//   - A blank inbound source on a produce position is allowed (Core picks the
//     bin). The refill pickup then names no node, and it went without its
//     Empty flag, so Core was asked for a FULL bin of the part where the
//     position needs an empty to fill. It now asks for an empty whether or not
//     the source is picked.

// swapModes is every mode BuildSwapDispatch builds.
var swapModes = []protocol.SwapMode{
	protocol.SwapModeSingleRobot,
	protocol.SwapModeTwoRobot,
	protocol.SwapModeTwoRobotPressIndex,
	protocol.SwapModeSequential,
}

func TestBuildSwapDispatch_EveryModeRefusesABlankOutboundDestination(t *testing.T) {
	t.Parallel()
	for _, role := range []protocol.ClaimRole{protocol.ClaimRoleProduce, protocol.ClaimRoleConsume} {
		for _, mode := range swapModes {
			role, mode := role, mode
			t.Run(string(role)+"/"+string(mode), func(t *testing.T) {
				t.Parallel()
				c := dispatchClaim(mode)
				c.Role = role
				c.OutboundDestination = ""
				d, err := BuildSwapDispatch(dispatchNode(), c)
				if err == nil {
					var nowhere []string
					for _, legs := range [][]protocol.ComplexOrderStep{d.StepsA, d.StepsB} {
						for i, s := range legs {
							if s.Action == protocol.ActionDropoff && s.Node == "" {
								nowhere = append(nowhere, strings.TrimSpace(string(s.Action))+"#"+string(rune('0'+i)))
							}
						}
					}
					t.Fatalf("a blank outbound destination was accepted; the dispatch drops a bin at no node (%v)", nowhere)
				}
				if d != nil {
					t.Errorf("a refused dispatch came back with legs: %+v", d)
				}
				want := "node CORE-NODE: " + swapModeWordFor(mode) + " requires an outbound destination (where the old bin goes)"
				if err.Error() != want {
					t.Errorf("refusal = %q\n           want %q (one sentence for every mode)", err.Error(), want)
				}
			})
		}
	}
}

// swapModeWordFor is the page's name for a mode, as the refusal says it.
func swapModeWordFor(mode protocol.SwapMode) string {
	return map[protocol.SwapMode]string{
		protocol.SwapModeSingleRobot:        "1‑Robot Swap",
		protocol.SwapModeTwoRobot:           "2‑Robot Swap",
		protocol.SwapModeTwoRobotPressIndex: "Index Swap",
		protocol.SwapModeSequential:         "Sequential A/B",
	}[mode]
}

// refillPickups is every pickup a mode's request makes at its inbound source:
// with the source blank, the pickups that name no node. Sequential A/B's fetch
// is its Order B, made at Order A's release (BuildSequentialBackfillSteps).
func refillPickups(t *testing.T, c *processes.NodeClaim) []protocol.ComplexOrderStep {
	t.Helper()
	var steps []protocol.ComplexOrderStep
	if c.SwapMode == protocol.SwapModeSequential {
		steps = BuildSequentialBackfillSteps(c)
	} else {
		d, err := BuildSwapDispatch(dispatchNode(), c)
		if err != nil {
			t.Fatalf("BuildSwapDispatch: %v", err)
		}
		steps = append(append(steps, d.StepsA...), d.StepsB...)
	}
	var out []protocol.ComplexOrderStep
	for _, s := range steps {
		if s.Action == protocol.ActionPickup && s.Node == c.InboundSource {
			out = append(out, s)
		}
	}
	return out
}

func TestRefill_AProduceRefillAsksForAnEmptyWithOrWithoutASource(t *testing.T) {
	t.Parallel()
	for _, mode := range swapModes {
		for _, source := range []string{"INBOUND-SRC", ""} {
			mode, source := mode, source
			t.Run(string(mode)+"/source="+source, func(t *testing.T) {
				t.Parallel()
				c := dispatchClaim(mode)
				c.InboundSource = source
				pickups := refillPickups(t, c)
				if len(pickups) == 0 {
					t.Fatalf("no refill pickup at the inbound source %q", source)
				}
				for _, p := range pickups {
					if !p.Empty {
						t.Errorf("produce refill pickup at %q asks for a FULL bin of the part (Empty=false): Core would hunt stock where the position needs an empty", source)
					}
				}
			})
		}
	}
}

func TestRefill_AConsumeRefillAsksForItsPartWithOrWithoutASource(t *testing.T) {
	t.Parallel()
	for _, mode := range swapModes {
		for _, source := range []string{"INBOUND-SRC", ""} {
			mode, source := mode, source
			t.Run(string(mode)+"/source="+source, func(t *testing.T) {
				t.Parallel()
				c := dispatchClaim(mode)
				c.Role = protocol.ClaimRoleConsume
				c.InboundSource = source
				for _, p := range refillPickups(t, c) {
					if p.Empty {
						t.Errorf("consume refill pickup at %q asks for an empty: a consume position takes a full of its part", source)
					}
				}
			})
		}
	}
}
