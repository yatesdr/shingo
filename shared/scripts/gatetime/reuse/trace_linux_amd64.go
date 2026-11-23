//go:build linux && amd64

package reuse

// THE LINUX TRACER. Two jobs, both exact by construction:
//
//  1. Count every process the binary or its descendants create. The wrapper
//     starts the binary (through the shim below) with PTRACE_TRACEME, and at
//     the exec stop, before the shim runs a line of its own, sets
//     PTRACE_O_TRACEFORK|TRACEVFORK|TRACECLONE: from then on every task any
//     traced task creates is attached by the kernel and stops once before it
//     runs, so none can be created unseen. A new task is a process when its
//     Tgid is its own tid (/proc/<tid>/status), else a thread.
//  2. See every path the binary and its children touch through a syscall. The
//     shim installs a seccomp filter (inherited by the binary and everything it
//     starts, kept across exec) that returns SECCOMP_RET_TRACE for the path
//     syscalls below; at each such stop the tracer reads the path out of the
//     stopped task and resolves it against its cwd or dirfd. This sees what the
//     test log cannot: reads in init, in package vars, in a TestMain before
//     m.Run, through raw syscalls, and by children.
//
// What it cannot follow is made to fail safe: a syscall from another ABI (x32,
// i386) or an open by handle marks the run unobservable (never stored), and
// io_uring_setup is refused with ENOSYS, as docker's own default profile does,
// so a program falls back to the plain syscalls the filter sees. If ptrace or
// the filter cannot be had, the run is redone untraced and counts as "children
// unknown" (whole-tree key).

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// ShimCommand is the subcommand the tracer starts the wrapper with to become
// the binary: the program's main must route it to ShimMain.
const ShimCommand = "seccomp-shim"

const (
	auditArchX8664     = 0xc000003e
	seccompRetAllow    = 0x7fff0000
	seccompRetTrace    = 0x7ff00000
	seccompRetErrno    = 0x00050000
	traceDataPath      = 1 // a path syscall: record it
	traceDataBlind     = 2 // cannot be followed: the run is unobservable
	sysSeccomp         = 317
	seccompSetFilter   = 1
	seccompFlagTsync   = 1
	prSetNoNewPrivs    = 38
	sysProcessVMReadv  = 310
	ptraceEventSeccomp = 7
	ptraceOTraceSeccmp = 0x80
	ptraceOExitKill    = 0x100000
	atFdcwd            = -100
	atEmptyPath        = 0x1000
	shimFailNoNewPrivs = 125
	shimFailFilter     = 124
	shimFailExec       = 123
)

// pathSyscalls are the traced syscalls: number, the argument slots holding
// (dirfd, path) pairs (dirfd -1: none, the path is cwd-relative), and the op.
var pathSyscalls = []struct {
	nr    uint64
	pairs [][2]int
	op    string
}{
	{2, [][2]int{{-1, 0}}, OpOpen},           // open
	{85, [][2]int{{-1, 0}}, OpOpen},          // creat
	{257, [][2]int{{0, 1}}, OpOpen},          // openat
	{437, [][2]int{{0, 1}}, OpOpen},          // openat2
	{4, [][2]int{{-1, 0}}, OpStat},           // stat
	{6, [][2]int{{-1, 0}}, OpStat},           // lstat
	{262, [][2]int{{0, 1}}, OpStat},          // newfstatat
	{332, [][2]int{{0, 1}}, OpStat},          // statx
	{21, [][2]int{{-1, 0}}, OpStat},          // access
	{269, [][2]int{{0, 1}}, OpStat},          // faccessat
	{439, [][2]int{{0, 1}}, OpStat},          // faccessat2
	{89, [][2]int{{-1, 0}}, OpStat},          // readlink
	{267, [][2]int{{0, 1}}, OpStat},          // readlinkat
	{59, [][2]int{{-1, 0}}, OpExec},          // execve
	{322, [][2]int{{0, 1}}, OpExec},          // execveat
	{76, [][2]int{{-1, 0}}, OpStat},          // truncate
	{80, [][2]int{{-1, 0}}, OpStat},          // chdir
	{83, [][2]int{{-1, 0}}, OpStat},          // mkdir
	{258, [][2]int{{0, 1}}, OpStat},          // mkdirat
	{84, [][2]int{{-1, 0}}, OpStat},          // rmdir
	{87, [][2]int{{-1, 0}}, OpStat},          // unlink
	{263, [][2]int{{0, 1}}, OpStat},          // unlinkat
	{82, [][2]int{{-1, 0}, {-1, 1}}, OpStat}, // rename
	{264, [][2]int{{0, 1}, {2, 3}}, OpStat},  // renameat
	{316, [][2]int{{0, 1}, {2, 3}}, OpStat},  // renameat2
	{86, [][2]int{{-1, 0}, {-1, 1}}, OpStat}, // link
	{265, [][2]int{{0, 1}, {2, 3}}, OpStat},  // linkat
	{88, [][2]int{{-1, 1}}, OpStat},          // symlink (arg 0 is the target text)
	{266, [][2]int{{1, 2}}, OpStat},          // symlinkat
	{133, [][2]int{{-1, 0}}, OpStat},         // mknod
	{259, [][2]int{{0, 1}}, OpStat},          // mknodat
	{90, [][2]int{{-1, 0}}, OpStat},          // chmod
	{268, [][2]int{{0, 1}}, OpStat},          // fchmodat
	{92, [][2]int{{-1, 0}}, OpStat},          // chown
	{94, [][2]int{{-1, 0}}, OpStat},          // lchown
	{260, [][2]int{{0, 1}}, OpStat},          // fchownat
	{132, [][2]int{{-1, 0}}, OpStat},         // utime
	{235, [][2]int{{-1, 0}}, OpStat},         // utimes
	{261, [][2]int{{0, 1}}, OpStat},          // futimesat
	{280, [][2]int{{0, 1}}, OpStat},          // utimensat
	{137, [][2]int{{-1, 0}}, OpStat},         // statfs
	{161, [][2]int{{-1, 0}}, OpStat},         // chroot
	{191, [][2]int{{-1, 0}}, OpStat},         // getxattr
	{192, [][2]int{{-1, 0}}, OpStat},         // lgetxattr
	{194, [][2]int{{-1, 0}}, OpStat},         // listxattr
	{195, [][2]int{{-1, 0}}, OpStat},         // llistxattr
	{254, [][2]int{{-1, 1}}, OpStat},         // inotify_add_watch
	{303, nil, ""},                           // name_to_handle_at: blind
	{304, nil, ""},                           // open_by_handle_at: blind
	{134, nil, ""},                           // uselib: blind
}

const sysIoUringSetup = 425

type sockFilter struct {
	code   uint16
	jt, jf uint8
	k      uint32
}

type sockFprog struct {
	n      uint16
	filter *sockFilter
}

// filterProgram is the shim's seccomp program.
func filterProgram() []sockFilter {
	const (
		ldAbs = 0x20 // BPF_LD|BPF_W|BPF_ABS
		jeq   = 0x15 // BPF_JMP|BPF_JEQ|BPF_K
		jge   = 0x35 // BPF_JMP|BPF_JGE|BPF_K
		ret   = 0x06 // BPF_RET|BPF_K
	)
	nrs := make([]uint64, 0, len(pathSyscalls))
	blind := map[uint64]bool{}
	for _, s := range pathSyscalls {
		nrs = append(nrs, s.nr)
		blind[s.nr] = s.op == ""
	}
	// 0 ld arch; 1 jeq x86_64 ? 3 : 2; 2 ret blind; 3 ld nr; 4 jge x32 ? blind;
	// 5 jeq io_uring_setup ? enosys; then one jeq per traced number; allow;
	// trace-path; trace-blind; enosys.
	n := len(nrs)
	allow := 6 + n
	tracePath, traceBlind, enosys := allow+1, allow+2, allow+3
	p := []sockFilter{
		{ldAbs, 0, 0, 4},
		{jeq, 1, 0, auditArchX8664},
		{ret, 0, 0, seccompRetTrace | traceDataBlind},
		{ldAbs, 0, 0, 0},
		{jge, uint8(traceBlind - 5), 0, 0x40000000},
		{jeq, uint8(enosys - 6), 0, sysIoUringSetup},
	}
	for i, nr := range nrs {
		at := 6 + i
		to := tracePath
		if blind[nr] {
			to = traceBlind
		}
		p = append(p, sockFilter{jeq, uint8(to - (at + 1)), 0, uint32(nr)})
	}
	return append(p,
		sockFilter{ret, 0, 0, seccompRetAllow},
		sockFilter{ret, 0, 0, seccompRetTrace | traceDataPath},
		sockFilter{ret, 0, 0, seccompRetTrace | traceDataBlind},
		sockFilter{ret, 0, 0, seccompRetErrno | uint32(syscall.ENOSYS)})
}

// ShimMain becomes the test binary: no new privileges, the filter, exec. It
// prints nothing (its stdout is the binary's); the tracer reads its exit code.
func ShimMain(args []string) int {
	if len(args) == 0 {
		return shimFailExec
	}
	runtime.LockOSThread()
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); e != 0 {
		return shimFailNoNewPrivs
	}
	prog := filterProgram()
	fp := sockFprog{n: uint16(len(prog)), filter: &prog[0]}
	if r, _, e := syscall.RawSyscall(sysSeccomp, seccompSetFilter, seccompFlagTsync, uintptr(unsafe.Pointer(&fp))); e != 0 || r != 0 {
		return shimFailFilter
	}
	runtime.KeepAlive(prog)
	err := syscall.Exec(args[0], args, os.Environ())
	_ = err
	return shimFailExec
}

// DefaultRunner is the tracer, falling back to an untraced run when the
// tracer cannot start the binary.
func DefaultRunner(spec RunSpec) RunResult {
	r, ok := ptraceRun(spec)
	if ok {
		return r
	}
	p := plainRun(spec)
	p.ChildHow = "unknown: the tracer could not run here (" + r.ChildHow + ")"
	return p
}

// ptraceRun runs spec under the tracer. ok is false when the binary never ran
// (ptrace or the filter refused): the caller runs it again untraced.
func ptraceRun(spec RunSpec) (RunResult, bool) {
	self, err := os.Executable()
	if err != nil {
		return RunResult{ChildHow: "no executable: " + err.Error()}, false
	}
	type done struct {
		r  RunResult
		ok bool
	}
	ch := make(chan done, 1)
	go func() {
		// Every ptrace request must come from the tracer thread: the thread that
		// forked the shim. It is never unlocked, so it exits with this goroutine.
		runtime.LockOSThread()
		r, ok := traceLoop(self, spec)
		ch <- done{r, ok}
	}()
	d := <-ch
	return d.r, d.ok
}

type tracer struct {
	root      int
	tgid      map[int]int  // tid -> its process
	live      map[int]bool // tids traced and not yet reaped (their pids cannot be reused)
	known     map[int]bool // tids seen stopped at least once
	procs     map[int]bool // processes the binary started (not root)
	active    map[int]bool // of those, the ones that exec'd a program or made a path syscall
	execs     int          // exec events of the root: 1 is the shim becoming the binary
	started   bool
	parent    pathSet
	children  pathSet
	blind     string
	rootState syscall.WaitStatus
	rootDone  bool
	leaked    bool
}

func traceLoop(self string, spec RunSpec) (RunResult, bool) {
	rd, wr, err := os.Pipe()
	if err != nil {
		return RunResult{ChildHow: "pipe: " + err.Error()}, false
	}
	copied := make(chan struct{})
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := rd.Read(buf)
			if n > 0 {
				_, _ = spec.Out.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		close(copied)
	}()
	argv := append([]string{self, ShimCommand, spec.Path}, spec.Args...)
	attr := &syscall.ProcAttr{Dir: spec.Dir, Env: spec.Env, Files: []uintptr{os.Stdin.Fd(), wr.Fd(), wr.Fd()},
		Sys: &syscall.SysProcAttr{Ptrace: true, Pdeathsig: syscall.SIGKILL}}
	t0 := time.Now()
	pid, err := syscall.ForkExec(self, argv, attr)
	wr.Close()
	if err != nil {
		rd.Close()
		<-copied
		return RunResult{ChildHow: "fork: " + err.Error()}, false
	}
	t := &tracer{root: pid, tgid: map[int]int{pid: pid}, live: map[int]bool{pid: true}, known: map[int]bool{},
		procs: map[int]bool{}, active: map[int]bool{}, parent: pathSet{}, children: pathSet{}}
	stopFwd := forwardSignals(func(s os.Signal) {
		if sig, ok := s.(syscall.Signal); ok {
			_ = syscall.Kill(pid, sig)
		}
	})
	setupErr := t.loop()
	stopFwd()
	<-copied
	rd.Close()
	if !t.started {
		why := setupErr
		if why == "" {
			why = fmt.Sprintf("the shim exited %d before the binary started", t.rootState.ExitStatus())
		}
		return RunResult{ChildHow: why}, false
	}
	return t.result(time.Since(t0)), true
}

// loop is the wait loop, on the tracer thread, until every tracee is gone.
func (t *tracer) loop() string {
	for {
		var ws syscall.WaitStatus
		tid, err := syscall.Wait4(-1, &ws, syscall.WALL, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil { // ECHILD: nothing left
			return ""
		}
		if ws.Exited() || ws.Signaled() {
			delete(t.live, tid)
			if tid == t.root {
				t.rootState, t.rootDone = ws, true
				t.killLeft()
			}
			continue
		}
		if !ws.Stopped() {
			continue
		}
		if first := !t.known[tid]; first {
			t.known[tid] = true
			if tid == t.root && t.execs == 0 {
				// The shim's own exec stop: set the options before it runs.
				if err := syscall.PtraceSetOptions(tid, syscall.PTRACE_O_TRACEFORK|syscall.PTRACE_O_TRACEVFORK|
					syscall.PTRACE_O_TRACECLONE|syscall.PTRACE_O_TRACEEXEC|ptraceOTraceSeccmp|ptraceOExitKill); err != nil {
					_ = syscall.Kill(tid, syscall.SIGKILL)
					return "ptrace options: " + err.Error()
				}
				t.execs = 1
				_ = syscall.PtraceCont(tid, 0)
				continue
			}
			t.classify(tid)
			if ws.StopSignal() == syscall.SIGSTOP {
				_ = syscall.PtraceCont(tid, 0) // a new task's first stop
				continue
			}
		}
		t.stop(tid, ws)
	}
}

// stop handles one ptrace stop of a known task and resumes it.
func (t *tracer) stop(tid int, ws syscall.WaitStatus) {
	sig := ws.StopSignal()
	if sig != syscall.SIGTRAP {
		_ = syscall.PtraceCont(tid, int(sig)) // a signal for the task: deliver it
		return
	}
	switch ws.TrapCause() {
	case ptraceEventSeccomp:
		t.seccomp(tid)
	case syscall.PTRACE_EVENT_FORK, syscall.PTRACE_EVENT_VFORK, syscall.PTRACE_EVENT_CLONE:
		if msg, err := syscall.PtraceGetEventMsg(tid); err == nil {
			t.classify(int(msg))
		}
	case syscall.PTRACE_EVENT_EXEC:
		g := t.tgidOf(tid)
		switch {
		case g != t.root:
			t.active[g] = true // a child ran a program
		case t.execs+1 == 2:
			t.execs, t.started = 2, true // the shim became the binary
		default:
			t.execs++
			t.procs[-t.execs], t.active[-t.execs] = true, true // the binary replaced itself: a child
		}
	case 0:
		_ = syscall.PtraceCont(tid, int(syscall.SIGTRAP)) // a real SIGTRAP
		return
	}
	_ = syscall.PtraceCont(tid, 0)
}

// classify records a task the first time it is seen: a thread of a process
// already known, or a new process.
func (t *tracer) classify(tid int) {
	if _, ok := t.tgid[tid]; ok {
		return
	}
	g := readTgid(tid)
	if g <= 0 {
		g = tid // unreadable: counted as a process of its own (fail safe)
	}
	t.tgid[tid], t.live[tid] = g, true
	if g != t.root {
		t.procs[g] = true
	}
}

func (t *tracer) tgidOf(tid int) int {
	if g, ok := t.tgid[tid]; ok {
		return g
	}
	t.classify(tid)
	return t.tgid[tid]
}

func readTgid(tid int) int {
	f, err := os.Open("/proc/" + strconv.Itoa(tid) + "/status")
	if err != nil {
		return 0
	}
	defer f.Close() // read-only
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "Tgid:"); ok {
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			return n
		}
	}
	return 0
}

// killLeft ends every task still traced once the binary has exited: children
// it left running. Without this the wrapper would wait on them; the run is
// marked, and never stored.
func (t *tracer) killLeft() {
	for tid := range t.live {
		if t.tgid[tid] != t.root {
			if err := syscall.Kill(tid, syscall.SIGKILL); err == nil {
				t.leaked = true
			}
		}
	}
}

// seccomp records the path(s) of one traced syscall.
func (t *tracer) seccomp(tid int) {
	if g := t.tgidOf(tid); g != t.root {
		t.active[g] = true // a child that makes a path syscall may read: it counts
	}
	data, err := syscall.PtraceGetEventMsg(tid)
	if err != nil {
		t.markBlind(fmt.Sprintf("no event data for a traced syscall: %v", err))
		return
	}
	var regs syscall.PtraceRegs
	if err := syscall.PtraceGetRegs(tid, &regs); err != nil {
		t.markBlind("registers unreadable: " + err.Error())
		return
	}
	if data&0xffff == traceDataBlind {
		t.markBlind(fmt.Sprintf("syscall %d (another ABI or an open by handle)", regs.Orig_rax))
		return
	}
	args := [6]uint64{regs.Rdi, regs.Rsi, regs.Rdx, regs.R10, regs.R8, regs.R9}
	for _, s := range pathSyscalls {
		if s.nr != regs.Orig_rax {
			continue
		}
		for _, pair := range s.pairs {
			p, err := t.resolve(tid, pair, args)
			if err != nil {
				t.markBlind(fmt.Sprintf("syscall %d: %v", s.nr, err))
				return
			}
			t.addPath(tid, p, s.op)
		}
		return
	}
}

func (t *tracer) markBlind(why string) {
	if t.blind == "" {
		t.blind = why
	}
}

func (t *tracer) addPath(tid int, p, op string) {
	if t.tgidOf(tid) == t.root {
		t.parent.add(p, op, false)
	} else {
		t.children.add(p, op, true)
	}
}

// resolve reads one (dirfd, path) argument pair as an absolute path.
func (t *tracer) resolve(tid int, pair [2]int, args [6]uint64) (string, error) {
	p, err := readString(tid, uintptr(args[pair[1]]))
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	base := "/proc/" + strconv.Itoa(tid) + "/cwd"
	if pair[0] >= 0 {
		if fd := int32(args[pair[0]]); fd != atFdcwd {
			base = "/proc/" + strconv.Itoa(tid) + "/fd/" + strconv.Itoa(int(fd))
		}
	}
	dir, err := os.Readlink(base)
	if err != nil {
		return "", fmt.Errorf("%s: %v", base, err)
	}
	if !filepath.IsAbs(dir) {
		return "", nil // a pipe, socket or anon inode ("pipe:[N]"): no path to keep
	}
	if p == "" {
		return filepath.Clean(dir), nil // AT_EMPTY_PATH: the fd itself
	}
	return filepath.Join(dir, p), nil
}

// readString reads a NUL-terminated string (at most 4096 bytes) out of a
// stopped task, a page at a time so a read never crosses into an unmapped one.
func readString(tid int, addr uintptr) (string, error) {
	if addr == 0 {
		return "", fmt.Errorf("null path")
	}
	var out []byte
	for len(out) < 4096 {
		n := 4096 - int(addr%4096)
		buf := make([]byte, n)
		got, err := readMem(tid, addr, buf)
		if err != nil {
			return "", err
		}
		if i := indexByte(buf[:got], 0); i >= 0 {
			return string(append(out, buf[:i]...)), nil
		}
		out = append(out, buf[:got]...)
		addr += uintptr(got)
		if got < n {
			return "", fmt.Errorf("short read")
		}
	}
	return "", fmt.Errorf("path longer than 4096 bytes")
}

type iovec struct {
	base uintptr
	n    uint64
}

func readMem(tid int, addr uintptr, buf []byte) (int, error) {
	local := iovec{uintptr(unsafe.Pointer(&buf[0])), uint64(len(buf))}
	remote := iovec{addr, uint64(len(buf))}
	n, _, e := syscall.Syscall6(sysProcessVMReadv, uintptr(tid), uintptr(unsafe.Pointer(&local)), 1,
		uintptr(unsafe.Pointer(&remote)), 1, 0)
	runtime.KeepAlive(buf)
	if e == 0 {
		return int(n), nil
	}
	// process_vm_readv refused (a policy, a kernel): the tracer may read
	// /proc/<tid>/mem instead.
	f, err := os.Open("/proc/" + strconv.Itoa(tid) + "/mem")
	if err != nil {
		return 0, fmt.Errorf("process_vm_readv: %v; mem: %v", e, err)
	}
	defer f.Close() // read-only
	return f.ReadAt(buf, int64(addr))
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

func (t *tracer) result(wall time.Duration) RunResult {
	// A process that neither ran a program nor made one path syscall cannot have
	// read a path: every path syscall of every traced task stops here, and a
	// fork that did not exec runs the parent's own code. go1.25's os package
	// forks exactly such a process (syscall's clone-pidfd check, which exits at
	// once) the first time a binary calls os.StartProcess or os.FindProcess, so
	// it is counted, as SilentForks, but does not key the binary on the tree.
	silent := 0
	for g := range t.procs {
		if !t.active[g] {
			silent++
		}
	}
	r := RunResult{Wall: wall, Processes: len(t.procs) - silent, SilentForks: silent, ChildHow: "ptrace", Traced: true,
		Parent: t.parent, Children: t.children, Unobservable: t.blind}
	if t.leaked && r.Unobservable == "" {
		r.Unobservable = "children were still running when the binary exited (killed)"
	}
	ws := t.rootState
	switch {
	case !t.rootDone:
		r.Code, r.Unobservable = 1, "the binary's exit was not seen"
	case ws.Signaled():
		r.Signaled, r.Signal, r.Code = true, ws.Signal(), 128+int(ws.Signal())
	default:
		r.Code = ws.ExitStatus()
	}
	return r
}
