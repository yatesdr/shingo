//go:build windows

package main

// The little of kernel32 and ntdll gatetime needs: a job object to own a run's
// processes and their CPU. Ported from the baseline's winjob/win
// (work/loc-census-2026-11-20/tools/base/winjob), which timed every package of
// the 2026-11-22 baseline; what changed is said at each function.

import (
	"fmt"
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
)

const jobBasicAccounting = 1 // JobObjectBasicAccountingInformation

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
// process already inherited (Windows 8 and later), so a gatetime run started
// inside another job (a harness's, or another gatetime run's) still gets its
// own.
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
