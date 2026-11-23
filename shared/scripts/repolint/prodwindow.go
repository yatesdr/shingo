package main

import (
	"fmt"
	"go/ast"
	"go/types"
	"regexp"
	"sort"
	"strings"
)

// --- production-window -----------------------------------------------------
//
// The push gate skips the integration module's production-window leaves by
// name: the CATID cells that wait out the Edge monitor's production windows in
// real time (scripts/gate.sh, PUSH_SKIP_TESTS; scripts/check-push-skip.sh pins
// that push skips exactly that list and says so). What push loses there must be
// held somewhere, and named:
//
//   - every listed name is a top-level test of the module that calls t.Parallel
//     first, so that a cert or GitHub run should start it with the first wave
//     and not behind the matrices' subtests;
//   - every listed test but the named exemptions is the call of one body with
//     the production window (w6ProductionWindow), and another test of its
//     package makes the same call with the short window (w6ShortWindow): its
//     twin, which push runs. Same body, same arguments, the window swapped;
//   - a listed name is a *_ProductionWindow test or an exemption, and every
//     *_ProductionWindow test is listed (one left out runs its 60 s in push);
//   - no file under .github names a listed test or the suffix, and every -skip
//     there is one of the known complements (integration-slices proves the
//     integration one; sharded-suite's is core's and edge's): GitHub runs them.
//
// An exemption that is no longer listed is reported: the list of what push
// cannot twin must stay true.

// pushSkipVar is the gate's list of the tests push skips by name.
const pushSkipVar = "PUSH_SKIP_TESTS"

// prodWindowSuffix names a production-window leaf.
const prodWindowSuffix = "_ProductionWindow"

// The window arguments a production-window leaf and its twin pass the body.
const (
	prodWindowIdent  = "w6ProductionWindow"
	shortWindowIdent = "w6ShortWindow"
)

// prodWindowExempt are the listed tests with no twin, and why (the ok line
// prints it, so the gap is read every run). A twin of S5
// needs a seam for the Edge's post-cutover verify window
// (postCutoverVerifyWindow, a package var with no Config field), which is
// product code; until it exists S5's shape is proven by cert and GitHub only.
var prodWindowExempt = map[string]string{
	"TestCatid4x2_S5_UnmappedPart": "its twin needs a Config seam for the Edge's post-cutover verify window",
}

// workflowSkipOK are the lines under .github that may pass -skip: the
// integration slices' complement (each test in one slice: integration-slices)
// and the sharded suite's (core and edge, never this module).
var workflowSkipOK = map[string]bool{
	`c) slice=(-skip "^Test($SLICE_A|$SLICE_B)") ;;`:       true,
	`[ -n "$SLICE_SKIP" ] && slice+=(-skip "$SLICE_SKIP")`: true,
}

var pushSkipRe = regexp.MustCompile(`(?s)\n` + pushSkipVar + `="([^"]*)"`)

// workflowSkipRe finds a -skip flag with its pattern in a workflow line: the
// flag, then = or a pattern (quoted, a variable, anchored or a name), so that
// prose saying "-skip over all of them" is not read as one.
var workflowSkipRe = regexp.MustCompile(`(^|[\s(])-(test\.)?skip(=|\s+["'$^A-Z])`)

func productionWindow(t *tree) result {
	var gate *file
	for _, f := range t.files {
		if f.path == "scripts/gate.sh" {
			gate = f
		}
	}
	if gate == nil || gate.data == nil {
		return result{findings: []Finding{{File: "scripts/gate.sh", Why: "missing: the rule has no push skip list to read"}}}
	}
	listed, listLine, findings := readPushSkipList(gate)
	if listed == nil && listLine == 0 {
		return result{findings: findings}
	}
	seen := map[string]bool{}
	for _, name := range listed {
		seen[name] = true
	}
	tests, tf := integrationTests(t)
	findings = append(findings, tf...)
	byName := map[string]*integrationTest{}
	for _, tc := range tests {
		byName[tc.name] = tc
	}
	for _, name := range listed {
		tc := byName[name]
		if tc == nil {
			findings = append(findings, Finding{File: gate.path, Line: listLine, Why: fmt.Sprintf("push skips %s by name, and the integration module has no such test: the sentence names a test that is not there", name)})
			continue
		}
		findings = append(findings, checkListedLeaf(tc, tests, seen)...)
	}
	for name := range prodWindowExempt {
		if !seen[name] {
			findings = append(findings, Finding{File: gate.path, Line: listLine, Why: fmt.Sprintf("%s is exempt from a twin and not listed: drop the exemption", name)})
		}
	}
	for _, tc := range tests {
		if strings.HasSuffix(tc.name, prodWindowSuffix) && !seen[tc.name] {
			findings = append(findings, Finding{File: tc.file, Line: tc.line, Why: fmt.Sprintf("%s is a production-window leaf and not in %s: push would run its window out", tc.name, pushSkipVar)})
		}
	}
	findings = append(findings, workflowsSkipNone(t, listed)...)

	twinned := 0
	for _, name := range listed {
		if prodWindowExempt[name] == "" {
			twinned++
		}
	}
	exempt := make([]string, 0, len(prodWindowExempt))
	for name, why := range prodWindowExempt {
		exempt = append(exempt, name+" ("+why+")")
	}
	sort.Strings(exempt)
	return result{findings: findings, ok: fmt.Sprintf("push skips %d by name, %d with a twin, no twin: %s; no workflow skips them", len(listed), twinned, strings.Join(exempt, ", "))}
}

// readPushSkipList reads PUSH_SKIP_TESTS from gate.sh: the names in order, the
// line the assignment starts on (0 when there is none), and what is wrong with
// it.
func readPushSkipList(gate *file) (listed []string, line int, findings []Finding) {
	m := pushSkipRe.FindSubmatchIndex(gate.data)
	if m == nil {
		return nil, 0, []Finding{{File: gate.path, Why: pushSkipVar + `="..." not found: the rule has no list to read`}}
	}
	line = 1 + strings.Count(string(gate.data[:m[2]]), "\n")
	seen := map[string]bool{}
	for _, w := range strings.Fields(string(gate.data[m[2]:m[3]])) {
		if seen[w] {
			findings = append(findings, Finding{File: gate.path, Line: line, Why: w + " is listed twice"})
			continue
		}
		seen[w] = true
		listed = append(listed, w)
	}
	if len(listed) == 0 {
		findings = append(findings, Finding{File: gate.path, Line: line, Why: pushSkipVar + " is empty: the rule read nothing"})
	}
	return listed, line, findings
}

// checkListedLeaf is what one listed test must be: named as a production-window
// leaf (or exempt), parallel first, and, unless exempt, one body's call at the
// production window with a twin push runs.
func checkListedLeaf(tc *integrationTest, tests []*integrationTest, listed map[string]bool) (findings []Finding) {
	name := tc.name
	if !strings.HasSuffix(name, prodWindowSuffix) && prodWindowExempt[name] == "" {
		findings = append(findings, Finding{File: tc.file, Line: tc.line, Why: fmt.Sprintf("push skips %s, which is neither a *%s test nor an exemption: push skips only production-window leaves", name, prodWindowSuffix)})
	}
	if !parallelFirst(tc) {
		findings = append(findings, Finding{File: tc.file, Line: tc.line, Why: fmt.Sprintf("%s does not call %s.Parallel() first: a production-window leaf starts with the first wave, not after the serial tests", name, paramOr(tc))})
	}
	if prodWindowExempt[name] != "" {
		return findings
	}
	call, n := windowCall(tc.decl, prodWindowIdent)
	if n != 1 {
		return append(findings, Finding{File: tc.file, Line: tc.line, Why: fmt.Sprintf("%s makes %d calls passing %s, want one: the production leaf is one body at the production window", name, n, prodWindowIdent)})
	}
	if twin := findTwin(tests, tc, call, listed); twin == "" {
		findings = append(findings, Finding{File: tc.file, Line: tc.line, Why: fmt.Sprintf("%s has no twin: no test push runs calls %s with %s in place of %s", name, types.ExprString(call), shortWindowIdent, prodWindowIdent)})
	}
	return findings
}

// workflowsSkipNone reports each line under .github that names a listed test
// or the suffix, and each -skip there that is not a known complement.
func workflowsSkipNone(t *tree, listed []string) (findings []Finding) {
	names := append([]string{prodWindowSuffix}, listed...)
	for _, f := range t.files {
		if f.data == nil || !strings.HasPrefix(f.path, ".github/") {
			continue
		}
		eachLine(f.data, func(n int, raw []byte) {
			line := strings.TrimSpace(strings.TrimRight(string(raw), "\r"))
			for _, nm := range names {
				if strings.Contains(line, nm) {
					findings = append(findings, Finding{File: f.path, Line: n, Why: fmt.Sprintf("names %s: GitHub runs every production-window leaf, and selects none by name", nm)})
				}
			}
			if strings.HasPrefix(line, "#") || !workflowSkipRe.MatchString(line) {
				return
			}
			if !workflowSkipOK[line] {
				findings = append(findings, Finding{File: f.path, Line: n, Why: "a -skip that is not a known slice complement: GitHub is the full check, and skips nothing"})
			}
		})
	}
	return findings
}

func paramOr(tc *integrationTest) string {
	if tc.tparam == "" {
		return "t"
	}
	return tc.tparam
}

// parallelFirst reports whether the test's first statement is
// <param>.Parallel().
func parallelFirst(tc *integrationTest) bool {
	if tc.tparam == "" || tc.decl.Body == nil || len(tc.decl.Body.List) == 0 {
		return false
	}
	es, ok := tc.decl.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	return types.ExprString(es.X) == tc.tparam+".Parallel()"
}

// windowCall returns the call in fd's body that passes ident as an argument,
// and how many such calls there are.
func windowCall(fd *ast.FuncDecl, ident string) (*ast.CallExpr, int) {
	var found *ast.CallExpr
	n := 0
	ast.Inspect(fd.Body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && hasIdentArg(call, ident) {
			found = call
			n++
		}
		return true
	})
	return found, n
}

// hasIdentArg reports whether one of call's arguments is the identifier.
func hasIdentArg(call *ast.CallExpr, ident string) bool {
	for _, a := range call.Args {
		if id, ok := a.(*ast.Ident); ok && id.Name == ident {
			return true
		}
	}
	return false
}

// callShape is a call's function and arguments as source, with the window
// argument written as a placeholder.
func callShape(call *ast.CallExpr, window string) string {
	args := make([]string, len(call.Args))
	for i, a := range call.Args {
		args[i] = types.ExprString(a)
		if args[i] == window {
			args[i] = "<window>"
		}
	}
	return types.ExprString(call.Fun) + "(" + strings.Join(args, ", ") + ")"
}

// findTwin returns the name of a test of prod's package that push runs (not
// listed, built without -race) whose body makes prod's call with the short
// window in place of the production one.
func findTwin(tests []*integrationTest, prod *integrationTest, call *ast.CallExpr, listed map[string]bool) string {
	want := callShape(call, prodWindowIdent)
	for _, tc := range tests {
		if tc == prod || listed[tc.name] || tc.raceOnly || tc.dir != prod.dir || tc.decl.Body == nil {
			continue
		}
		found := false
		ast.Inspect(tc.decl.Body, func(node ast.Node) bool {
			if c, ok := node.(*ast.CallExpr); ok && hasIdentArg(c, shortWindowIdent) && callShape(c, shortWindowIdent) == want {
				found = true
			}
			return !found
		})
		if found {
			return tc.name
		}
	}
	return ""
}
