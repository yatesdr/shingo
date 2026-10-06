package www

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// processes_desktop_list_state_column_test.go — the process list's STATE
// column is as wide as the longest running-style name in it.
//
// The name is whole and on one line (.pd-statename is nowrap), and the list
// table was table-layout: fixed with an 8% <col> for the state, so a fixed
// layout never widened it: at 1280 a LOADER-*-RUN name ran 30-40 px past its
// cell into the actions column. The list table lays out by its content now,
// and the state column carries no width of its own, so it takes what its
// longest name needs and the columns with room give way. Each group is its
// own table, so the State heading carries every running name, unseen, and
// every group's column is the same width.
func TestProcessListStateColumnSizesToTheName(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("static", "js", "pages", "processes-desktop.js"))
	if err != nil {
		t.Fatalf("read processes-desktop.js: %v", err)
	}
	js := string(b)

	if !strings.Contains(js, `'<table class="pd-tbl pd-proctbl">' + cols + head`) {
		t.Fatalf("the process list's table must carry pd-proctbl, the class that lays it out by content")
	}
	if !hasDecl(cssRule(t, desktopCSS(t), ".pd-proctbl"), "table-layout", "auto") {
		t.Errorf(".pd-proctbl must be table-layout: auto; a fixed layout never widens a column for its content")
	}

	m := regexp.MustCompile(`(?s)const cols = '<colgroup>(.*?)</colgroup>';`).FindStringSubmatch(js)
	if m == nil {
		t.Fatalf("processes-desktop.js: no colgroup for the process list")
	}
	colsSrc := regexp.MustCompile(`'\s*\+\s*'`).ReplaceAllString(m[1], "")
	col := regexp.MustCompile(`<col[^>]*>`).FindAllString(colsSrc, -1)
	head := regexp.MustCompile(`(?s)const head = '<thead>(.*?)</thead>';`).FindStringSubmatch(js)
	if head == nil {
		t.Fatalf("processes-desktop.js: no thead for the process list")
	}
	ths := strings.Split(head[1], "<th")[1:]
	if len(col) != len(ths) {
		t.Fatalf("process list: %d <col>s for %d headings", len(col), len(ths))
	}
	state := -1
	for i, th := range ths {
		if strings.HasPrefix(th, ">State") {
			state = i
		}
	}
	if state < 0 {
		t.Fatalf("process list: no State heading")
	}
	if !strings.HasPrefix(ths[state], ">State' + sizer + '</th>") {
		t.Errorf("process list: the State heading must carry the sizer, so every group's column is one width")
	}
	if !hasDecl(cssRule(t, desktopCSS(t), ".pd-statesizer span"), "white-space", "nowrap") {
		t.Errorf(".pd-statesizer span must be nowrap; it measures each name on one line, as the cell shows it")
	}
	if strings.Contains(col[state], "width") {
		t.Errorf("process list: the State <col> sets a width (%s); it must size to its longest name", col[state])
	}
}
