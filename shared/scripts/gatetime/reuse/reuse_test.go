package reuse

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestChildEnv_StripsTheSwitchesAndPointsTempAtThePrivateDir(t *testing.T) {
	got := childEnv([]string{"GATE_TIER=push", "GATETIME_REUSE=reuse", "GATE_REUSE=on", "GATE_REUSE_LOG=x",
		"TMPDIR=/tmp", "KEEP=1", "SHINGO_TEST_PG=127.0.0.1:5"}, "/private")
	want := []string{"KEEP=1", "SHINGO_TEST_PG=127.0.0.1:5", "TMPDIR=/private"}
	if runtime.GOOS == "windows" {
		want = append(want, "TMP=/private", "TEMP=/private")
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("childEnv = %q, want %q", got, want)
	}
}

func TestKeyEnv_MasksRunScopedValuesAndKeepsTheirNames(t *testing.T) {
	a := keyEnv([]string{"SHINGO_TEST_PG=127.0.0.1:5", "SHINGO_TEST_PG_TEMPLATE=template_gate_1", "GATE_LOCK_HELD=t1", "B=2", "A=1"})
	b := keyEnv([]string{"SHINGO_TEST_PG=127.0.0.1:9", "SHINGO_TEST_PG_TEMPLATE=template_gate_2", "GATE_LOCK_HELD=t2", "B=2", "A=1"})
	if strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Fatalf("two gate runs key differently:\n%q\n%q", a, b)
	}
	if a[0] != "A=1" || !strings.HasPrefix(a[len(a)-1], "SHINGO_TEST_PG_TEMPLATE=<") {
		t.Fatalf("keyEnv not sorted or not masked: %q", a)
	}
	if c := keyEnv([]string{"A=2", "B=2"}); strings.Join(c, " ") == strings.Join(keyEnv([]string{"A=1", "B=2"}), " ") {
		t.Fatal("an ordinary variable's value is not in the key")
	}
}

func TestKeyArgvAndFiltered(t *testing.T) {
	argv := []string{"-test.testlogfile=/tmp/x", "-test.paniconexit0", "-test.timeout=10m0s", "--test.testlogfile=/y"}
	if got := keyArgv(argv); strings.Join(got, " ") != "-test.paniconexit0 -test.timeout=10m0s" {
		t.Fatalf("keyArgv = %q", got)
	}
	for _, a := range [][]string{{"-test.run=TestA"}, {"--test.skip=X"}, {"-test.run", "TestA"}, {"-test.list=."}, {"-test.bench=."}} {
		if filtered(a) == "" {
			t.Errorf("%q is not seen as a filtered run", a)
		}
	}
	for _, a := range [][]string{{"-test.run="}, {"-test.v=true", "-test.count=1"}, {}} {
		if f := filtered(a); f != "" {
			t.Errorf("%q is seen as filtered: %s", a, f)
		}
	}
}

func testEntry(t *testing.T) *Entry {
	t.Helper()
	k := Key{Binary: "b", Argv: []string{"-test.v"}, Cwd: "/c", Env: []string{"A=1"}, Identity: "i", Wrapper: "w"}
	return &Entry{Version: entryVersion, Key: k, KeyID: k.ID(), Output: []byte("PASS\n"),
		Paths: []PathRec{{Path: "/x", Op: OpOpen, State: "missing"}}}
}

func TestStore_RoundTripAndEveryTornFormIsRefused(t *testing.T) {
	store := t.TempDir()
	e := testEntry(t)
	f, err := writeEntry(store, e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := loadEntry(f)
	if err != nil || string(got.Output) != "PASS\n" || got.Paths[0].Path != "/x" {
		t.Fatalf("round trip: %v %+v", err, got)
	}
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string][]byte{
		"truncated":     b[:len(b)/2],
		"output flip":   []byte(strings.Replace(string(b), `"output":"UEFTUwo="`, `"output":"UEFTVwo="`, 1)),
		"path changed":  []byte(strings.Replace(string(b), `"path":"/x"`, `"path":"/y"`, 1)),
		"key changed":   []byte(strings.Replace(string(b), `"identity_sha256":"i"`, `"identity_sha256":"j"`, 1)),
		"empty":         {},
		"other version": []byte(strings.Replace(string(b), `"version":1`, `"version":2`, 1)),
	} {
		if string(bad) == string(b) {
			t.Fatalf("%s: the corruption did not apply", name)
		}
		p := filepath.Join(t.TempDir(), "e.json")
		if err := os.WriteFile(p, bad, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadEntry(p); err == nil {
			t.Errorf("%s: a torn entry verified", name)
		}
	}
	dot := filepath.Join(filepath.Dir(f), ".zzz.json.tmp")
	if err := os.WriteFile(dot, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if files := entryFiles(store, e.KeyID); len(files) != 1 || files[0] != f {
		t.Fatalf("entryFiles = %q, want only %s (a dot file is a write in progress)", files, f)
	}
}

func TestStore_KeepsTheNewestEntriesOfAKey(t *testing.T) {
	store := t.TempDir()
	var last string
	for i := 0; i < keepPerKey+3; i++ {
		e := testEntry(t)
		e.Output = []byte{byte('a' + i)}
		f, err := writeEntry(store, e)
		if err != nil {
			t.Fatal(err)
		}
		last = f
	}
	files := entryFiles(store, testEntry(t).KeyID)
	if len(files) != keepPerKey || files[0] != last {
		t.Fatalf("kept %d (newest %s), want %d with %s first", len(files), files[0], keepPerKey, last)
	}
}

func TestStateOf_ByContentNeverByTime(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("aaaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	s1, err := stateOf(p, OpOpen)
	if err != nil || !strings.HasPrefix(s1, "file:") {
		t.Fatalf("stateOf = %q, %v", s1, err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if s, err := stateOf(p, OpOpen); err != nil || s != s1 {
		t.Fatal("a time change moved the state")
	}
	if err := os.WriteFile(p, []byte("bbbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(p, old, old) // touch -r: same size, time put back
	if s, err := stateOf(p, OpOpen); err != nil || s == s1 {
		t.Fatal("a same-size edit with its time put back kept the state")
	}
	if s, err := stateOf(filepath.Join(dir, "none"), OpStat); err != nil || s != "missing" {
		t.Fatalf("missing = %q", s)
	}
	l1, err := stateOf(dir, OpOpen)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := stateOf(dir, OpStat); err != nil || s != "dir" {
		t.Fatalf("a statted dir = %q, want dir", s)
	}
	if err := os.WriteFile(filepath.Join(dir, "g"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if l2, err := stateOf(dir, OpOpen); err != nil || l2 == l1 {
		t.Fatal("a new entry did not move a listed directory's state")
	}
}

func TestChangedSince(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !changedSince(p, OpOpen, time.Now().Add(-time.Hour)) {
		t.Error("a file written after the start is not changed")
	}
	if changedSince(p, OpOpen, time.Now().Add(time.Hour)) {
		t.Error("a file written an hour before the start is changed")
	}
	if !changedSince(p, OpOpen, time.Now().Add(time.Second)) {
		t.Error("a file written inside the 2 s slack is not changed")
	}
}

func TestParseTestlog(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "log")
	abs := filepath.Join(dir, "abs")
	sub := filepath.Join(dir, "sub")
	body := "# test log\ngetenv HOME\nopen rel.txt\nstat " + abs + "\nchdir " + sub + "\nopen ../up.txt\nstat rel.txt\nopen rel.txt\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := parseTestlog(p, dir)
	if tl.state != "complete" || strings.Join(tl.env, ",") != "HOME" {
		t.Fatalf("state %q env %q", tl.state, tl.env)
	}
	want := map[string]string{filepath.Join(dir, "rel.txt"): OpOpen, abs: OpStat, sub: OpStat,
		filepath.Join(dir, "up.txt"): OpOpen, filepath.Join(sub, "rel.txt"): OpOpen}
	for k, v := range want {
		if tl.paths[k] != v {
			t.Errorf("%s: %q, want %q (all: %v)", k, tl.paths[k], v, tl.paths)
		}
	}
	for state, b := range map[string]string{"truncated": "# test log\nopen a", "malformed": "nope\n", "empty": ""} {
		if err := os.WriteFile(p, []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := parseTestlog(p, dir).state; got != state {
			t.Errorf("%q parses as %s, want %s", b, got, state)
		}
	}
	if got := parseTestlog(filepath.Join(dir, "none"), dir).state; got != "absent" {
		t.Errorf("no file parses as %s", got)
	}
}

// TestExclusions_TheNullDeviceIsNoInputInAnyDirectory pins the null device as
// machine state, never an input. exec.Command opens os.DevNull through package
// os for a nil Stdin or Stdout, so the test log of every binary that starts a
// child names it: "/dev/null" on Linux, and on Windows "NUL", which the log
// parser resolves against the cwd (<cwd>\NUL). Win32 resolves a final element
// NUL in any directory to the device, so <cwd>\NUL is excluded exactly as
// /dev/null is. Before this, 13 Windows probes refused reuse on it
// (gate-fast/lanes/reuse/raw/probes-windows-1.log, "NUL: unhashable").
func TestExclusions_TheNullDeviceIsNoInputInAnyDirectory(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	body := "# test log\nopen " + os.DevNull + "\nstat " + os.DevNull + "\nopen kept.txt\n"
	if err := os.WriteFile(log, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	set := pathSet{}
	for p, op := range parseTestlog(log, dir).paths {
		set.add(p, op, false)
	}
	recs := set.recs(newExclusions("", ""))
	if len(recs) != 1 || recs[0].Path != filepath.Join(dir, "kept.txt") {
		t.Fatalf("records %+v, want only %s: the null device (%s, resolved against %s) is not an input",
			recs, filepath.Join(dir, "kept.txt"), os.DevNull, dir)
	}
	if runtime.GOOS != "windows" {
		return
	}
	x := newExclusions("", "")
	for _, p := range []string{`C:\a\NUL`, `C:\a\b\nul`, `D:\Nul`, filepath.Join(dir, "sub", "nUL")} {
		if !x.has(p) {
			t.Errorf("%s is the null device (Win32 resolves a final NUL in any directory) and is not excluded", p)
		}
	}
	// Narrower than Win32 on purpose: a name left out is stated, found to be
	// a device and refused (never stored), which is safe; a real file left
	// out would be a false green. `\\?\` paths skip Win32's device names, and
	// Windows 11 lets NUL.txt be a file.
	for _, p := range []string{`C:\a\NUL.txt`, `C:\a\NULL`, `C:\a\xNUL`, `C:\NUL\a.txt`, `\\?\C:\a\NUL`} {
		if x.has(p) {
			t.Errorf("%s is excluded, but it can be a real file", p)
		}
	}
}

func TestHasTestMain(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a_test.go", "package a\n\nfunc TestA(t *testing.T) {}\n")
	write("main.go", "package a\n\nfunc TestMain() {}\n") // not a _test.go file
	if has, why, files := hasTestMain(dir); has || len(files) != 1 {
		t.Fatalf("has %v (%s) from %q", has, why, files)
	}
	write("b_test.go", "//go:build docker\n\npackage a_test\n\nfunc TestMain(m *testing.M) { m.Run() }\n")
	if has, _, _ := hasTestMain(dir); !has {
		t.Fatal("a TestMain behind a build tag is not seen (it must be: the check errs towards having one)")
	}
	if has, _, _ := hasTestMain(filepath.Join(dir, "none")); !has {
		t.Fatal("an unreadable package directory does not count as having a TestMain")
	}
}

func TestReadConfig_GitHubNeverServes(t *testing.T) {
	t.Setenv("GATETIME_REUSE", ModeReuse)
	t.Setenv("GITHUB_ACTIONS", "true")
	if c := readConfig(); c.mode != ModeRecord {
		t.Fatalf("mode %q under GITHUB_ACTIONS=true, want record", c.mode)
	}
	t.Setenv("GATETIME_REUSE", "on")
	t.Setenv("GITHUB_ACTIONS", "")
	if c := readConfig(); c.mode != ModeRecord || c.note == "" {
		t.Fatalf("an unknown mode is %q (%q), want record with a note", c.mode, c.note)
	}
}

// TestNoWorkflowSwitchesReuseOn pins "GitHub never reuses" (R15, SYNTH-round2
// section 1 row 4) where a workflow would break it: by naming a reuse switch.
func TestNoWorkflowSwitchesReuseOn(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "..", ".github", "workflows")
	files, err := filepath.Glob(filepath.Join(dir, "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows under %s (%v): this pin would pass on nothing", dir, err)
	}
	re := regexp.MustCompile(`GATE_REUSE|GATETIME_REUSE|GATETIME_STORE`)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if m := re.Find(b); m != nil {
			t.Errorf("%s names %s: GitHub never reuses; take it out", f, m)
		}
	}
}
