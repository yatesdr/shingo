//go:build !windows

package main

// Off Windows there is no job object. What that costs:
//   - CPU is the command's rusage once waited for, which on Linux includes the
//     descendants it waited for and misses any it left running (an orphan);
//   - the count of processes it started is unknown: children is -1, which a
//     reader of the manifests must treat as "may have started children".
// The gate wraps only its Windows-native test steps; race and the cert tier's
// sim run in WSL and stay unwrapped.

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

type jobRun struct{ cmd *exec.Cmd }

func startInJob(cmd *exec.Cmd) (*jobRun, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &jobRun{cmd: cmd}, nil
}

func (r *jobRun) wait() (waitErr error, cpu time.Duration, children int, jobNote string) {
	waitErr = r.cmd.Wait()
	cpu, children, jobNote = -1, -1, "no job object on this OS: cpu is the waited rusage, children unknown"
	if ps := r.cmd.ProcessState; ps != nil {
		cpu = ps.UserTime() + ps.SystemTime()
	}
	return
}

// exitFor is the code the wrapper exits with. A binary killed by a signal is
// re-raised on the wrapper, so go test reports "signal: killed" as it would
// have without the wrapper; when the re-raise does not end the process, 128
// plus the signal number is the shell's convention.
func exitFor(waitErr error) int {
	var ee *exec.ExitError
	if !errors.As(waitErr, &ee) {
		if waitErr != nil {
			return 1
		}
		return 0
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		sig := ws.Signal()
		signal.Reset(sig)
		if p, err := os.FindProcess(os.Getpid()); err == nil {
			_ = p.Signal(sig)
			time.Sleep(100 * time.Millisecond)
		}
		return 128 + int(sig)
	}
	return ee.ExitCode()
}
