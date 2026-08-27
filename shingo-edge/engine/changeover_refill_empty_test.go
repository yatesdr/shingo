package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/engine/changeover"
	"shingoedge/store/processes"
)

// ── THE REFILL-EMPTY INVARIANT, PINNED ACROSS EVERY MODE ────────────────────
//
// A produce node's changeover refill leg fetches a fresh EMPTY carrier. Leave
// the step unflagged and it becomes a payload-matched FULL retrieve of the
// INCOMING part number, aimed at the empty-carrier supermarket — a question no
// inventory can answer. The order does not fail; the group resolver classifies
// it transient and requeues forever, so two presses sit until a person cancels.
// That is Hopkinsville PLN_03/PLN_06 on 2026-08-26 and 2026-08-27.
//
// The invariant had been enforced one builder at a time, by a call placed after
// each builder's step literal. git log -S markInboundEmpty: ten commits, most of
// them fixes re-adding the same call to one more builder after it stalled a
// floor. Six of the eight changeover builders were missing it when this test was
// written; buildTwoRobotChangeoverSwap had never carried it since e02a526b
// (2026-05-03) and had never produced a working order.
//
// ── WHY THIS ITERATES ConfigurableSwapModes ─────────────────────────────────
//
// A hand-kept list of modes is the same failure one level up: mode seven gets
// added, nobody extends the list, the test passes and the floor stalls.
// ConfigurableSwapModes is the set a mode MUST join to be storable on a style
// node claim at all — the claim-upsert allowlist, the editor's dropdown, and its
// drift test all key on it. So a new mode either joins that set and lands here
// automatically, or it is not selectable anywhere. There is no third door.
//
// This is the shape TestEveryStagingDropoffIsDeclared uses for ExclusiveSlot and
// the AllQueueCodes walk uses for queue sentences. Same problem, same remedy.

// refillClaims builds a from/to claim pair carrying every field any mode's
// preflight asks for, so each mode reaches its BUILDER rather than falling out
// at validation — a test that silently planned nothing would pass vacuously.
func refillClaims(mode protocol.SwapMode, role protocol.ClaimRole, keepStaged bool) (from, to processes.NodeClaim) {
	from = fullSwapClaim("N1", "PART-FROM", role)
	to = fullSwapClaim("N1", "PART-TO", role)
	for _, c := range []*processes.NodeClaim{&from, &to} {
		c.SwapMode = mode
		c.PairedCoreNode = "PAIR_N1" // press-index + sequential geometry
	}
	from.KeepStaged = keepStaged
	return from, to
}

// refillPickupsIn returns every pickup step in the action that targets the
// to-claim's inbound source — the refill legs, whichever builder produced them.
func refillPickupsIn(action changeover.NodeAction, inboundSource string) []protocol.ComplexOrderStep {
	var out []protocol.ComplexOrderStep
	for _, spec := range []*changeover.OrderSpec{action.SupplyOrder, action.EvacOrder} {
		if spec == nil || spec.Complex == nil {
			continue
		}
		for _, s := range spec.Complex.Steps {
			if s.Action == protocol.ActionPickup && s.Node == inboundSource {
				out = append(out, s)
			}
		}
	}
	return out
}

func TestEveryChangeoverRefillFetchesAnEmpty(t *testing.T) {
	t.Parallel()
	// pressPositionSwapMode is deliberately NOT in ConfigurableSwapModes — it is
	// the in-memory marker the press-index different-bin-type fan-out synthesizes
	// — so it is appended by hand. It is the ONE mode this test names directly,
	// and it is named because it cannot be reached through the registry.
	modes := append(protocol.ConfigurableSwapModes(), pressPositionSwapMode)

	for _, mode := range modes {
		for _, situation := range []ChangeoverSituation{SituationSwap, SituationEvacuate} {
			for _, keepStaged := range []bool{false, true} {
				name := string(mode) + "/" + string(situation)
				if keepStaged {
					name += "/keep_staged"
				}
				t.Run(name, func(t *testing.T) {
					from, to := refillClaims(mode, protocol.ClaimRoleProduce, keepStaged)
					diff := ChangeoverNodeDiff{
						CoreNodeName: "N1",
						Situation:    situation,
						FromClaim:    &from,
						ToClaim:      &to,
					}
					action := planNodeAction(diff, &processes.Node{ID: 1, Name: "N1"}, false, nil)
					if action.Err != nil {
						t.Fatalf("planning failed, so this mode was never exercised: %v", action.Err)
					}

					refills := refillPickupsIn(action, to.InboundSource)
					if len(refills) == 0 {
						// Not pedantry: a builder that stops fetching a carrier
						// would otherwise make this test pass by doing nothing.
						t.Fatalf("no pickup at inbound source %q — the refill leg vanished", to.InboundSource)
					}
					for i, s := range refills {
						if !s.Empty {
							t.Errorf("refill pickup %d at %q is not Empty: it will hunt a FULL bin of %q in the empty-carrier pool and park on \"no bin of requested payload\"",
								i, s.Node, to.PayloadCode)
						}
						// Empty drops the content match but NOT bin-type
						// compatibility, which resolves against the order's
						// payload — the OUTGOING style's. A refill that names the
						// from-style fetches the carrier type the cell is
						// leaving (N1-c, sim 2026-08-24).
						if s.PayloadCode == from.PayloadCode {
							t.Errorf("refill pickup %d names the OUTGOING style %q; it must name the incoming style or stay silent",
								i, s.PayloadCode)
						}
					}
				})
			}
		}
	}
}

// TestChangeoverRefillStaysFullForConsume pins the dual. Empty is not
// unconditional: a CONSUME node's inbound leg pulls a payload-matched FULL bin
// off the market, and flagging it would send the line an empty carrier. The
// produce-only gate is the whole reason the flag is a decision rather than a
// constant, so it needs a test of its own — otherwise "always set Empty" passes
// everything above.
func TestChangeoverRefillStaysFullForConsume(t *testing.T) {
	t.Parallel()
	for _, mode := range protocol.ConfigurableSwapModes() {
		t.Run(string(mode), func(t *testing.T) {
			from, to := refillClaims(mode, protocol.ClaimRoleConsume, false)
			diff := ChangeoverNodeDiff{
				CoreNodeName: "N1",
				Situation:    SituationSwap,
				FromClaim:    &from,
				ToClaim:      &to,
			}
			action := planNodeAction(diff, &processes.Node{ID: 1, Name: "N1"}, false, nil)
			if action.Err != nil {
				t.Fatalf("planning failed: %v", action.Err)
			}
			for i, s := range refillPickupsIn(action, to.InboundSource) {
				if s.Empty {
					t.Errorf("consume refill pickup %d at %q is flagged Empty — it must fetch a FULL payload-matched bin",
						i, s.Node)
				}
			}
		})
	}
}
