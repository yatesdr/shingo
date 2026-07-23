package shared

import "testing"

func TestIsEmoji(t *testing.T) {
	emoji := []rune{
		0x1F512, // 🔒 lock (the historical bins.js catch)
		0x1F4E6, // 📦 package
		0x1F534, // 🔴 red circle
		0x1F7E2, // 🟢 green circle
		0x2705,  // ✅ check mark button (BMP default-emoji)
		0x274C,  // ❌ cross mark
		0x2B50,  // ⭐ star
		0xFE0F,  // VS16 selector
	}
	for _, r := range emoji {
		if !IsEmoji(r) {
			t.Errorf("IsEmoji(U+%04X) = false, want true", r)
		}
	}

	// Monochrome geometric / technical glyphs the surfaces use as text — must
	// NOT be flagged, or the drift test would reject legitimate UI affordances.
	notEmoji := []rune{
		'A', '7', ' ', '·', // ascii + middle dot
		0x2192, // → arrow
		0x2190, // ← arrow
		0x2194, // ↔ left-right arrow
		0x25B6, // ▶ play/expand chevron
		0x25BE, // ▾ small down triangle
		0x25CF, // ● bullet
		0x2713, // ✓ check
		0x2717, // ✗ cross
		0x26A0, // ⚠ bare warning sign (no VS16)
		0x28FF, // ⠿ braille (loaders drag grip)
		0x2026, // … ellipsis
	}
	for _, r := range notEmoji {
		if IsEmoji(r) {
			t.Errorf("IsEmoji(U+%04X) = true, want false", r)
		}
	}
}

func TestFirstEmoji(t *testing.T) {
	if r, ok := FirstEmoji("plain → text, no emoji ⚠ here"); ok {
		t.Errorf("FirstEmoji found U+%04X in emoji-free string", r)
	}
	if r, ok := FirstEmoji("has a lock 🔒 in it"); !ok || r != 0x1F512 {
		t.Errorf("FirstEmoji = U+%04X, %v; want U+1F512, true", r, ok)
	}
}
