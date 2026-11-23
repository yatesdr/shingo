//go:build !windows

package main

// Off Windows `gatetime run` builds and samples what it can, and never calls
// a run clean: there is no job object to hold the command's orphans and their
// CPU, and no Modern Standby log. limits() names that in every verdict. The
// timing tool's home is the Windows laptop; Linux is here so the package
// builds, vets and tests in CI and in WSL.

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type otherMachine struct{}

func newMachine() machine { return otherMachine{} }

func (otherMachine) enterJob() error { return nil }

// jobCPU is the CPU of this process and its waited-for children: the best
// stand-in for a job without one.
func (otherMachine) jobCPU() (time.Duration, error) {
	var self, kids syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &self); err != nil {
		return 0, err
	}
	if err := syscall.Getrusage(syscall.RUSAGE_CHILDREN, &kids); err != nil {
		return 0, err
	}
	return tv(self.Utime) + tv(self.Stime) + tv(kids.Utime) + tv(kids.Stime), nil
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
// machine has none (a desktop, a VM, CI).
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

func (otherMachine) standby(from, to time.Time) ([]standbyEvent, error) { return nil, nil }

func (otherMachine) limits() []string {
	return []string{"unsupported(no job object or standby log off Windows)"}
}
