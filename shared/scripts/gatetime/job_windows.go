//go:build windows

package main

// The little of kernel32 and ntdll gatetime needs: a job object to own a run's
// processes and their CPU. Ported from the baseline's winjob/win
// (work/loc-census-2026-11-20/tools/base/winjob), which timed every package of
// the 2026-11-22 baseline; what changed is said at each function.

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	k32           = syscall.NewLazyDLL("kernel32.dll")
	ntdll         = syscall.NewLazyDLL("ntdll.dll")
	procCreateJob = k32.NewProc("CreateJobObjectW")
	procAssign    = k32.NewProc("AssignProcessToJobObject")
	procQueryJob  = k32.NewProc("QueryInformationJobObject")
	procSetJob    = k32.NewProc("SetInformationJobObject")
	procResume    = ntdll.NewProc("NtResumeProcess")
)

const (
	createSuspended = 0x00000004
	// What AssignProcessToJobObject and NtResumeProcess need, and the
	// process's times.
	processAccess = 0x0001 | 0x0100 | 0x0800 | 0x1000 // TERMINATE | SET_QUOTA | SUSPEND_RESUME | QUERY_LIMITED_INFORMATION

	jobBasicAccounting = 1 // JobObjectBasicAccountingInformation
	jobExtendedLimits  = 9 // JobObjectExtendedLimitInformation
	killOnJobClose     = 0x2000
)

// job is a job object.
type job struct{ h syscall.Handle }

func newJob() (*job, error) {
	h, _, err := procCreateJob.Call(0, 0)
	if h == 0 {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	return &job{h: syscall.Handle(h)}, nil
}

// assign puts a process in the job. The job is nested under any job the
// process already inherited (Windows 8 and later), so a wrapper that itself
// runs under `gatetime run`, or under a harness's job, still gets its own.
func (j *job) assign(h syscall.Handle) error {
	if r, _, err := procAssign.Call(uintptr(j.h), uintptr(h)); r == 0 {
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return nil
}

// accounting is the user and kernel time of every process that has been in
// the job, exited ones included, and how many processes it has held.
func (j *job) accounting() (cpu time.Duration, total uint32, err error) {
	var info struct {
		User, Kernel, PeriodUser, PeriodKernel int64
		Faults, Total, Active, Terminated      uint32
	}
	if r, _, e := procQueryJob.Call(uintptr(j.h), jobBasicAccounting, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info), 0); r == 0 {
		return 0, 0, fmt.Errorf("QueryInformationJobObject: %w", e)
	}
	return time.Duration((info.User + info.Kernel) * 100), info.Total, nil
}

// extendedLimits is JOBOBJECT_EXTENDED_LIMIT_INFORMATION; Go's alignment of
// these fields is the C layout on amd64 and arm64.
type extendedLimits struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
	IoInfo                  [6]uint64
	ProcessMemoryLimit      uintptr
	JobMemoryLimit          uintptr
	PeakProcessMemoryUsed   uintptr
	PeakJobMemoryUsed       uintptr
}

// killOnClose sets or clears JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE. Set while the
// test binary runs, so a wrapper that go test kills (its -timeout, an
// interrupted gate) takes the binary with it, as killing the binary itself did
// before there was a wrapper. Cleared before a normal exit, so a process the
// binary left running outlives the wrapper exactly as it outlived the binary.
// New here: the baseline's testexec had neither half.
func (j *job) killOnClose(on bool) error {
	var l extendedLimits
	if on {
		l.LimitFlags = killOnJobClose
	}
	if r, _, err := procSetJob.Call(uintptr(j.h), jobExtendedLimits, uintptr(unsafe.Pointer(&l)), unsafe.Sizeof(l)); r == 0 {
		return fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return nil
}

// jobRun is one command started in a job of its own.
type jobRun struct {
	cmd    *exec.Cmd
	job    *job
	jobErr error // why the job's accounting is missing; nil when it is there
}

// startInJob starts cmd suspended, puts it in a new job and resumes it, so the
// job holds the binary and everything it starts from its first instruction,
// and nothing of the wrapper. The baseline's testexec put itself in the job
// instead and subtracted its own CPU; starting suspended makes the count of
// processes exact (TotalProcesses less one is the children). When the job
// cannot be had the command still runs, unaccounted: the wrapper records, it
// never decides whether a test runs.
func startInJob(cmd *exec.Cmd) (*jobRun, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createSuspended
	j, jerr := newJob()
	if jerr == nil {
		jerr = j.killOnClose(true)
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	r := &jobRun{cmd: cmd, job: j}
	h, err := syscall.OpenProcess(processAccess, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("OpenProcess: %w", err)
	}
	defer syscall.CloseHandle(h) // a handle only used below; nothing to report on close
	if jerr == nil {
		jerr = j.assign(h)
	}
	r.jobErr = jerr
	if st, _, _ := procResume.Call(uintptr(h)); st != 0 {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("NtResumeProcess: status 0x%x", st)
	}
	return r, nil
}

// wait waits for the command and reads the job. cpu and children are -1 when
// the job's accounting is missing; jobNote says why, or "ok".
func (r *jobRun) wait() (waitErr error, cpu time.Duration, children int, jobNote string) {
	waitErr = r.cmd.Wait()
	cpu, children, jobNote = -1, -1, "ok"
	if r.jobErr != nil {
		jobNote = r.jobErr.Error()
		return
	}
	var notes []string
	if c, total, err := r.job.accounting(); err != nil {
		notes = append(notes, err.Error())
	} else {
		cpu, children = c, int(total)-1
	}
	if err := r.job.killOnClose(false); err != nil {
		notes = append(notes, err.Error())
	}
	if len(notes) > 0 {
		jobNote = strings.Join(notes, "; ")
	}
	return
}

// exitFor is the code the wrapper exits with for the command's outcome:
// Windows has no signals to re-raise, so it is the command's exit code.
func exitFor(waitErr error) int {
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		return ee.ExitCode()
	}
	if waitErr != nil {
		return 1
	}
	return 0
}
