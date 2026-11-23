package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// --- integration-slices ----------------------------------------------------
//
// GitHub runs the integration module's docker suite twice, plain and under
// -race, each as a matrix of slices by top-level test name
// (.github/workflows/integration.yml): a `go test ... "${slice[@]}" ./...`
// whose slice is chosen by a `case "$SLICE" in` arm per matrix value. Today
// slice a is `-run "^Test($SLICE_A)"`, b `-run "^Test($SLICE_B)"` and c the
// complement, `-skip "^Test($SLICE_A|$SLICE_B)"`. GitHub is the full check:
// every test of the module must run in exactly one slice of each job. In none,
// it runs nowhere on GitHub; in two, it is paid for twice.
//
// THE RULE RUNS THE SELECTION, IT DOES NOT TRUST THE COMMENT. It reads the
// slice variables, each job's matrix values and case arms from the workflow,
// lists the module's top-level tests from its _test.go files as those jobs
// build them (linux/amd64, tags docker and lockcheck, and race for the job
// that passes -race), and matches every name as go test does: the pattern is
// split as testing's splitRegexp splits it (an unbracketed | alternates, an
// unbracketed / starts a subtest element), -run keeps a test whose name
// matches an alternative's first element, and -skip drops one only on a full
// match. A pattern with a subtest element is reported, not read: it selects
// part of a test, which a list of top-level names cannot see.
//
// A job, an arm or a test list it cannot read is a finding: a selection the
// rule did not evaluate must not read as one that holds.

const integrationWorkflow = ".github/workflows/integration.yml"

// sliceEnvRe is a workflow env line naming a slice pattern: `SLICE_A: "..."`.
var sliceEnvRe = regexp.MustCompile(`^\s+(SLICE_[A-Z0-9_]+):\s*"([^"\\]*)"\s*$`)

// sliceArmRe is a case arm choosing a slice: `a) slice=(-run "^Test($SLICE_A)") ;;`.
var sliceArmRe = regexp.MustCompile(`^\s+([a-z0-9]+)\)\s*slice=\((-run|-skip)\s+"([^"]*)"\)\s*;;\s*$`)

// sliceMatrixRe is a job's matrix line: `slice: [a, b, c]`.
var sliceMatrixRe = regexp.MustCompile(`^\s+slice:\s*\[([^\]]*)\]\s*$`)

// jobRe is a job's first line under `jobs:`, two spaces in.
var jobRe = regexp.MustCompile(`^  ([A-Za-z0-9_-]+):\s*$`)

// sliceVarRe is a shell reference to a variable in an arm's pattern.
var sliceVarRe = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?`)

// sliceArm is one case arm: the matrix value, the flag and the expanded
// pattern.
type sliceArm struct {
	value, flag, pattern string
	line                 int
}

// sliceJob is one job of the workflow that runs go test by slice.
type sliceJob struct {
	name   string
	line   int
	race   bool
	matrix []string
	arms   map[string]sliceArm
}

// integrationTest is one top-level test of the integration module.
type integrationTest struct {
	name, file string // file is the repo path
	line       int
	dir        string // the package directory, repo path
	decl       *ast.FuncDecl
	tparam     string // the *testing.T parameter's name, "" for an example or a fuzz target
	plainOnly  bool   // built without -race only
	raceOnly   bool   // built with -race only
}

// goPattern is a -run or -skip pattern as testing reads it: alternatives, each
// a sequence of per-level regexps.
type goPattern [][]*regexp.Regexp

// splitTestPattern is testing's splitRegexp (src/testing/match.go, go1.25),
// compiled: | and / split only outside brackets and parentheses, and a
// backslash escapes the next byte.
func splitTestPattern(s string) (goPattern, error) {
	var alts [][]string
	var cur []string
	cs, cp := 0, 0
	for i := 0; i < len(s); {
		switch s[i] {
		case '[':
			cs++
		case ']':
			if cs--; cs < 0 {
				cs = 0
			}
		case '(':
			if cs == 0 {
				cp++
			}
		case ')':
			if cs == 0 {
				cp--
			}
		case '\\':
			i++
		case '/', '|':
			if cs == 0 && cp == 0 {
				cur = append(cur, s[:i])
				if s[i] == '|' {
					alts = append(alts, cur)
					cur = nil
				}
				s = s[i+1:]
				i = 0
				continue
			}
		}
		i++
	}
	alts = append(alts, append(cur, s))
	out := make(goPattern, len(alts))
	for i, alt := range alts {
		for _, el := range alt {
			re, err := regexp.Compile(el)
			if err != nil {
				return nil, err
			}
			out[i] = append(out[i], re)
		}
	}
	return out, nil
}

// subtestElement reports whether any alternative names a subtest level.
func (p goPattern) subtestElement() bool {
	for _, alt := range p {
		if len(alt) > 1 {
			return true
		}
	}
	return false
}

// matchesTop reports whether a top-level name matches an alternative's first
// element: for -run, the test runs; for a one-element -skip, it is skipped.
func (p goPattern) matchesTop(name string) bool {
	for _, alt := range p {
		if alt[0].MatchString(name) {
			return true
		}
	}
	return false
}

// readSliceJobs reads the slice variables and every job that runs go test by
// slice. It returns findings for what it cannot read.
func readSliceJobs(f *file) (jobs []*sliceJob, findings []Finding) {
	vars := map[string]string{}
	var cur *sliceJob
	inJobs := false
	eachLine(f.data, func(n int, raw []byte) {
		line := strings.TrimRight(string(raw), "\r")
		if m := sliceEnvRe.FindStringSubmatch(line); m != nil {
			vars[m[1]] = m[2]
		}
		if line == "jobs:" {
			inJobs = true
			return
		}
		if !inJobs {
			return
		}
		if m := jobRe.FindStringSubmatch(line); m != nil {
			cur = &sliceJob{name: m[1], line: n, arms: map[string]sliceArm{}}
			jobs = append(jobs, cur)
			return
		}
		if cur == nil {
			return
		}
		if m := sliceMatrixRe.FindStringSubmatch(line); m != nil {
			for _, v := range strings.Split(m[1], ",") {
				if v = strings.TrimSpace(v); v != "" {
					cur.matrix = append(cur.matrix, v)
				}
			}
		}
		if m := sliceArmRe.FindStringSubmatch(line); m != nil {
			cur.arms[m[1]] = sliceArm{value: m[1], flag: m[2], pattern: m[3], line: n}
		}
		if strings.Contains(line, "go test") && strings.Contains(line, "-race") {
			cur.race = true
		}
	})
	var sliced []*sliceJob
	for _, j := range jobs {
		if j.matrix == nil && len(j.arms) == 0 {
			continue
		}
		sliced = append(sliced, j)
		if len(j.matrix) == 0 {
			findings = append(findings, Finding{File: f.path, Line: j.line, Why: fmt.Sprintf("job %s has slice arms and no `slice: [...]` matrix line the rule can read", j.name)})
			continue
		}
		for _, v := range j.matrix {
			arm, ok := j.arms[v]
			if !ok {
				findings = append(findings, Finding{File: f.path, Line: j.line, Why: fmt.Sprintf("job %s: matrix value %s has no `%s) slice=(-run|-skip \"...\") ;;` arm the rule can read", j.name, v, v)})
				continue
			}
			var unknown []string
			arm.pattern = sliceVarRe.ReplaceAllStringFunc(arm.pattern, func(ref string) string {
				name := sliceVarRe.FindStringSubmatch(ref)[1]
				val, ok := vars[name]
				if !ok {
					unknown = append(unknown, name)
				}
				return val
			})
			if len(unknown) > 0 {
				findings = append(findings, Finding{File: f.path, Line: arm.line, Why: fmt.Sprintf("job %s slice %s: the pattern reads %s, which no `SLICE_...: \"...\"` env line sets", j.name, v, strings.Join(unknown, ", "))})
				delete(j.arms, v)
				continue
			}
			j.arms[v] = arm
		}
	}
	if len(sliced) == 0 {
		findings = append(findings, Finding{File: f.path, Why: "no job runs go test by slice (a `slice: [...]` matrix and its `case \"$SLICE\"` arms): the rule read nothing"})
	}
	return sliced, findings
}

// integrationTests lists the integration module's top-level tests, examples
// and fuzz targets, with which build (plain, race) compiles each file.
func integrationTests(t *tree) (tests []*integrationTest, findings []Finding) {
	data := map[string][]byte{}
	for _, f := range t.files {
		if f.data != nil {
			data[f.path] = f.data
		}
	}
	ctx := func(race bool) *build.Context {
		c := build.Default
		c.GOOS, c.GOARCH, c.CgoEnabled, c.Compiler = "linux", "amd64", true, "gc"
		c.BuildTags = []string{"docker", "lockcheck"}
		if race {
			c.BuildTags = append(c.BuildTags, "race")
		}
		c.JoinPath = path.Join
		c.OpenFile = func(p string) (io.ReadCloser, error) {
			b, ok := data[p]
			if !ok {
				return nil, fmt.Errorf("not in the tree: %s", p)
			}
			return io.NopCloser(bytes.NewReader(b)), nil
		}
		return &c
	}
	plain, race := ctx(false), ctx(true)
	fset := token.NewFileSet()
	for _, f := range t.files {
		if f.data == nil || !strings.HasPrefix(f.path, "integration/") || !strings.HasSuffix(f.path, "_test.go") || ignoredGoDir(f.path) {
			continue
		}
		dir, name := path.Split(f.path)
		dir = strings.TrimSuffix(dir, "/")
		inPlain, err1 := plain.MatchFile(dir, name)
		inRace, err2 := race.MatchFile(dir, name)
		if err1 != nil || err2 != nil {
			findings = append(findings, Finding{File: f.path, Why: fmt.Sprintf("cannot read its build constraints: %v", firstErr(err1, err2))})
			continue
		}
		if !inPlain && !inRace {
			continue
		}
		af, err := parser.ParseFile(fset, f.path, f.data, parser.SkipObjectResolution)
		if err != nil {
			findings = append(findings, Finding{File: f.path, Why: "does not parse: " + err.Error()})
			continue
		}
		testing := testingImportName(af)
		for _, d := range af.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv != nil {
				continue
			}
			tp, ok := goTestFunc(fd, testing)
			if !ok {
				continue
			}
			tests = append(tests, &integrationTest{name: fd.Name.Name, file: f.path, line: fset.Position(fd.Pos()).Line,
				dir: dir, decl: fd, tparam: tp, plainOnly: inPlain && !inRace, raceOnly: inRace && !inPlain})
		}
	}
	if len(tests) == 0 {
		findings = append(findings, Finding{Why: "the integration module has no top-level test the rule can read: it read nothing"})
	}
	return tests, findings
}

// ignoredGoDir reports whether go would skip the file's directory: testdata,
// or an element starting with _ or .
func ignoredGoDir(p string) bool {
	parts := strings.Split(p, "/")
	for _, el := range parts[:len(parts)-1] {
		if el == "testdata" || strings.HasPrefix(el, "_") || strings.HasPrefix(el, ".") {
			return true
		}
	}
	return false
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// testingImportName is the name the file imports "testing" under, "" when it
// does not.
func testingImportName(af *ast.File) string {
	for _, im := range af.Imports {
		if im.Path.Value == `"testing"` {
			if im.Name != nil {
				return im.Name.Name
			}
			return "testing"
		}
	}
	return ""
}

// goTestFunc reports whether fd is a function go test runs by name (a Test
// taking *testing.T, a Fuzz taking *testing.F, or an Example with no
// parameters), and the *testing.T parameter's name for a Test.
func goTestFunc(fd *ast.FuncDecl, testing string) (tparam string, ok bool) {
	name := fd.Name.Name
	params := fd.Type.Params.List
	switch {
	case isGoTestName(name, "Test"):
		if name == "TestMain" || testing == "" || len(params) != 1 || len(params[0].Names) > 1 || !isTestingPtr(params[0].Type, testing, "T") {
			return "", false
		}
		if len(params[0].Names) == 1 {
			tparam = params[0].Names[0].Name
		}
		return tparam, true
	case isGoTestName(name, "Fuzz"):
		return "", testing != "" && len(params) == 1 && isTestingPtr(params[0].Type, testing, "F")
	case isGoTestName(name, "Example"):
		return "", len(params) == 0 && fd.Type.Results == nil
	}
	return "", false
}

// isGoTestName is go test's isTest: the prefix, then nothing or a rune that is
// not lower case.
func isGoTestName(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	if len(name) == len(prefix) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(name[len(prefix):])
	return !unicode.IsLower(r)
}

func isTestingPtr(e ast.Expr, testing, typ string) bool {
	st, ok := e.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := st.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == testing && sel.Sel.Name == typ
}

func integrationSlices(t *tree) result {
	var wf *file
	for _, f := range t.files {
		if f.path == integrationWorkflow {
			wf = f
		}
	}
	if wf == nil || wf.data == nil {
		return result{findings: []Finding{{File: integrationWorkflow, Why: "missing: the rule has no slices to read"}}}
	}
	jobs, findings := readSliceJobs(wf)
	tests, tf := integrationTests(t)
	findings = append(findings, tf...)
	counts := map[string]int{}
	for _, j := range jobs {
		patterns := map[string]goPattern{}
		for _, v := range j.matrix {
			arm, ok := j.arms[v]
			if !ok {
				continue
			}
			p, err := splitTestPattern(arm.pattern)
			if err != nil {
				findings = append(findings, Finding{File: wf.path, Line: arm.line, Why: fmt.Sprintf("job %s slice %s: %s %q does not compile: %v", j.name, v, arm.flag, arm.pattern, err)})
				continue
			}
			if p.subtestElement() {
				findings = append(findings, Finding{File: wf.path, Line: arm.line, Why: fmt.Sprintf("job %s slice %s: %s %q names a subtest level, which selects part of a test: the rule reads top-level names only", j.name, v, arm.flag, arm.pattern)})
				continue
			}
			patterns[v] = p
		}
		if len(patterns) != len(j.matrix) {
			continue // reported above; a partial selection proves nothing
		}
		for _, tc := range tests {
			if (j.race && tc.plainOnly) || (!j.race && tc.raceOnly) {
				continue
			}
			var in []string
			for _, v := range j.matrix {
				p, arm := patterns[v], j.arms[v]
				runs := p.matchesTop(tc.name)
				if arm.flag == "-skip" {
					runs = !runs
				}
				if runs {
					in = append(in, v)
				}
			}
			if len(in) != 1 {
				where := "no slice"
				if len(in) > 1 {
					where = "slices " + strings.Join(in, " and ")
				}
				findings = append(findings, Finding{File: tc.file, Line: tc.line, Why: fmt.Sprintf("%s runs in %s of %s's job %s, want exactly one", tc.name, where, integrationWorkflow, j.name)})
				continue
			}
			counts[j.name+" "+in[0]]++
		}
	}
	var parts []string
	for _, j := range jobs {
		var per []string
		for _, v := range j.matrix {
			per = append(per, fmt.Sprintf("%s %d", v, counts[j.name+" "+v]))
		}
		parts = append(parts, j.name+": "+strings.Join(per, ", "))
	}
	sort.Strings(parts)
	return result{findings: findings, ok: fmt.Sprintf("%d integration tests, each in one slice of every job; %s", len(tests), strings.Join(parts, "; "))}
}
