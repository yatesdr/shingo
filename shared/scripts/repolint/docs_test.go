package main

import (
	"bytes"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The doc's lines, numbered: the findings below name them.
var docRefsDoc = lines(
	"# Sim", // 1
	"",      // 2
	"`engine/foo.go` resolves module-relative; `engine/gone.go` does not.",                         // 3
	"`binresolver/group_resolver.go`, `group_resolver.go`, `shingocore/engine`, `../lanes.md`.",    // 4
	"A line: `engine/foo.go:12`, and in prose www/templates/nodes.html:9.",                         // 5
	"[ok](../lanes.md) [dead](../gone.md) [dir](../../shingo-core/) [a](#x) [u](https://x.y/z.md)", // 6
	"`/api/orders`, `text/plain`, `github.com/go-chi/chi/v5`, `shingocore.yaml`, `TestX/case`.",    // 7
	"<!-- generated: protocol/types.go:AllTypes by docgen -->",                                     // 8
	"<!-- generated: protocol/gone.go by docgen -->",                                               // 9
	"```",                                   // 10
	"go test ./x/   # fails at foo.go:7",    // 11
	"`engine/fenced_gone.go`",               // 12
	"```",                                   // 13
	`<img src="../assets/gone.svg" alt="">`, // 14
)

func docRefsTree() *tree {
	return treeOf(
		"shingo-core/engine/foo.go", "package engine\n",
		"shingo-core/dispatch/binresolver/group_resolver.go", "package binresolver\n",
		"shingo-edge/www/templates/nodes.html", "<p>\n",
		"protocol/types.go", "package protocol\n",
		"docs/lanes.md", "# Lanes\n",
		"docs/dev-env/sim.md", docRefsDoc,
		"docs/testing/population-ledger.md", "# L\n\n`engine/gone.go` and gone.go:1 are exempt with the file.\n",
		"CHANGELOG.md", "# C\n\n`engine/gone.go`\n",
		"notes/x.md", "# N\n\n`engine/gone.go` is out of scope.\n",
	)
}

func TestDocReferences(t *testing.T) {
	res := docReferences(docRefsTree())
	want := []string{
		"docs/dev-env/sim.md:3",  // a dead cited path
		"docs/dev-env/sim.md:5",  // a line number in a code span
		"docs/dev-env/sim.md:5",  // and one in prose
		"docs/dev-env/sim.md:6",  // a dead relative link
		"docs/dev-env/sim.md:9",  // a dead generated source
		"docs/dev-env/sim.md:11", // a line number in a fence
		"docs/dev-env/sim.md:14", // a dead img src
	}
	sort.Strings(want)
	if got := wheres(res.findings); !reflect.DeepEqual(got, want) {
		t.Errorf("findings at %q, want %q\n%v", got, want, res.findings)
	}
	var whys []string
	for _, f := range res.findings {
		whys = append(whys, f.Why)
	}
	sort.Strings(whys)
	for _, must := range []string{
		"cites path engine/gone.go -- no such tracked file or directory",
		"cites engine/foo.go:12 -- a doc cites a line number; cite the symbol",
		"cites www/templates/nodes.html:9 -- a doc cites a line number; cite the symbol",
		"links ../gone.md -- no such tracked file or directory relative to the doc",
		"cites generated source protocol/gone.go -- no such tracked file or directory",
		"links ../assets/gone.svg -- no such tracked file or directory relative to the doc",
	} {
		if !strings.Contains(strings.Join(whys, "\n"), must) {
			t.Errorf("no finding says %q; findings:\n%s", must, strings.Join(whys, "\n"))
		}
	}
}

func TestRunDocs_PrintsEveryFinding(t *testing.T) {
	var body []string
	body = append(body, "# Many", "")
	for i := 0; i < maxShown+5; i++ {
		body = append(body, fmt.Sprintf("`gone/file%d.go`", i))
	}
	var buf bytes.Buffer
	if runDocs(treeOf("docs/many.md", lines(body...)), &buf) {
		t.Fatal("a dead path must fail the run")
	}
	if got := strings.Count(buf.String(), "FAIL comment-references  docs/many.md:"); got != maxShown+5 {
		t.Errorf("printed %d findings, want all %d: the run is a worklist\n%s", got, maxShown+5, buf.String())
	}
	buf.Reset()
	if !runDocs(treeOf("docs/clean.md", "# Clean\n\n`docs/clean.md`\n"), &buf) ||
		buf.String() != "ok   comment-references (1 docs: no line numbers; every cited path and link resolves)\n" {
		t.Errorf("clean output %q", buf.String())
	}
}
