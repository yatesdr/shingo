package domain_test

import (
	"testing"

	"shingo/shared/loadervectors"
	"shingoedge/domain"
)

// TestReservationTarget_GoldenVectors runs the Edge's delivery-target logic
// against the same shared vectors Core runs against.
//
// This is the half of the parity gate that will outlive the other. When the
// cutover deletes the Edge threshold path, this test goes with it — but the
// vectors stay, and Core keeps being held to the answers the plant actually ran.
// Until then, a change to either implementation that the other does not follow
// fails here or on the Core side.
func TestReservationTarget_GoldenVectors(t *testing.T) {
	t.Parallel()
	v := loadervectors.MustLoad()

	for _, c := range v.Targets {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if c.CoreOnly != "" {
				t.Skipf("core-only vector: %s", c.CoreOnly)
			}
			l := buildLoader(t, c)

			// The Edge asks the question as "is multi-window on", which is the
			// negation of the loader's funnel setting. The vectors state the
			// restriction, matching the stored column.
			nodes, budget := l.ReservationTarget(domain.NodeID(c.Member), domain.PayloadCode(c.Payload), !c.Loader.FunnelWindows)

			if budget != c.WantBudget {
				t.Errorf("budget = %d, want %d\nwhy this case exists: %s", budget, c.WantBudget, c.Why)
			}
			got := make([]string, len(nodes))
			for i, n := range nodes {
				got[i] = string(n)
			}
			if len(got) != len(c.WantNodes) {
				t.Fatalf("nodes = %v, want %v\nwhy this case exists: %s", got, c.WantNodes, c.Why)
			}
			for i := range got {
				if got[i] != c.WantNodes[i] {
					t.Fatalf("nodes = %v, want %v (order matters: the funnel takes the first and spreading fills in this order)\nwhy this case exists: %s",
						got, c.WantNodes, c.Why)
				}
			}
		})
	}
}

// buildLoader constructs the Edge aggregate for a vector.
//
// Windows are handed to the constructor IN NAME ORDER, because that is the order
// the running Edge builds them in: the cache read sorts by position_node, and no
// ordinal survives the trip from Core. Handing them over in the vector's written
// order instead would make this test assert something the plant does not do.
func buildLoader(t *testing.T, c loadervectors.TargetCase) *domain.Loader {
	t.Helper()
	id := domain.LoaderID("loader:vector")

	switch c.Loader.Layout {
	case string(domain.LayoutSharedWindow):
		names := make([]string, 0, len(c.Loader.Homes))
		for _, h := range c.Loader.Homes {
			names = append(names, h.Node)
		}
		sortStrings(names)
		windows := make([]domain.Window, len(names))
		for i, n := range names {
			windows[i] = domain.Window{Node: domain.NodeID(n)}
		}
		set := make([]domain.PayloadCode, 0, len(c.Loader.PayloadSet))
		for _, p := range c.Loader.PayloadSet {
			set = append(set, domain.PayloadCode(p.Code))
		}
		l, err := domain.NewSharedWindowLoader(id, "vector", domain.RoleProduce, domain.ReplenishmentThreshold,
			windows, set, domain.WithFunnelWindows(c.Loader.FunnelWindows))
		if err != nil {
			t.Fatalf("build shared loader for vector %q: %v", c.Name, err)
		}
		return l

	case string(domain.LayoutDedicatedPositions):
		positions := make([]domain.Position, 0, len(c.Loader.Homes))
		for _, h := range c.Loader.Homes {
			positions = append(positions, domain.Position{
				Node:    domain.NodeID(h.Node),
				Payload: domain.PayloadCode(h.Payload),
			})
		}
		l, err := domain.NewDedicatedPositionsLoader(id, "vector", domain.RoleProduce, domain.ReplenishmentThreshold,
			positions, domain.WithFunnelWindows(c.Loader.FunnelWindows))
		if err != nil {
			t.Fatalf("build dedicated loader for vector %q: %v", c.Name, err)
		}
		return l
	}
	t.Fatalf("vector %q has unknown layout %q", c.Name, c.Loader.Layout)
	return nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
