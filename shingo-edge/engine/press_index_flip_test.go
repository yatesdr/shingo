package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

func flipClaim(role protocol.ClaimRole, secondPaired string, flipped bool) *processes.NodeClaim {
	return &processes.NodeClaim{
		CoreNodeName:         "PRESS",
		Role:                 role,
		SwapMode:             protocol.SwapModeTwoRobotPressIndex,
		PayloadCode:          "PART-A",
		InboundSource:        "MARKET-EMPTIES",
		OutboundDestination:  "MARKET",
		PairedCoreNode:       "INDEX-B",
		SecondPairedCoreNode: secondPaired,
		IndexRobotSupplies:   flipped,
	}
}

func legTrace(steps []protocol.ComplexOrderStep) string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		n := s.Node
		if n == "" {
			n = "-"
		}
		out = append(out, string(s.Action)+"@"+n)
	}
	return strings.Join(out, " ")
}

// The flip, both layouts, spelled out. These are the shapes the brief
// specifies, pinned literally: the whole unit is a rearrangement of steps
// between two legs, and a rearrangement is exactly what a property test misses.
func TestBuildPressIndexSwapSteps_FlippedShapes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		secondPaired   string
		wantR1, wantR2 string
	}{
		{
			name:   "2-position",
			wantR1: "wait@PRESS pickup@PRESS dropoff@MARKET",
			wantR2: "wait@INDEX-B pickup@INDEX-B dropoff@PRESS pickup@MARKET-EMPTIES dropoff@INDEX-B",
		},
		{
			name:         "3-position",
			secondPaired: "INDEX-C",
			wantR1:       "wait@PRESS pickup@PRESS dropoff@MARKET",
			wantR2: "wait@INDEX-B pickup@INDEX-B dropoff@PRESS pickup@INDEX-C dropoff@INDEX-B " +
				"pickup@MARKET-EMPTIES dropoff@INDEX-C",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r1, r2 := BuildTwoRobotPressIndexSwapSteps(
				flipClaim(protocol.ClaimRoleProduce, tc.secondPaired, true))
			if got := legTrace(r1); got != tc.wantR1 {
				t.Errorf("R1 =\n  %s\nwant\n  %s", got, tc.wantR1)
			}
			if got := legTrace(r2); got != tc.wantR2 {
				t.Errorf("R2 =\n  %s\nwant\n  %s", got, tc.wantR2)
			}
		})
	}
}

// The unflipped shapes are UNCHANGED. The flip is opt-in per claim, and every
// press running today is unflipped — a builder refactor that quietly moved a
// step on the default path would ship to every plant at once.
func TestBuildPressIndexSwapSteps_UnflippedIsUnchanged(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		secondPaired   string
		wantR1, wantR2 string
	}{
		{
			name:   "2-position",
			wantR1: "wait@PRESS pickup@PRESS dropoff@MARKET pickup@MARKET-EMPTIES dropoff@INDEX-B",
			wantR2: "wait@INDEX-B pickup@INDEX-B dropoff@PRESS",
		},
		{
			name:         "3-position",
			secondPaired: "INDEX-C",
			wantR1:       "wait@PRESS pickup@PRESS dropoff@MARKET pickup@MARKET-EMPTIES dropoff@INDEX-C",
			wantR2:       "wait@INDEX-B pickup@INDEX-B dropoff@PRESS pickup@INDEX-C dropoff@INDEX-B",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r1, r2 := BuildTwoRobotPressIndexSwapSteps(
				flipClaim(protocol.ClaimRoleProduce, tc.secondPaired, false))
			if got := legTrace(r1); got != tc.wantR1 {
				t.Errorf("R1 =\n  %s\nwant\n  %s", got, tc.wantR1)
			}
			if got := legTrace(r2); got != tc.wantR2 {
				t.Errorf("R2 =\n  %s\nwant\n  %s", got, tc.wantR2)
			}
		})
	}
}

// TWO FLAGGERS, ONE LEG, AND THEY MUST COMPOSE.
//
// markPressIndexOnDeckEmpty flags the on-deck pickups (B, and C on a
// 3-position press) so Core sources the carrier whatever payload is stamped on
// it; markInboundEmpty flags the supermarket pickup so it fetches an empty
// rather than hunting a full. Unflipped they live on different legs and have
// never met. Flipped they are both on R2.
//
// Each matches on node name and only ever SETS Empty, so neither can clear the
// other's — but a flagger rewritten to ASSIGN rather than set would silently
// unflag the other's pickup, and the failure is the Hopkinsville
// waiting_for_material hang: an index pickup that matches no bin.
func TestPressIndexFlipped_BothEmptyFlaggersCompose(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		secondPaired string
		wantEmpty    []string
	}{
		{"2-position", "", []string{"INDEX-B", "MARKET-EMPTIES"}},
		{"3-position", "INDEX-C", []string{"INDEX-B", "INDEX-C", "MARKET-EMPTIES"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, r2 := BuildTwoRobotPressIndexSwapSteps(
				flipClaim(protocol.ClaimRoleProduce, tc.secondPaired, true))

			gotEmpty := map[string]bool{}
			for _, s := range r2 {
				if s.Action == protocol.ActionPickup && s.Empty {
					gotEmpty[s.Node] = true
				}
			}
			for _, node := range tc.wantEmpty {
				if !gotEmpty[node] {
					t.Errorf("pickup at %s is not flagged Empty — one flagger clobbered the other's. "+
						"R2 = %s", node, legTrace(r2))
				}
			}
			if len(gotEmpty) != len(tc.wantEmpty) {
				t.Errorf("Empty pickups = %v, want exactly %v", gotEmpty, tc.wantEmpty)
			}
		})
	}
}

// A CONSUME press-index indexes FULL bins forward, so nothing is flagged Empty
// — including the flipped supermarket pickup, which stays a full retrieve.
// Flagging it would put an empty carrier on a line that needs parts.
func TestPressIndexFlipped_ConsumeFlagsNothingEmpty(t *testing.T) {
	t.Parallel()
	_, r2 := BuildTwoRobotPressIndexSwapSteps(
		flipClaim(protocol.ClaimRoleConsume, "INDEX-C", true))
	for _, s := range r2 {
		if s.Empty {
			t.Errorf("consume press-index flagged %s@%s Empty; it indexes FULL bins forward",
				s.Action, s.Node)
		}
	}
}
