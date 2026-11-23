// Package reuse is stage 2 of the gate's test-result reuse (shape W of
// work/loc-census-2026-11-20/design-round2/SYNTH-round2.md section 2): go test
// runs every test binary through `-exec`, and this decides, per binary, whether
// a stored passing run of the same binary on the same inputs can stand in for
// running it again.
//
// A verdict is served only when a stored passing run has the same binary
// (sha256), argv (less -test.testlogfile), cwd, environment, identity and
// wrapper, AND every path that run touched is the same now, by content,
// re-hashed at lookup (stateOf). A run whose own reads are not the whole of its
// inputs (it started a child, it has a TestMain, or nothing counted its
// children) is keyed on the whole working tree as well (treeState). A failed,
// killed, filtered or otherwise doubtful run is never stored (refusal). The
// switch is $GATETIME_REUSE; see Main.
package reuse

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// The values of $GATETIME_REUSE.
const (
	// ModeRecord runs every binary and stores the passing runs; serves nothing.
	ModeRecord = "record"
	// ModeReuse serves a stored verdict when one matches, else runs and stores.
	ModeReuse = "reuse"
	// ModeAudit is cert's: never serves, works out what ModeReuse would have
	// served, runs fresh, and fails a binary whose would-be hit did not pass.
	ModeAudit = "audit"
)

var timeZero time.Time

// config is what Main reads from its environment.
type config struct {
	mode, store, log, identity string
	tier, run, sha             string
	note                       string
}

func readConfig() config {
	c := config{
		mode:     os.Getenv("GATETIME_REUSE"),
		store:    os.Getenv("GATETIME_STORE"),
		log:      os.Getenv("GATETIME_REUSE_LOG"),
		identity: os.Getenv("GATETIME_IDENTITY"),
		tier:     os.Getenv("GATE_TIER"),
		run:      os.Getenv("GATETIME_RUN"),
		sha:      os.Getenv("GATETIME_SHA"),
	}
	switch c.mode {
	case ModeRecord, ModeReuse, ModeAudit:
	default:
		c.note = "GATETIME_REUSE=" + c.mode + " is not a mode: record"
		c.mode = ModeRecord
	}
	// GitHub never reuses (owner, R15; SYNTH-round2 section 1 row 4), whatever
	// a workflow or an inherited environment says.
	if os.Getenv("GITHUB_ACTIONS") == "true" && c.mode == ModeReuse {
		c.mode, c.note = ModeRecord, "GITHUB_ACTIONS=true: never served"
	}
	return c
}

// invocation is one binary's trip through Main.
type invocation struct {
	cfg              config
	binary, self     string
	argv             []string
	cwd, root, pkg   string
	work, tmp, tlog  string
	env              []string
	key              Key
	keyErr           string
	start            time.Time
	dec              Decision
	out              bytes.Buffer
	lookup           lookupResult
	looked, auditHit bool
}

// Main is go test -exec's program: args[0] is the test binary, the rest its
// arguments. run is how the binary is run (nil: DefaultRunner).
func Main(args []string, run Runner) int {
	signal.Ignore(os.Interrupt)
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: <wrapper> exec BINARY ARGS...   (go test -exec's program)")
		return 2
	}
	if run == nil {
		run = DefaultRunner
	}
	inv := newInvocation(args, readConfig())
	defer os.RemoveAll(inv.work)
	if inv.cfg.mode != ModeRecord && inv.keyErr == "" {
		inv.lookup, inv.looked = lookup(inv.cfg.store, inv.key, newNow()), true
		if inv.cfg.mode == ModeReuse && inv.lookup.Hit != nil {
			return inv.replay()
		}
		inv.auditHit = inv.lookup.Hit != nil
	}
	r := inv.runBinary(run)
	if r.StartErr != nil {
		fmt.Fprintln(os.Stderr, "gatetime: could not start the test binary:", r.StartErr)
		inv.dec.Decision, inv.dec.Exit, inv.dec.Refused = "run", 1, "start: "+r.StartErr.Error()
		inv.writeDecision()
		return 1
	}
	inv.record(r)
	code := r.Code
	if inv.cfg.mode == ModeAudit {
		code = inv.audit(r)
	}
	inv.writeDecision()
	if r.Signaled {
		_ = os.RemoveAll(inv.work) // the re-raise ends the process before the deferred one runs
		reraise(r.Signal)
	}
	return code
}

func newInvocation(args []string, cfg config) *invocation {
	inv := &invocation{cfg: cfg, binary: args[0], argv: append([]string{}, args[1:]...)}
	inv.cwd, _ = os.Getwd()
	inv.root = repoRoot(inv.cwd)
	inv.pkg = modulePackage(inv.cwd)
	if inv.cfg.store == "" && inv.root != "" {
		inv.cfg.store = filepath.Join(inv.root, ".gate", "reuse")
	}
	inv.self, _ = os.Executable()
	var err error
	if inv.work, err = os.MkdirTemp("", "gatetime-reuse-*"); err == nil {
		inv.tmp, inv.tlog = filepath.Join(inv.work, "tmp"), filepath.Join(inv.work, "testlog.txt")
		err = os.Mkdir(inv.tmp, 0o700)
	}
	if err != nil {
		inv.keyErr = "work dir: " + err.Error()
		inv.tmp = os.TempDir()
	}
	inv.env = childEnv(os.Environ(), inv.tmp)
	inv.key = Key{Argv: keyArgv(inv.argv), Cwd: inv.cwd, Env: keyEnv(inv.env)}
	inv.fillKey()
	inv.dec = Decision{Package: inv.pkg, Cwd: filepath.ToSlash(inv.cwd), Mode: cfg.mode, Note: cfg.note}
	return inv
}

// fillKey hashes what the key needs from disk. Any of it missing leaves keyErr
// set: such a binary runs, and is neither served nor stored (principle 6: an
// absent input is never "unchanged").
func (inv *invocation) fillKey() {
	fail := func(s string) {
		if inv.keyErr == "" {
			inv.keyErr = s
		}
	}
	var err error
	if inv.key.Binary, err = sha256File(inv.binary); err != nil {
		fail("binary hash: " + err.Error())
	}
	if inv.key.Wrapper, err = sha256File(inv.self); err != nil {
		fail("wrapper hash: " + err.Error())
	}
	switch {
	case inv.cfg.identity == "":
		fail("no identity ($GATETIME_IDENTITY unset)")
	default:
		if inv.key.Identity, err = sha256File(inv.cfg.identity); err != nil {
			fail("identity: " + err.Error())
		}
	}
	if inv.cfg.store == "" {
		fail("no store (no $GATETIME_STORE and no working tree)")
	}
}

func (inv *invocation) runBinary(run Runner) RunResult {
	inv.start = time.Now()
	args := append([]string{"-" + testlogFlag + inv.tlog}, inv.argv...)
	return run(RunSpec{Path: inv.binary, Args: args, Env: inv.env, Dir: inv.cwd, Out: tee{os.Stdout, &inv.out}})
}

// replay serves a hit: one marker line, then the stored bytes unchanged.
func (inv *invocation) replay() int {
	e := inv.lookup.Hit
	o := e.Origin
	fmt.Fprintf(os.Stdout, "gatetime: REUSED %s from %s run %s at %s (%.1fs, %s)\n",
		orDash(inv.pkg), orDash(o.Tier), orDash(o.Run), orDash(o.SHA), o.WallS, filepath.Base(inv.lookup.File))
	_, _ = os.Stdout.Write(e.Output)
	inv.dec.Decision, inv.dec.From = "reuse", &o
	inv.dec.Entries, inv.dec.Unverifiable = inv.lookup.Entries, inv.lookup.Unverifiable
	inv.dec.KeyID, inv.dec.WholeTree, inv.dec.WholeTreeWhy = inv.key.ID(), e.WholeTree, e.WholeTreeWhy
	inv.dec.Processes, inv.dec.ChildHow, inv.dec.Traced, inv.dec.Paths = e.Processes, e.ChildHow, e.Traced, len(e.Paths)
	inv.writeDecision()
	return 0
}

// audit is cert's verdict on what push would have done with this binary.
func (inv *invocation) audit(r RunResult) int {
	switch {
	case !inv.auditHit:
		inv.dec.Audit = "no-hit"
	case r.Code == 0 && !r.Signaled:
		inv.dec.Audit = "agree"
	default:
		o := inv.lookup.Hit.Origin
		inv.dec.Audit, inv.dec.From = "mismatch", &o
		fmt.Fprintf(os.Stdout, "gatetime: AUDIT MISMATCH %s: push would have reused the pass of %s run %s at %s; this fresh run exited %d\n",
			orDash(inv.pkg), orDash(o.Tier), orDash(o.Run), orDash(o.SHA), r.Code)
		if r.Code == 0 {
			return 1
		}
	}
	return r.Code
}

// tee writes to the wrapper's stdout and to the entry's buffer. A failed
// write to stdout does not stop the buffer, and is go test's to see.
type tee struct {
	stdout io.Writer
	buf    *bytes.Buffer
}

func (t tee) Write(p []byte) (int, error) {
	t.buf.Write(p)
	_, _ = t.stdout.Write(p)
	return len(p), nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// reraise ends the wrapper on the binary's signal, so go test reports
// "signal: killed" as it would have without the wrapper. Main returns 128 plus
// the signal when the re-raise does not end it (Windows).
func reraise(sig syscall.Signal) {
	signal.Reset(sig)
	if p, err := os.FindProcess(os.Getpid()); err == nil {
		_ = p.Signal(sig)
		time.Sleep(100 * time.Millisecond)
	}
}

// modulePackage is the import path of dir: the module path of the nearest
// go.mod at or above it, joined with dir's place under it; "" when none.
func modulePackage(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if data, err := os.ReadFile(filepath.Join(d, "go.mod")); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if f := strings.Fields(line); len(f) >= 2 && f[0] == "module" {
					rel, err := filepath.Rel(d, dir)
					if err != nil || rel == "." {
						return strings.Trim(f[1], `"`)
					}
					return strings.Trim(f[1], `"`) + "/" + filepath.ToSlash(rel)
				}
			}
			return ""
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}
