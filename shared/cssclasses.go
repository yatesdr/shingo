// cssclasses.go — reading class SELECTORS out of a stylesheet literally.
//
// Companion to CSSDeclaresClass in chips.go, which answers "is this class
// reachable at all". This file answers the different question that U5 turned
// out to be about: "is this class declared BARE, in more than one of the
// stylesheets a single page loads."
//
// U5's invisible chip was not a contrast bug with a colour fix. Two unrelated
// components were both called `.chip`, both selectors were (0,1,0), and Core's
// style.css loads after shared/components.css — so the picker's navy fill won
// on every health chip in the app and `.chip-err`'s near-black ink landed on it
// at 1.2:1. d01156b1 fixed that by scoping one side to `.chip-container .chip`,
// which cures the symptom: the two names still collide, and the next unscoped
// `.chip` rule in a later sheet re-breaks it.
//
// BARE is the whole predicate. A compound or descendant selector
// (`.chip-container .chip`, `.board-status.status-failed`) is scoped on
// purpose — it says which .chip it means. A bare single class says "every
// element with this name, anywhere", which is the only shape that can collide
// silently across two files that never mention each other.

package shared

import (
	"regexp"
	"sort"
	"strings"
)

// selectorBlockPattern captures the text before each `{` — the selector list of
// a rule, the prelude of an at-rule, or a keyframe stop. Nested at-rule bodies
// fall out for free: the prelude matches once (and is discarded by the `@`
// check in CSSBareClassSelectors) and the rules inside it match after it.
var selectorBlockPattern = regexp.MustCompile(`([^{}]+)\{`)

// bareClassPattern matches ONE class and nothing else: `.foo`, `.foo:hover`,
// `.foo::after`, `.foo:not(.bar)`. It deliberately does not match `.a .b`,
// `.a.b`, `.a > .b`, `div.a` or `.a[data-x]` — every one of those is scoped by
// something, and scoping is what makes a name safe to reuse.
var bareClassPattern = regexp.MustCompile(`^\.([A-Za-z_][0-9A-Za-z_-]*)(?:::?[0-9A-Za-z_-]+(?:\([^)]*\))?)*$`)

// CSSBareClassSelectors returns every class that src declares as a bare
// single-class selector, sorted and deduplicated. Comments are stripped first,
// for the reason CSSDeclaresClass strips them: prose above a rule describing
// the rule is normal, and counting it makes the scan report classes that were
// deleted.
func CSSBareClassSelectors(src string) []string {
	seen := map[string]bool{}
	for _, m := range selectorBlockPattern.FindAllStringSubmatch(cssCommentPattern.ReplaceAllString(src, " "), -1) {
		sel := strings.TrimSpace(m[1])
		if sel == "" || strings.HasPrefix(sel, "@") {
			continue
		}
		for _, part := range strings.Split(sel, ",") {
			if mm := bareClassPattern.FindStringSubmatch(strings.TrimSpace(part)); mm != nil {
				seen[mm[1]] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
