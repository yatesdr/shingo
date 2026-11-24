package main

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// samplesEvery builds n samples `every` apart starting at t0 from per-interval
// loads in cores: host, job, vm. The first sample is the zero point.
func samplesEvery(t0 time.Time, every time.Duration, loads [][3]float64) []sample {
	s := sample{at: t0, phase: "before", ac: 1}
	out := []sample{s}
	for i, l := range loads {
		s.at = s.at.Add(every)
		s.host += time.Duration(l[0] * float64(every))
		s.job += time.Duration(l[1] * float64(every))
		s.vm += time.Duration(l[2] * float64(every))
		s.phase = "during"
		if i == 0 {
			s.phase = "before"
		}
		out = append(out, s)
	}
	return out
}

func repeat(n int, l [3]float64) [][3]float64 {
	out := make([][3]float64, n)
	for i := range out {
		out[i] = l
	}
	return out
}

func hasReason(j judgement, prefix string) bool {
	for _, r := range j.reasons {
		if strings.HasPrefix(r, prefix) {
			return true
		}
	}
	return false
}

func TestJudge_CleanRun(t *testing.T) {
	ss := samplesEvery(time.Now(), 3*time.Second, repeat(30, [3]float64{10, 9.5, 0.3}))
	j := judge(ss, nil, 15*time.Second, nil, nil)
	if j.verdict() != "clean" || j.acState != "on" {
		t.Fatalf("verdict %q ac %q, want clean on AC (foreign %.2f)", j.verdict(), j.acState, j.foreignMax30)
	}
}

// TestJudge_ForeignLoadIsAveragedOver30s: two foreign cores for 45 s void
// the run; the same two cores for 9 s inside a quiet run do not (0.6 cores
// over the window), and neither does load the job or the VM accounts for.
func TestJudge_ForeignLoadIsAveragedOver30s(t *testing.T) {
	t0 := time.Now()
	long := append(repeat(5, [3]float64{4, 4, 0}), append(repeat(15, [3]float64{6, 4, 0}), repeat(5, [3]float64{4, 4, 0})...)...)
	if j := judge(samplesEvery(t0, 3*time.Second, long), nil, 15*time.Second, nil, nil); !hasReason(j, "foreign-load(2.00") {
		t.Errorf("45 s of 2 foreign cores: %q, want foreign-load(2.00 ...)", j.verdict())
	}
	short := append(repeat(10, [3]float64{4, 4, 0}), append(repeat(3, [3]float64{6, 4, 0}), repeat(10, [3]float64{4, 4, 0})...)...)
	if j := judge(samplesEvery(t0, 3*time.Second, short), nil, 15*time.Second, nil, nil); j.verdict() != "clean" || j.foreignMax30 < 0.5 || j.foreignMax30 > 0.7 {
		t.Errorf("9 s of 2 foreign cores: %q at %.2f cores, want clean at 0.6", j.verdict(), j.foreignMax30)
	}
	ours := append([][3]float64{{0.2, 0.1, 0.1}}, repeat(20, [3]float64{12, 8, 3.5})...) // the VM quiet before the command
	if j := judge(samplesEvery(t0, 3*time.Second, ours), nil, 15*time.Second, nil, nil); j.verdict() != "clean" {
		t.Errorf("host load the job and the VM account for: %q, want clean", j.verdict())
	}
	// A run shorter than one window is judged over its whole span.
	brief := repeat(4, [3]float64{3, 1, 0})
	if j := judge(samplesEvery(t0, 3*time.Second, brief), nil, 15*time.Second, nil, nil); !hasReason(j, "foreign-load(2.00") {
		t.Errorf("a 12 s run under 2 foreign cores: %q, want foreign-load", j.verdict())
	}
}

func TestJudge_PowerStandbyGapAndVM(t *testing.T) {
	t0 := time.Now()
	base := func() []sample { return samplesEvery(t0, 3*time.Second, repeat(10, [3]float64{2, 2, 0})) }

	ss := base()
	ss[4].ac = 0
	if j := judge(ss, nil, 15*time.Second, nil, nil); !hasReason(j, "not-on-AC(1 of 11") || j.acState != "off" {
		t.Errorf("one sample off AC: %q", j.verdict())
	}
	ss = base()
	ss[2].ac = -1
	if j := judge(ss, nil, 15*time.Second, nil, nil); !hasReason(j, "AC-unknown") {
		t.Errorf("one sample of unknown power: %q", j.verdict())
	}
	evs := []standbyEvent{{"506", t0.Add(5 * time.Second)}, {"507", t0.Add(9 * time.Second)}}
	if j := judge(base(), evs, 15*time.Second, nil, nil); !hasReason(j, "standby(506@") || j.standby != 2 {
		t.Errorf("standby inside the window: %q", j.verdict())
	}
	ss = base()
	for i := 6; i < len(ss); i++ {
		ss[i].at = ss[i].at.Add(40 * time.Second)
	}
	if j := judge(ss, nil, 15*time.Second, nil, nil); !hasReason(j, "sampler-gap(43s)") {
		t.Errorf("a 43 s hole between passes: %q", j.verdict())
	}
	ss = samplesEvery(t0, 3*time.Second, append([][3]float64{{3, 0.1, 2.9}}, repeat(10, [3]float64{2, 2, 0})...))
	if j := judge(ss, nil, 15*time.Second, nil, nil); !hasReason(j, "vm-busy-before(2.90") {
		t.Errorf("the VM at 2.9 cores before the command: %q", j.verdict())
	}
	ss = base()
	ss[3].err = "GetSystemTimes: boom"
	if j := judge(ss, nil, 15*time.Second, []string{"battery"}, []string{"unsupported(x)"}); !hasReason(j, "sampler-error(1") || !hasReason(j, "planted(battery)") || !hasReason(j, "unsupported(x)") {
		t.Errorf("errored pass, plant and OS limit: %q", j.verdict())
	}
	if j := judge(base()[:1], nil, 15*time.Second, nil, nil); !hasReason(j, "no-samples") {
		t.Errorf("one sample: %q", j.verdict())
	}
}

// fakeMachine answers from fields; the readers' input seam.
type fakeMachine struct {
	acState int
	evs     []standbyEvent
	evErr   error
	sc      scope
}

func (f *fakeMachine) enterJob() error                    { return nil }
func (f *fakeMachine) jobCPU() (time.Duration, error)     { return 0, nil }
func (f *fakeMachine) selfCPU() time.Duration             { return 0 }
func (f *fakeMachine) hostBusy() (time.Duration, error)   { return 0, nil }
func (f *fakeMachine) procs() (map[uint32]procCPU, error) { return nil, nil }
func (f *fakeMachine) jobPids() (map[uint32]bool, error)  { return nil, nil }
func (f *fakeMachine) ac() (int, error)                   { return f.acState, nil }
func (f *fakeMachine) stayAwake()                         {}
func (f *fakeMachine) standby(from, to time.Time) ([]standbyEvent, error) {
	return f.evs, f.evErr
}
func (f *fakeMachine) limits() []string { return nil }
func (f *fakeMachine) scope() scope     { return f.sc }

func TestPlant_BatteryAndStandby(t *testing.T) {
	m, names := plantFrom(&fakeMachine{acState: 1}, "standby, battery")
	if strings.Join(names, ",") != "battery,standby" {
		t.Errorf("plant names %v", names)
	}
	if a, err := m.ac(); a != 0 || err != nil {
		t.Errorf("planted battery reads AC %d (%v), want 0", a, err)
	}
	from := time.Now()
	evs, err := m.standby(from, from.Add(10*time.Second))
	if err != nil || len(evs) != 2 || evs[0].at.Before(from) || evs[1].at.After(from.Add(10*time.Second)) {
		t.Errorf("planted standby %v (%v), want a 506 and a 507 inside the window", evs, err)
	}
	if m, names := plantFrom(&fakeMachine{acState: 1}, ""); len(names) != 0 {
		t.Errorf("no plant gave %v", names)
	} else if a, err := m.ac(); a != 1 || err != nil {
		t.Errorf("no plant changed the AC reading to %d (%v)", a, err)
	}
}

// readingsMachine answers the sampler's CPU readings from fields.
type readingsMachine struct {
	fakeMachine
	host, job time.Duration
	table     map[uint32]procCPU
}

func (m *readingsMachine) hostBusy() (time.Duration, error)   { return m.host, nil }
func (m *readingsMachine) jobCPU() (time.Duration, error)     { return m.job, nil }
func (m *readingsMachine) procs() (map[uint32]procCPU, error) { return m.table, nil }

// TestPass_CountsTheVMApartFromTheJob: one sampler pass takes the host's busy
// time, the job's CPU, and the utility VM's as the sum of the processes isVM
// names; foreign is what is left. The job here runs outside the VM, as every
// job does on Windows.
func TestPass_CountsTheVMApartFromTheJob(t *testing.T) {
	m := &readingsMachine{fakeMachine: fakeMachine{acState: 1}, host: 10 * time.Second, job: 4 * time.Second,
		table: map[uint32]procCPU{7: {"vmmemWSL", 3 * time.Second}, 4: {"System", time.Second}, 9: {"MsMpEng.exe", 2 * time.Second}}}
	r := &runner{m: m, every: time.Second, out: t.TempDir(), phase: "before", rows: bufio.NewWriter(io.Discard)}
	r.pass()
	s := r.samples[0]
	if s.host != 10*time.Second || s.job != 4*time.Second || s.vm != 4*time.Second || s.foreign() != 2*time.Second || s.ac != 1 {
		t.Errorf("pass read host %v job %v vm %v foreign %v ac %d, want 10s 4s 4s 2s 1", s.host, s.job, s.vm, s.foreign(), s.ac)
	}
}

// TestPass_AJobInsideTheVMIsCountedOnce: read from inside WSL, the VM's
// reading already holds the job's CPU, so the pass takes the job out of the
// VM's share rather than out of the host's twice: foreign is the host's busy
// time less the whole VM, whatever the job's own reading says.
func TestPass_AJobInsideTheVMIsCountedOnce(t *testing.T) {
	for _, job := range []time.Duration{0, time.Second, 2500 * time.Millisecond} {
		m := &readingsMachine{fakeMachine: fakeMachine{acState: 1, sc: scope{jobInVM: true}}, host: 10 * time.Second, job: job,
			table: map[uint32]procCPU{7: {"vmmemWSL", 3 * time.Second}, 4: {"System", time.Second}, 9: {"MsMpEng.exe", 2 * time.Second}}}
		r := &runner{m: m, every: time.Second, out: t.TempDir(), phase: "before", rows: bufio.NewWriter(io.Discard)}
		r.pass()
		s := r.samples[0]
		if s.job != job || s.vm != 4*time.Second-job || s.foreign() != 6*time.Second {
			t.Errorf("job %v inside the VM: job %v vm %v foreign %v, want %v %v 6s", job, s.job, s.vm, s.foreign(), job, 4*time.Second-job)
		}
	}
}

// TestJudge_InsideTheVMOnlyIsNeverClean: a run whose host was not read drops
// the host readings it could not take (AC-unknown, standby-unread) and is at
// best "clean inside the VM only"; any other reason, an off-AC reading
// included, still voids it.
func TestJudge_InsideTheVMOnlyIsNeverClean(t *testing.T) {
	ss := samplesEvery(time.Now(), 3*time.Second, repeat(10, [3]float64{2, 2, 0}))
	for i := range ss {
		ss[i].ac = -1
	}
	j := judge(ss, nil, 15*time.Second, nil, nil)
	j.reasons = append(j.reasons, "standby-unread(no Modern Standby log off Windows)")
	if got := j.insideVM("no reader").verdict(); got != vmOnlyVerdict {
		t.Errorf("quiet, AC and standby unread, inside the VM: %q, want %q", got, vmOnlyVerdict)
	}
	if got := j.verdict(); !strings.HasPrefix(got, "VOID: AC-unknown standby-unread(") {
		t.Errorf("the same run judged as a whole host: %q, want VOID on AC-unknown and standby-unread", got)
	}
	ss[3].ac = 0
	if got := judge(ss, nil, 15*time.Second, nil, nil).insideVM("no reader").verdict(); got != "VOID: not-on-AC(1 of 11 samples)" {
		t.Errorf("an off-AC reading inside the VM: %q, want VOID: not-on-AC(1 of 11 samples)", got)
	}
	busy := samplesEvery(time.Now(), 3*time.Second, repeat(20, [3]float64{5, 2, 0}))
	if got := judge(busy, nil, 15*time.Second, []string{"battery"}, nil).insideVM("no reader").verdict(); !strings.Contains(got, "foreign-load(3.00") || !strings.Contains(got, "planted(battery)") {
		t.Errorf("foreign load and a plant inside the VM: %q, want both reasons", got)
	}
}

func TestResultLine_RoundTrip(t *testing.T) {
	l := resultLine{"wall_s": "1.50", "verdict": "VOID: a b", "cmd": "bash -c x\ty", "extra": "1"}
	back := parseResultLine(l.String())
	if back["verdict"] != "VOID: a b" || back["cmd"] != "bash -c x y" || back["wall_s"] != "1.50" || back["extra"] != "1" {
		t.Errorf("round trip: %q -> %v", l.String(), back)
	}
	if !strings.HasPrefix(l.String(), "wall_s=1.50\tverdict=") {
		t.Errorf("fields out of their order: %q", l.String())
	}
}

func TestFlag_HeldStaleAndReleased(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gate.timing")
	f, err := acquireFlag(dir, "/repo", "/out", "bash scripts/gate.sh full")
	if err != nil {
		t.Fatal(err)
	}
	o := readOwner(dir)
	for _, k := range []string{"holder", "pid", "cwd", "out", "cmd", "started", "token"} {
		if o[k] == "" {
			t.Errorf("owner has no %s: %v", k, o)
		}
	}
	if _, err := acquireFlag(dir, "/repo", "/out2", "x"); err == nil || !strings.Contains(err.Error(), "bash scripts/gate.sh full") {
		t.Errorf("second acquire: %v, want refused naming the holder's command", err)
	}
	old := time.Now().Add(-3 * flagStale)
	if err := os.Chtimes(filepath.Join(dir, "heartbeat"), old, old); err != nil {
		t.Fatal(err)
	}
	g, err := acquireFlag(dir, "/repo", "/out3", "the taker")
	if err != nil {
		t.Fatalf("a stale flag was not taken over: %v", err)
	}
	f.release() // the old holder's release must not remove the taker's flag
	if readOwner(dir)["cmd"] != "the taker" {
		t.Errorf("the old holder's release removed the new holder's flag")
	}
	g.release()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("flag left after release: %v", err)
	}
}

// TestRun_PlantedEndToEnd is the planted readings through the whole command:
// GATETIME_PLANT=battery,standby voids the run with both reasons and the
// plant, the command's exit code comes back unchanged, the flag is held
// during the run (the command sees its token) and gone after.
func TestRun_PlantedEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "run")
	flag := filepath.Join(t.TempDir(), "gate.timing")
	cmd := exec.Command(self, "run", "-out", out, "-every", "200ms", "-repo", t.TempDir(), "--", "go", "help", "no-such-topic")
	cmd.Env = append(os.Environ(), "GATETIME_TEST_AS_MAIN=1", plantEnv+"=battery,standby", flagEnvDir+"="+flag)
	b, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("gatetime run exit %v, want go help's 2\n%s", err, b)
	}
	data, err := os.ReadFile(filepath.Join(out, "result.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "\n") != 1 {
		t.Errorf("result.tsv is not one line: %q", data)
	}
	res := parseResultLine(string(data))
	for _, want := range []string{"not-on-AC(", "standby(506@", "planted(battery,standby)"} {
		if !strings.Contains(res["verdict"], want) {
			t.Errorf("verdict %q lacks %s", res["verdict"], want)
		}
	}
	if res["exit"] != "2" || res["plant"] != "battery,standby" || !strings.HasPrefix(res["flag"], "held ") {
		t.Errorf("result %v", res)
	}
	if _, err := os.Stat(flag); !os.IsNotExist(err) {
		t.Errorf("timing flag left behind after the run: %v", err)
	}
	for _, f := range []string{"samples.tsv", "cmd.log", "steps.tsv", "live.tsv"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("run wrote no %s: %v", f, err)
		}
	}
}

// TestJudge_ForeignLineIsTheRunsOwn: the void line is the run's -foreign-cores
// (default 1.0, the designed rule). Raised to 2.5 for a host whose quiet floor
// is 1.4-1.5 cores, two foreign cores for 45 s are clean and three are not; the
// result line names the line it judged by.
func TestJudge_ForeignLineIsTheRunsOwn(t *testing.T) {
	t0 := time.Now()
	two := append(repeat(5, [3]float64{4, 4, 0}), append(repeat(15, [3]float64{6, 4, 0}), repeat(5, [3]float64{4, 4, 0})...)...)
	three := append(repeat(5, [3]float64{4, 4, 0}), append(repeat(15, [3]float64{7, 4, 0}), repeat(5, [3]float64{4, 4, 0})...)...)
	defer func(old float64) { foreignLine = old }(foreignLine)
	if foreignLine != 1.0 {
		t.Fatalf("default foreign line %.2f, want 1.0 (the designed rule)", foreignLine)
	}
	foreignLine = 2.5
	if j := judge(samplesEvery(t0, 3*time.Second, two), nil, 15*time.Second, nil, nil); j.verdict() != "clean" {
		t.Errorf("2 foreign cores under a 2.5 line: %q, want clean", j.verdict())
	}
	if j := judge(samplesEvery(t0, 3*time.Second, three), nil, 15*time.Second, nil, nil); !hasReason(j, "foreign-load(3.00 cores over 30 s, line 2.50)") {
		t.Errorf("3 foreign cores under a 2.5 line: %q, want foreign-load(3.00 ..., line 2.50)", j.verdict())
	}
}

// TestIsVM_CountsDockerDesktopsHostSide: the utility VM and the host-side
// processes that carry its traffic are counted with it (machine.go's isVM says
// why: a quiet laptop's docker runs read 4-6 foreign cores without them); an
// unrelated process, and a go or test binary of another gate, are not.
func TestIsVM_CountsDockerDesktopsHostSide(t *testing.T) {
	for _, n := range []string{"vmmemWSL", "vmmem", "wslrelay.exe", "com.docker.backend.exe", "com.docker.build.exe", "vpnkit.exe", "System"} {
		if !isVM(n) {
			t.Errorf("isVM(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"go.exe", "engine.test.exe", "node.exe", "FortiESNAC.exe", "svchost.exe", "docker.exe", "System Idle Process"} {
		if isVM(n) {
			t.Errorf("isVM(%q) = true, want false", n)
		}
	}
}
