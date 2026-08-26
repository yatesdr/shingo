package dispatch

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Every fleet-request builder must decide the vehicle pin.
//
// A recovery order is pinned to ONE robot — the one with the bin on its deck —
// and pinnedVehicleFor is what turns order.RobotID plus the on-deck intent into
// a Vehicle on the request. A builder that omits it does not fail: it sends an
// UNPINNED request, the fleet gives the job to whichever robot is nearest, and
// that robot drives to a destination to unload a bin it is not carrying. There
// is nothing to see in the order row, and no test would fail.
//
// So the guard is a census of the construction sites, not a test of one path.
// Three builders carry the pin today. A fourth fails this test until somebody
// has decided which it is.
// ---------------------------------------------------------------------------

// fleetRequestBuilders are the files that construct a fleet.CreateOrderRequest,
// mapped to whether that construction must carry pinnedVehicleFor.
var fleetRequestBuilders = map[string]string{
	"dispatcher.go":         "pinned: the plain dispatch path, and the one a recovery order takes",
	"complex_dispatch.go":   "pinned: multi-step orders reach the fleet from here",
	"lane_gate_dispatch.go": "pinned: the lane-admitted path builds its own request",
}

var (
	createOrderRequestLit = regexp.MustCompile(`fleet\.CreateOrderRequest\{`)
	vehiclePin            = regexp.MustCompile(`Vehicle:\s*pinnedVehicleFor\(`)
)

func TestFleetRequestBuildersCarryTheVehiclePin(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	var missing, uncensused []string
	found := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(src)
		builds := createOrderRequestLit.FindAllString(body, -1)
		if len(builds) == 0 {
			continue
		}
		found[name] = true
		if _, censused := fleetRequestBuilders[name]; !censused {
			uncensused = append(uncensused, name)
			continue
		}
		if pins := vehiclePin.FindAllString(body, -1); len(pins) < len(builds) {
			missing = append(missing, name)
		}
	}

	sort.Strings(uncensused)
	sort.Strings(missing)

	if len(uncensused) > 0 {
		t.Errorf("file(s) build a fleet.CreateOrderRequest and are not censused:\n  %s\n\n"+
			"Decide whether this request can ever carry a pinned order. If it can, set "+
			"Vehicle: pinnedVehicleFor(order) and add the file to fleetRequestBuilders. An "+
			"unpinned recovery order sends whichever robot is nearest to unload a bin it is "+
			"not carrying, and nothing in the order row shows it.",
			strings.Join(uncensused, "\n  "))
	}
	if len(missing) > 0 {
		t.Errorf("censused builder(s) construct a request without pinnedVehicleFor:\n  %s",
			strings.Join(missing, "\n  "))
	}
	for name := range fleetRequestBuilders {
		if !found[name] {
			t.Errorf("fleetRequestBuilders names %s, which no longer builds a fleet request — "+
				"delete the entry rather than leaving the census claiming coverage", name)
		}
	}
}
