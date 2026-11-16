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
	t.Parallel()
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
	t.Parallel()
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

// THE SET PICK SCROLLS WITH WHAT HOLDS IT (owner, 2026-11-15: "its hard to
// scroll the page and the new thing"). Settings capped the pick's list at
// 360 px inside its scrolling column, with the list's overscroll contained:
// two scrolls under one pointer, and the column would not move while the
// pointer was over the list. The list has no height of its own now
// (shared/components.css), the sheet's body or the Settings column scrolls
// it, and the boxes and the "Picking …" head stick at the top of that scroll.
// Each scroller says its top padding as --setpick-top, so the stuck boxes sit
// flush with its edge; a padding changed without it leaves a strip of rows
// showing above them.
func TestSetPickScrollsWithWhatHoldsIt(t *testing.T) {
	t.Parallel()
	comp := sharedComponentsCSS(t)
	list := cssRule(t, comp, ".setpick .picker--open .picker-list")
	for _, want := range [][2]string{{"max-height", "none"}, {"overflow", "visible"}, {"overscroll-behavior", "auto"}} {
		if !hasDecl(list, want[0], want[1]) {
			t.Errorf("components.css .setpick .picker--open .picker-list: want %s: %s; the pick's list must not be a scroll box of its own", want[0], want[1])
		}
	}
	for _, sel := range []string{".setpick-boxes", ".setpick-head"} {
		if !hasDecl(cssRule(t, comp, sel), "position", "sticky") {
			t.Errorf("components.css %s: want position: sticky; what is picked scrolls out of view under the list", sel)
		}
	}
	css := desktopCSS(t)
	if regexp.MustCompile(`\.setpick[^{}]*\.picker-list[^{}]*\{[^}]*max-height`).MatchString(css) {
		t.Errorf("processes-desktop.css gives a set pick's list a height again; it scrolls with what holds it")
	}
	offset := regexp.MustCompile(`--setpick-top:\s*-(\d+)px`)
	padTop := regexp.MustCompile(`(^|[;\s])padding:\s*(\d+)px`)
	for _, c := range []struct{ scroller, padCSS, padSel string }{
		{".pd-modal .layer-body", css, ".pd-modal .layer-body"},
		{"body.pd-body .pd-sheet", comp, ".pd-sheet, .set-ui.set-page"},
	} {
		m := offset.FindStringSubmatch(cssRule(t, css, c.scroller))
		p := padTop.FindStringSubmatch(cssRule(t, c.padCSS, c.padSel))
		if m == nil || p == nil || m[1] != p[2] {
			t.Errorf("%s: --setpick-top %v is not minus its top padding %v", c.scroller, m, p)
		}
	}
}
