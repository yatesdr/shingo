package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Every probe below that a rule must fire on is ASSEMBLED at run time, so this
// file passes the rules it tests: the repolint reads it like every other
// tracked file.

func dq(a, b, c, d int) string  { return fmt.Sprintf("%d.%d.%d.%d", a, b, c, d) }
func marker(c string) string    { return strings.Repeat(c, 7) }
func lines(ls ...string) string { return strings.Join(ls, "\n") + "\n" }

// treeOf is an in-memory tree of path, content pairs, every file a
// text-attributed lf file with no diff attribute.
func treeOf(pairs ...string) *tree {
	t := &tree{root: "."}
	for i := 0; i+1 < len(pairs); i += 2 {
		t.files = append(t.files, &file{path: pairs[i], eol: "lf", text: "set", diff: "unspecified", data: []byte(pairs[i+1])})
	}
	sort.Slice(t.files, func(i, j int) bool { return t.files[i].path < t.files[j].path })
	return t
}

// with sets one file's attributes.
func with(t *tree, path, eol, text, diff string) *tree {
	for _, f := range t.files {
		if f.path == path {
			f.eol, f.text, f.diff = eol, text, diff
		}
	}
	return t
}

func wheres(fs []Finding) []string {
	out := []string{}
	for _, f := range fs {
		out = append(out, f.where())
	}
	sort.Strings(out)
	return out
}

func ruleNamed(name string) (rule, bool) {
	for _, r := range rules {
		if r.name == name {
			return r, true
		}
	}
	return rule{}, false
}

// ruleCase is one tree a rule runs over and the findings it must report.
type ruleCase struct {
	rule, name string
	tree       func(t *testing.T) *tree
	want       []string // where() of each finding, sorted
}

func fixed(tr *tree) func(*testing.T) *tree { return func(*testing.T) *tree { return tr } }

func plantCases() []ruleCase {
	// The guard's own probes: each bad line fires once, no good line fires.
	bad := []string{dq(10, 0, 0, 5), dq(10, 255, 1, 9), dq(172, 16, 0, 1), dq(172, 31, 255, 254),
		dq(192, 168, 0, 1), dq(192, 168, 2, 30), "url http://" + dq(10, 1, 2, 3) + ":8080/x",
		dq(192, 168, 1, 7) + " then " + dq(10, 9, 9, 9)}
	good := []string{dq(192, 168, 1, 76), dq(192, 0, 2, 1), dq(198, 51, 100, 7), dq(203, 0, 113, 9),
		dq(172, 15, 0, 1), dq(172, 32, 0, 1), dq(127, 0, 0, 1), dq(110, 1, 2, 3),
		"v1." + dq(10, 0, 0, 1), dq(10, 1, 2, 3) + ".4", dq(10, 1, 2, 300)}
	var want []string
	for i := range bad {
		want = append(want, fmt.Sprintf("bad.txt:%d", i+1))
	}
	return []ruleCase{
		{"plant-addresses", "the guard's probes", fixed(treeOf("bad.txt", lines(bad...), "good.txt", lines(good...))), want},
		{"plant-addresses", "two on one line, one per address", fixed(treeOf("a.md", dq(10, 0, 0, 1)+", "+dq(172, 20, 0, 1)+".\n")),
			[]string{"a.md:1", "a.md:1"}},
		{"plant-addresses", "binary files are skipped, as git grep -I skips them", fixed(with(treeOf(
			"blob.dat", "\x00"+dq(10, 0, 0, 5)+"\n", "pic.png", dq(10, 0, 0, 5)+"\n"), "pic.png", "-text", "unset", "unset")), []string{}},
	}
}

func syntheticCases() []ruleCase {
	fire := []string{
		fmt.Sprintf("%s-%s.%s", "74577", "6SA0A", "06"), // shape
		fmt.Sprintf("%s-%s.%s", "40421", "RVJ56", "37"),
		fmt.Sprintf(`%s-%s\.%s`, "76683", "6TA0A", "06"),            // shape, dot escaped as in a JS regex
		fmt.Sprintf("'%s-%s-%s%s'", "PART", "74577", "6SA0A", "06"), // no-dot
		fmt.Sprintf("%s-%s%s", "40421", "RVJ56", "37"),
		fmt.Sprintf("%s-%s.%s", "PART1", "6SA0A", "06"), // prefix
		fmt.Sprintf("%s-%s.%s", "SNF2", "6SA0A", "06"),
		fmt.Sprintf(`%s-%s\.%s`, "X", "6TA0A", "06"),
		fmt.Sprintf("MES_%s_Line_2.Prod_Counter_01", "P42"), // struct
		fmt.Sprintf("MES_%s_X", "P7"),
	}
	quiet := []string{"20060102-150405", "SYN-PART07A.06", "SYN-A-P007", "PLN_01", "Press B4", "SYN-PART02B.06",
		"MES_Press_A1", "MES_400Ton.Prod_Counter_01", "MES_OtherLine_Process", "MES_PX_1"}
	var want []string
	for i := range fire {
		want = append(want, fmt.Sprintf("fire.go:%d", i+1))
	}
	return []ruleCase{
		{"synthetic-part-numbers", "the guard's probes", fixed(treeOf("fire.go", lines(fire...), "quiet.go", lines(quiet...))), want},
		{"synthetic-part-numbers", "binary files are read; png, jpg and pdf are not", fixed(treeOf(
			"blob.bin", "\x00\n"+fire[0]+"\n", "a.png", fire[0], "a.jpg", fire[0], "a.pdf", fire[0])), []string{"blob.bin:2"}},
	}
}

func swapCases() []ruleCase {
	goTree := treeOf(
		"shingo-core/x/a.go", lines(
			`if m == "two_robot" {`,
			`// a comment naming "two_robot" is exempt`,
			`x := "simple" // a trailing comment is not`,
			`u := "http://h/" + "sequential" // the guard drops any line with a URL`,
			`m = "two_robot_press_index"`),
		"shingo-core/x/a_test.go", `m := "two_robot"`+"\n",
		"protocol/swap_mode.go", `SwapModeTwoRobot SwapMode = "two_robot"`+"\n",
		"shared/x.go", `m := "two_robot"`+"\n",
		"integration/y.go", `m := "manual_swap"`+"\n")
	jsTree := treeOf(
		"shingo-edge/www/static/operator-station/operator-modal.js", "if (m === 'two_robot') {}\n",
		"shingo-edge/www/static/operator-station/composer-model.js", "const m = 'two_robot';\n",
		"shingo-edge/www/static/operator-station/operator-render.js", "if (m === 'manual_swap') {}\n",
		"shingo-core/www/static/pages/other.js", "if (m === \"manual_swap\") {}\nconst s = `sequential`;\n",
		"shingo-core/www/static/pages/test-orders.js", "const m = 'simple';\n",
		"shingo-core/www/static/x.test.js", "const m = 'simple';\n",
		"shingo-core/node_modules/lib/a.js", "const m = 'simple';\n",
		"shared/www/a.js", "const m = 'simple';\n")
	return []ruleCase{
		{"swap-mode-literals", "Go", fixed(goTree), []string{"integration/y.go:1", "shingo-core/x/a.go:1", "shingo-core/x/a.go:3", "shingo-core/x/a.go:5"}},
		{"swap-mode-literals", "JavaScript", fixed(jsTree), []string{"shingo-core/www/static/pages/other.js:1",
			"shingo-core/www/static/pages/other.js:2", "shingo-edge/www/static/operator-station/operator-modal.js:1"}},
	}
}

func conflictCases() []ruleCase {
	text := lines(marker("<")+" HEAD", marker("|")+" base", marker("="), marker(">")+" theirs", marker("<"),
		strings.Repeat("=", 8), marker("<")+"x", " "+marker("="), marker("=")+"\r")
	return []ruleCase{
		{"conflict-markers", "marker lines", fixed(treeOf("CHANGELOG.md", text)),
			[]string{"CHANGELOG.md:1", "CHANGELOG.md:2", "CHANGELOG.md:3", "CHANGELOG.md:4", "CHANGELOG.md:5"}},
		{"conflict-markers", "binary files are skipped", fixed(with(treeOf("blob.dat", "\x00\n"+marker("=")+"\n",
			"a.ico", marker("=")+"\n"), "a.ico", "-text", "unset", "unset")), []string{}},
	}
}

func textFileCases() []ruleCase {
	tr := treeOf("bad.go", "", "favicon.ico", "", "x.txt", "", "ok.go", "")
	with(tr, "bad.go", "-text", "set", "unspecified")
	with(tr, "favicon.ico", "-text", "unset", "unset")
	with(tr, "x.txt", "-text", "auto", "unspecified") // text=auto is not "set"
	return []ruleCase{{"text-files", "a text-attributed file indexed -text", fixed(tr), []string{"bad.go"}}}
}

func commentRefCases() []ruleCase {
	n := func(k int) string { return strings.Repeat("x\n", k) }
	tr := treeOf(
		"shingo-core/dispatch/compound.go", n(100),
		"shingo-core/internal/testdb/compound.go", n(10),
		"shingo-edge/engine/changeover.go", n(50),
		"shingo-core/dispatch/reshuffle.go", n(20),
		"shingo-core/dispatch/complex_reshuffle.go", n(5),
		"scripts/tool.go", n(3),
		"shingo-core/c.go", lines(
			"// compound.go:90 fits the longer of the two it could name",
			"// compound.go:101 is past both",
			"// testdb/compound.go:50 resolves as a suffix, to the shorter one only",
			"\t// dispatch/compound.go:60 resolves as a suffix",
			"// reshuffle.go:15 is the whole last component, not a suffix of it",
			"// nosuch.go:1",
			"// migrateloaders.go:5 is allowed missing",
			"x := 1 // compound.go:999, a trailing comment, is not read",
			"// scripts/tool.go:3 resolves exactly from the root",
			"// tool.go:1 does not: a basename resolves in the five modules only",
			"// shingo-core/dispatch/compound.go:100 and compound.go:100 on one line",
			"// edge/engine/changeover.go:40 is no suffix of shingo-edge's; its basename resolves",
			"// edge/engine/changeover.go:60"),
		"scripts/notmodule.go", "// nosuch.go:1 outside the modules is not read\n")
	return []ruleCase{{"comment-references", "exact, suffix and basename", fixed(tr),
		[]string{"shingo-core/c.go:2", "shingo-core/c.go:3", "shingo-core/c.go:6", "shingo-core/c.go:10", "shingo-core/c.go:13"}}}
}

func expiryCases() []ruleCase {
	decl := "package x\n\ntype Engine struct{ inner int }\n\nfunc (e *Engine) flip() {}\n\nfunc Free() {}\n\ntype Doer interface{ Do() }\n"
	pins := "package y\n\n" + lines(
		"// EXPIRY: when Free is gone",
		"// EXPIRY: when x.Free is gone",
		"// EXPIRY: when `Engine.flip` is gone",
		"// EXPIRY: when (*Engine).flip() is gone",
		"// EXPIRY: when x.Engine.inner is gone",
		"// EXPIRY: when Doer.Do is gone",
		"// EXPIRY: when",
		"// x.Free is gone, read across the wrap",
		"// EXPIRY: when Missing is gone",
		"// EXPIRY: when y.Free is gone",
		"// EXPIRY: when Engine.gone is gone",
		"// EXPIRY: when the docs change, which is prose",
		"var _ = 1")
	// testdata is not built, so what it declares does not keep a pin alive.
	stale := "package x\n\nfunc Missing() {}\n"
	alwaysDead := []string{"shingo-edge/y/pin.go:11", "shingo-edge/y/pin.go:12", "shingo-edge/y/pin.go:13"}
	return []ruleCase{
		{"expiry", "the symbol is declared: only the pins on undeclared ones fire",
			fixed(treeOf("shingo-core/x/a.go", decl, "shingo-edge/y/pin.go", pins, "shingo-core/x/testdata/t.go", stale)), alwaysDead},
		{"expiry", "the symbol is deleted: its pins fire in the same run",
			fixed(treeOf("shingo-core/x/a.go", strings.Replace(decl, "func Free() {}\n", "", 1), "shingo-edge/y/pin.go", pins)),
			append([]string{"shingo-edge/y/pin.go:3", "shingo-edge/y/pin.go:4", "shingo-edge/y/pin.go:9"}, alwaysDead...)},
	}
}

func serialCases() []ruleCase {
	scope := lines("# the scope file", "package app/eng", "package app/store",
		"unaudited app/eng TestOld", "unaudited app/eng TestOldNowParallel", "unaudited app/eng TestGone",
		"unaudited app/web TestElsewhere", "bogus line")
	mark := "// " + "serial: "
	eng := "package eng\n\nimport \"testing\"\n\n" + lines(
		"func TestNew(t *testing.T) {}", // 5: serial, unmarked, unlisted
		"",
		"// TestDoc says why in its doc comment's last line.",
		mark+"it sets the process clock",
		"func TestMarked(t *testing.T) {}",
		"",
		mark,
		"func TestNoReason(t *testing.T) {}", // 12
		"",
		mark+"stale",
		"func TestMarkedButParallel(t *testing.T) { t.Parallel() }", // 15
		"",
		mark+"a blank line breaks it",
		"",
		"func TestGap(t *testing.T) {}", // 19
		"",
		"func TestPar(tt *testing.T) {",
		"\ttt.Parallel()",
		"}",
		"",
		"func TestOnlySubtestsParallel(t *testing.T) {", // 25
		"\tt.Run(\"a\", func(t *testing.T) { t.Parallel() })",
		"}",
		"",
		"func TestOld(t *testing.T) {}",
		"",
		"func TestOldNowParallel(t *testing.T) { t.Parallel() }",
		"",
		"func TestMain(m *testing.M) {}",
		"func helper(t *testing.T)       {}",
		"func TestBench(b *testing.B)    {}",
		"type x struct{}",
		"func (x) TestMethod(t *testing.T) {}")
	return []ruleCase{
		{"serial-tests", "unmarked, unreasoned, contradicted, gapped and stale lines fire; marked, parallel and listed pass",
			fixed(treeOf(serialScopeFile, scope,
				"app/eng/a_test.go", eng,
				"app/eng/sub/b_test.go", "package sub\n\nimport \"testing\"\n\nfunc TestNotInScope(t *testing.T) {}\n",
				"app/web/c_test.go", "package web\n\nimport \"testing\"\n\nfunc TestElsewhere(t *testing.T) {}\n",
				"app/store/d_test.go", "package store\n\nimport \"testing\"\n\nfunc TestStoreSerial(t *testing.T) {}\n",
				"app/store/e.go", "package store\n\nimport \"testing\"\n\nfunc TestNotATestFile(t *testing.T) {}\n")),
			[]string{"app/eng/a_test.go:12", "app/eng/a_test.go:15", "app/eng/a_test.go:19", "app/eng/a_test.go:25",
				"app/eng/a_test.go:5", "app/store/d_test.go:5",
				serialScopeFile + ":5", serialScopeFile + ":6", serialScopeFile + ":7", serialScopeFile + ":8"}},
		{"serial-tests", "no scope file is a finding, not a pass", fixed(treeOf("app/eng/a_test.go", eng)),
			[]string{serialScopeFile}},
	}
}

// probeFile is the reuse probe set, read from this module: the rules' cases
// run over the sources its probes write, so a case cannot drift from its probe.
var probeFile = filepath.Join("..", "gatetime", "reuse", "probes", "probes_test.go")

// probeSource is the test source probe fn of the probe set writes: its body's
// `src("a", imports, body)` call, both string arguments literals, put together
// as src does ("package a", the imports, a blank line, the body). A probe that
// no longer has that shape fails the case: it must not pass on no input.
func probeSource(t *testing.T, fn string) string {
	t.Helper()
	data, err := os.ReadFile(probeFile)
	if err != nil {
		t.Fatalf("the probe set is unreadable, so probe %s's source is unknown: %v", fn, err)
	}
	af, err := parser.ParseFile(token.NewFileSet(), probeFile, data, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range af.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn || fd.Body == nil {
			continue
		}
		var got string
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if got != "" {
				return false
			}
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, isID := c.Fun.(*ast.Ident); !isID || id.Name != "src" || len(c.Args) != 3 {
				return true
			}
			var s [3]string
			for i, a := range c.Args {
				lit, isLit := a.(*ast.BasicLit)
				if !isLit || lit.Kind != token.STRING {
					return true
				}
				s[i], _ = strconv.Unquote(lit.Value)
			}
			got = "package " + s[0] + "\n\nimport (\n" + s[1] + "\n)\n\n" + s[2]
			return false
		})
		if got != "" {
			return got
		}
	}
	t.Fatalf("%s has no func %s calling src with three string literals: the case has no probe source", probeFile, fn)
	return ""
}

func rawSyscallCases() []ruleCase {
	goMod := "module shingo/shared\n\ngo 1.25.0\n"
	// probe 23 (p23): a test reading its input through syscall.Open, which no
	// test log records; on Windows it came out RUN,REUSE,REUSE where FAIL was
	// wanted (probes-windows-1.log). Its Open (line 9) and Read (15) fire;
	// Close reads nothing.
	tr := func(t *testing.T) *tree {
		probe23 := probeSource(t, "p23")
		return treeOf(
			"shared/go.mod", goMod,
			"shared/probe/a/a_test.go", probe23,
			// not built, or not in a module: not read.
			"shared/probe/testdata/t_test.go", probe23,
			"scripts/x_test.go", probe23,
			// the probe harness's shape: the call is text in a literal, not a call.
			"shared/probe/harness_test.go", "package probe\n\nimport \"syscall\"\n\nvar _ = syscall.O_RDONLY\n\nconst p23 = `fd, err := syscall.Open(\"x\", syscall.O_RDONLY, 0)`\n",
			// a test-only helper (only a test imports it) and one only it imports.
			"shared/helper/h.go", lines("package helper", "", "import (", "\t\"syscall\"", "",
				"\t\"shingo/shared/helper/deep\"", ")", "",
				"func Size(p string) int64 {", "\tvar st syscall.Stat_t", "\t_ = syscall.Stat(p, &st)", // 11
				"\treturn st.Size + deep.N()", "}"),
			"shared/helper/deep/d.go", lines("package deep", "", "import xs \"syscall\"", "",
				"func N() int64 { r, _, _ := xs.Syscall6(0, 0, 0, 0, 0, 0, 0); return int64(r) }"), // 5
			"shared/user/u_test.go", "package user\n\nimport (\n\t\"testing\"\n\n\t\"shingo/shared/helper\"\n)\n\nfunc TestU(t *testing.T) { _ = helper.Size(\"x\") }\n",
			// the tracer's shape: a package a main program imports; its own tests do not make it test-only.
			"shared/tool/trace.go", "package tool\n\nimport \"syscall\"\n\nfunc Trace() { syscall.RawSyscall6(0, 0, 0, 0, 0, 0, 0) }\n",
			"shared/tool/trace_test.go", "package tool_test\n\nimport (\n\t\"testing\"\n\n\t\"shingo/shared/tool\"\n)\n\nfunc TestT(t *testing.T) { tool.Trace() }\n",
			"shared/tool/cmd/gt/main.go", "package main\n\nimport (\n\t\"shingo/shared/lib\"\n\t\"shingo/shared/tool\"\n)\n\nfunc main() { tool.Trace(); lib.L() }\n",
			// a package only its own tests import: a library, not a test helper.
			"shared/solo/s.go", "package solo\n\nimport \"syscall\"\n\nfunc S() { syscall.Open(\"x\", 0, 0) }\n",
			"shared/solo/s_test.go", "package solo_test\n\nimport (\n\t\"testing\"\n\n\t\"shingo/shared/solo\"\n)\n\nfunc TestS(t *testing.T) { solo.S() }\n",
			// a library a test and a product file both import, and a main package: product code.
			"shared/lib/l.go", "package lib\n\nimport \"syscall\"\n\nfunc L() { syscall.Open(\"x\", 0, 0) }\n",
			"shared/user/v_test.go", "package user\n\nimport (\n\t\"testing\"\n\n\t\"shingo/shared/lib\"\n)\n\nfunc TestV(t *testing.T) { lib.L() }\n",
			"shared/cmd/x/main.go", "package main\n\nimport \"syscall\"\n\nfunc main() { syscall.Open(\"x\", 0, 0) }\n",
			// raw calls that read nothing.
			"shared/sig/s_test.go", "package sig\n\nimport (\n\t\"syscall\"\n\t\"testing\"\n)\n\nfunc TestS(t *testing.T) { _ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM); syscall.Close(0) }\n",
			// x/sys, aliased; the DLL loader and CreateFile.
			"shingo-core/x/x_test.go", lines("package x", "", "import (", "\t\"syscall\"", "\t\"testing\"", "",
				"\tu \"golang.org/x/sys/unix\"", "\t\"golang.org/x/sys/windows\"", ")", "",
				"func TestX(t *testing.T) {",
				"\t_, _ = u.Openat(0, \"x\", 0, 0)",                    // 12
				"\t_, _ = windows.CreateFile(nil, 0, 0, nil, 0, 0, 0)", // 13
				"\t_ = syscall.NewLazyDLL(\"kernel32.dll\")",           // 14
				"}"))
	}
	return []ruleCase{
		{"raw-syscall-reads", "probe 23, a test-only helper and its import fire; tool, product and literal text do not",
			tr, []string{"shared/helper/deep/d.go:5", "shared/helper/h.go:11", "shared/probe/a/a_test.go:15",
				"shared/probe/a/a_test.go:9", "shingo-core/x/x_test.go:12", "shingo-core/x/x_test.go:13", "shingo-core/x/x_test.go:14"}},
		{"raw-syscall-reads", "no go.mod read: a non-test raw importer is a finding, not product code",
			fixed(treeOf("shared/lib/l.go", "package lib\n\nimport \"syscall\"\n\nfunc L() { syscall.Open(\"x\", 0, 0) }\n")),
			[]string{"shared/lib/l.go"}},
	}
}

func initReadCases() []ruleCase {
	// probe 25 (p25): a package-level var reading a file before m.Run; on
	// Windows it came out RUN,REUSE,REUSE where FAIL was wanted
	// (probes-windows-1.log). Its var (line 8) fires.
	tr := func(t *testing.T) *tree {
		probe25 := probeSource(t, "p25")
		return treeOf(
			"shared/probe/a/a_test.go", probe25,
			// probe 24: a TestMain read is the whole-tree key's, not this rule's.
			"shared/probe/b/b_test.go", probeSource(t, "p24"),
			// not built, or not in a module: not read.
			"shared/probe/testdata/x_test.go", probe25,
			"scripts/y_test.go", probe25,
			// env reads (the Edge station-view golden's shape): keyed whole, allowed.
			"shingo-edge/svc/golden_test.go", lines("package svc", "", "import (", "\t\"os\"", "\t\"testing\"", ")", "",
				"var updateGolden = os.Getenv(\"UPDATE_GOLDEN\") != \"\" ||",
				"\tfunc() bool {",
				"\t\tfor _, a := range os.Args {",
				"\t\t\tif a == \"-update\" {",
				"\t\t\t\treturn true",
				"\t\t\t}",
				"\t\t}",
				"\t\treturn false",
				"\t}()",
				"",
				"var home, _ = os.LookupEnv(\"HOME\")",
				"var env, path = os.Environ(), os.ExpandEnv(\"$PATH\")",
				"",
				"func TestG(t *testing.T) {}"),
			// product code counts: its package vars run in every test binary that links it.
			"shingo-core/cfg/cfg.go", lines("package cfg", "", "import (", "\t\"html/template\"", "\t\"io/ioutil\"",
				"\txos \"os\"", "\t\"os/exec\"", "\t\"path/filepath\"", ")", "",
				"var raw = func() []byte { b, _ := xos.ReadFile(\"cfg.yaml\"); return b }()", // 11: called on the spot
				"var later = func() []byte { b, _ := xos.ReadFile(\"cfg.yaml\"); return b }", // stored for later
				"var git = exec.Command(\"git\", \"rev-parse\", \"HEAD\")",                   // 13
				"var files, _ = filepath.Glob(\"testdata/*.json\")",                          // 14
				"var joined = filepath.Join(\"a\", \"b\")",                                   // reads nothing
				"var page = template.Must(template.ParseFiles(\"page.html\"))",               // 16
				"var old, _ = ioutil.ReadFile(\"x\")",                                        // 17
				"var readFile = xos.ReadFile",                                                // a value, not a call
				"var viaHelper = load()",                                                     // indirect: out of scope
				"",
				"func load() []byte { b, _ := xos.ReadFile(\"x\"); return b }",
				"",
				"func init() {",
				"\tif _, err := xos.Stat(\"x\"); err != nil {",    // 24
				"\t\tregister(func() { _, _ = xos.Open(\"y\") })", // runs when called, not at init
				"\t}",
				"\tfunc() { _ = xos.Remove(\"z\") }()", // 27
				"}",
				"",
				"func register(func()) {}",
				"",
				"func Run() { local := xos.Getpid(); _ = local }"))
	}
	return []ruleCase{{"init-reads", "probe 25 and direct reads in init and var initializers fire; env reads, TestMain, helpers and stored literals do not",
		tr, []string{"shared/probe/a/a_test.go:8", "shingo-core/cfg/cfg.go:11", "shingo-core/cfg/cfg.go:13",
			"shingo-core/cfg/cfg.go:14", "shingo-core/cfg/cfg.go:16", "shingo-core/cfg/cfg.go:17",
			"shingo-core/cfg/cfg.go:24", "shingo-core/cfg/cfg.go:27"}}}
}

// jsTree writes path, content pairs under a temp root, as node reads them from
// disk. Without node it skips: the rule's own missing-node failure is pinned
// by TestJSParse_MissingNodeFails.
func jsTree(pairs ...string) func(*testing.T) *tree {
	return func(t *testing.T) *tree {
		if _, err := exec.LookPath(nodeBin); err != nil {
			t.Skip("node is not on PATH")
		}
		tr := treeOf(pairs...)
		tr.root = t.TempDir()
		for _, f := range tr.files {
			p := filepath.Join(tr.root, filepath.FromSlash(f.path))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, f.data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return tr
	}
}

func jsCases() []ruleCase {
	return []ruleCase{{"js-parse", "a stray brace fails both grammars; module, classic and CommonJS files pass", jsTree(
		"bad.js", "export const a = 1;\n}\n",
		"esm.js", "import x from './y.js';\nexport default x;\nawait Promise.resolve();\n",
		"classic.js", "with (Math) { abs(-1); }\nvar o = 010;\n",
		"cjs.js", "return module.exports;\n",
		"bang.js", "#!/usr/bin/env node\nconsole.log(1);\n",
		"lib/node_modules/dep.js", "}\n"), []string{"bad.js:2"}},
		{"js-undeclared", "a name no scope declares fires; hoisting, patterns, page and browser globals, tests and vendor pass", jsTree(
			// The 2026-10-08 shape: a caller's parameter read in a function it was never passed to.
			"app/www/static/pages/bad.js", "export function open(el, opts) { load(el); }\nfunction load(el) {\n  return opts && el;\n}\nlater = 1;\n",
			"app/www/static/pages/ok.js", "import { a } from '/static/app.js';\n"+
				"window.pageFn = function() { return 1; };\n"+
				"export function f({ x, y: [z] = [] }, ...rest) {\n"+
				"  if (typeof notDefinedHere === 'undefined') pageFn();\n"+
				"  outer: for (const k of rest) { if (k) continue outer; }\n"+
				"  switch (x) { case 1: let w = z; return w + hoisted(); }\n"+
				"  try { a(); } catch ({ message }) { return message; }\n"+
				"  for (var i = 0; i < 1; i++) { var inLoop = i; }\n"+
				"  return arguments.length + inLoop + PAGE_DATA + document.title + new Map().size;\n"+
				"  function hoisted() { return 2; }\n"+
				"}\n"+
				"class C { static n = 1; #p = 2; m() { return C.n + this.#p; } }\n"+
				"export default (function named() { return named; });\n"+
				"export { C };\n",
			"app/www/templates/page.html", "<script>\n  var PAGE_DATA = {{.Data}};\n</script>\n<script type=\"module\">\n  var MODULE_ONLY = 1;\n</script>\n",
			"app/www/static/pages/uses-module-only.js", "export const v = MODULE_ONLY;\n",
			"app/www/static/pages/page.test.js", "require('fs'); injectedByTheHarness();\n",
			"app/www/static/vendor/lib.js", "vendorGlobal();\n",
			"lib/outside.js", "notAPageScript();\n"),
			[]string{"app/www/static/pages/bad.js:3", "app/www/static/pages/bad.js:5", "app/www/static/pages/uses-module-only.js:1"}},
		{"js-imports", "a name the module does not export, and a module that is no file, fire; every other form links", jsTree(
			// The 2026-11-21 shape: a helper deleted with an import of it left.
			"app/www/static/pages/a.js", "import { gone, kept } from './b.js';\n",
			"app/www/static/pages/b.js", "export function kept() {}\n",
			"app/www/static/pages/c.js", "import { el } from '/static/shared/dom.js';\n"+
				"import d from './d.js';\n"+
				"import * as ns from './b.js';\n"+
				"import { star, sx } from './re.js';\n"+
				"export { kept as k } from './b.js';\n"+
				"export { nope } from './b.js';\n"+
				"import './b.js';\n"+
				"export { el, d, ns, star, sx };\n",
			"shared/dom.js", "export function el() {}\n",
			"app/www/static/pages/d.js", "export default 1;\n",
			"app/www/static/pages/re.js", "export * from './star.js';\n",
			"app/www/static/pages/star.js", "export const star = 1, { sx } = {};\nexport default 2;\n",
			"app/www/static/pages/e.js", "import { a } from './nofile.js';\n",
			// A full URL is not ours; import() reads its names when it runs.
			"app/www/static/pages/f.js", "import { thing } from 'https://cdn.test/x.js';\nimport('./b.js').then(function(m) { return m.gone; });\n",
			"app/www/static/pages/f.test.js", "import { gone } from './b.js';\n",
			"app/www/static/vendor/v.js", "import { gone } from '../pages/b.js';\n",
			"app/www/static/pages/classic.js", "with (Math) { abs(-1); }\n",
			"app/www/static/pages/g.js", "import { x } from './classic.js';\n",
			"app/www/static/js/h.js", "import { kept } from '/static/pages/b.js';\nimport { gone } from '/static/pages/b.js';\n",
			// A default is not lent by export *.
			"app/www/static/pages/i.js", "import { default as two } from './re.js';\n"),
			[]string{"app/www/static/js/h.js:2", "app/www/static/pages/a.js:1", "app/www/static/pages/c.js:6",
				"app/www/static/pages/e.js:1", "app/www/static/pages/i.js:1"}},
	}
}

func allCases() []ruleCase {
	var cs []ruleCase
	for _, f := range []func() []ruleCase{commentRefCases, conflictCases, expiryCases, initReadCases, jsCases, plantCases,
		rawSyscallCases, serialCases, sliceCases, swapCases, syntheticCases, textFileCases} {
		cs = append(cs, f()...)
	}
	return cs
}

// TestEveryRuleHasACaseThatFires is the liveness property of the runner, not of
// each rule: a pattern that matches nothing prints the same "ok" as a clean
// tree, so a rule with no case that must fire fails here.
func TestEveryRuleHasACaseThatFires(t *testing.T) {
	fires := map[string]bool{}
	for _, c := range allCases() {
		if _, ok := ruleNamed(c.rule); !ok {
			t.Errorf("case %q names no rule %q", c.name, c.rule)
		}
		fires[c.rule] = fires[c.rule] || len(c.want) > 0
	}
	for _, r := range rules {
		if !fires[r.name] {
			t.Errorf("rule %s has no case that must fire", r.name)
		}
	}
}

func TestRules(t *testing.T) {
	for _, c := range allCases() {
		t.Run(c.rule+"/"+c.name, func(t *testing.T) {
			r, ok := ruleNamed(c.rule)
			if !ok {
				t.Fatalf("no rule is named %q, so nothing reports %q", c.rule, c.want)
			}
			res := r.check(c.tree(t))
			want := append([]string{}, c.want...)
			sort.Strings(want)
			if got := wheres(res.findings); !reflect.DeepEqual(got, want) {
				t.Errorf("findings at %q, want %q\n%v", got, want, res.findings)
			}
		})
	}
}

func TestJSParse_MissingNodeFails(t *testing.T) {
	defer func(n string) { nodeBin = n }(nodeBin)
	nodeBin = "node-that-is-not-installed"
	res := jsParse(treeOf("a.js", "export {};\n"))
	if len(res.findings) != 1 || !strings.Contains(res.findings[0].Why, "NOT verified") {
		t.Fatalf("a missing node must fail the run, got %v", res.findings)
	}
}

func TestJSUndeclared_MissingNodeFails(t *testing.T) {
	defer func(n string) { nodeBin = n }(nodeBin)
	nodeBin = "node-that-is-not-installed"
	res := jsUndeclared(treeOf("app/www/static/a.js", "export {};\n"))
	if len(res.findings) != 1 || !strings.Contains(res.findings[0].Why, "NOT checked") {
		t.Fatalf("a missing node must fail the run, got %v", res.findings)
	}
}

func TestJSImports_MissingNodeFails(t *testing.T) {
	defer func(n string) { nodeBin = n }(nodeBin)
	nodeBin = "node-that-is-not-installed"
	res := jsImports(treeOf("app/www/static/a.js", "export {};\n"))
	if len(res.findings) != 1 || !strings.Contains(res.findings[0].Why, "NOT checked") {
		t.Fatalf("a missing node must fail the run, got %v", res.findings)
	}
}

func TestJSParse_NamesTheLineAndTheToken(t *testing.T) {
	tr := jsTree("bad.js", "const a = 1;\nif (a) {\n}\n}\n")(t)
	res := jsParse(tr)
	if len(res.findings) != 1 || res.findings[0].Line != 4 || !strings.Contains(res.findings[0].Why, "SyntaxError") {
		t.Fatalf("want one finding at line 4 naming the SyntaxError, got %+v", res.findings)
	}
}

// gitRepo makes a repo under a temp dir from path, content pairs and stages
// every file, so the loader reads git's own answers.
func gitRepo(t *testing.T, pairs ...string) string {
	t.Helper()
	dir := t.TempDir()
	for i := 0; i+1 < len(pairs); i += 2 {
		p := filepath.Join(dir, filepath.FromSlash(pairs[i]))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(pairs[i+1]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}} {
		cmd := exec.Command("git", append([]string{"-c", "core.autocrlf=false", "-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// TestLoad_ReadsTheIndexAsGitDoes pins the loader against a real index: the
// eol class and the attributes come from git, binary detection is git grep's,
// and an untracked file is not read.
func TestLoad_ReadsTheIndexAsGitDoes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	dir := gitRepo(t,
		".gitattributes", "*.txt text\n*.png binary\n",
		"bad.txt", "a\x00b\n",
		"ok.txt", "fine\n",
		"pic.png", marker("=")+"\n",
		"blob.dat", "\x00\n"+marker("=")+"\n",
		"notes.md", marker("=")+"\n",
		"sub dir/odd name.md", marker("=")+"\n")
	if err := os.WriteFile(filepath.Join(dir, "untracked.md"), []byte(marker("=")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(tr.files); got != 7 {
		t.Errorf("loaded %d tracked files, want 7", got)
	}
	for _, c := range []struct {
		rule string
		want []string
	}{
		{"text-files", []string{"bad.txt"}},
		{"conflict-markers", []string{"notes.md:1", "sub dir/odd name.md:1"}},
	} {
		r, _ := ruleNamed(c.rule)
		if got := wheres(r.check(tr).findings); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: findings at %q, want %q", c.rule, got, c.want)
		}
	}
}

func TestRun_GuardShapedOutput(t *testing.T) {
	tr := treeOf("CHANGELOG.md", marker("=")+"\n", "README.md", "clean\n")
	var buf bytes.Buffer
	if run(tr, "conflict-markers", &buf) {
		t.Fatal("a marker line must fail the run")
	}
	want := "FAIL conflict-markers  CHANGELOG.md:1  merge-conflict marker line\n     " + rules[1].hint + "\n"
	if buf.String() != want {
		t.Errorf("output %q, want %q", buf.String(), want)
	}
	buf.Reset()
	if !run(treeOf("README.md", "clean\n"), "conflict-markers", &buf) || buf.String() != "ok   conflict-markers (1 tracked files, no marker line)\n" {
		t.Errorf("clean output %q", buf.String())
	}
	buf.Reset()
	if run(tr, "no-such-rule", &buf) || !strings.HasPrefix(buf.String(), "FAIL repolint") {
		t.Errorf("an unknown rule must fail, got %q", buf.String())
	}
}

func TestReport_CapsTheFindingsShown(t *testing.T) {
	var fs []Finding
	for i := 1; i <= maxShown+5; i++ {
		fs = append(fs, Finding{File: "a.md", Line: i, Why: "x"})
	}
	var buf bytes.Buffer
	report(&buf, rule{name: "r", hint: "h"}, result{findings: fs})
	out := buf.String()
	if strings.Count(out, "\nFAIL r  a.md:")+1 != maxShown || !strings.Contains(out, "FAIL r  ...  and 5 more") {
		t.Errorf("want %d findings shown and the rest counted, got\n%s", maxShown, out)
	}
}
