package shared

import (
	"slices"
	"testing"
)

// TestCSSBareClassSelectorsSeparatesBareFromScoped pins the predicate in both
// directions, because a scanner that matches nothing makes every drift test
// built on it green and meaningless — the exact way the chip drift test was
// nearly shipped satisfied by its own comment.
//
// The `want` list is what must be reported; the `reject` list is what must NOT
// be, and it is the half that carries the meaning: `.chip-container .chip` is
// the d01156b1 fix, and a scan that flags it as a bare `.chip` declaration
// would report the cure as the disease.
func TestCSSBareClassSelectorsSeparatesBareFromScoped(t *testing.T) {
	src := `
/* .commented-out { color: red; } — prose about a deleted rule */
.plain { color: red; }
.with-pseudo:hover { color: red; }
.with-element::after { content: ""; }
.with-not:not(.other) { color: red; }
.a, .b { color: red; }

.chip-container .chip { background: navy; }
.compound.second { color: red; }
.parent > .child { color: red; }
div.qualified { color: red; }
.attr[data-x="1"] { color: red; }
#id-only { color: red; }
table { color: red; }

@media (max-width: 600px) {
  .inside-media { color: red; }
}
@keyframes spin { 0%, 100% { opacity: 1; } 50% { opacity: 0; } }
:root { --token: #fff; }
`
	got := CSSBareClassSelectors(src)

	want := []string{"a", "b", "inside-media", "plain", "with-element", "with-not", "with-pseudo"}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("bare selector .%s was not reported; got %v", w, got)
		}
	}

	// Every one of these is scoped, commented out, or not a class at all. Any
	// of them appearing means the scan cannot tell a shared name from a
	// deliberately-narrowed one, which is the only distinction it exists to make.
	reject := []string{"chip", "chip-container", "commented-out", "compound", "second",
		"parent", "child", "qualified", "attr", "id-only"}
	for _, r := range reject {
		if slices.Contains(got, r) {
			t.Errorf(".%s was reported as a BARE class selector, but it is scoped, commented out, or not a bare class; got %v", r, got)
		}
	}

	if len(got) != len(want) {
		t.Errorf("reported %d classes, expected exactly %d (%v); got %v", len(got), len(want), want, got)
	}
}
