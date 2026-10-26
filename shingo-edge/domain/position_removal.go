package domain

import (
	"sort"
	"strings"
)

// PositionClaims is what still names one position a save is taking off a
// screen: the recipes whose shape has a cell on it or partners it, and the
// styles whose live claims do. Active is the process's active style, when it
// is one of Styles.
type PositionClaims struct {
	Position string
	Recipes  []string
	Styles   []string
	Active   string
}

// PositionRemovalWarning is the screen sheet's warning before a save that
// takes positions off a station. Retiring a position leaves every recipe and
// style that names it pointing at a position the process no longer has, so the
// sheet names them and waits for a second Save. It is a warning, not a
// refusal. Empty when nothing names any of the positions.
//
// One position reads as one sentence; several name each position with what
// claims it, in the order given.
func PositionRemovalWarning(claims []PositionClaims) string {
	var named []PositionClaims
	for _, c := range claims {
		if len(c.Recipes)+len(c.Styles) > 0 {
			named = append(named, c)
		}
	}
	switch len(named) {
	case 0:
		return ""
	case 1:
		return "Removing " + named[0].Position + " leaves " + strings.Join(claimantPhrases(named[0]), " and ") +
			" naming a position this process no longer has. Save again to remove it anyway."
	}
	positions := make([]string, len(named))
	lines := make([]string, len(named))
	for i, c := range named {
		positions[i] = c.Position
		lines[i] = c.Position + ": " + strings.Join(claimantPhrases(c), "; ") + "."
	}
	return "Removing " + andList(positions) + " leaves these naming positions this process no longer has — " +
		strings.Join(lines, " ") + " Save again to remove them anyway."
}

// claimantPhrases is `recipe "A"` or `recipes "A" and "B"`, then the same
// for styles, the active style marked.
func claimantPhrases(c PositionClaims) []string {
	var out []string
	if len(c.Recipes) > 0 {
		out = append(out, kindWord("recipe", len(c.Recipes))+" "+andList(quotedNames(c.Recipes, "")))
	}
	if len(c.Styles) > 0 {
		out = append(out, kindWord("style", len(c.Styles))+" "+andList(quotedNames(c.Styles, c.Active)))
	}
	return out
}

func kindWord(word string, n int) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// quotedNames sorts the names and quotes each, marking the active style.
func quotedNames(names []string, active string) []string {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	out := make([]string, len(sorted))
	for i, n := range sorted {
		out[i] = `"` + n + `"`
		if active != "" && n == active {
			out[i] += " (the active style)"
		}
	}
	return out
}

// andList is "a", "a and b", "a, b and c".
func andList(items []string) string {
	if len(items) <= 1 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}
