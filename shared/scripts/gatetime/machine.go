package main

import (
	"os"
	"sort"
	"strings"
	"time"
)

// machine is what `gatetime run` reads from the host it runs on. Every reader
// is behind this interface so the verdict's rules can be fed planted readings
// (tests, and GATETIME_PLANT for one end-to-end run).
type machine interface {
	// enterJob puts gatetime in a new job, so the command and everything it
	// starts, orphans included, are in it.
	enterJob() error
	// jobCPU is the job's cumulative user+kernel time, gatetime's own included.
	jobCPU() (time.Duration, error)
	// selfCPU is gatetime's own user+kernel time.
	selfCPU() time.Duration
	// hostBusy is the cumulative non-idle CPU time of every logical processor.
	hostBusy() (time.Duration, error)
	// procs is every process's cumulative CPU, by pid.
	procs() (map[uint32]procCPU, error)
	// jobPids are the job's live processes.
	jobPids() (map[uint32]bool, error)
	// ac is 1 on mains power, 0 on battery, -1 unknown.
	ac() (int, error)
	// stayAwake holds the system and display requirement while the calling OS
	// thread lives.
	stayAwake()
	// standby is every Modern Standby entry or exit logged between from and to.
	standby(from, to time.Time) ([]standbyEvent, error)
	// limits are the reasons this OS cannot give a clean verdict at all.
	limits() []string
}

type procCPU struct {
	name string
	cpu  time.Duration
}

type standbyEvent struct {
	id string // 506 entered, 507 left
	at time.Time
}

// isVM is whether a process is the WSL 2 / Docker Desktop utility VM, whose
// CPU is the Postgres the run's own docker tests start. Its CPU is subtracted
// from the host's before foreign load is judged: that is the one process
// matched by name, and the hole it leaves is said in run.go.
func isVM(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), "vmmem")
}

// planted is a machine with readings put in by GATETIME_PLANT, TEST ONLY: a
// comma list of "battery" (every sample reads off AC) and "standby" (a 506
// and a 507 inside the run's window). A planted run always carries
// planted(...) among its VOID reasons, so it can never be quoted.
type planted struct {
	machine
	battery, standbyEv bool
}

func plantFrom(m machine, spec string) (machine, []string) {
	if spec == "" {
		return m, nil
	}
	p := &planted{machine: m}
	var names []string
	for _, s := range strings.Split(spec, ",") {
		switch s = strings.TrimSpace(s); s {
		case "battery":
			p.battery = true
		case "standby":
			p.standbyEv = true
		default:
			s = "unknown:" + s
		}
		names = append(names, s)
	}
	sort.Strings(names)
	return p, names
}

func (p *planted) ac() (int, error) {
	if p.battery {
		return 0, nil
	}
	return p.machine.ac()
}

func (p *planted) standby(from, to time.Time) ([]standbyEvent, error) {
	evs, err := p.machine.standby(from, to)
	if p.standbyEv {
		mid := from.Add(to.Sub(from) / 2)
		evs = append(evs, standbyEvent{id: "506", at: mid}, standbyEvent{id: "507", at: mid.Add(time.Millisecond)})
	}
	return evs, err
}

// plantEnv is the test-only seam's variable.
const plantEnv = "GATETIME_PLANT"

func plantedMachine() (machine, []string) { return plantFrom(newMachine(), os.Getenv(plantEnv)) }
