package www

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"shingo/protocol"
)

// TestComposerCharacterizationSwapsAreProtocolModes pins the Flow Composer
// characterization suite's SWAPS list to protocol.AllSwapModes().
//
// That suite walks every (role, swap mode) cell of the composer and asserts
// what each field does there. Its mode list was typed out by hand, so a swap
// mode added to protocol/ would be a composer cell nothing characterizes, and
// the suite would stay green saying it covered them all. It is ALL modes, not
// ConfigurableSwapModes: the suite also pins what the composer does with the
// retired `simple` and with `manual_swap`.
//
// flowspec-data.js's `modes` is not pinned here: that file is generated from
// protocol.ConfigurableSwapModes() and TestStationFlowspecFileMatchesGo fails
// on any hand edit.
func TestComposerCharacterizationSwapsAreProtocolModes(t *testing.T) {
	t.Parallel()
	path := filepath.Join("static", "js", "pages", "composer-fields.characterization.test.js")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := regexp.MustCompile(`const SWAPS = \[([^\]]*)\];`).FindSubmatch(body)
	if m == nil {
		t.Fatalf("%s: `const SWAPS = [...]` not found — has the list moved?", path)
	}
	var js []string
	for _, v := range regexp.MustCompile(`'([a-z_]+)'`).FindAllSubmatch(m[1], -1) {
		js = append(js, string(v[1]))
	}
	var goModes []string
	for _, mode := range protocol.AllSwapModes() {
		goModes = append(goModes, string(mode))
	}
	sort.Strings(js)
	sort.Strings(goModes)
	if len(js) != len(goModes) {
		t.Fatalf("SWAPS = %v, protocol.AllSwapModes() = %v", js, goModes)
	}
	for i := range js {
		if js[i] != goModes[i] {
			t.Fatalf("SWAPS = %v, protocol.AllSwapModes() = %v", js, goModes)
		}
	}
}
