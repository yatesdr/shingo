package www

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"shingo/protocol"
)

// mission-detail.js used to carry its own copy of the vendor→Core state
// mapping, and it had drifted from fleet.Backend.MapState — the mapping the
// engine actually dispatches on — in three places. RDS CREATED read as
// "created" where Core says dispatched, FINISHED as "completed" where Core says
// delivered, and FAILED as "failed" where Core says FAULTED. The last one is
// the one that mattered: faulted is the non-terminal grace state with a
// recovery timer running, failed is terminal, and the page reported a mission
// dead while Core still expected it back.
//
// The mapping now happens once, server-side (handlers_missions.go). These two
// tests are what stop a second copy growing back — the same job
// order_status_js_drift_test.go does for the Edge HMI, which is where the shape
// is borrowed from.

var missionDetailJS = filepath.Join("static", "pages", "mission-detail.js")

// TestMissionDetailStateColorsAreRealStatuses pins the hue table's keys to the
// protocol status enum.
//
// The table used to be keyed on raw RDS words while the badge rendered beside it
// was keyed on mapped labels — two vocabularies deciding one row's appearance,
// which is how the segment and its own badge ended up painted different colours
// for the same event. One key now, and this asserts it is the Core one: a hue
// keyed on a word that is not a status can only ever be dead or wrong.
func TestMissionDetailStateColorsAreRealStatuses(t *testing.T) {
	src := readMissionDetailJS(t)

	block := captureBetween(t, src, "var stateColors = {", "};")
	keys := regexp.MustCompile(`'([a-z_]+)'\s*:`).FindAllStringSubmatch(block, -1)
	if len(keys) == 0 {
		t.Fatalf("%s: found no keys in stateColors — has the table been renamed?", missionDetailJS)
	}

	known := map[string]bool{}
	for _, s := range protocol.AllStatuses() {
		known[string(s)] = true
	}
	for _, m := range keys {
		if !known[m[1]] {
			t.Errorf("stateColors key %q is not a protocol status — the hue table has drifted back "+
				"onto a second vocabulary (protocol.AllStatuses: %v)", m[1], protocol.AllStatuses())
		}
	}
}

// TestMissionDetailCarriesNoVendorStateMap fails if the page starts translating
// vendor states again.
//
// Deliberately a ban on the VALUES rather than on a function name: the defect
// was not that a function called stateLabel existed, it was that a second copy
// of the mapping existed anywhere on this page. Renaming it would sidestep a
// name-based check and reintroduce exactly the drift.
//
// STOPPED is exempt and stays a raw comparison on purpose — missionWasStopped
// asks what the FLEET did, so the vendor's own word is the right thing to test
// there. It is exempted by name so that the exemption is a decision rather than
// a hole: any OTHER vendor state appearing in this file is a new second mapper.
func TestMissionDetailCarriesNoVendorStateMap(t *testing.T) {
	src := readMissionDetailJS(t)

	// Vendor order states, from fleet/seerrds/mappers.go's switch. STOPPED is
	// omitted — see the doc comment.
	for _, vendorState := range []string{"CREATED", "TOBEDISPATCHED", "RUNNING", "WAITING", "FINISHED", "FAILED"} {
		if strings.Contains(src, "'"+vendorState+"'") || strings.Contains(src, `"`+vendorState+`"`) {
			t.Errorf("%s references vendor state %q. The vendor→Core mapping belongs to "+
				"fleet.Backend.MapState and is applied server-side in handlers_missions.go; a copy here "+
				"is the drift that reported faulted missions as failed.", missionDetailJS, vendorState)
		}
	}
}

func readMissionDetailJS(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(missionDetailJS)
	if err != nil {
		t.Fatalf("read %s: %v", missionDetailJS, err)
	}
	return string(body)
}

// captureBetween returns the text between the first open marker and the next
// close marker after it. Fails loudly rather than returning "" so a renamed
// marker is a red test instead of a vacuously passing one — the failure mode
// §16 of the reshuffle design spends a page on.
func captureBetween(t *testing.T, src, open, closeMark string) string {
	t.Helper()
	_, rest, found := strings.Cut(src, open)
	if !found {
		t.Fatalf("%s: marker %q not found — the test can no longer see what it claims to check", missionDetailJS, open)
	}
	body, _, found := strings.Cut(rest, closeMark)
	if !found {
		t.Fatalf("%s: no %q after %q", missionDetailJS, closeMark, open)
	}
	return body
}
