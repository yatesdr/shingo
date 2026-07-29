package shared

import (
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Chip-class drift support.
//
// A chip is two classes: `.chip` gives it the pill shape, and a `.chip-<x>`
// modifier gives it its colour. A modifier with no rule anywhere therefore
// renders as the bare pill — chip-shaped, and the same colour as the page
// behind it. That has now happened twice: `.chip-err` shipped at 1.2:1
// contrast, and `.chip-warn` was referenced by inventory.js with no rule in
// any stylesheet at all.
//
// Both were found by eye. The per-surface chip_drift_test.go tests use these
// helpers to find the next one in CI instead.

// chipUsePattern matches a class attribute or className assignment that
// applies the base `chip` class, capturing the whole class list so the
// modifiers can be pulled out of it.
//
// Requiring the base class is the point of the predicate: `plc-chip-connected`
// (shingoedge.css) is a different component's own vocabulary, not a chip
// modifier, and needs no colour rule. Only a class list that says `chip` AND
// `chip-<x>` is making the promise this test holds it to.
//
// Core's node-detail picker used to supply a second example here — a marker
// class `chip-hidden` on a hidden <input>. It is `tag-hidden` now: that widget
// left the `chip` namespace entirely in U5's rename, because its PILL was also
// called `.chip` and silently overrode the health chips app-wide. The predicate
// is unchanged; there is simply one less near-miss for it to survive.
var chipUsePattern = regexp.MustCompile(`class(?:Name)?\s*=\s*["']([^"']*\bchip\b[^"']*)["']`)

// chipModifierPattern pulls the `chip-<x>` tokens out of one class list.
var chipModifierPattern = regexp.MustCompile(`\bchip-([a-z0-9-]+)\b`)

// ChipModifiers returns every `chip-<x>` modifier applied alongside the base
// `chip` class in src, sorted and deduplicated.
func ChipModifiers(src string) []string {
	seen := map[string]bool{}
	for _, m := range chipUsePattern.FindAllStringSubmatch(src, -1) {
		classes := m[1]
		// The base class must stand alone in the list. `plc-chip` contains
		// "chip" as a substring but is not it.
		if !hasClass(classes, "chip") {
			continue
		}
		for _, mod := range chipModifierPattern.FindAllStringSubmatch(classes, -1) {
			seen["chip-"+mod[1]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// cssCommentPattern matches a /* ... */ block, including multi-line ones.
var cssCommentPattern = regexp.MustCompile(`(?s)/\*.*?\*/`)

// CSSDeclaresClass reports whether src contains a rule for `.class`.
//
// Permissive on purpose: any occurrence followed by a selector terminator
// counts, wherever it sits in the cascade. The question this answers is "is
// the class REACHABLE at all", which is what separates an invisible chip from
// a styled one — not which rule ultimately wins.
//
// Comments are stripped first, and that is not a nicety. Documenting a class
// in the comment above its rule is normal and good; without the strip, the
// sentence explaining `.chip-warn` satisfied the check on its own, so deleting
// the rule underneath it left the drift test green. The negative case caught
// that — which is the argument for always running one.
func CSSDeclaresClass(src, class string) bool {
	re := regexp.MustCompile(`\.` + regexp.QuoteMeta(class) + `[\s,{:.]`)
	return re.MatchString(cssCommentPattern.ReplaceAllString(src, " "))
}

func hasClass(classList, want string) bool {
	return slices.Contains(strings.Fields(classList), want)
}
