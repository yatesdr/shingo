package scenarios

import (
	"testing"

	"shingo/shared/loadervectors"
	"shingocore/dispatch"
)

// loader_parity_test.go — what is left of the loader cutover's parity harness.
//
// It swept a large space of loader shapes through Core's delivery targets and
// the Edge's own, and failed on any disagreement. The Edge's implementation went when the last Edge decider of a
// loader's windows did (the loader retrieve ask: Core counts and places every
// carrier a loader asks for), and the sweep with it: there is one
// implementation left, and the vectors pin it
// (shingo-core/store/loaders TestDeliveryTargets_GoldenVectors).
//
// This lives in integration/ because it is the only module that imports both
// shingocore and shingoedge; what remains reads Core alone.

// TestSizingVectorsStillDescribeCore checks the checked-in sizing vectors
// against the one implementation that is left.
//
// This used to compare Core against the Edge. The Edge's sizing arithmetic went
// with the cutover, and so did the sweep that compared them — a green test
// asserting Core matches a photocopy of code that no longer exists is worse
// than no test, because it reads as coverage. What survives is the vectors
// themselves, which go on pinning Core to the answers the plant actually ran.
func TestSizingVectorsStillDescribeCore(t *testing.T) {
	t.Parallel()
	v := loadervectors.MustLoad()
	for _, c := range v.Sizing {
		coreBins, coreOutcome, _ := dispatch.BinsToReachThreshold(c.Threshold, c.CurrentUOP, c.PerBinCapacity)
		if coreBins != c.WantBins || string(coreOutcome) != c.WantOutcome {
			t.Errorf("vector %q: core gives %d/%s, vector says %d/%s", c.Name, coreBins, coreOutcome, c.WantBins, c.WantOutcome)
		}
	}
}
