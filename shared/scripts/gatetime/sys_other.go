//go:build !windows

package main

// Off Windows `gatetime run` builds and samples what it can. On its own it
// never calls a run clean: there is no job object to hold the command's
// orphans and their CPU, and no Modern Standby log, and limits() names that in
// every verdict. Inside WSL it reads the Windows host instead, through the
// Windows build as `gatetime host` (host.go), or, with no reader that answers,
// judges the run inside the VM only and says so. The timing tool's home is
// the Windows laptop and its WSL; plain Linux is here so the package builds,
// vets and tests in CI.

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type otherMachine struct{}

// newMachine is the host read from inside WSL when it can be (pickMachine),
// else this machine.
func newMachine() machine {
	self, _ := os.Executable()
	return pickMachine(localMachine(), os.Getenv(hostReaderEnv), inWSL(), self)
}

// localMachine is this machine's own readings.
func localMachine() machine { return otherMachine{} }

// inWSL is whether this is a WSL distro, by the variable WSL sets in every
// process it starts or by its interop handler.
func inWSL() bool {
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	_, err := os.Stat("/proc/sys/fs/binfmt_misc/WSLInterop")
	return err == nil
}

func (otherMachine) enterJob() error { return nil }

// jobCPU stands in for a job's CPU without a job object: this process and
// the children it waited for (rusage), plus every live descendant's CPU and
// the CPU of the children they waited for (/proc). A descendant that is
// orphaned leaves the tree, and its CPU with it, as a job would not let it.
func (otherMachine) jobCPU() (time.Duration, error) {
	var self, kids syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &self); err != nil {
		return 0, err
	}
	if err := syscall.Getrusage(syscall.RUSAGE_CHILDREN, &kids); err != nil {
		return 0, err
	}
	return tv(self.Utime) + tv(self.Stime) + tv(kids.Utime) + tv(kids.Stime) + liveDescendantsCPU(os.Getpid()), nil
}

// liveDescendantsCPU sums utime, stime, cutime and cstime (fields 14-17 of
// /proc/<pid>/stat, in USER_HZ ticks) over root's live descendants; 0 where
// there is no /proc.
func liveDescendantsCPU(root int) time.Duration {
	paths, _ := filepath.Glob("/proc/[0-9]*/stat")
	kids := map[int][]int{}
	ticks := map[int]int64{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue // gone since the listing
		}
		s := string(data)
		i := strings.LastIndexByte(s, ')') // comm may hold spaces and parentheses
		if i < 0 {
			continue
		}
		pid, err := strconv.Atoi(filepath.Base(filepath.Dir(p)))
		f := strings.Fields(s[i+1:]) // f[0] is field 3 (state)
		if err != nil || len(f) < 15 {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		var t int64
		for _, v := range f[11:15] {
			n, _ := strconv.ParseInt(v, 10, 64)
			t += n
		}
		kids[ppid] = append(kids[ppid], pid)
		ticks[pid] = t
	}
	var sum int64
	stack := append([]int(nil), kids[root]...)
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = append(stack[:len(stack)-1], kids[pid]...)
		sum += ticks[pid]
	}
	return time.Duration(sum) * (time.Second / 100)
}

func (otherMachine) selfCPU() time.Duration {
	var self syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &self) != nil {
		return 0
	}
	return tv(self.Utime) + tv(self.Stime)
}

func tv(t syscall.Timeval) time.Duration {
	return time.Duration(t.Sec)*time.Second + time.Duration(t.Usec)*time.Microsecond
}

// hostBusy reads /proc/stat's first line (user nice system idle iowait irq
// softirq steal ...), in USER_HZ ticks; busy is all but idle and iowait.
func (otherMachine) hostBusy() (time.Duration, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, err
	}
	line, _, _ := strings.Cut(string(data), "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return 0, errors.New("/proc/stat: no cpu line")
	}
	var busy int64
	for i, v := range f[1:] {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, err
		}
		if i != 3 && i != 4 && i < 8 { // not idle, not iowait, not guest (counted in user)
			busy += n
		}
	}
	return time.Duration(busy) * (time.Second / 100), nil
}

func (otherMachine) procs() (map[uint32]procCPU, error) { return map[uint32]procCPU{}, nil }

func (otherMachine) jobPids() (map[uint32]bool, error) { return map[uint32]bool{}, nil }

// ac reads the first mains supply under /sys/class/power_supply; -1 when the
// machine has none (a desktop, most VMs, CI). WSL 2 shows Hyper-V's virtual
// AC1, which read online=1 while Windows read on AC on the gate laptop
// (2026-11-24); whether it follows an unplug live is unchecked.
func (otherMachine) ac() (int, error) {
	dirs, err := os.ReadDir("/sys/class/power_supply")
	if err != nil {
		return -1, nil
	}
	for _, d := range dirs {
		base := "/sys/class/power_supply/" + d.Name()
		t, err := os.ReadFile(base + "/type")
		if err != nil || strings.TrimSpace(string(t)) != "Mains" {
			continue
		}
		on, err := os.ReadFile(base + "/online")
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(on)) == "1" {
			return 1, nil
		}
		return 0, nil
	}
	return -1, nil
}

func (otherMachine) stayAwake() {}

// standby says it cannot read a standby log here, so a stamp reads "unread",
// never "0": no log read is not the absence of a standby.
func (otherMachine) standby(from, to time.Time) ([]standbyEvent, error) {
	return nil, errors.New("no Modern Standby log off Windows")
}

func (otherMachine) limits() []string {
	return []string{"unsupported(no job object or standby log off Windows)"}
}

// scope: plain Linux reads no host, and limits() keeps every run void.
func (otherMachine) scope() scope { return scope{host: "none (off Windows, not in WSL)"} }
