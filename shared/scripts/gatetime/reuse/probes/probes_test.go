//go:build reuseprobe

package probes

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"shingo/shared/scripts/gatetime/reuse"
)

// probe is one row of the table in
// work/loc-census-2026-11-20/gate-fast/lanes/reuse/NOTES-reuse-2026-11-23.md,
// with the same number, sources and want. The want is fixed before the probe
// first runs and is never edited to make it pass.
type probe struct {
	n         int
	name, src string
	want      string
	run       func(h *harness) (got, how string)
}

// TestReuseProbes prints one line per probe. Kill criterion (the notes):
// every line prints, and every line ends "-- ok".
func TestReuseProbes(t *testing.T) {
	h := newHarness(t)
	probes := table()
	h.print(fmt.Sprintf("PROBES %d to print, %s/%s, wrapper %s; kill criterion: every line prints and ends -- ok",
		len(probes), runtime.GOOS, runtime.GOARCH, h.wrapper))
	wrong := 0
	for _, p := range probes {
		got, how := runOne(h, p)
		ok := got == p.want
		v := "ok"
		if !ok {
			v, wrong = "WRONG", wrong+1
		}
		if how != "" {
			how = " (" + how + ")"
		}
		h.print(fmt.Sprintf("PROBE %d %s [%s]: want %s, got %s%s -- %s", p.n, p.name, p.src, p.want, got, how, v))
	}
	h.print(fmt.Sprintf("PROBES DONE printed=%d ok=%d wrong=%d", len(probes), len(probes)-wrong, wrong))
	if wrong > 0 {
		t.Errorf("%d probe(s) WRONG: reuse stays off", wrong)
	}
}

func runOne(h *harness, p probe) (got, how string) {
	defer func() {
		if r := recover(); r != nil {
			got, how = "PANIC", fmt.Sprint(r)
		}
	}()
	return p.run(h)
}

const want3 = "RUN,REUSE,FAIL"

func table() []probe {
	return []probe{
		{1, "real engine: row-scale.js rewritten in place, run by node", "B P1, C P1, D11 P1", want3, p1},
		{2, "real engine: row-scale.js through sed -i (new inode)", "B P1b, D11 P1-sed", want3, p2},
		{3, "a .. read inside the module", "B P2, C P2, D11 P2", want3, p3},
		{4, "real engine: node child reads shared/scene-net.js", "B P3, C P3, D11 P3", want3, p4},
		{5, "a repo file outside the module, in-process", "B P4, C P4, D11 P4", want3, p5},
		{6, "a path built from variables by a helper", "B P5, C P5, D11 P5", want3, p6},
		{7, "a path named inside a markdown doc", "B P6, C P6, D11 P6", want3, p7},
		{8, "a walk, then a new untracked file in it", "B P7, C P7, D11 P7, R25/F40", want3, p8},
		{9, "real config: reads shingo-edge/uop/accumulator.go as text", "B P8, C P8, T P8, D11 P8", want3, p9},
		{10, "this module's .go (not compiled in) read by a child", "B P9, C P9, T P9, D11 P9", want3, p10},
		{11, "real config: never reaches testdb, reads shared/tokens.css", "B P10, C P10, T P10, D11 P10, R25", want3, p11},
		{12, "dirty edit, green, commit it, then a breaking edit", "B P11, C P11, T P16", want3, p12},
		{13, "a breaking edit inside the run's window", "B P12, C P12", "REFUSE,FAIL", p13},
		{14, "a breaking edit in the same second as the last run's end", "B P12", want3, p14},
		{15, "same-size edit, mtime put back (touch -r)", "B P13, T P13, brief", want3, p15},
		{16, "level tree, uncommitted breaking Go edit", "B P14, C P14, T P15, G11", want3, p16},
		{17, "real shared: helper census over both www, an Edge edit", "B P15, C P13, D4", want3, p17},
		{18, "an env var read after m.Run, changed", "B P16, C P19", want3, p18},
		{19, "an env var read before m.Run (package var), changed", "C P19, T P12", want3, p19},
		{20, "os.Environ() read (not in the test log), changed", "timing hole 4", want3, p20},
		{21, "the identity changed (image id, server flags)", "B P17, C P20, T P14", "RUN,REUSE,RUN", p21},
		{22, "a child started by absolute path reads a file", "B P18, T P11", want3, p22},
		{23, "a raw-syscall read (syscall.Open)", "B P19, T P20, brief", want3, p23},
		{24, "a TestMain reads a file before m.Run", "B P20, C P17, T P12, brief", want3, p24},
		{25, "a package-level var reads a file", "C P18, brief", want3, p25},
		{26, "a failing test, then again unchanged", "B P21, C P23", "FAIL,FAIL", p26},
		{27, "the binary killed mid-run, then run normally twice", "B P22, brief", "KILLED,RUN,REUSE", p27},
		{28, "the wrapper rebuilt (different bytes)", "B P23, C P22", "RUN,REUSE,RUN", p28},
		{29, "a filtered run, then a full run, then again", "B P24, C P25, brief", "REFUSE,RUN,REUSE", p29},
		{30, "an unbounded child (git) reads a changed file", "C P21", want3, p30},
		{31, "a tree that does not compile", "C P15", "RUN,REUSE,RED", p31},
		{32, "dev, then push, on one tree (the hit names dev)", "C P16", "RUN,REUSE(dev)", p32},
		{33, "a passing run that printed SHINGO-DOCKER-DOWN", "C P24", "REFUSE,REFUSE", p33},
		{34, "an ignored file read in-process", "C P26", want3, p34},
		{35, "a symlink (junction) retargeted", "C P27", want3, p35},
		{36, "a child reads an ignored file", "new: whole-tree key omits ignored files", want3, p36},
		{37, "a toolchain bump (identity's node line), twice", "T P0", "a=RUN,REUSE,RUN,REUSE b=RUN,REUSE,RUN,REUSE", p37},
		{38, "control: unchanged tree, six packages", "B C1, C C1, T P19",
			"child=RUN,REUSE clog=RUN,REUSE imp=RUN,REUSE inproc=RUN,REUSE leaf=RUN,REUSE tmain=RUN,REUSE", p38},
		{39, "control: a Go-only edit in one leaf", "B C2, C C2",
			"child=RUN clog=REUSE imp=RUN inproc=REUSE leaf=RUN tmain=RUN", p39},
		{40, "control: a CHANGELOG.md edit", "C C3",
			"child=RUN clog=RUN imp=REUSE inproc=REUSE leaf=REUSE tmain=RUN", p40},
		{41, "torn store: truncated; a flipped output byte; a leftover .tmp", "brief, T P18", "RUN,RUN,RUN,REUSE", p41},
		{42, "the store deleted", "T P18", "RUN,RUN", p42},
		{43, "the gate's scripts edited (identity changes)", "T P17", "RUN,REUSE,RUN", p43},
		{44, "the canary's record equals the expected one", "B 2.4 step 6, brief", "CANARY-OK", p44},
		{45, "a child reads a file outside the repo", "new: whole-tree key covers the repo only", want3, p45},
		{46, "cert audit: a would-be hit fails fresh, then one passes", "C cert as auditor, brief", "RUN,AUDIT-MISMATCH,AUDIT-AGREE", p46},
		{47, "GITHUB_ACTIONS=true with reuse on", "B, C, brief", "RUN,RUN", p47},
	}
}

// ---- the test sources the synths use ------------------------------------

func src(pkg, imports, body string) string {
	return "package " + pkg + "\n\nimport (\n" + imports + "\n)\n\n" + body
}

// readOK is a test asserting that expr (a string) is "ok".
func readOK(expr string) string {
	return src("a", "\t\"os\"\n\t\"testing\"", `var _ = os.ReadFile

func TestA(t *testing.T) {
	got := `+expr+`
	if got != "ok" {
		t.Fatalf("read %q, want ok", got)
	}
}

func read(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}
`)
}

// childOK is a test whose child (the test binary again) evaluates expr, and
// which passes only when the child printed "ok".
func childOK(imports, expr string) string {
	return src("a", "\t\"os\"\n\t\"os/exec\"\n\t\"strings\"\n\t\"testing\"\n"+imports, `func TestA(t *testing.T) {
	if os.Getenv("PROBE_CHILD") == "1" {
		os.Stdout.WriteString("CHILD:" + `+expr+` + "\n")
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestA$")
	cmd.Env = append(os.Environ(), "PROBE_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "CHILD:ok\n") {
		t.Fatalf("the child said %q", out)
	}
}

func read(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}
`)
}

func base(extra map[string]string) map[string]string {
	m := map[string]string{"go.mod": modFile, "data/x.txt": "ok"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// three is the common shape: run, run unchanged, edit, run.
func (h *harness) three(name string, files map[string]string, pkg string, edit func(*synth)) (string, string) {
	s := h.newSynth(name, files)
	a := s.goTest(pkg)
	b := s.goTest(pkg)
	edit(s)
	c := s.goTest(pkg)
	return seq("probe/"+strings.TrimPrefix(pkg, "./"), a, b, c)
}

func breakX(s *synth) { s.edit("data/x.txt", "no") }

// ---- the real-tree probes ---------------------------------------------------

func (h *harness) realThree(n int, module, pkg, importPath, file string, change func(string) string, inPlace bool) (string, string) {
	s, err := h.realSynth(n, module)
	if err != nil {
		return "SETUP", err.Error()
	}
	f := s.path(file)
	orig, err := os.ReadFile(f)
	if err != nil {
		return "SETUP", err.Error()
	}
	a := s.goTest(pkg)
	b := s.goTest(pkg)
	before, _ := os.Stat(f)
	if err := rewrite(f, change(string(orig)), inPlace); err != nil {
		return "SETUP", err.Error()
	}
	after, _ := os.Stat(f)
	c := s.goTest(pkg)
	_ = rewrite(f, string(orig), true)
	settle()
	got, how := seq(importPath, a, b, c)
	if same := os.SameFile(before, after); same != inPlace {
		how += fmt.Sprintf("; SETUP: same inode after the edit = %v, wanted %v", same, inPlace)
		got += "!"
	}
	return got, how
}

// rewrite replaces a file's bytes: in place (same inode, O_TRUNC) or as sed -i
// does (a new file renamed over it).
func rewrite(f, content string, inPlace bool) error {
	if inPlace {
		fd, err := os.OpenFile(f, os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			return err
		}
		_, err = fd.WriteString(content)
		return errorsJoin(err, fd.Close())
	}
	tmp := f + ".sedtmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, f)
}

func errorsJoin(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

func p1(h *harness) (string, string) {
	return h.realThree(1, "shingo-core", "./engine", "shingocore/engine", "shingo-core/engine/testdata/row-scale.js",
		func(s string) string { return "process.exit(3);\n" + s }, true)
}

func p2(h *harness) (string, string) {
	return h.realThree(2, "shingo-core", "./engine", "shingocore/engine", "shingo-core/engine/testdata/row-scale.js",
		func(s string) string { return "process.exit(3);\n" + s }, false)
}

func p4(h *harness) (string, string) {
	return h.realThree(4, "shingo-core", "./engine", "shingocore/engine", "shared/scene-net.js",
		func(s string) string { return "throw new Error(\"reuse probe 4\");\n" + s }, true)
}

func p9(h *harness) (string, string) {
	return h.realThree(9, "shingo-core", "./config", "shingocore/config", "shingo-edge/uop/accumulator.go",
		func(s string) string {
			return strings.ReplaceAll(s, "defaultInventoryDeltaInterval", "defaultInventoryDeltaIntervaX")
		}, true)
}

func p11(h *harness) (string, string) {
	return h.realThree(11, "shingo-core", "./config", "shingocore/config", "shared/tokens.css",
		func(s string) string { return strings.ReplaceAll(s, "--viz-seq-2", "--viz-seq-9") }, true)
}

// p17: a second definition of a shared helper in an Edge script, which the
// shared census (TestSharedHelpersDefinedOnce) refuses.
func p17(h *harness) (string, string) {
	root, err := h.realCopy()
	if err != nil {
		return "SETUP", err.Error()
	}
	name, target := "", ""
	shared, _ := filepath.Glob(filepath.Join(root, "shared", "*.js"))
	sort.Strings(shared)
	re := regexp.MustCompile(`(?m)^export function ([A-Za-z_$][\w$]*)`)
	for _, f := range shared {
		b, _ := os.ReadFile(f)
		if m := re.FindSubmatch(b); m != nil {
			name = string(m[1])
			break
		}
	}
	js, _ := filepath.Glob(filepath.Join(root, "shingo-edge", "www", "static", "js", "*.js"))
	sort.Strings(js)
	for _, f := range js {
		if !strings.HasSuffix(f, ".min.js") {
			target = f
			break
		}
	}
	if name == "" || target == "" {
		return "SETUP", "no shared export or Edge script found"
	}
	rel, _ := filepath.Rel(root, target)
	return h.realThree(17, "shared", ".", "shingo/shared", filepath.ToSlash(rel),
		func(s string) string { return s + "\nfunction " + name + "() {}\n" }, true)
}

// ---- the synthetic probes -----------------------------------------------------

func p3(h *harness) (string, string) {
	return h.three("p3", base(map[string]string{"a/a_test.go": readOK(`read("../data/x.txt")`)}), "./a", breakX)
}

func p5(h *harness) (string, string) {
	files := map[string]string{"root.txt": "ok", "sub/go.mod": modFile, "sub/a/a_test.go": readOK(`read("../../root.txt")`)}
	return h.three("p5", files, "./a", func(s *synth) { s.edit("root.txt", "no") })
}

func p6(h *harness) (string, string) {
	body := src("a", "\t\"os\"\n\t\"path/filepath\"\n\t\"testing\"", `func part(i int) string { return []string{"da", "ta", "y.", "txt"}[i] }

func TestA(t *testing.T) {
	p := filepath.Join("..", part(0)+part(1), part(2)+part(3))
	b, _ := os.ReadFile(p)
	if string(b) != "ok" {
		t.Fatalf("%s is %q", p, b)
	}
}
`)
	files := map[string]string{"go.mod": modFile, "data/y.txt": "ok", "a/a_test.go": body}
	return h.three("p6", files, "./a", func(s *synth) { s.edit("data/y.txt", "no") })
}

func p7(h *harness) (string, string) {
	body := src("a", "\t\"os\"\n\t\"regexp\"\n\t\"testing\"", `func TestA(t *testing.T) {
	doc, _ := os.ReadFile("../doc.md")
	m := regexp.MustCompile("<([^>]+)>").FindSubmatch(doc)
	if m == nil {
		t.Fatal("doc.md names no input")
	}
	b, _ := os.ReadFile("../" + string(m[1]))
	if string(b) != "ok" {
		t.Fatalf("%s is %q", m[1], b)
	}
}
`)
	files := map[string]string{"go.mod": modFile, "doc.md": "The input is <data/y.txt>.\n", "data/y.txt": "ok", "a/a_test.go": body}
	return h.three("p7", files, "./a", func(s *synth) { s.edit("data/y.txt", "no") })
}

func p8(h *harness) (string, string) {
	body := src("a", "\t\"io/fs\"\n\t\"path/filepath\"\n\t\"testing\"", `func TestA(t *testing.T) {
	n := 0
	err := filepath.WalkDir("../tree", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return err
	})
	if err != nil || n != 2 {
		t.Fatalf("tree holds %d files (%v), want 2", n, err)
	}
}
`)
	files := map[string]string{"go.mod": modFile, "tree/one.txt": "1", "tree/sub/two.txt": "2", "a/a_test.go": body}
	return h.three("p8", files, "./a", func(s *synth) { s.edit("tree/sub/three.txt", "3") })
}

func p10(h *harness) (string, string) {
	files := map[string]string{"go.mod": modFile,
		"other/ignored.go": "//go:build ignore\n\npackage other\n\nconst V = 1\n",
		"a/a_test.go":      childOK("", `map[bool]string{true: "ok", false: "no"}[strings.Contains(read("../other/ignored.go"), "V = 1")]`)}
	return h.three("p10", files, "./a", func(s *synth) {
		s.edit("other/ignored.go", "//go:build ignore\n\npackage other\n\nconst V = 2\n")
	})
}

func p12(h *harness) (string, string) {
	s := h.newSynth("p12", base(map[string]string{"data/x.txt": "v1", "a/a_test.go": src("a", "\t\"os\"\n\t\"testing\"",
		`func TestA(t *testing.T) {
	b, _ := os.ReadFile("../data/x.txt")
	if string(b) != "v2" {
		t.Fatalf("read %q, want v2", b)
	}
}
`)}))
	s.edit("data/x.txt", "v2") // dirty, uncommitted
	settle()
	a := s.goTest("./a")
	gitOut(h.t, s.dir, "add", "-A")
	gitOut(h.t, s.dir, "-c", "user.email=probe@example.invalid", "-c", "user.name=probe", "commit", "-qm", "the dirty edit")
	b := s.goTest("./a")
	s.edit("data/x.txt", "v3")
	c := s.goTest("./a")
	return seq("probe/a", a, b, c)
}

// gatedOK reads data/x.txt, then asks the probe's server before it passes:
// the probe edits the file while the binary waits.
const gatedOK = `func TestA(t *testing.T) {
	b, _ := os.ReadFile("../data/x.txt")
	if string(b) != "ok" {
		t.Fatalf("read %q, want ok", b)
	}
	c, err := net.Dial("tcp", os.Getenv("PROBE_ADDR"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "%d\n", os.Getpid())
	line, _ := bufio.NewReader(c).ReadString('\n')
	if strings.TrimSpace(line) != "go" {
		t.Fatalf("the server said %q", line)
	}
}
`

const gatedImports = "\t\"bufio\"\n\t\"fmt\"\n\t\"net\"\n\t\"os\"\n\t\"strings\"\n\t\"testing\""

func p13(h *harness) (string, string) {
	srv := newServer(h.t)
	s := h.newSynth("p13", base(map[string]string{"a/a_test.go": src("a", gatedImports, gatedOK)}))
	s.env = []string{"PROBE_ADDR=" + srv.addr()}
	srv.reply = func(string) string { s.edit("data/x.txt", "no"); return "go" }
	a := s.goTest("./a")
	srv.reply = func(string) string { return "go" }
	b := s.goTest("./a")
	return seq("probe/a", a, b)
}

func p14(h *harness) (string, string) {
	s := h.newSynth("p14", base(map[string]string{"a/a_test.go": readOK(`read("../data/x.txt")`)}))
	a := s.goTest("./a")
	var b step
	for try := 0; ; try++ {
		b = s.goTest("./a")
		end := time.Now()
		s.edit("data/x.txt", "no")
		fi, err := os.Stat(s.path("data/x.txt"))
		if err == nil && fi.ModTime().Unix() == end.Unix() {
			break
		}
		if try == 4 {
			return "SETUP", "five tries, and the edit never landed in the run's last second"
		}
		s.edit("data/x.txt", "ok")
		settle()
	}
	c := s.goTest("./a")
	return seq("probe/a", a, b, c)
}

func p15(h *harness) (string, string) {
	return h.three("p15", base(map[string]string{"data/x.txt": "aaaa", "a/a_test.go": src("a", "\t\"os\"\n\t\"testing\"",
		`func TestA(t *testing.T) {
	b, _ := os.ReadFile("../data/x.txt")
	if string(b) != "aaaa" {
		t.Fatalf("read %q", b)
	}
}
`)}), "./a", func(s *synth) {
		p := s.path("data/x.txt")
		fi, _ := os.Stat(p)
		s.edit("data/x.txt", "bbbb")
		_ = os.Chtimes(p, fi.ModTime(), fi.ModTime())
	})
}

func p16(h *harness) (string, string) {
	s := h.newSynth("p16", map[string]string{"go.mod": modFile,
		"lib/lib.go":      "package lib\n\nfunc V() int { return 1 }\n",
		"lib/lib_test.go": "package lib\n\nimport \"testing\"\n\nfunc TestV(t *testing.T) {\n\tif V() != 1 {\n\t\tt.Fatal(V())\n\t}\n}\n"})
	gitOut(h.t, s.dir, "branch", "base")
	a := s.goTest("./lib")
	b := s.goTest("./lib")
	s.edit("lib/lib.go", "package lib\n\nfunc V() int { return 2 }\n")
	level := gitOut(h.t, s.dir, "rev-list", "--count", "base..HEAD") == "0"
	dirty := gitOut(h.t, s.dir, "status", "--porcelain") != ""
	c := s.goTest("./lib")
	got, how := seq("probe/lib", a, b, c)
	if !level || !dirty {
		return got + "!", how + fmt.Sprintf("; SETUP: level=%v uncommitted=%v", level, dirty)
	}
	return got, how
}

func envTest(name, decl, expr string) string {
	return src("a", "\t\"os\"\n\t\"strings\"\n\t\"testing\"", decl+`
var _ = strings.Contains

func TestA(t *testing.T) {
	if got := `+expr+`; got != "a" {
		t.Fatalf("`+name+` is %q, want a", got)
	}
}
`)
}

func (h *harness) envProbe(name string, testSrc, variable string) (string, string) {
	s := h.newSynth(name, base(map[string]string{"a/a_test.go": testSrc}))
	s.env = []string{variable + "=a"}
	a := s.goTest("./a")
	b := s.goTest("./a")
	s.env = []string{variable + "=b"}
	c := s.goTest("./a")
	return seq("probe/a", a, b, c)
}

func p18(h *harness) (string, string) {
	return h.envProbe("p18", envTest("PROBE_ENV", "", `os.Getenv("PROBE_ENV")`), "PROBE_ENV")
}

func p19(h *harness) (string, string) {
	return h.envProbe("p19", envTest("PROBE_PRE", `var pre = os.Getenv("PROBE_PRE")`, "pre"), "PROBE_PRE")
}

func p20(h *harness) (string, string) {
	return h.envProbe("p20", envTest("PROBE_ENVIRON", `func environ() string {
	for _, kv := range os.Environ() {
		if v, ok := strings.CutPrefix(kv, "PROBE_ENVIRON="); ok {
			return v
		}
	}
	return ""
}`, "environ()"), "PROBE_ENVIRON")
}

func trivial() string {
	return "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n"
}

func p21(h *harness) (string, string) {
	s := h.newSynth("p21", base(map[string]string{"a/a_test.go": trivial()}))
	s.identity = filepath.Join(h.work, "synth", filepath.Base(s.dir)+".identity")
	writeFile(h.t, s.identity, "pg_image=sha256:one\npg_flags=-c fsync=off\n")
	a := s.goTest("./a")
	b := s.goTest("./a")
	writeFile(h.t, s.identity, "pg_image=sha256:two\npg_flags=-c fsync=off\n")
	c := s.goTest("./a")
	return seq("probe/a", a, b, c)
}

func p22(h *harness) (string, string) {
	body := src("a", "\t\"os/exec\"\n\t\"runtime\"\n\t\"strings\"\n\t\"testing\"", `func TestA(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "cat ../data/x.txt")
	if runtime.GOOS == "windows" {
		cmd = exec.Command(`+"`C:\\Windows\\System32\\cmd.exe`"+`, "/c", "type", `+"`..\\data\\x.txt`"+`)
	}
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("the child read %q (%v)", out, err)
	}
}
`)
	return h.three("p22", base(map[string]string{"a/a_test.go": body}), "./a", breakX)
}

func p23(h *harness) (string, string) {
	body := src("a", "\t\"syscall\"\n\t\"testing\"", `func TestA(t *testing.T) {
	fd, err := syscall.Open("../data/x.txt", syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	buf := make([]byte, 16)
	n, _ := syscall.Read(fd, buf)
	if string(buf[:n]) != "ok" {
		t.Fatalf("read %q", buf[:n])
	}
}
`)
	return h.three("p23", base(map[string]string{"a/a_test.go": body}), "./a", breakX)
}

func p24(h *harness) (string, string) {
	body := src("a", "\t\"os\"\n\t\"testing\"", `var data []byte

func TestMain(m *testing.M) {
	data, _ = os.ReadFile("../data/x.txt")
	os.Exit(m.Run())
}

func TestA(t *testing.T) {
	if string(data) != "ok" {
		t.Fatalf("read %q", data)
	}
}
`)
	return h.three("p24", base(map[string]string{"a/a_test.go": body}), "./a", breakX)
}

func p25(h *harness) (string, string) {
	body := src("a", "\t\"os\"\n\t\"testing\"", `var data, _ = os.ReadFile("../data/x.txt")

func TestA(t *testing.T) {
	if string(data) != "ok" {
		t.Fatalf("read %q", data)
	}
}
`)
	return h.three("p25", base(map[string]string{"a/a_test.go": body}), "./a", breakX)
}

func p26(h *harness) (string, string) {
	s := h.newSynth("p26", base(map[string]string{"a/a_test.go": "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) { t.Fatal(\"red on purpose\") }\n"}))
	a := s.goTest("./a")
	b := s.goTest("./a")
	return seq("probe/a", a, b)
}

func p27(h *harness) (string, string) {
	srv := newServer(h.t)
	s := h.newSynth("p27", base(map[string]string{"a/a_test.go": src("a", gatedImports, gatedOK)}))
	s.env = []string{"PROBE_ADDR=" + srv.addr()}
	srv.reply = func(pid string) string {
		var n int
		fmt.Sscan(pid, &n)
		if p, err := os.FindProcess(n); err == nil {
			_ = p.Kill()
		}
		return ""
	}
	a := s.goTest("./a")
	srv.reply = func(string) string { return "go" }
	b := s.goTest("./a")
	c := s.goTest("./a")
	return seq("probe/a", a, b, c)
}

func p28(h *harness) (string, string) {
	s := h.newSynth("p28", base(map[string]string{"a/a_test.go": trivial()}))
	a := s.goTest("./a")
	b := s.goTest("./a")
	w2 := filepath.Join(h.work, "bin", "gatetime-rebuilt"+exeSuffix())
	bin, err := os.ReadFile(h.wrapper)
	if err != nil {
		return "SETUP", err.Error()
	}
	writeFile(h.t, w2, string(bin)+"\x00") // the same program, other bytes
	_ = os.Chmod(w2, 0o755)
	s.wrapper = w2
	c := s.goTest("./a")
	return seq("probe/a", a, b, c)
}

func p29(h *harness) (string, string) {
	s := h.newSynth("p29", base(map[string]string{"a/a_test.go": "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n\nfunc TestB(t *testing.T) {}\n"}))
	s.extra = []string{"-run", "TestA"}
	a := s.goTest("./a")
	s.extra = nil
	b := s.goTest("./a")
	c := s.goTest("./a")
	return seq("probe/a", a, b, c)
}

func p30(h *harness) (string, string) {
	body := src("a", "\t\"os\"\n\t\"os/exec\"\n\t\"strings\"\n\t\"testing\"", `func TestA(t *testing.T) {
	out, err := exec.Command("git", "hash-object", "../data/x.txt").Output()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile("../data/x.sha")
	if strings.TrimSpace(string(out)) != strings.TrimSpace(string(want)) {
		t.Fatalf("git says %s, data/x.sha says %s", out, want)
	}
}
`)
	files := base(map[string]string{"a/a_test.go": body})
	files["data/x.sha"] = gitHashObject("ok")
	return h.three("p30", files, "./a", breakX)
}

// gitHashObject is git's blob id of content (sha1 of "blob <n>\0<content>").
func gitHashObject(content string) string {
	cmd := exec.Command("git", "hash-object", "--stdin")
	cmd.Stdin = strings.NewReader(content)
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out)) + "\n"
}

func p31(h *harness) (string, string) {
	return h.three("p31", base(map[string]string{"a/a_test.go": trivial()}), "./a", func(s *synth) {
		s.edit("a/a_test.go", trivial()+"\nfunc broken( {\n")
	})
}

func p32(h *harness) (string, string) {
	s := h.newSynth("p32", base(map[string]string{"a/a_test.go": readOK(`read("../data/x.txt")`)}))
	s.env = []string{"GATE_TIER=dev"}
	a := s.goTest("./a")
	s.env = []string{"GATE_TIER=push"}
	b := s.goTest("./a")
	got, how := seq("probe/a", a, b)
	if d := b.decision("probe/a"); d != nil && d.From != nil {
		got = strings.Replace(got, "REUSE", "REUSE("+d.From.Tier+")", 1)
	}
	return got, how
}

func p33(h *harness) (string, string) {
	s := h.newSynth("p33", base(map[string]string{"a/a_test.go": "package a\n\nimport (\n\t\"fmt\"\n\t\"testing\"\n)\n\nfunc TestA(t *testing.T) { fmt.Println(\"SHINGO-DOCKER-DOWN: reuse probe 33\") }\n"}))
	a := s.goTest("./a")
	b := s.goTest("./a")
	return seq("probe/a", a, b)
}

func p34(h *harness) (string, string) {
	files := map[string]string{"go.mod": modFile, "data/x.local": "ok", "a/a_test.go": readOK(`read("../data/x.local")`)}
	return h.three("p34", files, "./a", func(s *synth) { s.edit("data/x.local", "no") })
}

func p35(h *harness) (string, string) {
	files := map[string]string{"go.mod": modFile, "dirA/v.txt": "ok", "dirB/v.txt": "no", "a/a_test.go": readOK(`read("../lnk/v.txt")`)}
	s := h.newSynth("p35", files)
	if err := link(s.path("dirA"), s.path("lnk")); err != nil {
		return "SETUP", err.Error()
	}
	settle()
	a := s.goTest("./a")
	b := s.goTest("./a")
	_ = os.Remove(s.path("lnk"))
	if err := link(s.path("dirB"), s.path("lnk")); err != nil {
		return "SETUP", err.Error()
	}
	c := s.goTest("./a")
	return seq("probe/a", a, b, c)
}

// link makes a directory link: a symlink, or on Windows a junction (which
// needs no privilege).
func link(target, name string) error {
	if runtime.GOOS == "windows" {
		out, err := exec.Command("cmd", "/c", "mklink", "/J", name, target).CombinedOutput()
		if err != nil {
			return fmt.Errorf("mklink /J: %v: %s", err, out)
		}
		return nil
	}
	return os.Symlink(target, name)
}

func p36(h *harness) (string, string) {
	files := map[string]string{"go.mod": modFile, "data/x.local": "ok", "a/a_test.go": childOK("", `read("../data/x.local")`)}
	return h.three("p36", files, "./a", func(s *synth) { s.edit("data/x.local", "no") })
}

func p37(h *harness) (string, string) {
	s := h.newSynth("p37", map[string]string{"go.mod": modFile, "a/a_test.go": trivial(),
		"b/b_test.go": "package b\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) {}\n"})
	s.identity = filepath.Join(h.work, "synth", filepath.Base(s.dir)+".identity")
	writeFile(h.t, s.identity, "go=go1.25.6\nnode=v26.0.0\n")
	a := s.goTest("./...")
	b := s.goTest("./...")
	writeFile(h.t, s.identity, "go=go1.25.6\nnode=v26.1.0\n")
	c := s.goTest("./...")
	d := s.goTest("./...")
	return pkgTokens([]string{"a", "b"}, a, b, c, d)
}

// The controls share one module of six packages (38, then 39, then 40).
var ctl *synth

func ctlFiles() map[string]string {
	return map[string]string{
		"go.mod":       modFile,
		"CHANGELOG.md": "# changelog\n",
		"data/x.txt":   "ok",
		"inproc/inproc_test.go": "package inproc\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\n" +
			"func TestIn(t *testing.T) {\n\tif b, _ := os.ReadFile(\"../data/x.txt\"); string(b) != \"ok\" {\n\t\tt.Fatal(string(b))\n\t}\n}\n",
		"leaf/leaf.go":      "package leaf\n\nfunc V() int { return 1 }\n",
		"leaf/leaf_test.go": "package leaf\n\nimport \"testing\"\n\nfunc TestV(t *testing.T) {\n\tif V() != 1 {\n\t\tt.Fatal(V())\n\t}\n}\n",
		"imp/imp_test.go": "package imp\n\nimport (\n\t\"testing\"\n\n\t\"probe/leaf\"\n)\n\n" +
			"func TestImp(t *testing.T) {\n\tif leaf.V() != 1 {\n\t\tt.Fatal(leaf.V())\n\t}\n}\n",
		"child/child_test.go": strings.Replace(childOK("", `read("../data/x.txt")`), "package a", "package child", 1),
		"tmain/tmain_test.go": "package tmain\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\n" +
			"func TestMain(m *testing.M) { os.Exit(m.Run()) }\n\nfunc TestT(t *testing.T) {}\n",
		"clog/clog_test.go": "package clog\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\n" +
			"func TestLog(t *testing.T) {\n\tif b, _ := os.ReadFile(\"../CHANGELOG.md\"); len(b) == 0 {\n\t\tt.Fatal(\"empty\")\n\t}\n}\n",
	}
}

var ctlAll = []string{"child", "clog", "imp", "inproc", "leaf", "tmain"}

func p38(h *harness) (string, string) {
	ctl = h.newSynth("ctl", ctlFiles())
	a := ctl.goTest("./...")
	b := ctl.goTest("./...")
	return pkgTokens(ctlAll, a, b)
}

func p39(h *harness) (string, string) {
	if ctl == nil {
		return "SETUP", "probe 38 did not make the control module"
	}
	ctl.edit("leaf/leaf.go", "package leaf\n\nfunc V() int { return 1 }\n\n// W is new in probe 39.\nfunc W() int { return 2 }\n")
	settle()
	a := ctl.goTest("./...")
	return pkgTokens(ctlAll, a)
}

func p40(h *harness) (string, string) {
	if ctl == nil {
		return "SETUP", "probe 38 did not make the control module"
	}
	ctl.edit("CHANGELOG.md", "# changelog\n\n- probe 40\n")
	settle()
	a := ctl.goTest("./...")
	return pkgTokens(ctlAll, a)
}

func newestEntry(s *synth) string {
	files, _ := filepath.Glob(filepath.Join(s.store, "*", "*.json"))
	sort.Strings(files)
	if len(files) == 0 {
		return ""
	}
	return files[len(files)-1]
}

func p41(h *harness) (string, string) {
	s := h.newSynth("p41", base(map[string]string{"a/a_test.go": trivial()}))
	a := s.goTest("./a")
	e := newestEntry(s)
	b0, _ := os.ReadFile(e)
	if err := os.WriteFile(e, b0[:len(b0)/2], 0o644); err != nil {
		return "SETUP", err.Error()
	}
	b := s.goTest("./a")
	e = newestEntry(s)
	b1, _ := os.ReadFile(e)
	i := strings.Index(string(b1), `"output":"`) + len(`"output":"`)
	if i < len(`"output":"`) || i >= len(b1) {
		return "SETUP", "no output field in the entry"
	}
	flip := byte('A')
	if b1[i] == 'A' {
		flip = 'B'
	}
	b1[i] = flip
	_ = os.WriteFile(e, b1, 0o644)
	c := s.goTest("./a")
	e = newestEntry(s)
	b2, _ := os.ReadFile(e)
	_ = os.WriteFile(filepath.Join(filepath.Dir(e), ".99999999999999999999-torn.json.tmp"), b2[:len(b2)/3], 0o644)
	d := s.goTest("./a")
	got, how := seq("probe/a", a, b, c, d)
	for i, st := range []step{b, c, d} {
		if dd := st.decision("probe/a"); dd != nil {
			how += fmt.Sprintf("; %d: %d unverifiable of %d", i+2, dd.Unverifiable, dd.Entries)
		}
	}
	return got, strings.TrimPrefix(how, "; ")
}

func p42(h *harness) (string, string) {
	s := h.newSynth("p42", base(map[string]string{"a/a_test.go": trivial()}))
	a := s.goTest("./a")
	_ = os.RemoveAll(s.store)
	b := s.goTest("./a")
	return seq("probe/a", a, b)
}

// p43: the identity the gate writes must move when the gate's scripts move.
func p43(h *harness) (string, string) {
	root, err := h.realCopy()
	if err != nil {
		return "SETUP", err.Error()
	}
	gate := filepath.Join(root, "scripts", "gate.sh")
	src, _ := os.ReadFile(gate)
	if !regexp.MustCompile(`(?m)^\s*identity\)`).Match(src) {
		return "NOT-BUILT", "scripts/gate.sh has no identity mode (the gate wiring is not there yet)"
	}
	// `gate.sh identity` prints what the gate records as a run's identity.
	n := 0
	ident := func() string {
		n++
		out := filepath.Join(h.work, "stores", fmt.Sprintf("43-identity-%d", n))
		cmd := exec.Command("bash", "scripts/gate.sh", "identity")
		cmd.Dir = root
		b, err := cmd.Output()
		if err != nil {
			return "error: " + err.Error()
		}
		writeFile(h.t, out, string(b))
		return out
	}
	s := h.newSynth("p43", base(map[string]string{"a/a_test.go": trivial()}))
	s.identity = ident()
	a := s.goTest("./a")
	b := s.goTest("./a")
	_ = os.WriteFile(gate, append(src, []byte("\n# reuse probe 43\n")...), 0o755)
	s.identity = ident()
	c := s.goTest("./a")
	_ = os.WriteFile(gate, src, 0o755)
	return seq("probe/a", a, b, c)
}

func p44(h *harness) (string, string) {
	root, err := h.realCopy()
	if err != nil {
		return "SETUP", err.Error()
	}
	cmd := exec.Command(h.wrapper, "canary", "-root", root, "-identity", h.identity)
	out, err := cmd.CombinedOutput()
	line := strings.TrimSpace(string(out))
	if err == nil && strings.HasPrefix(line, "ok   reuse canary") {
		return "CANARY-OK", strings.TrimPrefix(line, "ok   reuse canary ")
	}
	return "CANARY-FAIL", clip(line)
}

func p45(h *harness) (string, string) {
	outside := filepath.Join(h.work, "outside-45", "o.txt")
	writeFile(h.t, outside, "ok")
	files := base(map[string]string{"a/a_test.go": childOK("", `read(os.Getenv("PROBE_OUTSIDE"))`)})
	s := h.newSynth("p45", files)
	s.env = []string{"PROBE_OUTSIDE=" + outside}
	a := s.goTest("./a")
	b := s.goTest("./a")
	writeFile(h.t, outside, "no")
	c := s.goTest("./a")
	return seq("probe/a", a, b, c)
}

func p46(h *harness) (string, string) {
	srv := newServer(h.t)
	s := h.newSynth("p46", base(map[string]string{"a/a_test.go": src("a", gatedImports, gatedOK)}))
	s.env = []string{"PROBE_ADDR=" + srv.addr()}
	a := s.goTest("./a") // reuse mode, the server says go
	s.mode = reuse.ModeAudit
	srv.reply = func(string) string { return "stop" } // a network-only flake
	b := s.goTest("./a")
	srv.reply = func(string) string { return "go" }
	c := s.goTest("./a")
	return seq("probe/a", a, b, c)
}

func p47(h *harness) (string, string) {
	s := h.newSynth("p47", base(map[string]string{"a/a_test.go": trivial()}))
	s.env = []string{"GITHUB_ACTIONS=true"}
	a := s.goTest("./a")
	b := s.goTest("./a")
	return seq("probe/a", a, b)
}

var _ = testing.Verbose
