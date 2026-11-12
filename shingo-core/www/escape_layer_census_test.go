package www

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// escapeLeftToPage are the Escape tests in Core's browser code that are not a
// page closing its modals, as "path<TAB>the line", each with why it stays.
// An entry stands for one line: a second line that reads the same is not
// listed by it. The list fails on a stale entry, so the change that moves one
// onto the layer deletes its line.
var escapeLeftToPage = map[string]string{
	// An input's own Escape: thrKey puts the stored threshold back in the
	// box and leaves it. It closes nothing.
	"static/pages/inventory.js\t} else if (e.key === 'Escape') {": "the threshold box's own Escape (thrKey): puts the stored value back",
	// The two drill pop-ups draw their own overlay and close it themselves
	// (Escape, backdrop, ✕, a 60 s idle timer); they are not on the layer.
	"static/components/CellDrill.js\tstate.onEsc = (e) => { if (e.key === 'Escape') closeCellDrill(); };": "the cell drill's own overlay, not on the layer",
	"static/components/DrillModal.js\tstate.onEsc = (e) => { if (e.key === 'Escape') close(); };":         "the metric drill's own overlay, not on the layer",
}

// TestCorePagesLeaveEscapeToTheLayer: on a Core page Escape belongs to the one
// overlay stack (shared/layer.js, style guide §10). It closes the innermost
// entry first, and asks before it discards typed work. A page's own document
// Escape handler ran before the layer's and closed the page's modals as a
// deliberate close. One Escape then shut a pop-up and the confirm opened over
// it, and typed work went without a question. So a page may still test for
// Escape only for what is not a layer entry, and only while the layer is empty
// (`layerDepth()` on the same line). The page keeps its close-time state in
// the entry's onClose, which runs however the entry closed. Anything else is
// listed above with its reason.
func TestCorePagesLeaveEscapeToTheLayer(t *testing.T) {
	escape := regexp.MustCompile(`\.key\s*[!=]==?\s*['"]Escape['"]`)
	seen := map[string]int{}
	var bad []string
	for path, src := range pageJS(t) {
		for i, line := range strings.Split(stripLineComments(src), "\n") {
			if !escape.MatchString(line) {
				continue
			}
			key := path + "\t" + strings.TrimSpace(line)
			if _, ok := escapeLeftToPage[key]; ok {
				if seen[key]++; seen[key] == 1 {
					continue
				}
			}
			if strings.Contains(line, "layerDepth()") {
				continue
			}
			bad = append(bad, path+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("a page's own Escape beside the layer (leave it to the layer; keep close-time state in onClose, or test !layerDepth() for what is not an entry): %s", b)
	}
	for key, why := range escapeLeftToPage {
		if seen[key] == 0 {
			t.Errorf("escapeLeftToPage lists %q (%s), which is no longer there: delete the entry", key, why)
		}
	}
}
