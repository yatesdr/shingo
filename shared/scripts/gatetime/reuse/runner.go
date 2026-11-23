package reuse

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// RunSpec is one run of a test binary.
type RunSpec struct {
	Path string
	Args []string // without argv[0]
	Env  []string
	Dir  string
	// Out receives the binary's stdout and stderr as one stream, as go test
	// hands a binary one writer for both.
	Out io.Writer
}

// RunResult is what a Runner saw.
type RunResult struct {
	// StartErr is set when the binary never ran.
	StartErr error
	Code     int  // exit code (when Signaled: 128 + the signal)
	Signaled bool // the binary died on a signal
	Signal   syscall.Signal
	Wall     time.Duration
	// Processes is how many processes the binary started, directly or not; -1
	// when nothing counted them. ChildHow is what counted ("ptrace", "job") or
	// why nothing did.
	Processes int
	ChildHow  string
	// SilentForks is how many more processes the tracer saw that neither ran a
	// program nor made a path syscall (not in Processes: they read nothing).
	SilentForks int
	// Traced is true when every path syscall of the binary and its children was
	// seen (the Linux tracer): Parent and Children are then complete.
	Traced   bool
	Parent   pathSet
	Children pathSet
	// Unobservable is why a traced run must not be stored: a syscall the trace
	// cannot follow (another ABI, a handle open), or children left running.
	Unobservable string
}

// Runner runs one test binary. The default is the Linux tracer where it
// exists (ptraceRun), else plainRun; the timing lane's Windows job object is
// another (it counts children exactly).
type Runner func(RunSpec) RunResult

// plainRun runs the binary with nothing watching it: children unknown.
func plainRun(spec RunSpec) RunResult {
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Env, cmd.Dir = spec.Env, spec.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, spec.Out, spec.Out
	// A child the binary left holding the pipe would hold the wrapper open.
	cmd.WaitDelay = 5 * time.Second
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		return RunResult{StartErr: err, Processes: -1}
	}
	stop := forwardSignals(func(s os.Signal) { _ = cmd.Process.Signal(s) })
	err := cmd.Wait()
	stop()
	r := RunResult{Wall: time.Since(t0), Processes: -1, ChildHow: "unknown: no job object or tracer here"}
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		r.Code = ee.ExitCode()
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			r.Signaled, r.Signal, r.Code = true, ws.Signal(), 128+int(ws.Signal())
		}
	case err != nil:
		r.Code = 1
		r.Unobservable = "wait: " + err.Error()
	}
	return r
}

// forwardSignals passes the signals go test sends a wrapper (SIGQUIT on its
// timeout, SIGTERM) on to the binary, which decides what they mean, as it did
// with no wrapper. An interrupt reaches both through the process group, and
// Main ignores it in the wrapper.
func forwardSignals(send func(os.Signal)) (stop func()) {
	if len(forwarded) == 0 {
		// signal.Notify with no signals would relay every signal.
		return func() {}
	}
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, forwarded...)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-ch:
				send(s)
			case <-done:
				return
			}
		}
	}()
	return func() { signal.Stop(ch); close(done) }
}
