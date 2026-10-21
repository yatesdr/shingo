package loaders_test

import (
	"testing"

	"shingo/shared/loadervectors"
	"shingocore/store/loaders"
)

// TestDeliveryTargets_GoldenVectors runs Core's delivery-target logic against
// the shared vectors: which windows a loader's carrier may go to, in what
// order, and the never-2N budget. It is the one implementation: the Edge asks
// Core for every carrier a loader asks for (the loader retrieve ask) and keeps
// no copy, so a change here is a change to where carriers go, and a failed
// vector says which case moved.
func TestDeliveryTargets_GoldenVectors(t *testing.T) {
	t.Parallel()
	v := loadervectors.MustLoad()

	for _, c := range v.Targets {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			in := inputFor(c)
			targets, budget := loaders.DeliveryTargets(in)

			if budget != c.WantBudget {
				t.Errorf("budget = %d, want %d\nwhy this case exists: %s", budget, c.WantBudget, c.Why)
			}
			got := make([]string, len(targets))
			for i, tg := range targets {
				got[i] = tg.NodeName
			}
			if !sameOrder(got, c.WantNodes) {
				t.Errorf("nodes = %v, want %v (order matters: the funnel takes the first and spreading fills in this order)\nwhy this case exists: %s",
					got, c.WantNodes, c.Why)
			}
		})
	}
}

// inputFor converts a vector's loader shape into the Core call. Node ids are
// synthesised from the slice index — Core resolves ids to names and the vectors
// speak names, so the mapping just has to be consistent within a case.
func inputFor(c loadervectors.TargetCase) loaders.DeliveryTargetsInput {
	homes := make([]loaders.Home, 0, len(c.Loader.Homes))
	names := make(map[int64]string, len(c.Loader.Homes))
	for i, h := range c.Loader.Homes {
		id := int64(i + 1)
		homes = append(homes, loaders.Home{
			PositionNodeID: id,
			PayloadCode:    h.Payload,
			Kind:           h.Kind,
			SortOrder:      h.SortOrder,
		})
		names[id] = h.Node
	}
	payloads := make([]loaders.Payload, 0, len(c.Loader.PayloadSet))
	for _, p := range c.Loader.PayloadSet {
		payloads = append(payloads, loaders.Payload{PayloadCode: p.Code, UOPThreshold: p.UOPThreshold})
	}
	return loaders.DeliveryTargetsInput{
		Layout:        c.Loader.Layout,
		FunnelWindows: c.Loader.FunnelWindows,
		Homes:         homes,
		NodeNames:     names,
		Payloads:      payloads,
		Member:        c.Member,
		PayloadCode:   c.Payload,
	}
}

func sameOrder(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestDeliveryTargets_VectorsCoverBothLayouts guards the vectors themselves. A
// file that lost its dedicated cases, or its funnel cases, would still pass
// every assertion above while pinning nothing.
func TestDeliveryTargets_VectorsCoverBothLayouts(t *testing.T) {
	t.Parallel()
	v := loadervectors.MustLoad()
	var shared, dedicated, funnel int
	for _, c := range v.Targets {
		switch c.Loader.Layout {
		case loaders.LayoutSharedWindow:
			shared++
		case loaders.LayoutDedicatedPositions:
			dedicated++
		default:
			t.Errorf("vector %q has unknown layout %q", c.Name, c.Loader.Layout)
		}
		if c.Loader.FunnelWindows {
			funnel++
		}
	}
	if shared == 0 || dedicated == 0 || funnel == 0 {
		t.Errorf("vector coverage: shared=%d dedicated=%d funnel=%d; each must be non-zero or the file pins less than it appears to",
			shared, dedicated, funnel)
	}
}
