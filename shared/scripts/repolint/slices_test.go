package main

import (
	"strings"
	"testing"
)

// sliceWorkflow is a two-job integration workflow shaped like the real one:
// slices a and b by -run of SLICE_A and SLICE_B, c the -skip of both, plain
// and race. Each argument replaces one line of it (by its first words), so a
// case plants one defect. Its arms are lines 15-17 (docker) and 27-29 (race).
func sliceWorkflow(replace ...string) string {
	ls := []string{
		"name: Integration", // 1
		"env:",
		`  SLICE_A: "Alpha|Beta"`,
		`  SLICE_B: "Gamma"`,
		"jobs:", // 5
		"  build:",
		"    runs-on: ubuntu-latest",
		"  docker:",
		"    strategy:",
		"      matrix:", // 10
		"        slice: [a, b, c]",
		"    steps:",
		"      - run: |",
		`          case "$SLICE" in`,
		`            a) slice=(-run "^Test($SLICE_A)") ;;`, // 15
		`            b) slice=(-run "^Test($SLICE_B)") ;;`,
		`            c) slice=(-skip "^Test($SLICE_A|$SLICE_B)") ;;`,
		`            *) echo "no pattern"; exit 1 ;;`,
		"          esac",
		`          go test -tags=docker,lockcheck -count=1 "${slice[@]}" ./...`, // 20
		"  docker-race:",
		"    strategy:",
		"      matrix:",
		"        slice: [a, b, c]",
		"    steps:", // 25
		`      - run: case "$SLICE" in`,
		`            a) slice=(-run "^Test($SLICE_A)") ;;`,
		`            b) slice=(-run "^Test($SLICE_B)") ;;`,
		`            c) slice=(-skip "^Test($SLICE_A|$SLICE_B)") ;;`,
		`          go test -tags=docker,lockcheck -race -count=1 "${slice[@]}" ./...`, // 30
	}
	for _, r := range replace {
		at, with, _ := strings.Cut(r, "=>")
		for i, l := range ls {
			if strings.HasPrefix(strings.TrimSpace(l), at) {
				ls[i] = with
			}
		}
	}
	return lines(ls...)
}

// sliceTests is a scenarios file under the docker tag: a test per slice, one in
// none of a's or b's names (so c's), an example and a helper that is no test.
func sliceTests(extra ...string) string {
	return lines(append([]string{
		"//go:build docker", // 1
		"",
		"package scenarios",
		"",
		`import "testing"`, // 5
		"",
		"func TestAlpha1(t *testing.T) {}",
		"func TestBeta(t *testing.T)   {}",
		"func TestGamma(t *testing.T)  {}",
		"func TestOther(t *testing.T)  {}", // 10
		"func Testlower(t *testing.T)  {}",
		"func TestMain(m *testing.M)   {}",
		"func ExampleOther()           {}",
	}, extra...)...)
}

func sliceCases() []ruleCase {
	wf := integrationWorkflow
	tests := "integration/scenarios/x_test.go"
	clean := treeOf(wf, sliceWorkflow(), tests, sliceTests())
	return []ruleCase{
		{"integration-slices", "every test in exactly one slice of both jobs", fixed(clean), []string{}},
		{"integration-slices", "a test both slices name runs twice, in each job", fixed(treeOf(
			wf, sliceWorkflow(`SLICE_B:=>  SLICE_B: "Gamma|Alpha"`), tests, sliceTests())),
			[]string{tests + ":7", tests + ":7"}},
		{"integration-slices", "a c that runs instead of skipping drops the rest and doubles the named", fixed(treeOf(
			wf, sliceWorkflow(`c) slice=(-skip=>            c) slice=(-run "^Test($SLICE_A|$SLICE_B)") ;;`), tests, sliceTests())),
			// docker and race: Alpha1, Beta and Gamma in two slices; Other and the example in none.
			[]string{tests + ":7", tests + ":7", tests + ":8", tests + ":8", tests + ":9", tests + ":9",
				tests + ":10", tests + ":10", tests + ":13", tests + ":13"}},
		{"integration-slices", "a subtest element is not read", fixed(treeOf(
			wf, sliceWorkflow(`a) slice=(-run=>            a) slice=(-run "^Test($SLICE_A)/^x$") ;;`), tests, sliceTests())),
			[]string{wf + ":15", wf + ":27"}},
		{"integration-slices", "a variable no env line sets", fixed(treeOf(
			wf, sliceWorkflow(`b) slice=(-run=>            b) slice=(-run "^Test($SLICE_X)") ;;`), tests, sliceTests())),
			[]string{wf + ":16", wf + ":28"}},
		{"integration-slices", "a matrix value with no arm", fixed(treeOf(
			wf, sliceWorkflow("slice: [a, b, c]=>        slice: [a, b, c, d]"), tests, sliceTests())),
			[]string{wf + ":8", wf + ":21"}},
		{"integration-slices", "build tags decide which job sees a file; testdata and other modules are not read", fixed(treeOf(
			wf, sliceWorkflow(), tests, sliceTests(),
			// Only the plain job builds it, and there it is a's.
			"integration/scenarios/plain_test.go", lines("//go:build docker && !race", "", "package scenarios", "",
				`import "testing"`, "", "func TestAlpha2(t *testing.T) {}"),
			// Under a tag neither job sets: never built, so never selected.
			"integration/scenarios/sim_test.go", lines("//go:build sim", "", "package scenarios", "",
				`import "testing"`, "", "func TestGammaSim(t *testing.T) {}"),
			"integration/scenarios/testdata/t_test.go", lines("package x", "", `import "testing"`, "", "func TestGammaData(t *testing.T) {}"),
			"shingo-core/x_test.go", lines("package x", "", `import "testing"`, "", "func TestGammaCore(t *testing.T) {}"))),
			[]string{}},
		{"integration-slices", "a test built only under -race is checked against the race job", fixed(treeOf(
			wf, sliceWorkflow(`SLICE_B:=>  SLICE_B: "Gamma|AlphaRace"`), tests, sliceTests(),
			"integration/scenarios/race_test.go", lines("//go:build docker && race", "", "package scenarios", "",
				`import "testing"`, "", "func TestAlphaRace(t *testing.T) {}"))),
			[]string{"integration/scenarios/race_test.go:7"}},
		{"integration-slices", "no workflow, nothing to read", fixed(treeOf(tests, sliceTests())), []string{wf}},
		{"integration-slices", "no test, nothing to read", fixed(treeOf(wf, sliceWorkflow())), []string{"-"}},
		{"integration-slices", "no slice job, nothing to read", fixed(treeOf(
			wf, lines("name: Integration", "jobs:", "  build:", "    runs-on: x"), tests, sliceTests())), []string{wf}},
	}
}

// TestIntegrationSlices_CountsEachSlice pins the ok line's account, which is
// what a reader takes as the slice sizes.
func TestIntegrationSlices_CountsEachSlice(t *testing.T) {
	res := integrationSlices(treeOf(integrationWorkflow, sliceWorkflow(), "integration/scenarios/x_test.go", sliceTests()))
	if len(res.findings) != 0 {
		t.Fatalf("findings on the clean tree: %v", res.findings)
	}
	want := "5 integration tests, each in one slice of every job; docker-race: a 2, b 1, c 2; docker: a 2, b 1, c 2"
	if res.ok != want {
		t.Errorf("ok line %q, want %q", res.ok, want)
	}
}

// TestSplitTestPattern_AsTestingSplitsIt pins the port of testing's
// splitRegexp on the shapes the workflows use.
func TestSplitTestPattern_AsTestingSplitsIt(t *testing.T) {
	for _, c := range []struct {
		pat  string
		want string // each alternative in [], its elements space-separated
		deep bool   // an alternative names a subtest level
	}{
		{"^Test(A|B)", "[^Test(A|B)]", false},
		{"^TestA$|^TestB$/^x$", "[^TestA$] [^TestB$ ^x$]", true},
		{"^Test[|/]x", "[^Test[|/]x]", false},
		{`^Test\/x|y`, `[^Test\/x] [y]`, false},
		{"A/B/C", "[A B C]", true},
	} {
		p, err := splitTestPattern(c.pat)
		if err != nil {
			t.Fatalf("%q: %v", c.pat, err)
		}
		var alts []string
		for _, alt := range p {
			var els []string
			for _, re := range alt {
				els = append(els, re.String())
			}
			alts = append(alts, "["+strings.Join(els, " ")+"]")
		}
		if got := strings.Join(alts, " "); got != c.want {
			t.Errorf("%q split to %s, want %s", c.pat, got, c.want)
		}
		if p.subtestElement() != c.deep {
			t.Errorf("%q: subtestElement %v, want %v", c.pat, !c.deep, c.deep)
		}
	}
	p, err := splitTestPattern("^TestA$|^TestB$/^x$")
	if err != nil {
		t.Fatal(err)
	}
	if !p.matchesTop("TestA") || !p.matchesTop("TestB") || p.matchesTop("TestAB") {
		t.Errorf("^TestA$|^TestB$/^x$ must match TestA and TestB at the top level and not TestAB")
	}
}
