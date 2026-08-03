// Package windoworder decides what order a loader's windows come back in.
//
// It lives in shared/ because both sides need the same answer and they compute
// it in separate modules. Core resolves delivery targets from its own tables;
// the Edge reads its cached copy. If the two order the windows differently, a
// carrier goes to a window nobody expected — the funnel case delivers to "the
// first window" and the spreading case fills free windows in order, so the
// order IS the decision.
//
// The rule has two parts.
//
// FIRST, the operator's arrangement. An operator can drag a loader's windows
// into the order they want them filled, and the arrangement is persisted and
// synced. It used to be discarded on arrival: there was no ordinal on the wire
// and no ordinal column in the Edge's cache, so the Edge sorted by name and the
// operator's intent was accepted, stored, transmitted, and thrown away. The
// ordinal is now carried end to end and it wins.
//
// SECOND, when the ordinals cannot decide — because they tie, or because they
// are all zero, which is what a Core that predates the field sends — the
// fallback is a NUMBER-AWARE name sort. Plain text order matches intent only
// while window names stay uniform: a plant names its windows W1, W2, W3 and
// everything looks right until the loader reaches ten, where plain text puts
// W10 before W2 and the funnel target moves without anybody touching it. So a
// run of digits inside a name compares as a number.
//
// Ordering is TOTAL and STABLE: ordinal, then number-aware name, then plain
// text as the last tiebreak. Two windows can never compare equal unless their
// names are identical, so neither side can return a different arrangement from
// the other for the same input.
package windoworder

import "strings"

// Window is the minimum needed to place a window in the order: where the
// operator put it, and what it is called.
type Window struct {
	Ordinal int
	Name    string
}

// Less reports whether a sorts before b.
func Less(a, b Window) bool {
	if a.Ordinal != b.Ordinal {
		return a.Ordinal < b.Ordinal
	}
	return LessName(a.Name, b.Name)
}

// LessName is the number-aware name comparison, exported because the fallback
// is worth being able to test and reason about on its own.
//
// Names are compared as an alternating sequence of text runs and digit runs.
// Text runs compare as text; digit runs compare as numbers, so W2 sorts before
// W10 and SMN_003 sorts before SMN_004 and before SMN_010. Leading zeros do not
// change a number's value, so SMN_03 and SMN_3 compare equal on that run and
// fall through to the plain-text tiebreak, which keeps the order total.
func LessName(a, b string) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		ad, bd := isDigit(a[i]), isDigit(b[j])
		if ad != bd {
			// One name has a digit where the other has text. Compare the bytes:
			// digits sort below letters in ASCII and keeping that is what makes
			// the result stable and unsurprising.
			return a[i] < b[j]
		}
		if !ad {
			if a[i] != b[j] {
				return a[i] < b[j]
			}
			i++
			j++
			continue
		}
		// Both are at the start of a digit run. Compare the runs as numbers:
		// after stripping leading zeros, the longer run is the larger number,
		// and equal lengths compare left to right as text.
		aStart, bStart := i, j
		for i < len(a) && isDigit(a[i]) {
			i++
		}
		for j < len(b) && isDigit(b[j]) {
			j++
		}
		aRun := strings.TrimLeft(a[aStart:i], "0")
		bRun := strings.TrimLeft(b[bStart:j], "0")
		if len(aRun) != len(bRun) {
			return len(aRun) < len(bRun)
		}
		if aRun != bRun {
			return aRun < bRun
		}
	}
	// One name is a prefix of the other, or they are identical so far. The
	// shorter one sorts first; identical names compare as not-less either way.
	return len(a)-i < len(b)-j
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
