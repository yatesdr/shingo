package www

import (
	"regexp"
	"testing"
)

// processes_desktop_picture_frame_test.go — on the desktop Flows tab the
// picture is never clipped, and the main column is the one vertical scroll.
//
// The frame used to be what gave way: `.pd-pic` shrank to a 240 px floor and
// scrolled inside itself, so on a short window the positions table cut the
// picture in half and the engineer scrolled a box inside a page to see the
// press they were editing. The frame takes the picture's height now and the
// column scrolls; the table may still scroll sideways in its own box, and its
// headings stick against the column's scroll rather than the table's.

// The frame takes the picture's whole height and never scrolls it.
func TestDesktopPictureIsNeverClipped(t *testing.T) {
	decls := cssRule(t, desktopCSS(t), ".pd-pic")
	if regexp.MustCompile(`(^|[;\s])min-height:`).MatchString(decls) {
		t.Errorf(".pd-pic sets a min-height; a floor is only there for a frame that shrinks, and this one must not")
	}
	if regexp.MustCompile(`(^|[;\s])overflow(-y)?:\s*(auto|scroll)`).MatchString(decls) {
		t.Errorf(".pd-pic scrolls vertically; the picture is the frame's height and the column does the scrolling")
	}
	if !hasDecl(decls, "flex", "none") && !hasDecl(decls, "flex", "0 0 auto") && !hasDecl(decls, "flex-shrink", "0") {
		t.Errorf(".pd-pic must not shrink (flex: none); a frame that shrinks below the svg clips the picture")
	}
}

// One vertical scroll: the main column. Nothing in it shrinks to make room,
// so nothing in it needs a scroller of its own.
func TestDesktopMainColumnIsTheOneVerticalScroller(t *testing.T) {
	css := desktopCSS(t)
	if !hasDecl(cssRule(t, css, ".pd-main"), "overflow-y", "auto") {
		t.Errorf(".pd-main must be the vertical scroller (overflow-y: auto) at every window height")
	}
	if regexp.MustCompile(`@media\s*\(max-height`).MatchString(css) {
		t.Errorf("a max-height media query is back; a short window and a tall one are the same layout now")
	}
	box := cssRule(t, css, ".pd-posbox")
	if !hasDecl(box, "flex", "none") {
		t.Errorf(".pd-posbox must not flex (flex: none); a box that shrinks in a scrolling column overlaps the bar")
	}
	if regexp.MustCompile(`(^|[;\s])overflow`).MatchString(box) {
		t.Errorf(".pd-posbox sets overflow; it would become the scroller the sticky headings stick to")
	}
	tbl := cssRule(t, css, ".pd-postbl")
	if !hasDecl(tbl, "overflow-x", "auto") || !hasDecl(tbl, "overflow-y", "hidden") {
		t.Errorf(".pd-postbl must scroll sideways only (overflow-x: auto; overflow-y: hidden)")
	}
	bar := cssRule(t, css, ".pd-bar")
	for _, want := range [][2]string{{"position", "sticky"}, {"bottom", "0"}} {
		if !hasDecl(bar, want[0], want[1]) {
			t.Errorf(".pd-bar: want %s: %s; Save has to stay on screen while the column scrolls", want[0], want[1])
		}
	}
}
