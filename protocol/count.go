package protocol

// count.go — the arithmetic of a bin's count, in one place for both servers.
//
// THE RULE (owner, 2026-11-19). A UOP is one assembly cycle's worth. A bin's
// count is what is physically in the carrier: parts for a payload of one
// manifest line, kits for a payload of several. UOP is never stored; it is
// worked out wherever it is shown, count ÷ ratio, the whole part, with the
// parts left over shown beside it. ManifestItem's comment says the rule for
// the wire; docs/uop-architecture.md says it for people.
//
// The browser's twin is shared/count.js. Both are pinned to one file of worked
// examples, testdata/count-examples.json (TestCountExamples here,
// shared/testdata/js/count.test.js there), and the two SQL copies that cannot
// call Go (the Edge's claim read, capacity.PiecesPerCycleSQL; Core's inventory
// list) are tested over the same file.

// UOPRatio is a payload's UOP ratio: how many of a bin's counted things one
// assembly cycle takes. A payload of ONE line counts parts, so its ratio is
// that line's PartsPerCycle, at least 1 (a line that says less is 1). A kit
// (several lines) counts kits and a cycle takes one: 1. A payload with no
// line is 1. At a ratio of 1 the count is the UOP.
func UOPRatio(lines []ManifestItem) int {
	if len(lines) != 1 {
		return 1
	}
	return int(max(lines[0].PartsPerCycle, 1))
}

// SplitUOP splits a bin's count into the whole UOP it makes and the parts left
// over: count / ratio and count % ratio, truncated toward zero, so a count
// below zero splits the same way above it (-5 at 8 is 0 UOP and -5 parts) and
// uop*ratio + parts == count always: never a fraction, never a lost part. A
// ratio below 1 counts as 1, where the count is all UOP and no parts.
func SplitUOP(count, ratio int) (uop, parts int) {
	if ratio <= 1 {
		return count, 0
	}
	return count / ratio, count % ratio
}

// LinePieces is how many pieces of line i a bin of count holds: the count
// itself for a payload of one line (it counts that part's pieces), and count ×
// the line's PartsPerCycle for a kit (it counts kits). An index outside lines
// is 0.
func LinePieces(lines []ManifestItem, i int, count int64) int64 {
	if i < 0 || i >= len(lines) {
		return 0
	}
	if len(lines) == 1 {
		return count
	}
	return count * lines[i].PartsPerCycle
}
