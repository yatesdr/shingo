package main

// A run started inside WSL cannot see the Windows host from there: the VM has
// no Modern Standby log, and its CPU counters are the VM's, not the laptop's
// (R39). So it asks the host, through `gatetime host`: the Windows build of
// gatetime, started through WSL's interop, answering one JSON request a line
// on stdin with one JSON reply a line on stdout until stdin closes. It reads
// with the same calls a Windows-started run makes (sys_windows.go): the host's
// busy time and process table, the power line, the Kernel-Power 506/507 log,
// and it holds the system and display requirement for the run.
//
// hostMachine is the WSL side of that: the host's readings from the reader,
// the job's CPU from this side (the job runs inside the VM, so the VM's
// reading includes it: scope().jobInVM). Where no reader answers, a run
// started inside WSL is judged inside the VM only (vmOnlyMachine), and its
// verdict says so: "clean inside the VM only", never "clean".
//
// The reader is found as GATETIME_HOST_READER names it, else as gatetime's
// own path plus ".exe" (a Linux-side gate builds both, side by side). Its
// readings carry GATETIME_PLANT across through WSLENV, so a planted battery or
// standby comes back from the host itself, not from a local override.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// hostReaderEnv names the reader a gatetime inside WSL starts.
const hostReaderEnv = "GATETIME_HOST_READER"

// hostRequest is one request to the reader: hello, sample, standby, awake.
type hostRequest struct {
	Op   string `json:"op"`
	From int64  `json:"from,omitempty"` // standby window, Unix ns
	To   int64  `json:"to,omitempty"`
}

// hostReply is the reader's answer to one request.
type hostReply struct {
	Op     string      `json:"op"`
	At     int64       `json:"at"`               // the reader's clock as it answered, Unix ns
	OS     string      `json:"os,omitempty"`     // hello: GOOS/GOARCH of the reader
	Plants []string    `json:"plants,omitempty"` // hello: the plants the reader applies to its readings
	Busy   int64       `json:"busy,omitempty"`   // sample: the host's cumulative busy CPU, ns
	Self   int64       `json:"self,omitempty"`   // sample: the reader's own CPU, ns
	AC     int         `json:"ac"`               // sample: 1 on, 0 off, -1 unknown
	Procs  []hostProc  `json:"procs,omitempty"`  // sample: every process but the reader
	Events []hostEvent `json:"events,omitempty"` // standby: 506/507 in the window
	Err    string      `json:"err,omitempty"`
}

type hostProc struct {
	Pid  uint32 `json:"pid"`
	CPU  int64  `json:"cpu"` // ns
	Name string `json:"name"`
}

type hostEvent struct {
	ID string `json:"id"`
	At int64  `json:"at"` // Unix ns
}

// hostMain is `gatetime host`: the reader, on this machine's own readers.
func hostMain(args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "usage: gatetime host   (one JSON request a line on stdin; started by a gatetime inside WSL)")
		return 2
	}
	m, plants := plantFrom(localMachine(), os.Getenv(plantEnv))
	return serveHost(m, plants, os.Stdin, os.Stdout)
}

// serveHost answers requests until in closes.
func serveHost(m machine, plants []string, in io.Reader, out io.Writer) int {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	w := bufio.NewWriter(out)
	enc := json.NewEncoder(w)
	var awake sync.Once
	for sc.Scan() {
		var q hostRequest
		rep := hostReply{AC: -1}
		if err := json.Unmarshal(sc.Bytes(), &q); err != nil {
			rep.Err = "bad request: " + err.Error()
		} else {
			rep = answerHost(m, plants, q, &awake)
		}
		rep.At = time.Now().UnixNano()
		if enc.Encode(rep) != nil || w.Flush() != nil {
			return 1
		}
	}
	return 0
}

func answerHost(m machine, plants []string, q hostRequest, awake *sync.Once) hostReply {
	rep := hostReply{Op: q.Op, AC: -1}
	var errs []string
	note := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	switch q.Op {
	case "hello":
		rep.OS, rep.Plants = runtime.GOOS+"/"+runtime.GOARCH, plants
	case "sample":
		busy, err := m.hostBusy()
		note(err)
		procs, err := m.procs()
		note(err)
		rep.AC, err = m.ac()
		note(err)
		rep.Busy, rep.Self = int64(busy), int64(m.selfCPU())
		self := uint32(os.Getpid())
		for pid, p := range procs {
			if pid != self {
				rep.Procs = append(rep.Procs, hostProc{Pid: pid, CPU: int64(p.cpu), Name: p.name})
			}
		}
	case "standby":
		evs, err := m.standby(time.Unix(0, q.From), time.Unix(0, q.To))
		note(err)
		for _, e := range evs {
			rep.Events = append(rep.Events, hostEvent{ID: e.id, At: e.at.UnixNano()})
		}
	case "awake":
		// The requirement lives as long as the thread that set it: one thread,
		// locked, for the reader's whole life.
		awake.Do(func() {
			held := make(chan struct{})
			go func() {
				runtime.LockOSThread()
				m.stayAwake()
				close(held)
				select {}
			}()
			<-held
		})
	default:
		errs = append(errs, "unknown op "+q.Op)
	}
	rep.Err = strings.Join(errs, "; ")
	return rep
}

// hostMachine reads the Windows host through a reader; the job and gatetime's
// own CPU are this side's.
type hostMachine struct {
	local  machine
	reader string
	os     string   // what the reader runs on (hello)
	plants []string // what the reader says it plants (hello)

	mu      sync.Mutex
	in      io.WriteCloser
	replies chan hostReply
	stderr  *bytes.Buffer
	dead    error
	last    hostReply
	lastAt  time.Time
	skew    time.Duration // the largest gap seen between the two clocks
}

// hostWait bounds one request: the reader's own standby read waits up to 30 s,
// and a reader started from WSL's file system took 1-3 s to start on the gate
// laptop (2026-11-24: the endpoint agent scans it).
const hostWait = 60 * time.Second

// maxSkew is the clock gap past which the standby window asked of the host
// may not be the run's window: the run is void.
const maxSkew = 2 * time.Second

// startHostReader starts reader, says hello and returns the machine.
func startHostReader(local machine, reader string) (*hostMachine, error) {
	cmd := exec.Command(reader, "host")
	// WSL hands a Windows program only the variables WSLENV names.
	cmd.Env = append(os.Environ(), "WSLENV="+wslenvWith(os.Getenv("WSLENV"), plantEnv))
	h := &hostMachine{local: local, reader: reader, replies: make(chan hostReply, 1), stderr: &bytes.Buffer{}}
	cmd.Stderr = h.stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	h.in = in
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		for sc.Scan() {
			var rep hostReply
			if err := json.Unmarshal(sc.Bytes(), &rep); err != nil {
				rep = hostReply{Err: "unreadable reply: " + err.Error()}
			}
			h.replies <- rep
		}
		// Wait before the close: it finishes copying stderr, which request
		// reads once it sees the close.
		_ = cmd.Wait()
		close(h.replies)
	}()
	rep, err := h.request(hostRequest{Op: "hello"})
	if err == nil && rep.Err != "" {
		err = errors.New(rep.Err)
	}
	if err != nil {
		_ = in.Close()
		_ = cmd.Process.Kill()
		return nil, err
	}
	h.os, h.plants = rep.OS, rep.Plants
	return h, nil
}

// wslenvWith adds name to a WSLENV list once.
func wslenvWith(list, name string) string {
	for _, v := range strings.Split(list, ":") {
		if strings.SplitN(v, "/", 2)[0] == name {
			return list
		}
	}
	if list = strings.TrimSuffix(list, ":"); list == "" {
		return name
	}
	return list + ":" + name
}

// request sends q and waits for its reply. A reader that does not answer, or
// has gone, is dead for the rest of the run: every later reading is an error,
// so a run never reads clean on readings it did not get.
func (h *hostMachine) request(q hostRequest) (hostReply, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dead != nil {
		return hostReply{}, h.dead
	}
	b, _ := json.Marshal(q)
	t0 := time.Now()
	if _, err := h.in.Write(append(b, '\n')); err != nil {
		h.dead = fmt.Errorf("host reader %s: %w", h.reader, err)
		return hostReply{}, h.dead
	}
	select {
	case rep, ok := <-h.replies:
		if !ok {
			h.dead = fmt.Errorf("host reader %s exited: %s", h.reader, oneLine(h.stderr.String()))
			return hostReply{}, h.dead
		}
		if gap := rep.At - t0.Add(time.Since(t0)/2).UnixNano(); rep.At != 0 {
			h.skew = max(h.skew, time.Duration(max(gap, -gap)))
		}
		return rep, nil
	case <-time.After(hostWait):
		h.dead = fmt.Errorf("host reader %s: no reply to %s in %v", h.reader, q.Op, hostWait)
		return hostReply{}, h.dead
	}
}

// sample is the host's reading: a fresh one, or the last when it is at most
// maxAge old (one sampler pass asks for busy, then procs and ac).
func (h *hostMachine) sample(maxAge time.Duration) (hostReply, error) {
	h.mu.Lock()
	last, at := h.last, h.lastAt
	h.mu.Unlock()
	if maxAge > 0 && !at.IsZero() && time.Since(at) <= maxAge {
		return last, errOf(last)
	}
	rep, err := h.request(hostRequest{Op: "sample"})
	if err != nil {
		return hostReply{AC: -1}, err
	}
	h.mu.Lock()
	h.last, h.lastAt = rep, time.Now()
	h.mu.Unlock()
	return rep, errOf(rep)
}

func errOf(rep hostReply) error {
	if rep.Err == "" {
		return nil
	}
	return errors.New("host: " + rep.Err)
}

func (h *hostMachine) enterJob() error                { return h.local.enterJob() }
func (h *hostMachine) jobCPU() (time.Duration, error) { return h.local.jobCPU() }
func (h *hostMachine) selfCPU() time.Duration         { return h.local.selfCPU() }

// hostBusy is the host's busy time less the reader's own: the instrument is
// not foreign load.
func (h *hostMachine) hostBusy() (time.Duration, error) {
	rep, err := h.sample(0)
	return time.Duration(rep.Busy - rep.Self), err
}

// procs is the host's process table (the reader left out).
func (h *hostMachine) procs() (map[uint32]procCPU, error) {
	rep, err := h.sample(time.Second)
	out := make(map[uint32]procCPU, len(rep.Procs))
	for _, p := range rep.Procs {
		out[p.Pid] = procCPU{name: p.Name, cpu: time.Duration(p.CPU)}
	}
	return out, err
}

// jobPids: none on the host; the job runs inside the VM.
func (h *hostMachine) jobPids() (map[uint32]bool, error) { return map[uint32]bool{}, nil }

func (h *hostMachine) ac() (int, error) {
	rep, err := h.sample(time.Second)
	if err != nil {
		return -1, err
	}
	return rep.AC, nil
}

// stayAwake asks the reader to hold the system and display requirement on the
// host for as long as it lives (the run's).
func (h *hostMachine) stayAwake() { _, _ = h.request(hostRequest{Op: "awake"}) }

func (h *hostMachine) standby(from, to time.Time) ([]standbyEvent, error) {
	rep, err := h.request(hostRequest{Op: "standby", From: from.UnixNano(), To: to.UnixNano()})
	if err != nil {
		return nil, err
	}
	var evs []standbyEvent
	for _, e := range rep.Events {
		evs = append(evs, standbyEvent{id: e.ID, at: time.Unix(0, e.At)})
	}
	return evs, errOf(rep)
}

// limits: a host clock too far from this one voids the run (the standby
// window asked of the host may not be the run's).
func (h *hostMachine) limits() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.skew > maxSkew {
		return []string{fmt.Sprintf("host-clock-skew(%.1fs)", h.skew.Seconds())}
	}
	return nil
}

func (h *hostMachine) scope() scope {
	host := h.os + " host via " + h.reader
	if len(h.plants) > 0 {
		host += ", which planted " + strings.Join(h.plants, ",")
	}
	return scope{host: host, jobInVM: true}
}

// vmOnlyMachine is a run inside WSL whose host could not be read: the VM's own
// readings, judged inside the VM only (scope().vmOnly names why).
type vmOnlyMachine struct {
	machine
	why string
}

// limits: none of the off-Windows "unsupported" kind; what the run cannot
// cover is its scope, and its verdict says so.
func (v vmOnlyMachine) limits() []string { return nil }

func (v vmOnlyMachine) scope() scope {
	return scope{host: "unread: " + v.why, vmOnly: v.why}
}

// pickMachine is the machine a run off Windows judges by: the Windows host
// through a reader when one is named (reader) or, inside WSL (wsl), found
// beside gatetime (self + ".exe"); inside WSL with no reader that answers,
// the VM alone; anywhere else, local.
func pickMachine(local machine, reader string, wsl bool, self string) machine {
	if reader == "" && wsl && self != "" {
		if fi, err := os.Stat(self + ".exe"); err == nil && fi.Mode().IsRegular() {
			reader = self + ".exe"
		}
	}
	if reader == "" {
		if wsl {
			return vmOnlyMachine{local, "no Windows reader beside gatetime (GOOS=windows go build -o <gatetime>.exe ./scripts/gatetime) and " + hostReaderEnv + " unset"}
		}
		return local
	}
	h, err := startHostReader(local, reader)
	if err != nil {
		return vmOnlyMachine{local, "the Windows reader " + reader + " did not answer: " + oneLine(err.Error())}
	}
	return h
}
