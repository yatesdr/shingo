package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"shingo/shared/scripts/internal/testset"
)

func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// framed is a test binary's -test.v=test2json output: each result line behind
// the testing package's ^V.
func framed(ls ...string) []byte {
	var b strings.Builder
	for _, l := range ls {
		if strings.HasPrefix(l, "--- ") || strings.HasPrefix(l, "=== ") {
			b.WriteString("\x16")
		}
		b.WriteString(l + "\n")
	}
	return []byte(b.String())
}

func TestParseRun_TheLargerOfTheWallAndTheLeavesSummed(t *testing.T) {
	t.Parallel()
	out := framed(
		"=== RUN   TestA",
		"--- PASS: TestFake (99.00s)", // a test printing a look-alike: no ^V, not read
		"=== RUN   TestA/x",
		"--- PASS: TestA/x/y (4.00s)",
		"--- PASS: TestA/x (4.10s)",
		"--- PASS: TestA/z/w (3.00s)", // one subtest named "z/w": a leaf
		"--- PASS: TestAB (50.00s)",   // another test, its name this one's and more
		"--- PASS: TestA (5.00s)",
		"PASS")
	got, err := parseRun(out, "TestA")
	if err != nil {
		t.Fatal(err)
	}
	want := run{cost: secs(7), wall: secs(5), leaves: secs(7), nLeaves: 2}
	if got != want {
		t.Errorf("parseRun = %+v, want %+v (leaves x/y and z/w, summed over the wall)", got, want)
	}

	// The parent's own seconds before its subtests start show in its wall.
	got, err = parseRun(framed("--- PASS: TestB/one (1.00s)", "--- PASS: TestB (12.00s)"), "TestB")
	if err != nil || got.cost != secs(12) {
		t.Errorf("parseRun = %+v, %v; want the 12 s wall over the 1 s leaf", got, err)
	}
	// No subtests: the test is its one leaf.
	got, err = parseRun(framed("--- PASS: TestC (3.50s)"), "TestC")
	if err != nil || got != (run{cost: secs(3.5), wall: secs(3.5), leaves: secs(3.5), nLeaves: 1}) {
		t.Errorf("parseRun = %+v, %v; want one 3.5 s leaf", got, err)
	}
	// CRLF output reads the same.
	if got, err := parseRun(bytes.ReplaceAll(framed("--- PASS: TestC (3.50s)"), []byte("\n"), []byte("\r\n")), "TestC"); err != nil || got.cost != secs(3.5) {
		t.Errorf("parseRun over CRLF = %+v, %v", got, err)
	}
}

func TestParseRun_ARunThatDidNotPassHasNoCost(t *testing.T) {
	t.Parallel()
	if _, err := parseRun(framed("--- FAIL: TestA (20.00s)"), "TestA"); err == nil || errors.Is(err, errSkipped) {
		t.Errorf("a failed run: err = %v, want a failure", err)
	}
	if _, err := parseRun(framed("--- SKIP: TestA (0.00s)"), "TestA"); !errors.Is(err, errSkipped) {
		t.Errorf("a skipped run: err = %v, want errSkipped", err)
	}
	if _, err := parseRun([]byte("--- PASS: TestA (1.00s)\nPASS\n"), "TestA"); err == nil {
		t.Errorf("an unframed result line was read as one")
	}
	if _, err := parseRun(framed("testing: warning: no tests to run", "PASS"), "TestA"); err == nil {
		t.Errorf("a run that ran nothing has a cost")
	}
}

func TestTagsFor_TheFirstTagSetTheFileBuildsUnder(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	files := map[string]string{
		"p/plain_test.go":    "package p\n",
		"p/docker_test.go":   "//go:build docker\n\npackage p\n",
		"p/sim_test.go":      "//go:build sim\n\npackage p\n",
		"p/shots_test.go":    "//go:build shots\n\npackage p\n",
		"p/nolock_test.go":   "//go:build !lockcheck\n\npackage p\n",
		"p/os_linux_test.go": "package p\n",
	}
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	linux := []string(nil)
	if runtime.GOOS == "linux" {
		linux = []string{"lockcheck"}
	}
	for file, want := range map[string][]string{
		"p/plain_test.go":    {"lockcheck"},
		"p/docker_test.go":   {"docker", "lockcheck"},
		"p/sim_test.go":      {"sim", "lockcheck"},
		"p/shots_test.go":    nil,
		"p/nolock_test.go":   nil,
		"p/os_linux_test.go": linux,
	} {
		if got := tagsFor(root, file); !reflect.DeepEqual(got, want) {
			t.Errorf("tagsFor(%s) = %v, want %v", file, got, want)
		}
	}
}

// fakeRuns is a check's world: each test's run costs in order (a negative one
// a skip), the tests whose runs fail, and whether the host goes quiet.
type fakeRuns struct {
	costs map[string][]float64
	fail  map[string]bool
	quiet bool
	ran   []string
	waits int
}

func (f *fakeRuns) config(out *bytes.Buffer) *config {
	return &config{line: 10 * time.Second, quietCores: 3, quietWait: time.Minute, out: out, detail: out,
		runTest: func(t *test, _ string) (run, error) {
			f.ran = append(f.ran, t.Name)
			if f.fail[t.Name] {
				return run{}, errors.New("it failed")
			}
			cs := f.costs[t.Name]
			if len(cs) == 0 {
				return run{}, errors.New("ran more often than the fake says")
			}
			c := cs[0]
			f.costs[t.Name] = cs[1:]
			if c < 0 {
				return run{}, errSkipped
			}
			return run{cost: secs(c), wall: secs(c), leaves: secs(c), nLeaves: 1}, nil
		},
		waitQuiet: func() (bool, float64) {
			f.waits++
			if f.quiet {
				return true, 1.2
			}
			return false, 9.5
		}}
}

func newTests(names ...string) []*test {
	var ts []*test
	for i, n := range names {
		ts = append(ts, &test{Test: testset.Test{Dir: "app/eng", Name: n, File: "app/eng/a_test.go", Line: i + 1}, tags: []string{"lockcheck"}})
	}
	return ts
}

func allBuilt(ts []*test) (map[*test]string, error) {
	m := map[*test]string{}
	for _, t := range ts {
		m[t] = "bin"
	}
	return m, nil
}

func TestCheck_ARedTakesTwoRunsOverTheLineTheSecondOnAQuietHost(t *testing.T) {
	t.Parallel()
	ts := newTests("TestMarked", "TestByTag", "TestCheap", "TestSlow", "TestLoadOnly", "TestSkips", "TestFails")
	ts[0].reason = "it walks a plant"
	ts[1].tags, ts[1].why = nil, "not timed: its file builds under none"
	f := &fakeRuns{quiet: true, fail: map[string]bool{"TestFails": true}, costs: map[string][]float64{
		"TestCheap": {2}, "TestSlow": {14, 11}, "TestLoadOnly": {12, 4}, "TestSkips": {-1}}}
	var out bytes.Buffer
	ok, err := f.config(&out).check(ts, "6bd55616c786", allBuilt)
	if err != nil || ok {
		t.Fatalf("check = %v, %v; want a red\n%s", ok, err, out.String())
	}
	if want := []string{"TestCheap", "TestSlow", "TestLoadOnly", "TestSkips", "TestFails", "TestSlow", "TestLoadOnly"}; !reflect.DeepEqual(f.ran, want) {
		t.Errorf("runs = %v, want %v: every first run, then each suspect once", f.ran, want)
	}
	got := out.String()
	for _, want := range []string{
		"FAIL test-cost  app/eng/a_test.go:4  TestSlow costs over 10s run alone and does not say why: 14.0 s",
		"then 11.0 s (wall 11.0 s, 1 leaves 11.0 s), host quiet at 1.2 cores",
		"NOTE test-cost  app/eng/a_test.go:7  TestFails not judged: it failed",
		"`" + testset.CostMarker + " <what needs the time",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"TestMarked", "TestByTag", "TestCheap", "TestLoadOnly", "TestSkips"} {
		if strings.Contains(got, not) {
			t.Errorf("output names %s, which is not red and was judged:\n%s", not, got)
		}
	}
}

func TestCheck_ALoadedHostIsNeverARed(t *testing.T) {
	t.Parallel()
	ts := newTests("TestSlow", "TestCheap", "TestSlower")
	f := &fakeRuns{quiet: false, costs: map[string][]float64{"TestSlow": {40}, "TestCheap": {1}, "TestSlower": {60}}}
	var out bytes.Buffer
	ok, err := f.config(&out).check(ts, "6bd55616c786", allBuilt)
	if err != nil || !ok {
		t.Fatalf("check = %v, %v; want clean\n%s", ok, err, out.String())
	}
	want := "NOTE test-cost  app/eng/a_test.go:1  TestSlow not judged: 40.0 s (wall 40.0 s, 1 leaves 40.0 s) beside other load, " +
		"and the host was still 9.5 CPUs busy after 1m0s (the line is 3.0)\n" +
		"NOTE test-cost  app/eng/a_test.go:3  TestSlower not judged: 60.0 s (wall 60.0 s, 1 leaves 60.0 s) beside other load, " +
		"and the host was still 9.5 CPUs busy after 1m0s (the line is 3.0)\n" +
		"ok   test-cost (3 new since 6bd55616: 0 say why they cost, 1 under 10s alone, 0 skip alone, 0 not timed by tag, 2 not judged)\n"
	if out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
	}
	if !reflect.DeepEqual(f.ran, []string{"TestSlow", "TestCheap", "TestSlower"}) {
		t.Errorf("runs = %v: a suspect on a loaded host is not run again", f.ran)
	}
	if f.waits != 1 {
		t.Errorf("the host was waited for %d times, want once: a host that did not go quiet is not waited for again", f.waits)
	}
}

func TestCheck_TheOkLineCountsEachKind(t *testing.T) {
	t.Parallel()
	ts := newTests("TestMarked", "TestByTag", "TestCheap", "TestLoadOnly", "TestSkips", "TestUnbuilt")
	ts[0].reason = "it walks a plant"
	ts[1].tags, ts[1].why = nil, "not timed: its file builds under none"
	f := &fakeRuns{quiet: true, costs: map[string][]float64{"TestCheap": {2}, "TestLoadOnly": {12, 4}, "TestSkips": {-1}}}
	var out bytes.Buffer
	ok, err := f.config(&out).check(ts, "6bd55616c786", func(ts []*test) (map[*test]string, error) {
		m, err := allBuilt(ts)
		if err != nil {
			return nil, err
		}
		for t := range m {
			if t.Name == "TestUnbuilt" {
				m[t], t.why = "", "not judged: its package does not build with -tags lockcheck"
			}
		}
		return m, nil
	})
	if err != nil || !ok {
		t.Fatalf("check = %v, %v; want clean\n%s", ok, err, out.String())
	}
	want := "NOTE test-cost  app/eng/a_test.go:6  TestUnbuilt not judged: its package does not build with -tags lockcheck\n" +
		"ok   test-cost (6 new since 6bd55616: 1 say why they cost, 2 under 10s alone, 1 skip alone, 1 not timed by tag, 1 not judged)\n"
	if out.String() != want {
		t.Errorf("output:\n%s\nwant:\n%s", out.String(), want)
	}
}

// TestMain lets this test binary stand in for docker: with fakeDockerEnv
// naming a state directory it answers as fakeDocker does, so
// TestServerStart_PublishesOnAPortOfItsOwn can put it on PATH as docker.
func TestMain(m *testing.M) {
	if dir := os.Getenv(fakeDockerEnv); dir != "" {
		os.Exit(fakeDocker(dir, os.Args[1:]))
	}
	os.Exit(m.Run())
}

const fakeDockerEnv = "TESTCOST_FAKE_DOCKER"

// fakeDocker is a docker that only records, for one server start: each
// call's verb goes to dir/calls and each run's -p value to dir/ports. A run
// creates the container (dir/container) unless dir/nocreate exists (docker
// down, no image), and the first runs that dir/taken counts fail as docker
// fails on a taken port: the start fails and the container is left, created.
// `container inspect` answers whether it exists, rm removes it, port answers a
// port of docker's own choosing, and exec (pg_isready) answers ready. 4 is
// the fake's own failure.
func fakeDocker(dir string, args []string) int {
	if len(args) == 0 || appendLine(filepath.Join(dir, "calls"), args[0]) != nil {
		return 4
	}
	container := filepath.Join(dir, "container")
	switch args[0] {
	case "run":
		p := ""
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-p" {
				p = args[i+1]
			}
		}
		if appendLine(filepath.Join(dir, "ports"), p) != nil {
			return 4
		}
		runs := len(readLines(filepath.Join(dir, "ports")))
		taken, err := strconv.Atoi(strings.Join(readLines(filepath.Join(dir, "taken")), ""))
		switch {
		case err != nil:
			return 4
		case runs > 50:
			return 3 // a start that never stops asking
		case exists(filepath.Join(dir, "nocreate")):
			return 125
		}
		if os.WriteFile(container, nil, 0o644) != nil {
			return 4
		}
		if runs <= taken {
			return 125
		}
		fmt.Println("c0ffee")
	case "rm":
		if err := os.Remove(container); err != nil && !os.IsNotExist(err) {
			return 4
		}
	case "container":
		if !exists(container) {
			return 1
		}
	case "port":
		fmt.Println("127.0.0.1:54321")
	}
	return 0
}

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, line); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// readLines is the file's lines, none when it is missing.
func readLines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Fields(string(data))
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

var publishRe = regexp.MustCompile(`^127\.0\.0\.1:([0-9]+):5432$`)

// pickedPort is the host port of a -p value, when it is 127.0.0.1:<port>:5432
// with a port in 61000-65535.
func pickedPort(p string) (int, bool) {
	m := publishRe.FindStringSubmatch(p)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil && n >= 61000 && n <= 65535
}

// TestServerStart_PublishesOnAPortOfItsOwn pins the host port test-cost's own
// Postgres is published on (docket G23), as scripts/check-gate-exec.sh pins
// pg_up's. start, against a docker that only records (this binary on PATH),
// publishes the server on 127.0.0.1:<port>:5432 with a port it picks in
// 61000-65535, above the ports Linux hands a client, and its address is that
// pick; a pick docker cannot start the server on moves to another, the
// container the failed start left removed first; a docker that creates no
// container is asked once; with every pick taken it gives up and leaves no
// container. The root holds no module, so start's last step, the template
// warm-up, fails at once: what is pinned is the docker conversation before it.
func TestServerStart_PublishesOnAPortOfItsOwn(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"+exeSuffix()), exe, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	root := t.TempDir()

	for _, c := range []struct {
		name     string
		taken    int  // the first runs that find their port taken
		nocreate bool // no run creates a container
		started  bool // the server starts: start's error, if any, is the warm-up's
		runs     int  // docker runs; 0 is more than one
		runRm    string
	}{
		{name: "free", started: true, runs: 1, runRm: "run"},
		{name: "two picks taken", taken: 2, started: true, runs: 3, runRm: "run rm run rm run"},
		{name: "docker creates nothing", nocreate: true, runs: 1, runRm: "run"},
		{name: "every pick taken", taken: 1000},
	} {
		t.Run(c.name, func(t *testing.T) {
			state := t.TempDir()
			if err := os.WriteFile(filepath.Join(state, "taken"), []byte(strconv.Itoa(c.taken)), 0o644); err != nil {
				t.Fatal(err)
			}
			if c.nocreate {
				if err := os.WriteFile(filepath.Join(state, "nocreate"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv(fakeDockerEnv, state)
			s := &server{}
			err := s.start(root, t.TempDir())
			calls, ports := readLines(filepath.Join(state, "calls")), readLines(filepath.Join(state, "ports"))
			left := exists(filepath.Join(state, "container"))
			s.stop()

			seen := map[string]bool{}
			for _, p := range ports {
				if _, ok := pickedPort(p); !ok {
					t.Errorf("start published the server with -p %q: want -p 127.0.0.1:<port>:5432 with a port it picks in 61000-65535, where Linux hands no client port (a port left to docker is one Docker Desktop's listen inside WSL can lose to a client socket, and then every dial from WSL is refused: docket G23, pg_up in scripts/gate.sh)", p)
				}
				if seen[p] {
					t.Errorf("start tried one port twice: %v", ports)
				}
				seen[p] = true
			}
			want := "more than one"
			if c.runs > 0 {
				want = strconv.Itoa(c.runs)
			}
			if (c.runs > 0 && len(ports) != c.runs) || (c.runs == 0 && len(ports) < 2) {
				t.Errorf("start ran docker run %d times (%v), want %s: a start that fails on a taken port must move to another port, and one that created no container is not repeated", len(ports), ports, want)
			}
			var runRm []string
			for _, v := range calls {
				if v == "run" || v == "rm" {
					runRm = append(runRm, v)
				}
			}
			if c.runRm != "" && strings.Join(runRm, " ") != c.runRm {
				t.Errorf("start ran docker %v: want its runs and removals to be %q, the container each failed start left removed before the next run (its name is the next run's)", calls, c.runRm)
			}
			if c.started {
				if err != nil && !strings.Contains(err.Error(), "template warm-up") {
					t.Errorf("start: %v; want the server started, only the warm-up failing (the root holds no module)", err)
				}
				last := ""
				if len(ports) > 0 {
					last = ports[len(ports)-1]
				}
				if n, ok := pickedPort(last); !ok || s.addr != "127.0.0.1:"+strconv.Itoa(n) {
					t.Errorf("start's address is %q: want 127.0.0.1 and the port of the run that started the server (-p %q), not docker port's answer", s.addr, last)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "docker run") {
					t.Errorf("start: %v; want docker run's error", err)
				}
				if s.addr != "" || left {
					t.Errorf("start failed with address %q, its container left: %v; want no address, and each container a failed start created removed", s.addr, left)
				}
			}
			if exists(filepath.Join(state, "container")) {
				t.Errorf("stop left the server's container behind")
			}
		})
	}
}
