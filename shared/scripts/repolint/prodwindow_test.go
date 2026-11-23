package main

import "strings"

// pwGate is a gate.sh whose push skip list is names, one a line from line 3.
func pwGate(names ...string) string {
	return lines("#!/usr/bin/env bash", "# the push tier's skip", pushSkipVar+`="`+strings.Join(names, "\n")+`"`, "docker_skip() { :; }")
}

// pwTests is a scenarios file: a body taking the window, its twin in the S2
// test's subtest, the production leaf, and S5 with no twin. Each argument replaces
// one line (matched by its first words, after the tabs) so a case plants one
// defect. The production leaf is at line 22, S5 at line 27.
func pwTests(replace ...string) string {
	ls := []string{
		"//go:build docker", // 1
		"",
		"package scenarios",
		"",
		"import (", // 5
		`	"testing"`,
		`	"time"`,
		")",
		"",
		"const w6ProductionWindow time.Duration = 0", // 10
		"const w6ShortWindow = 10 * time.Second",
		"",
		"func body(t *testing.T, gate string, window time.Duration) {}",
		"",
		"func TestS2(t *testing.T) {", // 15
		"	t.Parallel()",
		`	t.Run("green", func(t *testing.T) { t.Parallel(); body(t, "green", w6ShortWindow) })`,
		"}",
		"",
		"// the production leaf", // 20
		"",
		"func TestS2Green_ProductionWindow(t *testing.T) {",
		"	t.Parallel()",
		`	body(t, "green", w6ProductionWindow)`,
		"}", // 25
		"",
		"func TestCatid4x2_S5_UnmappedPart(t *testing.T) {",
		"	t.Parallel()",
		"}",
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

// pwWorkflows are the two -skip lines the rule knows, as the real files carry
// them, and a description that says -skip in prose.
func pwWorkflows() []string {
	return []string{
		integrationWorkflow, lines("jobs:", `            c) slice=(-skip "^Test($SLICE_A|$SLICE_B)") ;;`),
		".github/workflows/sharded-suite.yml", lines("description: >",
			"  -run '^Test[<range>]', and one more slice runs with -skip over all of",
			`          [ -n "$SLICE_SKIP" ] && slice+=(-skip "$SLICE_SKIP")`),
	}
}

func pwTree(gate string, tests string, extra ...string) *tree {
	pairs := append([]string{"scripts/gate.sh", gate, "integration/scenarios/w_test.go", tests}, pwWorkflows()...)
	return treeOf(append(pairs, extra...)...)
}

func prodWindowCases() []ruleCase {
	const gate, tests = "scripts/gate.sh", "integration/scenarios/w_test.go"
	listed := []string{"TestS2Green_ProductionWindow", "TestCatid4x2_S5_UnmappedPart"}
	ok := pwGate(listed...)
	return []ruleCase{
		{"production-window", "every listed leaf twinned or exempt, no workflow skips it", fixed(pwTree(ok, pwTests())), []string{}},
		{"production-window", "a twin with other arguments is no twin", fixed(pwTree(ok, pwTests(
			`t.Run("green"=>	t.Run("green", func(t *testing.T) { t.Parallel(); body(t, "red", w6ShortWindow) })`))),
			[]string{tests + ":22"}},
		{"production-window", "a twin at the production window is no twin", fixed(pwTree(ok, pwTests(
			`t.Run("green"=>	t.Run("green", func(t *testing.T) { t.Parallel(); body(t, "green", w6ProductionWindow) })`))),
			[]string{tests + ":22"}},
		{"production-window", "a twin push also skips is no twin", fixed(pwTree(pwGate(append(listed, "TestS2")...), pwTests())),
			// The twin's test is listed: neither a *_ProductionWindow test nor one body at the production
			// window; and so the leaf has no twin push runs.
			[]string{tests + ":15", tests + ":15", tests + ":22"}},
		{"production-window", "a listed name with no test", fixed(pwTree(pwGate(append(listed, "TestGone_ProductionWindow")...), pwTests())),
			[]string{gate + ":3"}},
		{"production-window", "a production-window leaf not listed runs in push", fixed(pwTree(pwGate(listed[1]), pwTests())),
			[]string{tests + ":22"}},
		{"production-window", "a production leaf that is not parallel first", fixed(pwTree(ok, pwTests(
			"t.Parallel()=>	_ = 0"))),
			// The production leaf, S5 and the twin's test all start "t.Parallel()": only the listed two are checked.
			[]string{tests + ":22", tests + ":27"}},
		{"production-window", "a production leaf with two window calls", fixed(pwTree(ok, pwTests(
			`body(t, "green", w6ProductionWindow)=>	body(t, "green", w6ProductionWindow); body(t, "red", w6ProductionWindow)`))),
			[]string{tests + ":22"}},
		{"production-window", "an exemption no longer listed", fixed(pwTree(pwGate(listed[0]), pwTests())),
			[]string{gate + ":3"}},
		{"production-window", "a workflow that names a production leaf or skips by a pattern of its own", fixed(pwTree(ok, pwTests(),
			".github/workflows/edge.yml", lines("jobs:", `      run: go test -skip 'TestS2Green_ProductionWindow' ./...`,
				`      run: go test -skip "$OTHER" ./...`, "      # -skip 'X' in a comment is not a skip"))),
			// Line 2 names the suffix and the leaf, and skips; line 3 skips.
			[]string{".github/workflows/edge.yml:2", ".github/workflows/edge.yml:2", ".github/workflows/edge.yml:2",
				".github/workflows/edge.yml:3"}},
		{"production-window", "no list, nothing to read", fixed(pwTree(lines("#!/usr/bin/env bash"), pwTests())), []string{gate}},
		{"production-window", "an empty list", fixed(pwTree(pwGate(), pwTests())),
			// S5 is then an exemption not listed, and the leaf a production-window leaf not listed.
			[]string{gate + ":3", gate + ":3", tests + ":22"}},
	}
}
