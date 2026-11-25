// Command testcost is the time half of the 10 s rule for a new test
// (docs/ground-rules.md): a top-level test the base does not have that costs
// more than 10 s run alone says why, with `// cost: <reason>` in its doc
// comment. The repolint's test-costs rule checks where a marker stands; this
// times the new tests that carry none, and fails one over the line.
//
// WHAT IS NEW. testset.Added: a top-level test at the tip (the working tree,
// uncommitted and untracked files included) that the base does not declare
// in its package directory, build tags ignored; the base is the test ledger's
// (-base, TEST_LEDGER_BASE, else `git merge-base HEAD origin/main`). A test
// that existed at the base owes nothing until it is rewritten, and this cannot
// see a rewrite: the reviewer does.
//
// WHAT IT COSTS. The test run alone: its package's test binary with
// `-test.run '^Name$'`, nothing else of this tool running, against a server
// whose Core template is already built (as every docker package but the
// first meets the gate's), any other database on it dropped first. The cost
// is the larger of that binary's wall and the test's leaves' seconds summed (a
// test with no subtests is its one leaf): the leaves are what the T10 price
// was set on, and a test whose seconds sit in the parent before its subtests
// start, or in its package's TestMain, has a wall over its leaves. A test is run with the tags its file needs
// (lockcheck, as the gate runs every test; docker or sim where the file's
// constraint asks); a file only another tag builds is not timed, and says so.
//
// ALONE, AND A LOADED HOST IS NEVER A RED. A test's time inside a gate is 4-8x
// its time alone (the gate-time baseline, 2026-11-22), and load only adds. So
// a first run under the line passes whatever else the host was doing. A first
// run over it is a suspect, re-run once more after the host has been quiet
// (all its CPUs together under -quiet-cores busy over 3 s, read from the OS:
// GetSystemTimes, /proc/stat) for at most -quiet-wait: the red takes two runs
// over the line, the second started on a quiet host. A host that never goes
// quiet leaves the test NOT JUDGED: printed, and no red. Neither can a test
// that failed, skipped or did not build here be judged: the gate's own test
// steps say why it failed.
//
// WHERE IT RUNS. scripts/check-test-cost.sh, tier `alone`: only when the
// scripts step runs by itself (`gate.sh scripts`, CI's guards job), never
// beside a gate's own build and test steps, which are the load.
//
// Exit 0 = no new test over the line without a reason (some may be NOT
// JUDGED), 1 = one is, 2 = the tool could not run (no base, no tree).
//
// Usage: go run ./scripts/testcost [-root DIR] [-base REF] [-line 10s]
// [-quiet-cores N] [-quiet-wait 5m] [-v] [-list]
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"go/build"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"shingo/shared/scripts/internal/testset"
)

// The tag sets a test is run with, tried in order: the first under which its
// file builds. lockcheck is in every one because the gate runs every test with
// it (scripts/gate.sh: step_test, step_docker, step_sim).
var tagSets = [][]string{{"lockcheck"}, {"docker", "lockcheck"}, {"sim", "lockcheck"}}

// The Core template's warm-up: a docker test that opens a database cloned from
// the migrated template, so the template is built before the first test this
// tool times (the gate-time baseline's alone runs warmed it the same way).
const (
	warmDir  = "shingo-core/internal/testdb"
	warmTest = "TestSweepProbe_RunsOnAMigratedDatabase"
)

// test is one new test and what became of it.
type test struct {
	testset.Test
	module, rel string // module dir and the package path inside it
	tags        []string
	reason      string // the doc comment's `// cost:` reason, "" when none
	why         string // why it was not timed or not judged, "" when it was
	runs        []run
	red         bool
}

// run is one alone run of a test.
type run struct {
	cost, wall, leaves time.Duration
	nLeaves            int
	quiet              bool    // started after the host was quiet
	busy               float64 // the host's busy cores just before it started
}

func (r run) String() string {
	s := fmt.Sprintf("%.1f s (wall %.1f s, %d leaves %.1f s)", r.cost.Seconds(), r.wall.Seconds(), r.nLeaves, r.leaves.Seconds())
	if r.quiet {
		return s + fmt.Sprintf(", host quiet at %.1f cores", r.busy)
	}
	return s
}

// config is what a check runs with.
type config struct {
	root        string
	line        time.Duration
	quietCores  float64
	quietWait   time.Duration
	verbose     bool
	runTest     func(t *test, bin string) (run, error) // one alone run
	waitQuiet   func() (bool, float64)                 // whether the host went quiet, and its busy cores
	out, detail io.Writer
}

func main() {
	root := flag.String("root", ".", "repo root holding the five modules")
	baseRef := flag.String("base", os.Getenv("TEST_LEDGER_BASE"), "base ref (default: TEST_LEDGER_BASE, else merge-base HEAD origin/main)")
	line := flag.Duration("line", 10*time.Second, "the cost over which a new test says why")
	qc := flag.Float64("quiet-cores", envFloat("TEST_COST_QUIET_CORES", 1+0.1*float64(runtime.NumCPU())),
		"a quiet host has fewer than this many CPUs busy (default TEST_COST_QUIET_CORES, else 1 + a tenth of them)")
	qw := flag.Duration("quiet-wait", envDuration("TEST_COST_QUIET_WAIT", 5*time.Minute),
		"how long a suspect waits for a quiet host (default TEST_COST_QUIET_WAIT, else 5m)")
	verbose := flag.Bool("v", false, "print every new test's runs")
	list := flag.Bool("list", false, "list the new tests, their tags and reasons, and time nothing")
	timeFile := flag.String("time", "", "time each test a `<dir> <test>` line of this file names, once, alone, and print its cost (no verdict): the reading test-costs.txt is kept with")
	flag.Parse()

	var (
		base  string
		added []testset.Test
		err   error
	)
	if *timeFile != "" {
		added, err = named(*root, *timeFile)
	} else if base, err = testset.ResolveBase(*root, *baseRef); err == nil {
		added, err = testset.Added(*root, base)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "testcost:", err)
		os.Exit(2)
	}
	tests, err := classify(*root, added)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testcost:", err)
		os.Exit(2)
	}
	if *list {
		for _, t := range tests {
			fmt.Printf("%s\t%s\t%s\t%s\t%s\n", t.Dir, t.Name, strings.Join(t.tags, ","), t.reason, t.why)
		}
		return
	}
	c := &config{root: *root, line: *line, quietCores: *qc, quietWait: *qw, verbose: *verbose,
		out: os.Stdout, detail: os.Stderr}
	c.waitQuiet = func() (bool, float64) { return waitQuiet(c.quietCores, c.quietWait) }
	srv := &server{}
	defer srv.stop()
	stopOnSignal(srv)
	c.runTest = func(t *test, bin string) (run, error) { return runAlone(c.root, t, bin, srv) }
	if *timeFile != "" {
		c.timeEach(tests, func(ts []*test) (map[*test]string, error) { return compile(*root, ts, srv) })
		return
	}
	t0 := time.Now()
	ok, err := c.check(tests, base, func(ts []*test) (map[*test]string, error) { return compile(*root, ts, srv) })
	if err != nil {
		fmt.Fprintln(os.Stderr, "testcost:", err)
		srv.stop()
		os.Exit(2)
	}
	if c.verbose {
		fmt.Fprintf(c.detail, "testcost: %.1f s\n", time.Since(t0).Seconds())
	}
	if !ok {
		srv.stop()
		os.Exit(1)
	}
}

// named reads a -time file: `<dir> <test>` lines, `#` comments and blank lines
// skipped, each naming a top-level test at the tip.
func named(root, file string) ([]testset.Test, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var out []testset.Test
	tips := map[string]map[string]testset.Test{}
	for n, l := range strings.Split(string(data), "\n") {
		f := strings.Fields(l)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		if len(f) < 2 {
			return nil, fmt.Errorf("%s:%d: not `<dir> <test>`", file, n+1)
		}
		if tips[f[0]] == nil {
			if tips[f[0]], err = testset.TipTests(root, f[0]); err != nil {
				return nil, err
			}
		}
		t, ok := tips[f[0]][f[1]]
		if !ok {
			return nil, fmt.Errorf("%s:%d: no top-level test %s in %s", file, n+1, f[1], f[0])
		}
		out = append(out, t)
	}
	return out, nil
}

// timeEach runs each test once, alone, and prints its reading: the dir, the
// test, its tags, then its cost, wall, leaves and leaf seconds, or why it has
// none.
func (c *config) timeEach(tests []*test, compile func([]*test) (map[*test]string, error)) {
	var timed []*test
	for _, t := range tests {
		if t.why == "" {
			timed = append(timed, t)
		}
	}
	bins, err := compile(timed)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testcost:", err)
		return
	}
	for _, t := range tests {
		cols := []string{t.Dir, t.Name, strings.Join(t.tags, ",")}
		if bin := bins[t]; bin != "" && t.why == "" {
			r, err := c.runTest(t, bin)
			if err == nil {
				cols = append(cols, fmt.Sprintf("%.1f", r.cost.Seconds()), fmt.Sprintf("%.1f", r.wall.Seconds()),
					strconv.Itoa(r.nLeaves), fmt.Sprintf("%.1f", r.leaves.Seconds()))
			} else {
				t.why = whyNot(err)
			}
		}
		if t.why != "" {
			cols = append(cols, t.why)
		}
		fmt.Fprintln(c.out, strings.Join(cols, "\t"))
	}
}

// classify reads each new test's file: its reason, if any, and the tags it
// is run with.
func classify(root string, added []testset.Test) ([]*test, error) {
	var out []*test
	reasons := map[string]map[string]string{} // file -> test -> reason
	for _, a := range added {
		t := &test{Test: a}
		t.module, t.rel, _ = strings.Cut(a.Dir, "/")
		if t.rel == "" {
			t.rel = "."
		}
		if _, seen := reasons[a.File]; !seen {
			src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(a.File)))
			if err != nil {
				return nil, err
			}
			ls, err := testset.CostLines(a.File, src)
			if err != nil {
				return nil, err
			}
			reasons[a.File] = map[string]string{}
			for _, l := range ls {
				if l.Test != "" && l.Reason != "" {
					reasons[a.File][l.Test] = l.Reason
				}
			}
		}
		t.reason = reasons[a.File][a.Name]
		t.tags = tagsFor(root, a.File)
		if t.tags == nil {
			t.why = "not timed: its file builds under none of " + tagSetsText() + " on " + runtime.GOOS + "/" + runtime.GOARCH
		}
		out = append(out, t)
	}
	return out, nil
}

func tagSetsText() string {
	var s []string
	for _, ts := range tagSets {
		s = append(s, strings.Join(ts, ","))
	}
	return "{" + strings.Join(s, "} {") + "}"
}

// tagsFor is the first tag set under which file builds on this OS, or nil.
func tagsFor(root, file string) []string {
	dir := filepath.Join(root, filepath.FromSlash(path.Dir(file)))
	for _, ts := range tagSets {
		ctx := build.Default
		ctx.BuildTags = ts
		if ok, err := ctx.MatchFile(dir, path.Base(file)); err == nil && ok {
			return ts
		}
	}
	return nil
}

// check times every new test that carries no reason and reports. compile
// builds one binary per package and tag set, keyed by test.
func (c *config) check(tests []*test, base string, compile func([]*test) (map[*test]string, error)) (bool, error) {
	var timed []*test
	marked, untimed := 0, 0
	for _, t := range tests {
		switch {
		case t.reason != "":
			marked++
		case t.why != "":
			untimed++
		default:
			timed = append(timed, t)
		}
	}
	bins, err := compile(timed)
	if err != nil {
		return false, err
	}
	// The first run of each, one after another, nothing else of this tool's
	// running beside it.
	var suspects []*test
	for _, t := range timed {
		bin := bins[t]
		if bin == "" {
			if t.why == "" {
				t.why = "not judged: no test binary"
			}
			continue
		}
		r, err := c.runTest(t, bin)
		if err != nil {
			t.why = whyNot(err)
			continue
		}
		t.runs = append(t.runs, r)
		if r.cost > c.line {
			suspects = append(suspects, t)
		}
	}
	// Each suspect again, once the host is quiet. A host that did not go quiet
	// in one wait is not waited for again: the run would last a wait a suspect.
	loaded, busy := false, 0.0
	for _, t := range suspects {
		quiet := false
		if !loaded {
			quiet, busy = c.waitQuiet()
			loaded = !quiet
		}
		if !quiet {
			t.why = fmt.Sprintf("not judged: %s beside other load, and the host was still %.1f CPUs busy after %s (the line is %.1f)",
				t.runs[0], busy, c.quietWait, c.quietCores)
			continue
		}
		r, err := c.runTest(t, bins[t])
		if err != nil {
			t.why = whyNot(err)
			continue
		}
		r.quiet, r.busy = true, busy
		t.runs = append(t.runs, r)
		t.red = r.cost > c.line
	}
	return c.report(tests, base, marked, untimed), nil
}

// report prints the guard's lines and says whether it was clean.
func (c *config) report(tests []*test, base string, marked, untimed int) bool {
	var red, unjudged []*test
	under, skipped := 0, 0
	for _, t := range tests {
		switch {
		case t.red:
			red = append(red, t)
		case t.reason != "":
		case strings.HasPrefix(t.why, "not judged"):
			unjudged = append(unjudged, t)
		case strings.HasPrefix(t.why, "skipped"):
			skipped++
		case t.why == "":
			under++
		}
		if c.verbose {
			var rs []string
			for _, r := range t.runs {
				rs = append(rs, r.String())
			}
			fmt.Fprintf(c.detail, "  %s %s [%s] reason=%q runs=%s %s\n", t.Dir, t.Name, strings.Join(t.tags, ","), t.reason,
				strings.Join(rs, "; "), t.why)
		}
	}
	short := base
	if len(short) > 8 {
		short = short[:8]
	}
	for _, t := range unjudged {
		fmt.Fprintf(c.out, "NOTE test-cost  %s:%d  %s %s\n", t.File, t.Line, t.Name, t.why)
	}
	if len(red) == 0 {
		fmt.Fprintf(c.out, "ok   test-cost (%d new since %s: %d say why they cost, %d under %s alone, %d skip alone, %d not timed by tag, %d not judged)\n",
			len(tests), short, marked, under, c.line, skipped, untimed, len(unjudged))
		return true
	}
	for _, t := range red {
		fmt.Fprintf(c.out, "FAIL test-cost  %s:%d  %s costs over %s run alone and does not say why: %s, then %s\n",
			t.File, t.Line, t.Name, c.line, t.runs[0], t.runs[1])
	}
	fmt.Fprintf(c.out, "     Make it cheaper, or say why in its doc comment: `%s <what needs the time, and what a cheaper tier cannot assert>`.\n", testset.CostMarker)
	return false
}

// ── compiling ─────────────────────────────────────────────────────────────

// compile builds one test binary per package directory and tag set, in a
// temporary directory, and starts the server when any of them is docker's. A
// package that does not build leaves its tests NOT JUDGED.
func compile(root string, tests []*test, srv *server) (map[*test]string, error) {
	tmp, err := os.MkdirTemp("", "testcost-")
	if err != nil {
		return nil, err
	}
	srv.cleanup = append(srv.cleanup, func() { _ = os.RemoveAll(tmp) })
	bins := map[*test]string{}
	built := map[string]string{} // group key -> binary, "" when it did not build
	docker := false
	for i, t := range tests {
		key := t.Dir + " " + strings.Join(t.tags, ",")
		bin, ok := built[key]
		if !ok {
			bin = filepath.Join(tmp, fmt.Sprintf("%d.test%s", i, exeSuffix()))
			cmd := exec.Command("go", "test", "-c", "-o", bin, "-tags", strings.Join(t.tags, ","), "./"+t.rel)
			cmd.Dir = filepath.Join(root, t.module)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.why = "not judged: its package does not build with -tags " + strings.Join(t.tags, ",") + ": " + firstLine(out)
				bin = ""
			}
			built[key] = bin
		}
		if bin == "" && t.why == "" {
			t.why = "not judged: its package does not build with -tags " + strings.Join(t.tags, ",")
		}
		bins[t] = bin
		if bin != "" && hasTag(t.tags, "docker") {
			docker = true
		}
	}
	if docker {
		if err := srv.start(root, tmp); err != nil {
			for t, bin := range bins {
				if bin != "" && hasTag(t.tags, "docker") {
					bins[t] = ""
					t.why = "not judged: no Postgres for the docker tests: " + err.Error()
				}
			}
		}
	}
	return bins, nil
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func hasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}

func firstLine(b []byte) string {
	l, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(l)
}

// ── one run ───────────────────────────────────────────────────────────────

// runAlone runs one test from its binary, in its package directory, as go
// test runs it, and reads its seconds from the testing package's own framed
// result lines (-test.v=test2json marks them with ^V, so a test's output that
// looks like one is not one).
func runAlone(root string, t *test, bin string, srv *server) (run, error) {
	env := os.Environ()
	if hasTag(t.tags, "docker") {
		if err := srv.reset(); err != nil {
			return run{}, err
		}
		env = append(env, srv.env()...)
	}
	cmd := exec.Command(bin, "-test.run", "^"+regexp.QuoteMeta(t.Name)+"$", "-test.count=1", "-test.v=test2json",
		"-test.timeout=15m", "-test.paniconexit0")
	cmd.Dir = filepath.Join(root, filepath.FromSlash(t.Dir))
	cmd.Env = env
	t0 := time.Now()
	out, err := cmd.CombinedOutput()
	r, perr := parseRun(out, t.Name)
	switch {
	case perr != nil:
		return run{}, perr
	case err != nil:
		return run{}, fmt.Errorf("it failed (%v)", err)
	}
	// The binary's wall is the test's: a top-level test that calls t.Parallel()
	// reports about 0 s for itself, its subtests' time outside it.
	r.wall = max(r.wall, time.Since(t0))
	r.cost = max(r.wall, r.leaves)
	return r, nil
}

// errSkipped is a run whose test skipped: an on-demand test (a recorder, a
// known-red pin) skips unless asked for, and costs a gate nothing.
var errSkipped = errors.New("it skips when run alone")

// whyNot is what a run's error makes of its test: skipped, or not judged.
func whyNot(err error) string {
	if errors.Is(err, errSkipped) {
		return "skipped: " + err.Error()
	}
	return "not judged: " + err.Error()
}

var resultRe = regexp.MustCompile(`^\x16--- (PASS|FAIL|SKIP): (\S+) \(([0-9.]+)s\)`)

// parseRun is the cost of top from a binary's -test.v=test2json output: the
// larger of its own wall and its leaves' seconds summed. A top that did not
// pass is an error, not a cost: a failed or skipped run says nothing of what
// the test costs.
func parseRun(out []byte, top string) (run, error) {
	secs := map[string]time.Duration{}
	status := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		m := resultRe.FindStringSubmatch(strings.TrimRight(sc.Text(), "\r"))
		if m == nil || (m[2] != top && !strings.HasPrefix(m[2], top+"/")) {
			continue
		}
		f, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			continue
		}
		secs[m[2]] = time.Duration(f * float64(time.Second))
		status[m[2]] = m[1]
	}
	switch status[top] {
	case "PASS":
	case "":
		return run{}, fmt.Errorf("it printed no result line (did it run?): %s", firstLine(out))
	case "SKIP":
		return run{}, errSkipped
	default:
		return run{}, errors.New("it failed")
	}
	r := run{wall: secs[top]}
	for name, d := range secs {
		leaf := true
		for other := range secs {
			if strings.HasPrefix(other, name+"/") {
				leaf = false
				break
			}
		}
		if leaf {
			r.leaves += d
			r.nLeaves++
		}
	}
	r.cost = max(r.wall, r.leaves)
	return r, nil
}

// ── the host ──────────────────────────────────────────────────────────────

// waitQuiet polls the host's busy CPUs every second over a 3 s window until
// they are under line, or wait is spent. It returns whether they were, and the
// last window's reading (-1 when the host's CPU could not be read at all).
func waitQuiet(line float64, wait time.Duration) (bool, float64) {
	type sample struct {
		at   time.Time
		busy time.Duration
	}
	const window = 3 * time.Second
	deadline := time.Now().Add(wait)
	var ss []sample
	last := -1.0
	for {
		b, err := hostBusy()
		if err != nil {
			return false, -1
		}
		now := time.Now()
		ss = append(ss, sample{now, b})
		for len(ss) > 1 && now.Sub(ss[1].at) >= window {
			ss = ss[1:]
		}
		if span := now.Sub(ss[0].at); span >= window {
			last = float64(b-ss[0].busy) / float64(span)
			if last < line {
				return true, last
			}
		}
		if now.After(deadline) {
			return false, last
		}
		time.Sleep(time.Second)
	}
}

// ── the server the docker tests run against ───────────────────────────────

// server is a Postgres of this tool's own, never a server named in the
// environment: reset drops databases, and must only ever drop its own. It is
// started as scripts/gate.sh's pg_up starts the gate's: the same image and
// settings, published on a host port it picks itself, above the ports Linux
// hands a client, with another pick when docker cannot start the server on
// one (docket G23; pg_up's comment says why the number matters). Pinned by
// TestServerStart_PublishesOnAPortOfItsOwn.
type server struct {
	name, addr, template string
	cleanup              []func()
	stopped              bool
}

// The host ports the server may be published on, picked as pg_up picks them:
// a random first pick in 61000-65535, then pgPortStep on (past a Windows
// exclusion block of 100), pgPortPicks in all.
const (
	pgPortLo    = 61000
	pgPortSpan  = 4536 // 61000-65535
	pgPortStep  = 257
	pgPortPicks = 8
)

func (s *server) env() []string {
	return []string{"SHINGO_TEST_PG=" + s.addr, "SHINGO_TEST_PG_TEMPLATE=" + s.template, "SHINGO_TEST_REQUIRE_DOCKER=1"}
}

func (s *server) start(root, tmp string) error {
	s.name = fmt.Sprintf("shingo-testcost-%d", os.Getpid())
	s.template = fmt.Sprintf("template_testcost_%d", os.Getpid())
	port, err := s.run()
	if err != nil {
		return err
	}
	s.cleanup = append(s.cleanup, func() { _ = exec.Command("docker", "rm", "-f", s.name).Run() })
	deadline := time.Now().Add(60 * time.Second)
	for exec.Command("docker", "exec", s.name, "pg_isready", "-U", "test", "-q").Run() != nil {
		if time.Now().After(deadline) {
			return errors.New("the server was not ready in 60 s")
		}
		time.Sleep(500 * time.Millisecond)
	}
	s.addr = fmt.Sprintf("127.0.0.1:%d", port)
	return s.warm(root, tmp)
}

// run creates and starts the server's container on a host port of its own
// pick and returns the port. A start docker refuses on a taken port leaves
// the container created; it is removed before the next pick, whose run takes
// its name. A run that left no container did not fail on its port (docker
// down, no image) and is not repeated.
func (s *server) run() (int, error) {
	first := rand.IntN(pgPortSpan)
	var err error
	for try := range pgPortPicks {
		port := pgPortLo + (first+try*pgPortStep)%pgPortSpan
		out, rerr := exec.Command("docker", "run", "-d", "--name", s.name,
			"-e", "POSTGRES_USER=test", "-e", "POSTGRES_PASSWORD=test", "-e", "POSTGRES_DB=postgres",
			"-e", "PGDATA=/var/lib/postgresql/data/pgdata",
			"--tmpfs", "/var/lib/postgresql/data:rw,size=4g",
			"-p", fmt.Sprintf("127.0.0.1:%d:5432", port), "postgres:16-alpine",
			"-c", "fsync=off", "-c", "synchronous_commit=off", "-c", "full_page_writes=off",
			"-c", "max_connections=500", "-c", "shared_buffers=256MB").CombinedOutput()
		if rerr == nil {
			return port, nil
		}
		err = fmt.Errorf("docker run: %v: %s", rerr, firstLine(out))
		if exec.Command("docker", "container", "inspect", s.name).Run() != nil {
			return 0, err
		}
		_ = exec.Command("docker", "rm", "-f", s.name).Run()
	}
	return 0, fmt.Errorf("%w (each of %d host ports in %d-%d refused)", err, pgPortPicks, pgPortLo, pgPortLo+pgPortSpan-1)
}

// warm builds the Core template on the server: warmTest, from its own binary.
func (s *server) warm(root, tmp string) error {
	bin := filepath.Join(tmp, "warm.test"+exeSuffix())
	mod, rel, _ := strings.Cut(warmDir, "/")
	cmd := exec.Command("go", "test", "-c", "-o", bin, "-tags", "docker,lockcheck", "./"+rel)
	cmd.Dir = filepath.Join(root, mod)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("the template warm-up (%s) does not build: %s", warmDir, firstLine(out))
	}
	run := exec.Command(bin, "-test.run", "^"+warmTest+"$", "-test.count=1", "-test.v=test2json")
	run.Dir = filepath.Join(root, filepath.FromSlash(warmDir))
	run.Env = append(os.Environ(), s.env()...)
	out, _ := run.CombinedOutput()
	if _, err := parseRun(out, warmTest); err != nil {
		return fmt.Errorf("the template warm-up %s %s: %v (testcost's warmTest names a test that must open a migrated database)",
			warmDir, warmTest, err)
	}
	return nil
}

// reset drops every database but the server's own and the Core template, so
// each test pays for the seeded templates it builds, as it does alone.
func (s *server) reset() error {
	out, err := s.psql("SELECT datname FROM pg_database WHERE datname NOT IN ('postgres', 'template0', 'template1', '" + s.template + "')")
	if err != nil {
		return err
	}
	var stmts []string
	for _, db := range strings.Fields(string(out)) {
		stmts = append(stmts, `ALTER DATABASE "`+db+`" WITH IS_TEMPLATE false`, `DROP DATABASE IF EXISTS "`+db+`" WITH (FORCE)`)
	}
	if len(stmts) == 0 {
		return nil
	}
	_, err = s.psql(stmts...)
	return err
}

func (s *server) psql(stmts ...string) ([]byte, error) {
	args := []string{"exec", s.name, "psql", "-U", "test", "-d", "postgres", "-At", "-v", "ON_ERROR_STOP=1"}
	for _, st := range stmts {
		args = append(args, "-c", st)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("resetting the server: %v", err)
	}
	return out, nil
}

func (s *server) stop() {
	if s.stopped {
		return
	}
	s.stopped = true
	for i := len(s.cleanup) - 1; i >= 0; i-- {
		s.cleanup[i]()
	}
}

// stopOnSignal removes the server and the binaries when the run is
// interrupted: a killed gate must not leave a container behind.
func stopOnSignal(s *server) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		s.stop()
		os.Exit(2)
	}()
}

func envFloat(name string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(name), 64); err == nil && v > 0 {
		return v
	}
	return def
}

func envDuration(name string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}
