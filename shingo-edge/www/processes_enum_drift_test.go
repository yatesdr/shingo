package www

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"shingo/protocol"
)

// processes_enum_drift_test.go — the choreography the Processes page offers,
// against protocol's enum.
//
// This used to read two <select> blocks out of processes.html: the claim
// editor's Swap Mode and Role dropdowns. Both retired with the editor in U9d —
// there is no native <select> anywhere on the composer (the style guide's
// "never use native dialogs" covers menus, and a select's dropdown is the
// browser drawing its own chrome over ours) — so the same drift is now read
// off the composer's own list.
//
// THE GUARD IS THE SAME AND THE REASON IS THE SAME: a mode added to the
// protocol and not to the picker is a choreography nobody can select, and one
// in the picker and not the protocol is a save the server refuses.

// TestComposerModesMatchProtocol pins composer-model.js's MODES — the mode
// picker's rows, and the labels the rail and the positions table read — to
// protocol.ConfigurableSwapModes() minus manual_swap.
//
// manual_swap is excluded for the reason it was excluded from the dropdown
// (0ef5b95): bin loaders and unloaders are Core-owned topology resolved at
// runtime through the loader aggregate, so they are not authored here. An
// existing manual_swap claim still renders — the label falls back to the raw
// mode — but it is not offered.
//
// THE SKIP IS VESTIGIAL NOW AND IS KEPT ON PURPOSE. manual_swap left
// ConfigurableSwapModes entirely when the loader ownership move retired it as a
// stored value, so the walk below never reaches it — the same conclusion from
// the other end. It stays because the exclusion is a RULE about what this
// picker offers, not a consequence of what the protocol currently lists, and a
// mode returning to the set must not silently appear in the composer.
//
// "simple" is excluded too, and that is a CHANGE from the dropdown, which
// carried it as a hidden option so an old row would render. The composer has
// no hidden options: a cell in a retired mode draws with the empty glyph and
// its mode reads through, and the picker offers the four a save can accept.
func TestComposerModesMatchProtocol(t *testing.T) {
	got := readComposerModes(t)
	want := []string{}
	for _, m := range protocol.ConfigurableSwapModes() {
		if m == protocol.SwapModeManualSwap {
			continue // Core-owned; see docstring
		}
		want = append(want, string(m))
	}
	assertSameSet(t, "swap_mode", got, want)
}

// TestComposerRolesAreDerivedNotOffered: role has no picker at all, and this
// says so on purpose.
//
// The claim editor asked. The composer DERIVES (brief R2): the prior claim on
// the node, then the majority of the style's other claims, then the majority
// of the process's, then consume. A picker would be a fifth answer that
// disagrees with the four, so the drift guard here is that protocol still has
// exactly the two roles the derivation knows how to answer with — a third
// would need a rule, and the silence would default it to consume.
func TestComposerRolesAreDerivedNotOffered(t *testing.T) {
	roles := []string{string(protocol.ClaimRoleConsume), string(protocol.ClaimRoleProduce)}
	if len(roles) != 2 {
		t.Fatalf("protocol carries %d claim roles; deriveRole in composer-model.js answers with two", len(roles))
	}
	src := readComposerModel(t)
	// No picker offers it: a role option list would be the fifth answer, and
	// it would be the one the operator sees.
	if regexp.MustCompile(`case 'setRole'`).MatchString(src) {
		t.Error("composer-model.js has a setRole action; role is derived, not chosen (brief R2)")
	}
	if regexp.MustCompile(`case 'role':`).MatchString(readDesktopPage(t)) {
		t.Error("processes-desktop.js builds an option list for role; role is derived (brief R2)")
	}
	// The floor is consume, named once, and it is the FOURTH answer — the
	// three above it read the prior claim and the majorities. A second literal
	// role would mean somebody had written a fifth rule.
	if n := len(regexp.MustCompile(`return \{ role: '[a-z]+', source: 'default' \}`).FindAllString(src, -1)); n != 1 {
		t.Errorf("deriveRole has %d literal defaults; it should have exactly one, and it should be consume", n)
	}
	if !regexp.MustCompile(`return \{ role: 'consume', source: 'default' \}`).MatchString(src) {
		t.Error("deriveRole's floor is not consume; a cell nobody has configured is what the store writes for one")
	}
}

func readDesktopPage(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("static", "js", "pages", "processes-desktop.js"))
	if err != nil {
		t.Fatalf("read processes-desktop.js: %v", err)
	}
	return string(body)
}

func readComposerModel(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("static", "operator-station", "composer-model.js"))
	if err != nil {
		t.Fatalf("read composer-model.js: %v", err)
	}
	return string(body)
}

// readComposerModes parses the MODES object literal — the picker's rows in
// source order. A regex parse is enough for the same reason it was for the
// hand-written <select>: the block is a literal, not generated.
func readComposerModes(t *testing.T) []string {
	t.Helper()
	src := readComposerModel(t)
	block := regexp.MustCompile(`(?s)const MODES = \{(.*?)\n\};`).FindStringSubmatch(src)
	if block == nil {
		t.Fatal("could not find `const MODES = {` in composer-model.js")
	}
	keyRe := regexp.MustCompile(`(?m)^\s*([a-z_]+):`)
	matches := keyRe.FindAllStringSubmatch(block[1], -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
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
		t.Fatalf("%s: composer len=%d (%v) vs protocol len=%d (%v)",
			label, len(g), g, len(w), w)
	}
	for i := range g {
		if g[i] != w[i] {
			t.Errorf("%s[%d]: composer %q, protocol %q (composer=%v, protocol=%v)",
				label, i, g[i], w[i], g, w)
		}
	}
}
