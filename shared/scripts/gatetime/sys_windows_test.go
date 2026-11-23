//go:build windows

package main

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestProcs_ReadsThisProcessAndTheVM pins the SYSTEM_PROCESS_INFORMATION
// offsets against what the process itself knows: its own record carries its
// image name and a CPU time within a scheduler tick of GetProcessTimes'.
func TestProcs_ReadsThisProcessAndTheVM(t *testing.T) {
	w := &winMachine{}
	busy := time.Now()
	for time.Since(busy) < 50*time.Millisecond {
	}
	ps, err := w.procs()
	if err != nil {
		t.Fatal(err)
	}
	self := w.selfCPU()
	me, ok := ps[uint32(os.Getpid())]
	if !ok {
		t.Fatalf("no record for this process (pid %d) among %d", os.Getpid(), len(ps))
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(me.name, exe[strings.LastIndexAny(exe, `\/`)+1:]) {
		t.Errorf("own record named %q, want %q", me.name, exe)
	}
	if d := self - me.cpu; d < 0 || d > 100*time.Millisecond {
		t.Errorf("own record's CPU %v, GetProcessTimes says %v", me.cpu, self)
	}
	if len(ps) < 20 {
		t.Errorf("only %d processes read", len(ps))
	}
	for _, p := range ps {
		if isVM(p.name) {
			t.Logf("utility VM %s: %v CPU", p.name, p.cpu)
		}
	}
}

// TestStayAwake_HoldsSystemAndDisplay: the thread that called stayAwake holds
// the system AND the display requirement (the baseline's held only the system
// one, and Modern Standby came eight times in its window). The state is per
// thread, so the check runs on one locked thread and hands the old state back.
func TestStayAwake_HoldsSystemAndDisplay(t *testing.T) {
	done := make(chan uint32)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		before := setExecState(esAwake) // what the thread had: restored below
		(&winMachine{}).stayAwake()
		held := setExecState(before | 0x80000000)
		done <- held
	}()
	if held := <-done; held != esAwake {
		t.Errorf("after stayAwake the thread's execution state is 0x%x, want 0x%x (continuous, system, display)", held, uint32(esAwake))
	}
}
