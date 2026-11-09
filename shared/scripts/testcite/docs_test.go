package main

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// gitTree is writeTree with every file staged in a fresh repo: -docs lists
// the docs with git ls-files, as CI's checkout does.
func gitTree(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	root := writeTree(t, files)
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return root
}

const docsLaneTest = `package lane

import "testing"

func TestLock_Holds(t *testing.T) {
	t.Run("the first hold", func(t *testing.T) {})
}

func ExampleLock() {}
`

// The doc's lines, numbered: the hits below name them.
var docsFixture = strings.Join([]string{
	"# Lanes", // 1
	"",        // 2
	"TestLock_Holds pins the hold; TestLock_Gone is cited and is not there.",          // 3
	"TestLock_Holds/the_first_hold is a case, TestLock_Holds/no_case is not.",         // 4
	"It was TestLock_OldName, and that wrapped gravestone is",                         // 5
	"exempt: TestLock_Older was",                                                      // 6
	"deleted.",                                                                        // 7
	"Run `go test -run 'TestLock_Holds|TestLock_Nothing|ExampleLock' ./lane/`.",       // 8
	"Or -run=TestWire/no_such_level, -run '^TestLock_Holds$', -test.run TestLock.",    // 9
	"`simcalc -run 400m` is another command's flag; `go test -run Zzz ./x/` is not.",  // 10
	"And `go test -run '(bad' ./x/` is no regexp; `-run <pattern>` is a placeholder.", // 11
	"[TestLink_Gone](TestLinkTarget_Gone.md) <!-- TestCommented_Gone -->",             // 12
	"```bash",                            // 13
	"go test -run TestFenced_Gone ./...", // 14
	"# TestInFence_Gone",                 // 15
	"```",                                // 16
}, "\n") + "\n"

func docsTree(t *testing.T) string {
	return gitTree(t, map[string]string{
		"shingo-core/lane/lane_test.go":     docsLaneTest,
		"protocol/wire_test.go":             "package protocol\n\nimport \"testing\"\n\nfunc TestWire_Shape(t *testing.T) {}\n",
		"docs/lanes.md":                     docsFixture,
		"docs/testing/population-ledger.md": "# Ledger\n\nTestLedger_Gone is exempt with its file.\n",
		"CHANGELOG.md":                      "# Changelog\n\nTestChangelog_Gone is exempt.\n",
		"notes.md":                          "# Notes\n\nTestNotes_Gone is out of scope.\n",
	})
}

func TestScanDocs_Fixture(t *testing.T) {
	hits, graves, err := ScanDocs(docsTree(t))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range hits {
		got = append(got, h.String())
	}
	want := []string{
		`docs/lanes.md:3: TestLock_Gone is cited but no such test exists`,
		`docs/lanes.md:4: TestLock_Holds/no_case is cited but TestLock_Holds has no subtest "no_case" that can be read statically`,
		`docs/lanes.md:8: TestLock_Nothing is a -run alternative no Test, Fuzz, Benchmark or Example name matches`,
		`docs/lanes.md:10: Zzz is a -run alternative no Test, Fuzz, Benchmark or Example name matches`,
		"docs/lanes.md:11: (bad is a -run alternative that is not a Go regexp: error parsing regexp: missing closing ): `(bad`",
		`docs/lanes.md:12: TestLink_Gone is cited but no such test exists`,
		`docs/lanes.md:14: TestFenced_Gone is a -run alternative no Test, Fuzz, Benchmark or Example name matches`,
		`docs/lanes.md:15: TestInFence_Gone is cited but no such test exists`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hits =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	var gotGraves []string
	for _, g := range graves {
		gotGraves = append(gotGraves, g.String())
	}
	wantGraves := []string{
		`docs/lanes.md:5: TestLock_OldName is a gravestone`,
		`docs/lanes.md:6: TestLock_Older is a gravestone`,
	}
	if !reflect.DeepEqual(gotGraves, wantGraves) {
		t.Errorf("graves =\n  %s\nwant\n  %s", strings.Join(gotGraves, "\n  "), strings.Join(wantGraves, "\n  "))
	}
}

func TestRunArgs_Shapes(t *testing.T) {
	cases := []struct {
		text string
		want []string // the judged alternatives
	}{
		{"go test -run TestA ./x/", []string{"TestA"}},
		{"go test -run 'TestA|TestB/case|x' ./x/", []string{"TestA", "TestB"}},
		{`go test -run="^(TestA|TestB)$"`, []string{"^(TestA|TestB)$"}},
		{"(`-run TestA`).", []string{"TestA"}},
		{"see -run TestA.", []string{"TestA"}},
		{"go test -v \\\n  -run Catid4x2 ./scenarios/", []string{"Catid4x2"}},
		{"-test.run=Lane", []string{"Lane"}},
		// Not judged.
		{"a dry-run and a re-run", nil},
		{"simcalc -run 400m", nil},
		{"go test -run $PATTERN", nil},
		{"go test -run -v", nil},
		{"the `-run` flag", nil},
	}
	for _, c := range cases {
		var got []string
		for _, a := range runArgs(c.text) {
			for _, alt := range a.alts {
				got = append(got, alt.text)
			}
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("runArgs(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}
