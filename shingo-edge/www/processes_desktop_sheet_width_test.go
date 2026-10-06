package www

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// processes_desktop_sheet_width_test.go — the desktop dialog shell is wide
// enough for its own field.
//
// sheetField draws a label column beside an input of class .pd-inp.wide, and
// every sheet on the Processes page is a .pd-modal. The label column, the
// column gap, the wide input's min-width and the body's padding are each set in
// their own rule, and the modal's width was set by a fourth, per sheet — 480
// for a one-field modal, 600 by default. Neither holds the 664 px a field row
// needs, so the input ran out of the card on Clone, Rename and every other
// one-field sheet. This reads those rules from the stylesheet and holds every
// .pd-modal width to their sum, so changing any one of them without the width
// goes red here rather than on a laptop.

// sheetScrollbarAllowance is room for the body's vertical scrollbar, which
// appears when a long sheet scrolls and takes its width out of the field row.
const sheetScrollbarAllowance = 18

func desktopCSS(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("static", "css", "processes-desktop.css"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// cssPx returns the integer captures of re's first match in css, failing the
// test by name when the rule it reads is gone.
func cssPx(t *testing.T, css, what string, re *regexp.Regexp) []int {
	t.Helper()
	m := re.FindStringSubmatch(css)
	if m == nil {
		t.Fatalf("processes-desktop.css: no rule for %s (%s)", what, re)
	}
	out := make([]int, 0, len(m)-1)
	for _, s := range m[1:] {
		n, err := strconv.Atoi(s)
		if err != nil {
			t.Fatalf("%s: %q is not a px integer", what, s)
		}
		out = append(out, n)
	}
	return out
}

func TestDesktopSheetFitsItsWideField(t *testing.T) {
	css := desktopCSS(t)
	label := cssPx(t, css, ".pd-fld label column",
		regexp.MustCompile(`(?s)\.pd-fld \{[^}]*grid-template-columns:\s*(\d+)px 1fr;[^}]*gap:\s*\d+px (\d+)px;`))
	input := cssPx(t, css, ".pd-inp.wide min-width",
		regexp.MustCompile(`\.pd-inp\.wide \{ min-width: (\d+)px; \}`))[0]
	pad := cssPx(t, css, ".pd-modal .mb padding",
		regexp.MustCompile(`\.pd-modal \.mb \{ padding: \d+px (\d+)px \d+px;`))[0]
	border := cssPx(t, css, ".pd-modal border",
		regexp.MustCompile(`(?s)\.pd-modal \{[^}]*border: (\d+)px solid`))[0]
	need := label[0] + label[1] + input + 2*pad + 2*border + sheetScrollbarAllowance

	base := cssPx(t, css, ".pd-modal width",
		regexp.MustCompile(`(?s)\.pd-modal \{[^}]*?[;{\s]width: (\d+)px;`))[0]
	if base < need {
		t.Errorf(".pd-modal is %d px; a %d px label, %d px gap and %d px wide input inside "+
			"%d px padding need %d px", base, label[0], label[1], input, pad, need)
	}
	for _, m := range regexp.MustCompile(`\.pd-modal\.([\w-]+) \{ width: (\d+)px; \}`).FindAllStringSubmatch(css, -1) {
		w, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("width %q: %v", m[2], err)
		}
		if w < need {
			t.Errorf(".pd-modal.%s is %d px, narrower than the %d px a sheet field needs", m[1], w, need)
		}
	}

	// A window narrower than the sheet plus the scrim's margin caps the card
	// (max-width: calc(100vw - 48px)); from there the field must stack rather
	// than push the input out of the card.
	bp := cssPx(t, css, "the sheet's stacking breakpoint",
		regexp.MustCompile(`(?s)@media \(max-width: (\d+)px\) \{\s*\.pd-modal \.pd-fld \{ grid-template-columns: 1fr; \}`))[0]
	if bp < base+48 {
		t.Errorf("the sheet's fields stack below %d px, but the card is capped from %d px", bp, base+48)
	}
}
