//go:build docker

package main

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/dispatch/binresolver"
	"shingocore/store/bins"
)

// b1_pins_docker_test.go — B1's eligibility parity assertion. It was a
// verify-red pinned on the Stage A branch before any planner source changed, and
// became a plain assertion when B1 fixed the Go twin. The FIFO tie's assertions
// are in store/bins (b1_fifo_tiebreak_pin_test.go).

// TestB1_EligibilityParity_StagedAndTransit: the two eligibility questions
// (SYNTH F6), asked of the two rows the planner golden records where their
// spellings disagree (sourceable/every-bin.json). Sourceable is bins.BinSourceableSQL,
// "may the plant source this bin"; PickableAt is binresolver.BinUnavailableReason,
// "may I pick up what is at this named node".
//
// The owner's ruling (2026-09-26) splits the two rows:
//
//   - _TRANSIT: BOTH REFUSE. A bin on the synthetic _TRANSIT node is on a robot,
//     not at any node. The SQL always refused it; the Go twin called it
//     available until B1 (flat bug, was a verify-red here).
//   - STAGED IS A DIVERGENCE, BY DESIGN. Sourceable refuses a staged bin ("staged
//     keeps its guards": no line stealing), and PickableAt picks one, because a
//     bin arriving at a line node is staged and "what is at this node" must see
//     it (IsAvailableAtConcreteNode, the node-local tier, binsource's
//     Drain_staged_partial_is_visible). A plain assertion, green before and after.
func TestB1_EligibilityParity_StagedAndTransit(t *testing.T) {
	t.Parallel()
	g := newGoldenPlant(t)
	g.exec(`UPDATE bins SET status = 'staged' WHERE label = 'BIN-STUD-01'`)
	g.exec(`UPDATE bins SET node_id = (SELECT id FROM nodes WHERE name = '_TRANSIT') WHERE label = 'BIN-BRKT-03'`)

	verdicts := func(label string) (sqlSays bool, goReason string) {
		b := g.bin(label)
		testutil.MustNoErr(t, g.db.DB.QueryRow(`SELECT EXISTS (SELECT 1 FROM bins b LEFT JOIN nodes n ON n.id = b.node_id WHERE b.id = $1 AND `+
			bins.BinSourceableSQL("b.payload_code")+`)`, b.ID).Scan(&sqlSays), "SQL verdict for "+label)
		rule, err := g.db.LoadBinTypeRule(b.PayloadCode)
		testutil.MustNoErr(t, err, "bin type rule for "+label)
		return sqlSays, binresolver.BinUnavailableReason(b, b.PayloadCode, rule)
	}

	// The documented divergence: Sourceable refuses staged, PickableAt picks it.
	if sqlSays, goReason := verdicts("BIN-STUD-01"); sqlSays || goReason != "" {
		t.Errorf("staged BIN-STUD-01: want Sourceable=false and PickableAt=pickable (the divergence by design), "+
			"got Sourceable=%v, PickableAt reason %q", sqlSays, goReason)
	}

	// Parity: both refuse a bin on _TRANSIT.
	if sqlSays, goReason := verdicts("BIN-BRKT-03"); sqlSays || goReason == "" {
		t.Errorf("BIN-BRKT-03 on _TRANSIT: want both questions to refuse it, got Sourceable=%v, PickableAt reason %q",
			sqlSays, goReason)
	}
}
