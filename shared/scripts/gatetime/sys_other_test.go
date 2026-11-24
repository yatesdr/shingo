//go:build !windows

package main

import (
	"bufio"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestOtherMachine_SaysWhatItCannotRead: off Windows there is no standby log
// and no job object, so the stamp reads standby=unread (never a 0 nothing
// measured) and every run carries the unsupported reason. The machine is named,
// not picked from the environment, so the test reads the same on every host.
func TestOtherMachine_SaysWhatItCannotRead(t *testing.T) {
	m := otherMachine{}
	now := time.Now()
	if got := stamps(m, nil, now.Add(-time.Minute), now, ""); !strings.Contains(got, "standby=unread") {
		t.Errorf("stamp %q, want standby=unread", got)
	}
	if l := m.limits(); len(l) == 0 || !strings.HasPrefix(l[0], "unsupported(") {
		t.Errorf("limits %v, want an unsupported(...) reason", l)
	}
}

// TestOtherMachine_JobCPUCountsLiveDescendants: without a job object the job's
// CPU still holds a child that is running: a child that has burnt CPU and is
// still alive is in jobCPU before anyone waits for it, and stays in it after
// the wait (rusage then). Without that, a run judged inside the VM would count
// its own command as foreign load until the command ended.
func TestOtherMachine_JobCPUCountsLiveDescendants(t *testing.T) {
	m := otherMachine{}
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc here: jobCPU is rusage alone")
	}
	before, err := m.jobCPU()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", `i=0; while [ $i -lt 300000 ]; do i=$((i+1)); done; echo burnt; exec sleep 30`)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "burnt\n" {
		t.Fatalf("child said %q, %v", line, err)
	}
	during, err := m.jobCPU()
	if err != nil {
		t.Fatal(err)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	after, _ := m.jobCPU()
	if during-before < 100*time.Millisecond {
		t.Errorf("a live child that burnt CPU added %v to jobCPU, want 100ms or more", during-before)
	}
	if after < during {
		t.Errorf("jobCPU fell from %v to %v once the child was waited for", during, after)
	}
}

// TestOtherMachine_RunIsNeverClean: off Windows a run whose samples are quiet
// and on mains power is still VOID, on the unsupported reason: what it cannot
// read is never taken for the absence of a problem.
func TestOtherMachine_RunIsNeverClean(t *testing.T) {
	ss := samplesEvery(time.Now(), 3*time.Second, repeat(10, [3]float64{2, 2, 0}))
	j := judge(ss, nil, 15*time.Second, nil, otherMachine{}.limits())
	if j.verdict() == "clean" || !hasReason(j, "unsupported(") {
		t.Errorf("a quiet run off Windows: %q, want VOID with unsupported(...)", j.verdict())
	}
}
