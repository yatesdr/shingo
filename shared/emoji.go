package shared

// Emoji policy support for the "no emoji, ever" UI rule (see
// docs/ui-style-guide.md § Icons). Icons are vendored Lucide glyphs from
// icons.svg; emoji are forbidden in templates and page JS because they render
// inconsistently across platforms, can't take currentColor, and drift from the
// monochrome icon system. The per-surface drift tests
// (shingo-core/www/emoji_drift_test.go, shingo-edge/www/emoji_drift_test.go)
// call IsEmoji so the detection lives in exactly one place.

// IsEmoji reports whether r is an emoji-presentation code point — the kind of
// glyph the "no emoji" policy forbids. It deliberately does NOT flag the
// monochrome geometric / technical glyphs the surfaces legitimately use as text
// affordances: arrows (→ ← ↔), chevrons (▶ ▾), bullets (· ●), check/cross
// (✓ ✗), braille (⠿), and the bare warning sign (⚠). Those have
// Emoji_Presentation=No and render as text by default. It flags:
//
//   - the supplementary emoji / pictograph planes U+1F000–U+1FAFF (🔒 📦 🟢 …)
//   - the emoji variation selector U+FE0F — any symbol explicitly forced to
//     emoji presentation ("⚠️", "▶️")
//   - the BMP code points with Emoji_Presentation=Yes (✅ ❌ ⭐ …), which render
//     as colour emoji with no selector
//
// Go's regexp has no \p{Emoji} class, so the ranges are explicit. This tracks
// Unicode's Emoji_Presentation property closely enough for a CI guardrail; if a
// legitimately-needed glyph is ever misclassified, adjust the tables here (one
// place) rather than allow-listing per file.
func IsEmoji(r rune) bool {
	switch {
	case r == 0xFE0F: // VS16 — emoji presentation selector
		return true
	case r >= 0x1F000 && r <= 0x1FAFF: // supplementary emoji / pictograph planes
		return true
	}
	for _, rng := range bmpEmojiPresentation {
		if r >= rng[0] && r <= rng[1] {
			return true
		}
	}
	return false
}

// FirstEmoji returns the first emoji rune in s and true, or (0, false) if s
// contains none. Callers use it to report the offending glyph on a flagged line.
func FirstEmoji(s string) (rune, bool) {
	for _, r := range s {
		if IsEmoji(r) {
			return r, true
		}
	}
	return 0, false
}

// bmpEmojiPresentation lists the Basic-Multilingual-Plane code point ranges with
// Emoji_Presentation=Yes (they render as colour emoji without a variation
// selector). Bare geometric/technical symbols the codebase uses as text (⚠ 26A0,
// ▶ 25B6, ✓ 2713, · 00B7, arrows 219x) are intentionally absent.
var bmpEmojiPresentation = [][2]rune{
	{0x231A, 0x231B}, {0x23E9, 0x23EC}, {0x23F0, 0x23F0}, {0x23F3, 0x23F3},
	{0x25FD, 0x25FE}, {0x2614, 0x2615}, {0x2648, 0x2653}, {0x267F, 0x267F},
	{0x2693, 0x2693}, {0x26A1, 0x26A1}, {0x26AA, 0x26AB}, {0x26BD, 0x26BE},
	{0x26C4, 0x26C5}, {0x26CE, 0x26CE}, {0x26D4, 0x26D4}, {0x26EA, 0x26EA},
	{0x26F2, 0x26F3}, {0x26F5, 0x26F5}, {0x26FA, 0x26FA}, {0x26FD, 0x26FD},
	{0x2705, 0x2705}, {0x270A, 0x270B}, {0x2728, 0x2728}, {0x274C, 0x274C},
	{0x274E, 0x274E}, {0x2753, 0x2755}, {0x2757, 0x2757}, {0x2795, 0x2797},
	{0x27B0, 0x27B0}, {0x27BF, 0x27BF}, {0x2B1B, 0x2B1C}, {0x2B50, 0x2B50},
	{0x2B55, 0x2B55},
}
