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

// TestMissionDetailStageClassIsTheStatusSet pins the stage table's class map
// to the protocol status enum, in both directions, and the four classes to
// their labels and colours.
//
// Every key must be a real status: the page decides a span's class and its
// "terminal" end row by looking a status up here, so a key spelled in some
// other vocabulary is dead or wrong. And the set must be whole: every
// non-terminal status has a key, and no terminal status does. A status added to
// protocol/ without a class would otherwise read as terminal and silently end
// the order's life at the first row that held it.
//
// This replaces the pin on the retired stateColors hue table, which was keyed on
// the same enum for the same reason.
func TestMissionDetailStageClassIsTheStatusSet(t *testing.T) {
	src := readMissionDetailJS(t)

	block := captureBetween(t, src, "var STAGE_CLASS = {", "};")
	keys := regexp.MustCompile(`'([a-z_]+)'\s*:\s*'(waiting|moving|held|confirm)'`).FindAllStringSubmatch(block, -1)
	if len(keys) == 0 {
		t.Fatalf("%s: found no keys in STAGE_CLASS — has the table been renamed?", missionDetailJS)
	}
	if n := strings.Count(block, ":"); n != len(keys) {
		t.Errorf("STAGE_CLASS has %d entries but %d map to waiting|moving|held|confirm — a class outside the bar's four", n, len(keys))
	}

	classed := map[string]bool{}
	for _, m := range keys {
		classed[m[1]] = true
	}

	// Delivered is "waiting for confirm" (round-2 ruling 7, 2026-10-05): no
	// robot is on the order after delivery, so it is not held.
	if !regexp.MustCompile(`'delivered'\s*:\s*'confirm'`).MatchString(block) {
		t.Errorf("STAGE_CLASS: delivered must be 'confirm' (waiting for confirm), not held — no robot is on a delivered order")
	}

	// Every class the map uses has a legend label and a bar colour; a class
	// without either draws an unlabelled or invisible segment.
	labels := captureBetween(t, src, "var CLASS_LABEL = {", "};")
	css, err := os.ReadFile(filepath.Join("static", "style.css"))
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	for _, m := range keys {
		cls := m[2]
		if !strings.Contains(labels, cls+":") {
			t.Errorf("CLASS_LABEL has no label for class %q", cls)
		}
		if !strings.Contains(string(css), ".mission-stages .stage-"+cls+" {") {
			t.Errorf("style.css has no .mission-stages .stage-%s rule — the segment would not draw", cls)
		}
	}
	for _, s := range protocol.AllStatuses() {
		switch {
		case protocol.IsTerminal(s) && classed[string(s)]:
			t.Errorf("STAGE_CLASS classes terminal status %q — a terminal status ends the order's life, it does not occupy it", s)
		case !protocol.IsTerminal(s) && !classed[string(s)]:
			t.Errorf("STAGE_CLASS has no class for non-terminal status %q — the page would read it as terminal "+
				"and end the order there", s)
		}
		delete(classed, string(s))
	}
	for k := range classed {
		t.Errorf("STAGE_CLASS key %q is not a protocol status (protocol.AllStatuses: %v)", k, protocol.AllStatuses())
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
