package shared

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// zLayerOrder is the intended stacking order, weakest first. It is declared
// here rather than derived from the token values because the ORDER is the
// design and the numbers are an implementation of it: deriving the order from
// the numbers would make the test agree with whatever the numbers happen to
// say, which is the shape of a test that cannot fail.
var zLayerOrder = []string{
	"--z-raised",
	"--z-sticky",
	"--z-tooltip",
	"--z-dropdown",
	"--z-chrome",
	"--z-modal",
	"--z-modal-over",
	"--z-popover",
	"--z-toast",
}

// TestZLayerScaleIsDeclaredAndOrdered pins the scale itself: every rung is
// present, every rung is an integer, they are strictly increasing in the
// intended order, and tokens.css declares no --z-* token that has no place in
// that order.
//
// The last clause is the one that matters. Adding `--z-above-everything: 999`
// beside the scale would satisfy any check that only looked at the nine rungs,
// and it is exactly how the 9999/10000 ratchet started — not by editing an
// ordering, but by adding one more thing beside it.
func TestZLayerScaleIsDeclaredAndOrdered(t *testing.T) {
	scale := ZLayerScale(readShared(t, "tokens.css"))
	if scale == nil {
		t.Fatal("tokens.css has no :root block — the scale could not be read at all")
	}

	var prev int
	for i, name := range zLayerOrder {
		raw, ok := scale[name]
		if !ok {
			t.Errorf("tokens.css is missing layer token %s (rung %d of %d).\n"+
				"  Every rung must exist: a var(--z-...) that resolves to nothing makes z-index an invalid declaration, the element falls back to auto, and nothing reports it.",
				name, i+1, len(zLayerOrder))
			continue
		}
		v, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			t.Errorf("tokens.css %s = %q, which is not an integer", name, raw)
			continue
		}
		if i > 0 && v <= prev {
			t.Errorf("layer scale is NOT strictly increasing: %s = %d does not exceed %s = %d.\n"+
				"  zLayerOrder says %s sits above %s; the values say otherwise. Fix the value, or fix the order and say why in the commit.",
				name, v, zLayerOrder[i-1], prev, name, zLayerOrder[i-1])
		}
		prev = v
	}

	for name := range scale {
		if !slicesContains(zLayerOrder, name) {
			t.Errorf("tokens.css declares layer token %s, which has no place in zLayerOrder.\n"+
				"  A rung with no position in the ordering is the ratchet restarting. Give it a position, or use an existing rung.", name)
		}
	}
}

// TestNoRawZIndexInSharedAssets holds the shared stylesheets and JS to the
// scale. Core and Edge have the same check over their own trees; this one
// covers the assets both surfaces load.
func TestNoRawZIndexInSharedAssets(t *testing.T) {
	var scanned int
	err := fs.WalkDir(Files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		switch filepath.Ext(p) {
		case ".css", ".js":
		default:
			return nil
		}
		if p == "tokens.css" {
			return nil // the one file allowed to say a number: it defines them
		}
		body, err := fs.ReadFile(Files, p)
		if err != nil {
			return err
		}
		scanned++
		for _, raw := range RawZIndexUses(string(body)) {
			t.Errorf("shared/%s sets z-index to %q instead of a layer token.\n"+
				"  Use one of %s from shared/tokens.css. A raw value re-opens the ratchet the scale closed — before it, three separate layers (2000, 9999, 10000) each claimed to be the top.",
				p, raw, strings.Join(zLayerOrder, " / "))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk shared assets: %v", err)
	}
	if scanned == 0 {
		t.Fatal("scanned no shared CSS/JS at all — the walk has drifted and this test is a no-op")
	}
}

// TestRawZIndexUsesRecognisesTheFormsThatMatter pins the detector's own
// behaviour. It has to see a declaration in three different syntactic homes —
// a stylesheet rule, a JS cssText string, a template <style> block — and it
// has to not fire on a comment or on `auto`. Without this the guard could
// silently stop matching (a whitespace variant, a quote style) and go green
// while checking nothing.
func TestRawZIndexUsesRecognisesTheFormsThatMatter(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"css rule, spaced", ".a { z-index: 10; }", []string{"10"}},
		{"css rule, tight", ".a{z-index:9999;}", []string{"9999"}},
		{"js cssText, single quotes", `el.style.cssText = 'position:fixed;z-index:10000;top:0';`, []string{"10000"}},
		{"js cssText, double quotes", `s = "#x{position:sticky;z-index:9999;display:flex}";`, []string{"9999"}},
		{"token is accepted", ".a { z-index: var(--z-toast); }", nil},
		{"token with fallback is accepted", ".a { z-index: var(--z-modal, 50); }", nil},
		{"auto is accepted", ".a { z-index: auto; }", nil},
		{"important on a token is still a finding", "#k { z-index: var(--z-modal-over) !important; }", []string{"var(--z-modal-over) !important"}},
		{"important on a number is a finding", "#k { z-index: 1000 !important; }", []string{"1000 !important"}},
		{"css comment does not count", "/* z-index: 1000; was the old value */\n.a { z-index: var(--z-modal); }", nil},
		{"js line comment does not count", "// z-index: 9999 used to be here\nvar x = 1;", nil},
		{"prose without a colon does not count", "/* three layers claimed the top z-index */ .a{z-index:var(--z-toast)}", nil},
	}
	for _, c := range cases {
		got := RawZIndexUses(c.src)
		if !equalStrings(got, c.want) {
			t.Errorf("%s: RawZIndexUses(%q) = %v, want %v", c.name, c.src, got, c.want)
		}
	}
}

func slicesContains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
