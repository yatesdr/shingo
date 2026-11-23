//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"time"

	"shingo/shared/scripts/gatetime/reuse"
)

// reuseRunner runs a binary for package reuse in a job object of its own
// (startInJob), so the count of processes it started is exact: the job's
// TotalProcesses less the binary. Windows has no path trace: a child's reads,
// a package var's and a raw syscall's are not seen (reuse's exec.go header).
var reuseRunner reuse.Runner = func(spec reuse.RunSpec) reuse.RunResult {
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Env, cmd.Dir = spec.Env, spec.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, spec.Out, spec.Out
	// A process the binary left holding the pipe would hold the wrapper open.
	cmd.WaitDelay = 5 * time.Second
	t0 := time.Now()
	run, err := startInJob(cmd)
	if err != nil {
		return reuse.RunResult{StartErr: err, Processes: -1}
	}
	waitErr, _, children, note := run.wait()
	r := reuse.RunResult{Wall: time.Since(t0), Code: exitFor(waitErr), Processes: children, ChildHow: "job"}
	if children < 0 {
		r.ChildHow = "unknown: " + note
	}
	var ee *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &ee) {
		r.Unobservable = "wait: " + waitErr.Error()
	}
	return r
}
