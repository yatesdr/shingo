package engine

import (
	"testing"

	"shingo/protocol"
)

// TestBuildSequentialBackfillSteps_ProduceMarksInboundEmpty pins the latent-bug
// fix: a PRODUCE node's sequential backfill must fetch a fresh EMPTY carrier from
// the supermarket (the store dual of consume's full retrieve), so its inbound-source
// pickup must be flagged Empty. Before the fix this leg defaulted to a full retrieve
// and a produce A/B backfill hunted a full payload bin in the empty pool → dispatch
// failed ("no bin of requested payload"), stranding produce-side A/B after one removal.
func TestBuildSequentialBackfillSteps_ProduceMarksInboundEmpty(t *testing.T) {
	t.Parallel()
	steps := BuildSequentialBackfillSteps(dispatchClaim("sequential"))
	emptyPickups := 0
	for _, s := range steps {
		if s.Empty {
			if s.Action == "pickup" && s.Node == "INBOUND-SRC" {
				emptyPickups++
			} else {
				t.Errorf("only the InboundSource pickup may be flagged empty; got %+v", s)
			}
		}
	}
	if emptyPickups != 1 {
		t.Errorf("produce backfill InboundSource empty-flag count = %d, want 1", emptyPickups)
	}
}

// TestBuildSequentialBackfillSteps_ConsumeLeavesFullRetrieve is the dual: a CONSUME
// node's backfill fetches a payload-matched FULL bin to consume, so no leg may be
// flagged Empty.
func TestBuildSequentialBackfillSteps_ConsumeLeavesFullRetrieve(t *testing.T) {
	t.Parallel()
	claim := dispatchClaim("sequential")
	claim.Role = protocol.ClaimRoleConsume
	for _, s := range BuildSequentialBackfillSteps(claim) {
		if s.Empty {
			t.Errorf("consume backfill must not flag any empty leg; got %+v", s)
		}
	}
}
