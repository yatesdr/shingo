package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestMain lets this test binary stand in for gatetime itself: with
// GATETIME_TEST_AS_MAIN=1 it runs main's dispatch on its arguments, so a test
// can hand `go test -exec` this binary without building gatetime first.
func TestMain(m *testing.M) {
	if os.Getenv("GATETIME_TEST_AS_MAIN") == "1" {
		if len(os.Args) < 2 {
			os.Exit(usage())
		}
		run, ok := commands[os.Args[1]]
		if !ok {
			os.Exit(usage())
		}
		os.Exit(run(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func TestParseTestlog(t *testing.T) {
	base := filepath.Join(t.TempDir(), "pkg")
	other := filepath.Join(t.TempDir(), "elsewhere")
	log := "# test log\n" +
		"getenv HOME\n" +
		"open testdata/a.json\n" +
		"stat testdata/a.json\n" +
		"getenv HOME\n" +
		"chdir " + other + "\n" +
		"open b.txt\n" +
		"stat " + filepath.Join(base, "..", "c.md") + "\n"
	r := parseTestlog([]byte(log), base)
	if r.state != "complete" {
		t.Fatalf("state %q, want complete", r.state)
	}
	if strings.Join(r.env, ",") != "HOME" {
		t.Errorf("env %v, want [HOME] once", r.env)
	}
	want := map[string]string{
		filepath.Join(base, "testdata", "a.json"): "open,stat",
		filepath.Join(other, "b.txt"):             "open",
		other:                                     "chdir",
		filepath.Join(filepath.Dir(base), "c.md"): "stat",
	}
	if len(r.paths) != len(want) {
		t.Errorf("paths %v, want %v", r.paths, want)
	}
	for p, ops := range want {
		if got := strings.Join(r.paths[p], ","); got != ops {
			t.Errorf("%s: ops %q, want %q (paths %v)", p, got, ops, r.paths)
		}
	}
}

func TestParseTestlog_States(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
		n    int
	}{
		{"absent", nil, "absent", 0},
		{"empty", []byte{}, "empty", 0},
		{"no magic", []byte("open x\n"), "malformed", 0},
		{"torn last line", []byte("# test log\nopen /a\nopen /b"), "truncated", 1},
		{"unknown op", []byte("# test log\nread /a\n"), "malformed", 0},
	}
	for _, c := range cases {
		r := parseTestlog(c.data, string(filepath.Separator))
		if r.state != c.want || len(r.paths) != c.n {
			t.Errorf("%s: state %q with %d paths, want %q with %d", c.name, r.state, len(r.paths), c.want, c.n)
		}
	}
}

// TestHashPath_ContentNotMtime is the hole Go's own cache has (it keys a file
// on size and mtime): a same-size edit with the mtime put back must change the
// hash, and a new entry in a directory must change the directory's.
func TestHashPath_ContentNotMtime(t *testing.T) {
	d := t.TempDir()
	f := filepath.Join(d, "a.txt")
	if err := os.WriteFile(f, []byte("aaaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(f, old, old); err != nil {
		t.Fatal(err)
	}
	since := time.Now()
	h1, changed := hashPath(f, since)
	if !strings.HasPrefix(h1, "file:") || changed {
		t.Fatalf("hash %q changed=%v, want a file hash, unchanged", h1, changed)
	}
	if err := os.WriteFile(f, []byte("bbbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(f, old, old); err != nil {
		t.Fatal(err)
	}
	if h2, _ := hashPath(f, since); h2 == h1 {
		t.Errorf("same-size edit with the mtime put back kept hash %s", h1)
	}
	d1, _ := hashPath(d, since)
	if err := os.WriteFile(filepath.Join(d, "new.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	d2, changedDir := hashPath(d, since)
	if !strings.HasPrefix(d1, "dir:") || d1 == d2 {
		t.Errorf("directory hash %q -> %q, want a dir hash that moves with a new entry", d1, d2)
	}
	if !changedDir {
		t.Errorf("directory written after the run's start not marked changed_in_run")
	}
	if h, _ := hashPath(filepath.Join(d, "missing"), since); h != "absent" {
		t.Errorf("missing path hashed %q, want absent", h)
	}
}

func TestModulePackage(t *testing.T) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "go.mod"), []byte("module example.com/m\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(d, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	root, pkg := modulePackage(sub)
	if root != d || pkg != "example.com/m/a/b" {
		t.Errorf("modulePackage = %q %q, want %q example.com/m/a/b", root, pkg, d)
	}
	if _, pkg := modulePackage(d); pkg != "example.com/m" {
		t.Errorf("module root's package %q, want example.com/m", pkg)
	}
}

// fixture is a module with two test packages: reads, which reads an env var,
// a file inside the module, a file outside it, a directory, and starts one
// child (itself, listing no tests); and quiet, which reads nothing, starts
// nothing, and has one passing, one skipped and, when FIXTURE_FAIL is set, one
// failing test.
func writeFixture(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "mod")
	files := map[string]string{
		"go.mod":          "module fixture\n\ngo 1.25\n",
		"reads/data.txt":  "inside\n",
		"reads/x_test.go": readsTest,
		"quiet/x_test.go": quietTest,
	}
	for name, body := range files {
		p := filepath.Join(d, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

const readsTest = `package reads

import (
	"os"
	"os/exec"
	"testing"
)

func TestReads(t *testing.T) {
	if os.Getenv("FIXTURE_VAR") != "set" {
		t.Fatal("FIXTURE_VAR not passed through")
	}
	if _, err := os.ReadFile("data.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(os.Getenv("FIXTURE_OUTSIDE")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadDir("."); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(os.Args[0], "-test.run=^$").Run(); err != nil {
		t.Fatal(err)
	}
}
`

const quietTest = `package quiet

import (
	"os"
	"testing"
)

func TestPasses(t *testing.T) {}

func TestSkips(t *testing.T) { t.Skip("on purpose") }

func TestFailsWhenAsked(t *testing.T) {
	if os.Getenv("FIXTURE_FAIL") == "1" {
		t.Fatal("asked to fail")
	}
}
`

// goTest runs `go test -count=1 -vet=off -v ./...` in the fixture, with this binary as
// -exec when wrap is set. It returns the output with every duration masked,
// and the exit code.
func goTest(t *testing.T, dir string, wrap bool, env ...string) (string, int) {
	t.Helper()
	args := []string{"test", "-count=1", "-vet=off", "-v"}
	if wrap {
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		args = append(args, "-exec", "'"+filepath.ToSlash(self)+"' exec")
	}
	cmd := exec.Command("go", append(args, "./...")...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), append([]string{"GOWORK=off", "GOFLAGS=", "GOTOOLCHAIN=local"}, env...)...)
	if wrap {
		cmd.Env = append(cmd.Env, "GATETIME_TEST_AS_MAIN=1")
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("go test: %v\n%s", err, out)
	}
	masked := regexp.MustCompile(`\(?[0-9]+\.[0-9]+s\)?`).ReplaceAllString(string(out), "(T)")
	lines := strings.Split(strings.ReplaceAll(masked, "\r", ""), "\n")
	sort.Strings(lines) // packages may finish in either order
	return strings.Join(lines, "\n"), code
}

// TestExecUnderGoTest is the wrapper's contract, end to end through a real go
// test: the output and the exit code are the binary's whether or not it ran
// under the wrapper (pass, skip and fail counts included), and each binary
// leaves one manifest saying what it read.
func TestExecUnderGoTest(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	outside, err := filepath.Abs("main.go")
	if err != nil {
		t.Fatal(err)
	}
	dir := writeFixture(t)
	manifests := t.TempDir()
	env := []string{"FIXTURE_VAR=set", "FIXTURE_OUTSIDE=" + outside}

	for _, fail := range []string{"0", "1"} {
		e := append(append([]string(nil), env...), "FIXTURE_FAIL="+fail)
		plain, plainCode := goTest(t, dir, false, e...)
		wrapped, wrappedCode := goTest(t, dir, true, append(e, "GATETIME_MANIFESTS="+manifests)...)
		if plain != wrapped || plainCode != wrappedCode {
			t.Errorf("FIXTURE_FAIL=%s: output or exit differs under the wrapper\nplain (exit %d):\n%s\nwrapped (exit %d):\n%s",
				fail, plainCode, plain, wrappedCode, wrapped)
		}
		wantCode := map[string]int{"0": 0, "1": 1}[fail]
		if plainCode != wantCode {
			t.Errorf("FIXTURE_FAIL=%s: go test exited %d, want %d", fail, plainCode, wantCode)
		}
		for _, want := range []string{"--- PASS: TestPasses", "--- SKIP: TestSkips", "--- PASS: TestReads"} {
			if strings.Count(wrapped, want) != 1 {
				t.Errorf("FIXTURE_FAIL=%s: %q appears %d times under the wrapper, want 1", fail, want, strings.Count(wrapped, want))
			}
		}
	}

	got := map[string][]*Manifest{}
	ents, err := os.ReadDir(manifests)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		m, err := readManifest(filepath.Join(manifests, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		got[m.Package] = append(got[m.Package], m)
	}
	if len(got["fixture/reads"]) != 2 || len(got["fixture/quiet"]) != 2 {
		t.Fatalf("manifests per package %v, want two runs each of fixture/reads and fixture/quiet", keys(got))
	}
	checkReads(t, got["fixture/reads"][0], outside)
	q := got["fixture/quiet"][0]
	if q.Testlog != "complete" || q.Exit+got["fixture/quiet"][1].Exit != 1 {
		t.Errorf("quiet: testlog %q, exits %d and %d, want complete and one red run", q.Testlog, q.Exit, got["fixture/quiet"][1].Exit)
	}
	if runtime.GOOS == "windows" && q.Children != 0 {
		t.Errorf("quiet started no process but its manifest counts %d children (job: %s)", q.Children, q.Job)
	}
}

func checkReads(t *testing.T, m *Manifest, outside string) {
	t.Helper()
	if m.Testlog != "complete" || m.Exit != 0 || len(m.BinarySHA256) != 64 || m.TestlogArg == "" {
		t.Errorf("reads: testlog %q exit %d binary hash %q testlog arg %q", m.Testlog, m.Exit, m.BinarySHA256, m.TestlogArg)
	}
	for _, a := range m.Argv {
		if strings.HasPrefix(a, "-test.testlogfile") {
			t.Errorf("argv keeps the wrapper's own flag: %v", m.Argv)
		}
	}
	envs := map[string]string{}
	for _, e := range m.Env {
		envs[e.Name] = e.Value
	}
	if envs["FIXTURE_VAR"] != envValueOf("set") || envs["GODEBUG"] == "" {
		t.Errorf("env %v, want FIXTURE_VAR hashed and GODEBUG listed", m.Env)
	}
	scopes := map[string]PathRead{}
	for _, p := range m.Paths {
		scopes[p.Path] = p
	}
	in := scopes[m.Cwd+"/data.txt"]
	if in.Scope != "module" || !strings.HasPrefix(in.Hash, "file:") {
		t.Errorf("data.txt read as %+v, want a module file hash (paths %+v)", in, m.Paths)
	}
	out := scopes[filepath.ToSlash(outside)]
	if out.Scope != "outside" || !strings.HasPrefix(out.Hash, "file:") {
		t.Errorf("%s read as %+v, want an outside file hash", outside, out)
	}
	if cwd := scopes[m.Cwd]; !strings.HasPrefix(cwd.Hash, "dir:") {
		t.Errorf("the package directory's listing read as %+v, want a dir hash", cwd)
	}
	wantChildren := -1
	if runtime.GOOS == "windows" {
		wantChildren = 1
	}
	if m.Children != wantChildren || (runtime.GOOS == "windows" && (m.CPUS <= 0 || m.Job != "ok")) {
		t.Errorf("reads: children %d cpu %.3f job %q, want %d children", m.Children, m.CPUS, m.Job, wantChildren)
	}
}

func envValueOf(v string) string {
	sum := sha256.Sum256([]byte(v))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func keys(m map[string][]*Manifest) []string {
	var out []string
	for k, v := range m {
		out = append(out, k+":"+strings.Repeat("x", len(v)))
	}
	sort.Strings(out)
	return out
}
