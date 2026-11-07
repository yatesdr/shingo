package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func cited(cs []citation) []string {
	var out []string
	for _, c := range cs {
		s := c.text
		if c.grave {
			s += " (grave)"
		}
		if c.run {
			s += " (run)"
		}
		out = append(out, s)
	}
	return out
}

func TestCitations_Shapes(t *testing.T) {
	cases := []struct {
		text string
		want []string
	}{
		{"pinned by TestLaneLock_Holds and TestLaneLock_Frees.", []string{"TestLaneLock_Holds", "TestLaneLock_Frees"}},
		{"see TestRelease/at_the_wait/second_leg, and", []string{"TestRelease/at_the_wait/second_leg"}},
		{"the cases TestRelease/a... cover it", []string{"TestRelease/a"}},
		// Not citations of one test.
		{"every TestPlanReshuffle_ case", nil},
		{"TestPlanReshuffle* and TestPlan.* both", nil},
		{"TestMain opens the pool", nil},
		{"harness.TestHarness builds it", nil},
		{"Testing is not a name, nor is TestingT", nil},
		// Fuzz and Benchmark only after a PIN: marker.
		{"BenchmarkScan and FuzzDecode in prose", nil},
		{"PIN: FuzzDecode", []string{"FuzzDecode"}},
		{"PIN:BenchmarkScan_Repo", []string{"BenchmarkScan_Repo"}},
		// A -run argument is a pattern; a $ anchors it.
		{"go test -run TestGolden -update", []string{"TestGolden (run)"}},
		{"-run '^TestGolden$'", []string{"TestGolden"}},
		{"-run=TestBudget_C2c -v", []string{"TestBudget_C2c (run)"}},
	}
	for _, c := range cases {
		if got := cited(citations(c.text)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("citations(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}

func TestCitations_Gravestones(t *testing.T) {
	graves := []string{
		"It was TestStaleName, and it asserted the opposite.",
		"This was TestStaleName: a bucket delta",
		"(Was TestStaleName, which pinned it)",
		"It was called TestStaleName and it asserted the same check.",
		"used to be TestStaleName.",
		"TestNewName INVERTS TestStaleName, which",
		"TestNewName is the inverted\nTestStaleName: it",
		"It replaces TestStaleName, whose door is gone",
		"TestNewName ports TestStaleName: a run of",
		"this test was previously TestStaleName in store",
		"(Item 8 deleted TestStaleName and its fixture)",
		"Inverted pin: at base (TestStaleName) the count",
		"TestStaleName WAS HERE AND IS DELETED.",
		"TestStaleName stood here and pinned the refusal",
		"TestOne and TestStaleName were\ndeleted with the claim",
		"NOTE: TestStaleName was REMOVED with the trigger",
		"(TestStaleName) deleted with the overrides",
	}
	for _, text := range graves {
		cs := citations(text)
		var got *citation
		for i := range cs {
			if cs[i].name == "TestStaleName" {
				got = &cs[i]
			}
		}
		if got == nil || !got.grave {
			t.Errorf("citations(%q): TestStaleName is not read as a gravestone: %q", text, cited(cs))
		}
	}
	live := []string{
		"TestStaleName pins that the removed bin stays off the node",
		"the bin was removed; TestStaleName says so",
		"TestStaleName checks the removal order",
		"the unremoved TestStaleName", // "removed" inside a word
		"TestStaleName deletedness",   // "deleted" inside a word
	}
	for _, text := range live {
		for _, c := range citations(text) {
			if c.name == "TestStaleName" && c.grave {
				t.Errorf("citations(%q): TestStaleName read as a gravestone; it is a live claim", text)
			}
		}
	}
}

func TestCitations_OffsetsFindTheLine(t *testing.T) {
	c := comment{text: " first line\n second TestX here\n third", lines: []int{10, 11, 12}}
	cs := citations(c.text)
	if len(cs) != 1 || c.lineAt(cs[0].start) != 11 {
		t.Fatalf("citations = %+v; want TestX on line 11", cs)
	}
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// The fixture's production file cites tests across two modules; its test
// files declare them, their subtests in every form the check reads, and
// two names that are not tests.
const fixtureLane = `package lane

// Lock holds a lane. Pinned by TestLock_Holds, and by protocol's
// TestWire_Shape in another module. TestLock_Frees is not there.
// PIN: TestLock_Holds
// PIN: FuzzLock_Decode
// PIN: TestLock_Gone
// TestLock_Holds/the_first_hold and TestLock_Holds/a_second_hold are cases;
// TestLock_Holds/no_such_case is not.
// TestTable/row_one, TestTable/outside_row, TestMapped/key_b, TestSliced/b
// and TestTagged/docker_case resolve; TestBuilt/x_1 is built at run time.
// It was TestLock_OldName; TestLock_Older was deleted with the old lock.
// TestHarness is a type, not a test; go test -run TestLock_ covers both and
// -run TestLock_H matches TestLock_Holds, while -run TestNothingAtAll matches none.
// TestLock_Method is a method, not a test.
// PIN: TestHarness names a type, and PIN: TestLock_Older was deleted: a PIN:
// marker names a test by contract, so neither is exempt.
func Lock() {}
`

const fixtureLaneTest = `package lane

import (
	"fmt"
	"testing"
)

type TestHarness struct{}

var outside = []struct{ name string }{{name: "outside row"}}

func TestLock_Holds(t *testing.T) {
	t.Run("the first hold", func(t *testing.T) {})
	t.Run("a second hold", func(t *testing.T) {})
}

func TestTable(t *testing.T) {
	for _, tc := range []struct{ name string }{{name: "row one"}} {
		t.Run(tc.name, func(t *testing.T) {})
	}
	for _, tc := range outside {
		t.Run(tc.name, func(t *testing.T) {})
	}
}

func TestMapped(t *testing.T) {
	for name := range map[string]int{"key_a": 1, "key_b": 2} {
		t.Run(name, func(t *testing.T) {})
	}
}

func TestSliced(t *testing.T) {
	for _, name := range []string{"a", "b"} {
		t.Run(name, func(t *testing.T) {})
	}
}

func TestBuilt(t *testing.T) {
	for i := 0; i < 2; i++ {
		t.Run(fmt.Sprintf("x_%d", i), func(t *testing.T) {})
	}
}

func FuzzLock_Decode(f *testing.F) {}

// A method spelled like a test is not one.
func (TestHarness) TestLock_Method() {}
`

const fixtureTaggedTest = `//go:build docker

package lane

import "testing"

func TestTagged(t *testing.T) {
	t.Run("docker case", func(t *testing.T) {})
}
`

func fixtureTree(t *testing.T) string {
	return writeTree(t, map[string]string{
		"shingo-core/lane/lane.go":                fixtureLane,
		"shingo-core/lane/lane_test.go":           fixtureLaneTest,
		"shingo-core/lane/lane_docker_test.go":    fixtureTaggedTest,
		"protocol/wire_test.go":                   "package protocol\n\nimport \"testing\"\n\nfunc TestWire_Shape(t *testing.T) {}\n",
		"shingo-core/lane/testdata/broken.go":     "package ( not go",
		"shingo-core/vendor/dep/dep.go":           "package dep\n// TestVendored is not ours\n",
		"shingo-core/node_modules/x/y.go":         "package ( not go",
		"shingo-core/.gate/stale.go":              "package ( not go",
		"scripts/outside_the_modules.go":          "package scripts\n// TestOutsideTheModules is not read\n",
		"shingo-edge/www/static/vendor.min.js.go": "package static\n",
	})
}

func TestScan_Fixture(t *testing.T) {
	hits, graves, err := Scan(fixtureTree(t))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range hits {
		got = append(got, h.String())
	}
	want := []string{
		`shingo-core/lane/lane.go:4: TestLock_Frees is cited but no such test exists`,
		`shingo-core/lane/lane.go:7: TestLock_Gone is cited but no such test exists`,
		`shingo-core/lane/lane.go:9: TestLock_Holds/no_such_case is cited but TestLock_Holds has no subtest "no_such_case" that can be read statically`,
		`shingo-core/lane/lane.go:11: TestBuilt/x_1 is cited but TestBuilt has no subtest "x_1" that can be read statically`,
		`shingo-core/lane/lane.go:14: TestNothingAtAll is a -run pattern no test name contains`,
		`shingo-core/lane/lane.go:16: TestHarness is cited but no such test exists`,
		`shingo-core/lane/lane.go:16: TestLock_Older is cited but no such test exists`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hits =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	var gotGraves []string
	for _, g := range graves {
		gotGraves = append(gotGraves, g.String())
	}
	wantGraves := []string{
		`shingo-core/lane/lane.go:12: TestLock_OldName is a gravestone`,
		`shingo-core/lane/lane.go:12: TestLock_Older is a gravestone`,
	}
	if !reflect.DeepEqual(gotGraves, wantGraves) {
		t.Errorf("graves =\n  %s\nwant\n  %s", strings.Join(gotGraves, "\n  "), strings.Join(wantGraves, "\n  "))
	}
}

func TestScan_ParseErrorFails(t *testing.T) {
	root := writeTree(t, map[string]string{"shared/bad.go": "package ( not go"})
	if _, _, err := Scan(root); err == nil {
		t.Fatal("Scan on an unparseable file returned no error; it must not pass silently")
	}
}
