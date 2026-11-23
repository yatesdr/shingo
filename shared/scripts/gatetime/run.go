package main

// `gatetime run -out DIR [-every 3s] [-repo DIR] -- cmd args...` times one
// command and judges the machine it ran on.
//
// It puts itself in a job object, so the command and everything it starts are
// in it, orphans included; holds the system and display requirement for the
// run (no standby, no display-off); holds the timing flag (flag.go) so no gate
// of this clone starts beside it; and samples every -every from two passes
// before the command to one after it. Into DIR it writes:
//
//	samples.tsv  one row per pass (columns in its header)
//	cmd.log      the command's stdout and stderr
//	steps.tsv    the lines the run added to <repo>/.gate/steps.log (the gate's
//	             own record: selection, package and total lines), so a reader
//	             of DIR has them whichever tree the gate came from
//	live.tsv     the judgement so far, rewritten every pass (gatetime stamp
//	             reads it for the gate's total line)
//	result.tsv   ONE line, tab-separated key=value fields: start, end, wall_s,
//	             job_cpu_s, host_cpu_s, vm_cpu_s, foreign_max30, samples,
//	             standby, exit, verdict, flag, plant, cmd
//
// The verdict is "clean" or "VOID: reasons". The exit code is the command's,
// whatever the verdict: a reader reads both. A run is void when:
//   - not-on-AC / AC-unknown: any sample not on mains power;
//   - standby(...): a Kernel-Power 506 or 507 in the System log inside the
//     run's window (first to last pass);
//   - sampler-gap: two passes further apart on the wall clock than 4 x -every
//     (at least 15 s): the machine slept or froze;
//   - foreign-load: host CPU less the job's less the utility VM's above one
//     core (or the run's -foreign-cores line, judge.go's foreignLine)
//     averaged over any 30 s window (the whole span when shorter). The
//     load is MEASURED, not named: whatever ran outside the job counts;
//   - vm-busy-before: the utility VM above one core between the two passes
//     before the command started;
//   - sampler-error, no-samples, planted(...) (GATETIME_PLANT, test only), and
//     on an OS without a job object, unsupported(...).
//
// THE HOLE THE VM LEAVES. The utility VM (vmmemWSL) runs this run's Postgres
// and also every other WSL 2 distro and Docker container on the machine, and
// its CPU is subtracted whole, with Docker Desktop's host-side relays and the
// host kernel ("System"), which carry the VM's traffic (machine.go's isVM).
// Another lane's container or WSL race run during the command is therefore
// invisible to foreign-load, and so is another workload's kernel time (its
// user-mode CPU still counts); vm-busy-before catches a VM already busy when
// the command starts. The vm_cpu_s field
// and the vm column of samples.tsv let a reader see the VM's share.

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type runner struct {
	m      machine
	plants []string
	every  time.Duration
	out    string

	mu      sync.Mutex
	samples []sample
	phase   string
	last    map[uint32]procCPU
	rows    *bufio.Writer
	flag    *timingFlag
}

func runMain(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	out := fs.String("out", "", "directory for the run's record")
	every := fs.Duration("every", 3*time.Second, "time between sampler passes")
	repo := fs.String("repo", ".", "the checkout whose timing flag the run holds and whose .gate/steps.log it copies from")
	line := fs.Float64("foreign-cores", foreignCores, "foreign cores over any 30 s that void the run (default: the designed rule; raise it only to a measured quiet floor plus one core)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *line <= 0 {
		fmt.Fprintln(os.Stderr, "gatetime run: -foreign-cores must be positive")
		return 2
	}
	foreignLine = *line
	cmdArgs := fs.Args()
	if *out == "" || len(cmdArgs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: gatetime run -out DIR [-every 3s] [-repo DIR] -- cmd args...")
		return 2
	}
	m, plants := plantedMachine()
	r := &runner{m: m, plants: plants, every: *every, out: *out, phase: "before"}
	code, err := r.run(*repo, cmdArgs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gatetime run:", err)
		return 125
	}
	return code
}

// timedCmd is one command's run inside the job.
type timedCmd struct {
	t0, t1 time.Time
	cpu    time.Duration // the job's growth less gatetime's own
	code   int
}

// run is the whole timed run; err is gatetime's own failure (exit 125).
func (r *runner) run(repo string, cmdArgs []string) (int, error) {
	out, repoAbs, err := absDirs(r.out, repo)
	if err != nil {
		return 0, err
	}
	cmdLine := strings.Join(cmdArgs, " ")
	flagNote := "none: " + repoAbs + " is not a git checkout"
	if dir := flagDir(repoAbs); dir != "" {
		if r.flag, err = acquireFlag(dir, repoAbs, out, cmdLine); err != nil {
			return 0, err
		}
		defer r.flag.release()
		flagNote = "held " + filepath.ToSlash(dir)
	}
	if err := r.enter(); err != nil {
		return 0, err
	}
	samplesF, err := os.Create(filepath.Join(out, "samples.tsv"))
	if err != nil {
		return 0, err
	}
	r.rows = bufio.NewWriter(samplesF)
	fmt.Fprintln(r.rows, "unix_ms\tphase\tinterval_s\thost_cores\tjob_cores\tvm_cores\tforeign_cores\tac\ttop_foreign\tstate")
	logF, err := os.Create(filepath.Join(out, "cmd.log"))
	if err != nil {
		return 0, err
	}
	stepsLog := filepath.Join(repoAbs, ".gate", "steps.log")
	stepsFrom := fileSize(stepsLog)

	r.pass()
	time.Sleep(r.every)
	r.pass()
	r.setPhase("during")
	stop, stopped := make(chan struct{}), make(chan struct{})
	go r.sampleUntil(stop, stopped)
	w := bufio.NewWriterSize(logF, 1<<16)
	tc, err := r.execute(cmdArgs, out, w)
	close(stop)
	<-stopped
	if err != nil {
		return 0, err
	}
	r.setPhase("after")
	r.pass()

	if err := errors.Join(w.Flush(), logF.Close(), r.rows.Flush(), samplesF.Close()); err != nil {
		return 0, err
	}
	if err := copyTail(stepsLog, stepsFrom, filepath.Join(out, "steps.tsv")); err != nil {
		return 0, err
	}
	return tc.code, r.writeResult(out, tc, flagNote, cmdLine)
}

// absDirs creates the record's directory and makes both paths absolute.
func absDirs(out, repo string) (string, string, error) {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return "", "", err
	}
	o, err := filepath.Abs(out)
	if err != nil {
		return "", "", err
	}
	r, err := filepath.Abs(repo)
	return o, r, err
}

// enter holds the standby requirement on a thread that lives as long as the
// process, and puts gatetime in its job.
func (r *runner) enter() error {
	awake := make(chan struct{})
	go func() {
		runtime.LockOSThread() // the requirement lives as long as this thread
		r.m.stayAwake()
		close(awake)
		select {}
	}()
	<-awake
	return r.m.enterJob()
}

// execute runs the command, with the flag's token and the record's directory
// in its environment, its output to w.
func (r *runner) execute(cmdArgs []string, out string, w io.Writer) (timedCmd, error) {
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Env = append(os.Environ(), flagEnvToken+"="+r.token(), "GATETIME_RUN_DIR="+filepath.ToSlash(out))
	cmd.Stdout, cmd.Stderr, cmd.Stdin = w, w, os.Stdin
	self0 := r.m.selfCPU()
	cpu0, err := r.m.jobCPU()
	if err != nil {
		return timedCmd{}, err
	}
	tc := timedCmd{t0: time.Now()}
	runErr := cmd.Run()
	tc.t1 = time.Now()
	cpu1, err := r.m.jobCPU()
	if err != nil {
		return timedCmd{}, err
	}
	tc.cpu = (cpu1 - cpu0) - (r.m.selfCPU() - self0)
	if tc.code = exitCode(runErr); tc.code == 126 {
		fmt.Fprintln(w, "gatetime run: could not run:", runErr)
	}
	return tc, nil
}

// writeResult judges the whole run and writes its one result line.
func (r *runner) writeResult(out string, tc timedCmd, flagNote, cmdLine string) error {
	j := r.final()
	res := resultLine{
		"start": tc.t0.UTC().Format(time.RFC3339Nano), "end": tc.t1.UTC().Format(time.RFC3339Nano),
		"wall_s":        fmt.Sprintf("%.2f", tc.t1.Sub(tc.t0).Seconds()),
		"job_cpu_s":     fmt.Sprintf("%.2f", tc.cpu.Seconds()),
		"host_cpu_s":    fmt.Sprintf("%.2f", r.span(func(s sample) time.Duration { return s.host })),
		"vm_cpu_s":      fmt.Sprintf("%.2f", r.span(func(s sample) time.Duration { return s.vm })),
		"foreign_max30": fmt.Sprintf("%.2f", j.foreignMax30),
		"foreign_line":  fmt.Sprintf("%.2f", foreignLine),
		"samples":       fmt.Sprint(len(r.samples)),
		"standby":       fmt.Sprint(j.standby),
		"exit":          fmt.Sprint(tc.code),
		"verdict":       j.verdict(),
		"flag":          flagNote,
		"plant":         strings.Join(r.plants, ","),
		"cmd":           oneLine(cmdLine),
	}
	if err := os.WriteFile(filepath.Join(out, "result.tsv"), []byte(res.String()+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Printf("gatetime: exit=%d wall=%.1fs cpu=%ss %s\n", tc.code, tc.t1.Sub(tc.t0).Seconds(), res["job_cpu_s"], j.verdict())
	return nil
}

func (r *runner) token() string {
	if r.flag == nil {
		return ""
	}
	return r.flag.token
}

func (r *runner) setPhase(p string) {
	r.mu.Lock()
	r.phase = p
	r.mu.Unlock()
}

func (r *runner) sampleUntil(stop <-chan struct{}, stopped chan<- struct{}) {
	defer close(stopped)
	t := time.NewTicker(r.every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			r.pass()
		}
	}
}

// pass takes one sample, writes its row and the judgement so far.
func (r *runner) pass() {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := sample{at: time.Now().Round(0), phase: r.phase, ac: -1}
	var errs []string
	note := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	var err error
	s.host, err = r.m.hostBusy()
	note(err)
	s.job, err = r.m.jobCPU()
	note(err)
	procs, err := r.m.procs()
	note(err)
	for _, p := range procs {
		if isVM(p.name) {
			s.vm += p.cpu
		}
	}
	s.ac, err = r.m.ac()
	note(err)
	s.top = r.topForeign(procs)
	if len(errs) > 0 {
		s.err = strings.Join(errs, "; ")
	}
	r.samples = append(r.samples, s)
	if r.flag != nil {
		r.flag.beat()
	}
	r.writeRow(s)
	r.writeLive()
}

// topForeign names the three processes outside the job and the VM that used
// the most CPU since the last pass. For a reader of a void run only: the
// verdict never reads names.
func (r *runner) topForeign(procs map[uint32]procCPU) string {
	mine, _ := r.m.jobPids()
	type use struct {
		name string
		d    time.Duration
	}
	var uses []use
	for pid, p := range procs {
		if prev, ok := r.last[pid]; ok && prev.name == p.name && !mine[pid] && !isVM(p.name) {
			if d := p.cpu - prev.cpu; d > 0 {
				uses = append(uses, use{p.name, d})
			}
		}
	}
	r.last = procs
	sort.Slice(uses, func(a, b int) bool { return uses[a].d > uses[b].d })
	var out []string
	for i := 0; i < len(uses) && i < 3; i++ {
		out = append(out, fmt.Sprintf("%s:%.1f", uses[i].name, uses[i].d.Seconds()))
	}
	return strings.Join(out, ",")
}

func (r *runner) writeRow(s sample) {
	interval, hc, jc, vc, fc := 0.0, 0.0, 0.0, 0.0, 0.0
	if n := len(r.samples); n >= 2 {
		p := r.samples[n-2]
		if interval = s.at.Sub(p.at).Seconds(); interval > 0 {
			hc = (s.host - p.host).Seconds() / interval
			jc = (s.job - p.job).Seconds() / interval
			vc = (s.vm - p.vm).Seconds() / interval
			fc = (s.foreign() - p.foreign()).Seconds() / interval
		}
	}
	state := "ok"
	if s.err != "" {
		state = "error: " + oneLine(s.err)
	}
	fmt.Fprintf(r.rows, "%d\t%s\t%.2f\t%.2f\t%.2f\t%.2f\t%.2f\t%d\t%s\t%s\n",
		s.at.UnixMilli(), s.phase, interval, hc, jc, vc, fc, s.ac, s.top, state)
}

// maxGap is the sampler-gap threshold.
func (r *runner) maxGap() time.Duration { return max(15*time.Second, 4*r.every) }

// writeLive rewrites live.tsv with the judgement of the samples so far. The
// standby log is not read per pass (wevtutil is a process); gatetime stamp
// reads it itself.
func (r *runner) writeLive() {
	j := judge(r.samples, nil, r.maxGap(), r.plants, r.m.limits())
	line := resultLine{
		"samples": fmt.Sprint(len(r.samples)), "ac": j.acState,
		"gap_max_s":     fmt.Sprintf("%.1f", j.gapMax.Seconds()),
		"foreign_max30": fmt.Sprintf("%.2f", j.foreignMax30),
		"verdict":       j.verdict(),
		"plant":         strings.Join(r.plants, ","),
	}
	tmp := filepath.Join(r.out, ".live.tsv.tmp")
	if os.WriteFile(tmp, []byte(line.String()+"\n"), 0o644) == nil {
		_ = os.Rename(tmp, filepath.Join(r.out, "live.tsv"))
	}
}

// final is the judgement over every sample and the standby log for the run's
// whole window.
func (r *runner) final() judgement {
	from, to := r.samples[0].at, r.samples[len(r.samples)-1].at
	evs, err := r.m.standby(from, to)
	j := judge(r.samples, evs, r.maxGap(), r.plants, r.m.limits())
	if err != nil {
		j.reasons = append(j.reasons, "standby-unread("+oneLine(err.Error())+")")
	}
	return j
}

// span is a cumulative reading's growth from the first pass to the last.
func (r *runner) span(f func(sample) time.Duration) float64 {
	if len(r.samples) < 2 {
		return 0
	}
	return (f(r.samples[len(r.samples)-1]) - f(r.samples[0])).Seconds()
}

func exitCode(err error) int {
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		return ee.ExitCode()
	case err != nil:
		return 126
	}
	return 0
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// copyTail copies what was appended to src after offset from into dst; an
// absent src gives an empty dst (the command was not a gate, or it logs
// nothing).
func copyTail(src string, from int64, dst string) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err == nil {
		if fileSize(src) < from {
			from = 0 // the log was replaced during the run
		}
		if _, err = in.Seek(from, io.SeekStart); err == nil {
			_, err = io.Copy(out, in)
		}
		err = errors.Join(err, in.Close())
	} else if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return errors.Join(err, out.Close())
}

// resultLine is one line of tab-separated key=value fields, keys in a fixed
// order for a reader's eye; values never hold a tab or a newline.
type resultLine map[string]string

var resultOrder = []string{"start", "end", "wall_s", "job_cpu_s", "host_cpu_s", "vm_cpu_s", "foreign_max30", "foreign_line",
	"samples", "standby", "exit", "verdict", "flag", "plant", "cmd", "ac", "gap_max_s"}

func (l resultLine) String() string {
	var out []string
	seen := map[string]bool{}
	for _, k := range resultOrder {
		if v, ok := l[k]; ok {
			out = append(out, k+"="+clean(v))
			seen[k] = true
		}
	}
	var rest []string
	for k := range l {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		out = append(out, k+"="+clean(l[k]))
	}
	return strings.Join(out, "\t")
}

func clean(v string) string { return strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(v) }

// parseResultLine reads a resultLine back.
func parseResultLine(s string) resultLine {
	l := resultLine{}
	for _, f := range strings.Split(strings.TrimRight(s, "\r\n"), "\t") {
		if k, v, ok := strings.Cut(f, "="); ok {
			l[k] = v
		}
	}
	return l
}
