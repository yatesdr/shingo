//go:build windows

package main

// The machine `gatetime run` samples, on Windows: the host's CPU
// (GetSystemTimes), every process's CPU in one call
// (NtQuerySystemInformation), the run's job, the power line
// (GetSystemPowerStatus), the standby requirement (SetThreadExecutionState)
// and the Modern Standby log (Kernel-Power 506/507, through wevtutil). Ported
// from the baseline's winjob/win; what is new is said at each function.

import (
	"context"
	"encoding/binary"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

var (
	procSystemTimes = k32.NewProc("GetSystemTimes")
	procPower       = k32.NewProc("GetSystemPowerStatus")
	procExecState   = k32.NewProc("SetThreadExecutionState")
	procQuerySystem = ntdll.NewProc("NtQuerySystemInformation")
)

type winMachine struct{ j *job }

func newMachine() machine { return &winMachine{} }

// enterJob creates a job and puts gatetime itself in it, so the command and
// everything it starts are in it too, orphans included.
func (w *winMachine) enterJob() error {
	j, err := newJob()
	if err != nil {
		return err
	}
	self, err := syscall.GetCurrentProcess()
	if err != nil {
		return err
	}
	if err := j.assign(self); err != nil {
		return err
	}
	w.j = j
	return nil
}

func (w *winMachine) jobCPU() (time.Duration, error) {
	if w.j == nil {
		return 0, fmt.Errorf("no job")
	}
	c, _, err := w.j.accounting()
	return c, err
}

func (w *winMachine) selfCPU() time.Duration {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0
	}
	var c, e, k, u syscall.Filetime
	if syscall.GetProcessTimes(h, &c, &e, &k, &u) != nil {
		return 0
	}
	return time.Duration((ft(k) + ft(u)) * 100)
}

func ft(f syscall.Filetime) int64 { return int64(f.HighDateTime)<<32 | int64(f.LowDateTime) }

// hostBusy is the CPU time every logical processor has spent not idle since
// boot. GetSystemTimes' kernel time includes its idle time.
func (w *winMachine) hostBusy() (time.Duration, error) {
	var i, k, u syscall.Filetime
	if r, _, err := procSystemTimes.Call(uintptr(unsafe.Pointer(&i)), uintptr(unsafe.Pointer(&k)), uintptr(unsafe.Pointer(&u))); r == 0 {
		return 0, fmt.Errorf("GetSystemTimes: %w", err)
	}
	return time.Duration((ft(k) + ft(u) - ft(i)) * 100), nil
}

// procs is every process's cumulative CPU, read in one
// NtQuerySystemInformation(SystemProcessInformation) call. New here: the
// baseline opened each process, and a user cannot open vmmemWSL (Get-Process
// shows its CPU blank), whose CPU this call does report.
func (w *winMachine) procs() (map[uint32]procCPU, error) {
	const (
		systemProcessInformation = 5
		infoLengthMismatch       = 0xC0000004
	)
	buf := make([]byte, 1<<20)
	for {
		var n uint32
		st, _, _ := procQuerySystem.Call(systemProcessInformation, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&n)))
		if st == infoLengthMismatch && len(buf) < 64<<20 {
			buf = make([]byte, max(2*len(buf), int(n)+64<<10))
			continue
		}
		if st != 0 {
			return nil, fmt.Errorf("NtQuerySystemInformation: status 0x%x", st)
		}
		return parseProcessInfo(buf), nil
	}
}

// parseProcessInfo walks SYSTEM_PROCESS_INFORMATION records (x64 layout:
// UserTime at 0x28, KernelTime at 0x30, ImageName at 0x38, UniqueProcessId at
// 0x50). The name's buffer points into buf itself.
func parseProcessInfo(buf []byte) map[uint32]procCPU {
	out := map[uint32]procCPU{}
	base := uintptr(unsafe.Pointer(&buf[0]))
	for off := 0; off+0x58 <= len(buf); {
		rec := buf[off:]
		next := binary.LittleEndian.Uint32(rec[0:])
		user := int64(binary.LittleEndian.Uint64(rec[0x28:]))
		kern := int64(binary.LittleEndian.Uint64(rec[0x30:]))
		nameLen := int(binary.LittleEndian.Uint16(rec[0x38:]))
		namePtr := uintptr(binary.LittleEndian.Uint64(rec[0x40:]))
		pid := uint32(binary.LittleEndian.Uint64(rec[0x50:]))
		name := "System Idle Process"
		if namePtr != 0 && nameLen > 0 {
			start := int(namePtr - base)
			if start >= 0 && start+nameLen <= len(buf) {
				u := make([]uint16, nameLen/2)
				for i := range u {
					u[i] = binary.LittleEndian.Uint16(buf[start+2*i:])
				}
				name = string(utf16.Decode(u))
			}
		}
		if pid != 0 { // the idle process's "CPU" is idle time
			out[pid] = procCPU{name: name, cpu: time.Duration((user + kern) * 100)}
		}
		if next == 0 {
			break
		}
		off += int(next)
	}
	return out
}

// jobPids are the job's live processes.
func (w *winMachine) jobPids() (map[uint32]bool, error) {
	if w.j == nil {
		return nil, fmt.Errorf("no job")
	}
	const (
		basicProcessIDList = 3
		moreData           = syscall.Errno(234)
	)
	for n := 1024; ; n *= 2 {
		// DWORD assigned, DWORD in-list, then ULONG_PTR ids
		buf := make([]uintptr, n+1)
		r, _, err := procQueryJob.Call(uintptr(w.j.h), basicProcessIDList,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))*unsafe.Sizeof(buf[0]), 0)
		if r == 0 {
			if err == moreData && n < 64<<10 {
				continue
			}
			return nil, fmt.Errorf("QueryInformationJobObject (process list): %w", err)
		}
		counts := (*[2]uint32)(unsafe.Pointer(&buf[0]))
		out := map[uint32]bool{}
		for _, id := range buf[1 : 1+int(counts[1])] {
			out[uint32(id)] = true
		}
		return out, nil
	}
}

// ac is 1 on mains power, 0 on battery, -1 when Windows does not know.
func (w *winMachine) ac() (int, error) {
	var p struct {
		AC, Flag, Percent, Status byte
		Life, FullLife            uint32
	}
	if r, _, err := procPower.Call(uintptr(unsafe.Pointer(&p))); r == 0 {
		return -1, fmt.Errorf("GetSystemPowerStatus: %w", err)
	}
	switch p.AC {
	case 0:
		return 0, nil
	case 1:
		return 1, nil
	}
	return -1, nil
}

// stayAwake holds the system AND the display requirement for as long as the
// calling OS thread lives. New here: the baseline held only the system one,
// and Modern Standby came eight times in its window (BASELINE "Machine
// state"); Windows should enter Modern Standby when the screen turns off,
// which the system requirement alone does not stop.
func (w *winMachine) stayAwake() { setExecState(esAwake) }

// esAwake is ES_CONTINUOUS | ES_SYSTEM_REQUIRED | ES_DISPLAY_REQUIRED.
const esAwake = 0x80000000 | 0x00000001 | 0x00000002

// setExecState sets the calling thread's execution state and returns the
// previous one (0 when the call failed).
func setExecState(flags uint32) uint32 {
	prev, _, _ := procExecState.Call(uintptr(flags))
	return uint32(prev)
}

var eventTimeRE = regexp.MustCompile(`<EventID>(50[67])</EventID>.*?SystemTime='([^']+)'`)

// standby is every Kernel-Power 506 (entered Modern Standby) and 507 (left
// it) the System log holds between from and to.
func (w *winMachine) standby(from, to time.Time) ([]standbyEvent, error) {
	q := fmt.Sprintf("*[System[Provider[@Name='Microsoft-Windows-Kernel-Power'] and (EventID=506 or EventID=507) and TimeCreated[@SystemTime>='%s' and @SystemTime<='%s']]]",
		from.UTC().Format("2006-01-02T15:04:05.000Z"), to.UTC().Format("2006-01-02T15:04:05.000Z"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "wevtutil", "qe", "System", "/q:"+q, "/f:xml").Output()
	if err != nil {
		return nil, fmt.Errorf("wevtutil: %w", err)
	}
	var evs []standbyEvent
	for _, ev := range strings.SplitAfter(string(out), "</Event>") {
		m := eventTimeRE.FindStringSubmatch(ev)
		if m == nil {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, m[2])
		if err != nil {
			return nil, fmt.Errorf("wevtutil: event time %q: %w", m[2], err)
		}
		evs = append(evs, standbyEvent{id: m[1], at: t})
	}
	return evs, nil
}

// limits: nothing this OS cannot see.
func (w *winMachine) limits() []string { return nil }
