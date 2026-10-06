package www

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// processes_desktop_positions_table_test.go — the positions table on the
// desktop Flows tab stays readable however it is scrolled.
//
// These read processes-desktop.css. The geometry they guard is a browser's to
// produce, but each defect came from a rule that was missing, not from a
// number that was off, so the rule's presence is the honest thing to hold.

// cssRule returns the declarations of the rule whose selector is exactly sel,
// starting a line, failing the test by name when there is none.
func cssRule(t *testing.T, css, sel string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(sel) + `\s*\{([^}]*)\}`).FindStringSubmatch(css)
	if m == nil {
		t.Fatalf("processes-desktop.css: no rule for %s", sel)
	}
	return m[1]
}

// hasDecl reports whether decls sets prop to a value matching val.
func hasDecl(decls, prop, val string) bool {
	return regexp.MustCompile(`(^|[;{\s])` + regexp.QuoteMeta(prop) + `:\s*` + val + `\s*(;|$)`).
		MatchString(strings.TrimSpace(decls))
}

// The column headings stay on screen while the rows scroll under them. When
// the scroller moved to bring a selected row into view, the header row went
// up with the rows and its bottom few pixels were left showing under the
// POSITIONS caption, reading as the caption printed over the headings.
func TestPositionsHeaderStaysAboveTheRows(t *testing.T) {
	decls := cssRule(t, desktopCSS(t), ".pd-postbl thead th")
	for _, want := range [][2]string{
		{"position", "sticky"},
		{"top", "0"},
		{"background", `var\(--[\w-]+\)`},
	} {
		if !hasDecl(decls, want[0], want[1]) {
			t.Errorf(".pd-postbl thead th: want %s: %s; the header row scrolls away with the rows", want[0], want[1])
		}
	}
}

// An empty field's placeholder ellipsises its word rather than cutting it.
// The placeholder is a flex button, and text-overflow on a flex container does
// nothing, so `Outbound staging —` in a staging column at 1280 read
// `Outbound stagin` with the dash gone. Each part is its own box that can
// shrink and ellipsise; the dash after a word keeps its size, because the
// dash is what says the field is empty. The full label is in the title.
func TestEmptyFieldPlaceholderEllipsises(t *testing.T) {
	css := desktopCSS(t)
	part := cssRule(t, css, ".pd-blank > *")
	for _, want := range [][2]string{
		{"min-width", "0"},
		{"overflow", "hidden"},
		{"text-overflow", "ellipsis"},
		{"white-space", "nowrap"},
	} {
		if !hasDecl(part, want[0], want[1]) {
			t.Errorf(".pd-blank > *: want %s: %s; a placeholder too wide for its column clips mid-word", want[0], want[1])
		}
	}
	if !hasDecl(cssRule(t, css, ".pd-blank > .k ~ .pd-dim"), "flex", "none") {
		t.Errorf(".pd-blank > .k ~ .pd-dim: want flex: none; the dash that says the field is empty must not be the part that shrinks")
	}
}

// The positions table keeps a readable width and scrolls sideways inside its
// own box when the column is narrower than that, never the page. Without a
// floor, ten percentage columns in the 545 px an 833 px window leaves them
// drew every heading and chip as one or two letters.
//
// The floor must not make the table scroll on a window the page is laid out
// for at full width: just above the 1280 breakpoint the rail is at its wide
// size, which is the narrowest table column a desktop gets, and the scroller
// may be carrying a vertical scrollbar.
func TestPositionsTableKeepsAReadableWidth(t *testing.T) {
	css := desktopCSS(t)
	floor := cssPx(t, css, ".pd-postbl table min-width",
		regexp.MustCompile(`(?m)^\.pd-postbl table \{[^}]*min-width:\s*(\d+)px;`))[0]
	column := fullSizeTableWidth(t, css)
	if floor > column {
		t.Errorf(".pd-postbl table min-width is %d px; at %d px the table has %d px beside a scrollbar, so it would scroll sideways on a full-size desktop",
			floor, fullSizeWindow, column)
	}
	if floor < 800 {
		t.Errorf(".pd-postbl table min-width is %d px; ten columns under that draw their chips as a letter or two", floor)
	}
	if !hasDecl(cssRule(t, css, ".pd-postbl"), "overflow", "auto") {
		t.Errorf(".pd-postbl must scroll (overflow: auto) so a table at its floor scrolls inside the box, not the page")
	}
}

// fullSizeWindow is the narrowest window the page lays out at full width: one
// pixel above the 1280 breakpoint, where the rail is still at its wide size.
const fullSizeWindow = 1281

// fullSizeTableWidth is the positions table's width at fullSizeWindow, read
// from the rules that set it: the window less the rail and its border, the
// box's side margins, and room for the scroller's vertical scrollbar.
func fullSizeTableWidth(t *testing.T, css string) int {
	t.Helper()
	rail := cssPx(t, css, ".pd-rail width and border",
		regexp.MustCompile(`(?s)\.pd-rail \{\s*width:\s*(\d+)px;[^}]*border-right:\s*(\d+)px solid`))
	margin := cssPx(t, css, ".pd-posbox side margin",
		regexp.MustCompile(`\.pd-posbox \{[^}]*margin:\s*\d+px (\d+)px`))[0]
	const scrollbar = 18
	return fullSizeWindow - rail[0] - rail[1] - 2*margin - scrollbar
}

// roleChipNeed is what the Role cell needs for `consume ▾` whole, the wider of
// its two words: the chip measured 79 px on the live sim at 1281 (label,
// padding, gap, caret and border), plus the cell's 4 px padding each side.
const roleChipNeed = 79 + 2*4

// The Role chip reads whole on every full-size desktop. At 7% it was 65 px at
// 1281 and drew `co…`; the role is one word and the column exists to show it.
// Below fullSizeWindow the table's min-width floor applies instead.
func TestRoleColumnHoldsItsWord(t *testing.T) {
	css := desktopCSS(t)
	m := regexp.MustCompile(`\.pd-postbl col\.c-role \{ width: ([\d.]+)%; \}`).FindStringSubmatch(css)
	if m == nil {
		t.Fatalf("processes-desktop.css: no width for col.c-role")
	}
	share, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatalf("col.c-role width %q: %v", m[1], err)
	}
	table := fullSizeTableWidth(t, css)
	if got := share / 100 * float64(table); got < roleChipNeed {
		t.Errorf("col.c-role is %.1f%% of a %d px table at %d px = %.0f px; `consume ▾` needs %d",
			share, table, fullSizeWindow, got, roleChipNeed)
	}
}

// The column shares are written out, one per column, and add to 100.
func TestPositionsColumnSharesAddTo100(t *testing.T) {
	sum := 0.0
	ms := regexp.MustCompile(`\.pd-postbl col\.c-[\w-]+ \{ width: ([\d.]+)%; \}`).FindAllStringSubmatch(desktopCSS(t), -1)
	if len(ms) != 10 {
		t.Fatalf("want 10 column widths, found %d", len(ms))
	}
	for _, m := range ms {
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			t.Fatalf("column width %q: %v", m[1], err)
		}
		sum += v
	}
	if sum < 99.99 || sum > 100.01 {
		t.Errorf("the positions columns add to %.2f%%, not 100", sum)
	}
}
