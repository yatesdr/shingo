package domain

import (
	"fmt"
	"sort"
	"strings"
)

// PositionClaims is what still names one position a save is taking off a
// screen: the recipes whose shape has a cell on it or partners it, and the
// styles whose live claims do. Active is the process's active style, when it
// is one of Styles.
//
// And what the position takes with it. Taking a position off retires its
// process_nodes row and deletes its runtime row; adding the name back makes a
// new row with a new id, so what a person set on the old ones does not come
// back. Curtain: a light curtain interlock is set up at it (switched on, or a
// PLC or tag typed in). CountedAt: the station counter it is counted at, by
// name ("" is the process's production counter, which is nothing to lose).
// ReorderOff: a person switched its auto-reorder off, which only a person
// clears; the new row has no hold, so the position asks again by itself.
type PositionClaims struct {
	Position string
	Recipes  []string
	Styles   []string
	Active   string

	Curtain    bool
	CountedAt  string
	ReorderOff bool
}

// PositionRemovalWarning is the screen sheet's warning before a save that
// takes positions off a station. Retiring a position leaves every recipe and
// style that names it pointing at a position the process no longer has, and
// clears what a person set at it, so the sheet says both and waits for a
// second Save. It is a warning, not a refusal. Empty when nothing names any of
// the positions and none of them holds a setting.
//
// What names the positions comes first: one position reads as one sentence,
// several name each position with what claims it, in the order given. Then a
// sentence for each position that holds a setting, in the order given (see
// takenSentence). Then the one "Save again", for every position either part
// named.
func PositionRemovalWarning(claims []PositionClaims) string {
	var named, holding []PositionClaims
	told := map[string]bool{}
	for _, c := range claims {
		if len(c.Recipes)+len(c.Styles) > 0 {
			named = append(named, c)
			told[c.Position] = true
		}
		if len(takenPhrases(c)) > 0 {
			holding = append(holding, c)
			told[c.Position] = true
		}
	}
	if len(told) == 0 {
		return ""
	}
	var said []string
	switch len(named) {
	case 0:
	case 1:
		said = append(said, "Removing "+named[0].Position+" leaves "+strings.Join(claimantPhrases(named[0]), " and ")+
			" naming a position this process no longer has.")
	default:
		positions := make([]string, len(named))
		lines := make([]string, len(named))
		for i, c := range named {
			positions[i] = c.Position
			lines[i] = c.Position + ": " + strings.Join(claimantPhrases(c), "; ") + "."
		}
		said = append(said, "Removing "+andList(positions)+" leaves these naming positions this process no longer has — "+
			strings.Join(lines, " "))
	}
	for _, c := range holding {
		said = append(said, takenSentence(c))
	}
	if len(told) == 1 {
		return strings.Join(append(said, "Save again to remove it anyway."), " ")
	}
	return strings.Join(append(said, "Save again to remove them anyway."), " ")
}

// takenSentence is what one position takes with it: `PLN_03 has a light
// curtain and is counted at Station 2; taking it off clears both, and adding it
// back does not restore them.` It says the re-add because that is the move an
// engineer makes believing it undoes the take-off.
func takenSentence(c PositionClaims) string {
	held := takenPhrases(c)
	clears, them := "that", "it"
	switch n := len(held); {
	case n == 2:
		clears, them = "both", "them"
	case n == 3:
		clears, them = "all three", "them"
	case n > 3:
		clears, them = fmt.Sprintf("all %d", n), "them"
	}
	return c.Position + " " + andList(held) + "; taking it off clears " + clears +
		", and adding it back does not restore " + them + "."
}

// takenPhrases is what c holds that a take-off clears, as the verb phrases
// takenSentence lists: the curtain, the counter, a person's off.
func takenPhrases(c PositionClaims) []string {
	var out []string
	if c.Curtain {
		out = append(out, "has a light curtain")
	}
	if c.CountedAt != "" {
		out = append(out, "is counted at "+c.CountedAt)
	}
	if c.ReorderOff {
		out = append(out, "has auto-reorder switched off")
	}
	return out
}

// claimantPhrases is `recipe "A"` or `2 recipes "A" and "B"`, then the same
// for styles, the active style marked.
func claimantPhrases(c PositionClaims) []string {
	var out []string
	if len(c.Recipes) > 0 {
		out = append(out, countedKind("recipe", len(c.Recipes))+" "+andList(quotedNames(c.Recipes, "")))
	}
	if len(c.Styles) > 0 {
		out = append(out, countedKind("style", len(c.Styles))+" "+andList(quotedNames(c.Styles, c.Active)))
	}
	return out
}

// countedKind is `recipe` for one and `2 recipes` for more.
func countedKind(word string, n int) string {
	if n == 1 {
		return word
	}
	return fmt.Sprintf("%d %ss", n, word)
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
