package main

import (
	"go/parser"
	"go/token"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

const docsFixtureGo = `package demo

import (
	"github.com/segmentio/kafka-go"
)

var _ kafka.Writer

type BinService struct{}

func (BinService) ApplyArrival() {}

func planTransport() {}

func BuildSeqRemovalSteps() {}

func BuildSeqBackfillSteps() {}
`

// The doc's lines, numbered: the hits below name them.
var docsFixtureDoc = strings.Join([]string{
	"# Demo", // 1
	"",       // 2
	"`planTransport` is live; `staleHelperName` is not.",                                       // 3
	"A span `BinService.ApplyArrival(ctx,",                                                     // 4
	"staleWrappedName)` wraps, and is read whole.",                                             // 5
	"Prose calls staleCallName(x) and reads BinService.ApplyStaleArrival here.",                // 6
	"Bare prose staleBareWord is not read.",                                                    // 7
	"`staleGoneName` was removed: a gravestone line.",                                          // 8
	"`Makefile` and Dockerfile.dev are files; `SYN_{Descriptive}` a placeholder.",              // 9
	"`BuildSeq{Removal,Backfill}Steps` expands live; `BuildSeq{Removal,Stale}Steps` does not.", // 10
	"[`staleLinkText`](staleLinkTarget.md) <!-- `staleCommentName` -->",                        // 11
	"```go",             // 12
	"staleFencedName()", // 13
	"```",               // 14
	"`kafka.LeastBytes` is another module's; `renderBoardView()` is JS.",                         // 15
	"`kafka.ReaderConfig.StartOffset` is kafka-go's, whole; `BinService.StaleFieldName` is not.", // 16
	"`/staleRouteName` and `/api/staleRoutePath/{id}` are routes, not Go names.",                 // 17
}, "\n") + "\n"

func TestScanDocs_Fixture(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	root := writeTree(t, map[string]string{
		"mod/demo.go":                       docsFixtureGo,
		"mod/www/board.js":                  "function renderBoardView() {}\n",
		"Makefile":                          "all:\n",
		"Dockerfile.dev":                    "FROM scratch\n",
		"docs/demo.md":                      docsFixtureDoc,
		"docs/testing/population-ledger.md": "# Ledger\n\n`staleLedgerName` is exempt with its file.\n",
		"notes.md":                          "# Notes\n\n`staleNotesName` is out of scope.\n",
	})
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	got, err := ScanDocs(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []Hit{
		{"docs/demo.md", 3, "staleHelperName"},
		{"docs/demo.md", 4, "staleWrappedName"},
		{"docs/demo.md", 6, "staleCallName"},
		{"docs/demo.md", 6, "ApplyStaleArrival"},
		{"docs/demo.md", 10, "BuildSeqStaleSteps"},
		{"docs/demo.md", 11, "staleLinkText"},
		{"docs/demo.md", 16, "StaleFieldName"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ScanDocs =\n  %v\nwant\n  %v", got, want)
	}
}

func TestExpandBraces(t *testing.T) {
	cases := map[string]string{ // the variants, joined by " | "
		"Build{A,B}Steps":      "BuildASteps | BuildBSteps",
		"SYN_{Descriptive}":    "SYN_",
		"{ok, status, body}":   "{ok, status, body}", // an object literal, not an expansion
		"f(x, {…})":            "f(x, {…})",
		"pre{One,Two} and {z}": "preOne and  | preTwo and ",
		"a{B,C}.d{E,F}()":      "aB.dE() | aB.dF() | aC.dE() | aC.dF()",
	}
	for in, want := range cases {
		if got := strings.Join(expandBraces(in), " | "); got != want {
			t.Errorf("expandBraces(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestExternalName_IsThePackageName pins the names an out-of-repo import is
// used by, where the path's last element is not the package name.
func TestExternalName_IsThePackageName(t *testing.T) {
	src := "package x\n\nimport (\n" +
		"\t\"github.com/segmentio/kafka-go\"\n" +
		"\t\"gopkg.in/yaml.v3\"\n" +
		"\t\"github.com/go-chi/chi/v5\"\n" +
		"\t\"github.com/dustin/go-humanize\"\n" +
		"\tpg \"github.com/jackc/pgx/v5\"\n" +
		"\t\"shingo/protocol\"\n)\n"
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, spec := range f.Imports {
		got = append(got, externalName(spec))
	}
	want := []string{"kafka", "yaml", "chi", "humanize", "pg", ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("externalName = %q, want %q", got, want)
	}
}
