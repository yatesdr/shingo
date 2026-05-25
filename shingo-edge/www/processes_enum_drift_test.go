package www

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"shingo/protocol"
)

// TestProcessesTemplateSwapModeOptions pins the <option value="..."> set
// in the Swap Mode dropdown to protocol.AllSwapModes(). Removing a swap
// mode from the protocol without removing its <option> (or vice versa)
// fails this test in CI.
//
// "simple" is included as a hidden option — it's the default swap mode
// for bare delivery (no swap choreography). New claims default to
// single_robot in the editor, so "simple" is not user-selectable, but
// existing claims with swap_mode="simple" must still render in edit mode.
func TestProcessesTemplateSwapModeOptions(t *testing.T) {
	got := readSelectOptions(t, "claims-add-swap")
	want := make([]string, 0, len(protocol.AllSwapModes()))
	for _, m := range protocol.AllSwapModes() {
		want = append(want, string(m))
	}
	assertSameSet(t, "swap_mode", got, want)
}

// TestProcessesTemplateClaimRoleOptions pins the Role dropdown options
// to protocol's ClaimRole constants. Pattern identical to swap mode.
//
// Post-cleanup: protocol has 2 roles (consume, produce). The legacy
// "changeover" role was removed — changeover mechanics are now driven
// entirely by swap_mode + EvacuateOnChangeover on the active claim.
func TestProcessesTemplateClaimRoleOptions(t *testing.T) {
	got := readSelectOptions(t, "claims-add-role")
	want := []string{
		string(protocol.ClaimRoleConsume),
		string(protocol.ClaimRoleProduce),
	}
	assertSameSet(t, "claim_role", got, want)
}

// readSelectOptions parses processes.html for a <select id="..."> block
// and returns the value attributes of every <option> in it (including
// hidden ones). The HTML is hand-written, not template-generated for
// these specific selects, so a regex parse is robust enough.
func readSelectOptions(t *testing.T, selectID string) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("templates", "processes.html"))
	if err != nil {
		t.Fatalf("read processes.html: %v", err)
	}
	src := string(body)

	// Find the <select id="..."> opening, then capture up to the next </select>.
	selectRe := regexp.MustCompile(`(?s)<select[^>]*\bid="` + regexp.QuoteMeta(selectID) + `"[^>]*>(.*?)</select>`)
	m := selectRe.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("could not find <select id=%q> in processes.html", selectID)
	}

	optRe := regexp.MustCompile(`<option[^>]*\bvalue="([^"]*)"`)
	matches := optRe.FindAllStringSubmatch(m[1], -1)
	out := make([]string, 0, len(matches))
	for _, mm := range matches {
		if mm[1] == "" {
			continue // skip the placeholder "-- Select --"
		}
		out = append(out, mm[1])
	}
	return out
}

func assertSameSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	if len(g) != len(w) {
		t.Fatalf("%s: html len=%d (%v) vs protocol len=%d (%v)",
			label, len(g), g, len(w), w)
	}
	for i := range g {
		if g[i] != w[i] {
			t.Errorf("%s mismatch at index %d: html=%q protocol=%q (html=%v protocol=%v)",
				label, i, g[i], w[i], g, w)
		}
	}
}
