package www

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"shingoedge/engine"
)

var updateCatidHealthPageGolden = flag.Bool("update-catid-health-page-golden", false,
	"rewrite testdata/catid-health-page/*.txt from the Processes page's health strip")

// TestCatidHealthPagePins characterises the CATID health strip on the
// Processes page (processes-desktop.js refreshCatidHealth, catid-health.js) as
// it runs today, before S4 takes the strip off the list:
//
//   - requests-list.txt: the requests a boot of the list and each 10 s tick
//     send with 0, 1 and 3 processes the list shows (1 summary read plus one
//     items read per shown process), focus and visibility (none), and the two
//     refusals (an items read: the row falls back to its headline; the
//     summary: the strip is left as it was);
//   - strip.txt: what the strip draws from those answers, and item 5's two
//     styles whose names differ by a trailing space;
//   - requests-process.txt: a process open reads its items on the Settings
//     tab only, never on the tick, and draws the info items as its Notes;
//   - sse-hint.txt: the page opens no /events stream, so a "catid-health"
//     frame makes it fetch nothing.
//
// The answers are TestCatidHealthBodiesPins' goldens (testdata/catid-health/),
// so regenerate that one first. The harness is testdata/catid-health-harness
// (pins.js), on U0's page harness, unmodified. Regenerate with
//
//	go test ./www -run TestCatidHealthPagePins -update-catid-health-page-golden
//
// and read the diff.
func TestCatidHealthPagePins(t *testing.T) {
	t.Parallel()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the health strip page pins")
	}
	cmd := exec.Command(nodePath, filepath.Join("testdata", "catid-health-harness", "pins.js"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("run the health strip pins: %v\n%s", err, stderr.String())
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode the pins' output: %v\n%s", err, stderr.String())
	}
	if len(got) == 0 {
		t.Fatal("the pins produced no golden; the harness has drifted")
	}
	checkGoldenDir(t, filepath.Join("testdata", "catid-health-page"), got, *updateCatidHealthPageGolden,
		"-update-catid-health-page-golden")
}

// TestCatidHealthHint_FrameIsDurable pins the hint's server half as it is:
// EventCATIDHealthChanged goes out as the SSE event "catid-health" with the
// process's id, on the durable queue (www/sse.go SetupEngineListeners).
func TestCatidHealthHint_FrameIsDurable(t *testing.T) {
	t.Parallel()
	if lossySSETopics["catid-health"] {
		t.Error("catid-health is classified lossy")
	}
	hub := NewEventHub()
	eng := &engine.Engine{Events: engine.NewEventBus()}
	hub.SetupEngineListeners(eng)
	eng.Events.Emit(engine.Event{Type: engine.EventCATIDHealthChanged,
		Payload: engine.CATIDHealthChangedEvent{ProcessID: 7}})
	select {
	case evt := <-hub.broadcast:
		data, err := json.Marshal(evt.Data)
		if evt.Type != "catid-health" || err != nil || string(data) != `{"process_id":7}` {
			t.Errorf("broadcast %q %s (%v), want catid-health {\"process_id\":7}", evt.Type, data, err)
		}
	default:
		t.Error("no SSE event for EventCATIDHealthChanged")
	}
}

// TestCatidHealthHint_NoEdgePageListens pins the hint's other half as it is
// today, a gap: no Edge page or template listens for "catid-health". The
// Processes page opens no stream (sse-hint.txt); the pages that do
// (createSSE, onSSE, htmx sse triggers) name no handler for it. A listener
// added later fails this and is listed, which is the point: the change that
// adds it updates this pin.
func TestCatidHealthHint_NoEdgePageListens(t *testing.T) {
	t.Parallel()
	// The ways an Edge page subscribes to an SSE type: createSSE's handler
	// key, onSSE's or a station's type string, an htmx trigger. A fetch URL
	// (/api/catid-health) is none of them.
	listen := regexp.MustCompile(`\bonCatidHealth\b|sse:catid-health|['"]catid-health['"]`)
	var found []string
	for _, root := range []string{"static", "templates"} {
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if ext := filepath.Ext(p); ext != ".js" && ext != ".html" {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(b), "\n") {
				if listen.MatchString(line) {
					found = append(found, filepath.ToSlash(p)+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	sort.Strings(found)
	if len(found) != 0 {
		t.Errorf("a page now listens for the catid-health hint; update this pin and sse-hint.txt:\n  %s",
			strings.Join(found, "\n  "))
	}
}
