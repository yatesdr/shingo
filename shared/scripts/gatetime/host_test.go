package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestServeHost_AnswersEachRequest: the reader answers one JSON line per
// request line, in order, from its machine's readings, each stamped with its
// clock; a bad line or an unknown op gets an error reply, never silence.
func TestServeHost_AnswersEachRequest(t *testing.T) {
	m := &readingsMachine{fakeMachine: fakeMachine{acState: 1, evs: []standbyEvent{{"506", time.Unix(100, 0)}}},
		host: 9 * time.Second, table: map[uint32]procCPU{7: {"vmmemWSL", 3 * time.Second}, uint32(os.Getpid()): {"gatetime.exe", time.Second}}}
	in := strings.Join([]string{`{"op":"hello"}`, `{"op":"sample"}`, `{"op":"standby","from":1,"to":2}`, `{"op":"bogus"}`, `not json`}, "\n") + "\n"
	var out bytes.Buffer
	before := time.Now().UnixNano()
	if rc := serveHost(m, []string{"battery"}, strings.NewReader(in), &out); rc != 0 {
		t.Fatalf("serveHost exit %d", rc)
	}
	var reps []hostReply
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		var r hostReply
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("reply %q: %v", sc.Text(), err)
		}
		if r.At < before {
			t.Errorf("reply %q carries no clock reading", sc.Text())
		}
		reps = append(reps, r)
	}
	if len(reps) != 5 {
		t.Fatalf("%d replies to 5 requests: %+v", len(reps), reps)
	}
	if r := reps[0]; r.Op != "hello" || !strings.Contains(r.OS, "/") || strings.Join(r.Plants, ",") != "battery" || r.Err != "" {
		t.Errorf("hello: %+v", r)
	}
	if r := reps[1]; r.Busy != int64(9*time.Second) || r.AC != 1 || len(r.Procs) != 1 || r.Procs[0].Name != "vmmemWSL" || r.Err != "" {
		t.Errorf("sample: %+v (want busy 9s, AC 1, the process table without the reader itself)", r)
	}
	if r := reps[2]; len(r.Events) != 1 || r.Events[0].ID != "506" || r.Events[0].At != time.Unix(100, 0).UnixNano() {
		t.Errorf("standby: %+v", r)
	}
	if r := reps[3]; !strings.Contains(r.Err, "unknown op bogus") {
		t.Errorf("unknown op: %+v", r)
	}
	if r := reps[4]; !strings.HasPrefix(r.Err, "bad request") || r.AC != -1 {
		t.Errorf("bad line: %+v", r)
	}
}

// TestHostMachine_ReadsAndPlantsThroughTheReader: a machine reading the host
// through a real reader process (this test binary as `gatetime host`), with
// GATETIME_PLANT in the reader's environment only: every sample reads off AC
// and the standby log holds a 506 and a 507, so the run is void on both, and
// the plant came back from the reader, not from a local override. The job
// runs inside the VM; the host field names the reader.
func TestHostMachine_ReadsAndPlantsThroughTheReader(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GATETIME_TEST_AS_MAIN", "1") // the reader's: this process read it at start
	t.Setenv(plantEnv, "battery,standby")
	local := &readingsMachine{job: time.Second}
	m := pickMachine(local, self, false, "")
	h, ok := m.(*hostMachine)
	if !ok {
		t.Fatalf("pickMachine with a reader gave %T (%+v), want the host read through it", m, m.scope())
	}
	t.Cleanup(func() { _ = h.in.Close() }) // the reader exits on EOF
	if strings.Join(h.plants, ",") != "battery,standby" {
		t.Errorf("the reader says it plants %v, want battery,standby", h.plants)
	}
	r := &runner{m: h, every: 100 * time.Millisecond, out: t.TempDir(), phase: "before", rows: bufio.NewWriter(io.Discard)}
	for i := 0; i < 3; i++ {
		r.pass()
		time.Sleep(50 * time.Millisecond)
	}
	for _, s := range r.samples {
		if s.err != "" || s.host <= 0 || s.job != time.Second {
			t.Errorf("sample %+v: want no error, the host's busy time, the local job", s)
		}
	}
	v := r.final().verdict()
	for _, want := range []string{"not-on-AC(3 of 3 samples)", "standby(506@"} {
		if !strings.Contains(v, want) {
			t.Errorf("verdict %q lacks %s", v, want)
		}
	}
	sc := h.scope()
	if !sc.jobInVM || sc.vmOnly != "" || !strings.Contains(sc.host, self) || !strings.Contains(sc.host, "planted battery,standby") {
		t.Errorf("scope %+v: want the job inside the VM, the host read, by %s, planted there", sc, self)
	}
	if l := h.limits(); len(l) != 0 {
		t.Errorf("limits %v: the two clocks are one here", l)
	}
}

// TestHostMachine_AGoneReaderIsAnErrorForGood: once the reader has gone, every
// reading is an error, so no later pass reads clean on nothing.
func TestHostMachine_AGoneReaderIsAnErrorForGood(t *testing.T) {
	replies := make(chan hostReply)
	close(replies)
	h := &hostMachine{local: &fakeMachine{}, reader: "r.exe", replies: replies, stderr: bytes.NewBufferString("reader: boom\n"), in: nopWriteCloser{}}
	if _, err := h.hostBusy(); err == nil || !strings.Contains(err.Error(), "exited: reader: boom") {
		t.Errorf("first reading after the reader exited: %v, want its exit and stderr", err)
	}
	if a, err := h.ac(); a != -1 || err == nil {
		t.Errorf("a later reading: ac %d, %v; want -1 and the same error", a, err)
	}
	if _, err := h.standby(time.Now(), time.Now()); !errors.Is(err, h.dead) {
		t.Errorf("standby after: %v, want %v", err, h.dead)
	}
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// TestPickMachine_WhereTheHostIsRead: off WSL with no reader, the local
// machine; inside WSL with none, the VM only, saying why; a reader found
// beside gatetime, or named, that does not answer, the VM only, naming it.
func TestPickMachine_WhereTheHostIsRead(t *testing.T) {
	local := &fakeMachine{acState: 1}
	if m := pickMachine(local, "", false, ""); m != machine(local) {
		t.Errorf("off WSL, no reader: %T, want the local machine", m)
	}
	m := pickMachine(local, "", true, "")
	if sc := m.scope(); !strings.Contains(sc.vmOnly, "no Windows reader") || !strings.HasPrefix(sc.host, "unread: ") || len(m.limits()) != 0 {
		t.Errorf("inside WSL, no reader: scope %+v limits %v", sc, m.limits())
	}
	dir := t.TempDir()
	self := filepath.Join(dir, "gatetime")
	for _, f := range []string{self, self + ".exe"} {
		if err := os.WriteFile(f, []byte("not a program\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if sc := pickMachine(local, "", true, self).scope(); !strings.Contains(sc.vmOnly, self+".exe did not answer") {
		t.Errorf("a sibling reader that cannot run: scope %+v, want it named as not answering", sc)
	}
	if sc := pickMachine(local, filepath.Join(dir, "absent.exe"), false, "").scope(); !strings.Contains(sc.vmOnly, "absent.exe did not answer") {
		t.Errorf("a named reader that is not there: scope %+v", sc)
	}
}

func TestWSLENV_CarriesThePlantOnce(t *testing.T) {
	for in, want := range map[string]string{
		"":                          "GATETIME_PLANT",
		"WT_SESSION:WT_PROFILE_ID:": "WT_SESSION:WT_PROFILE_ID:GATETIME_PLANT",
		"A/p:GATETIME_PLANT/u":      "A/p:GATETIME_PLANT/u",
	} {
		if got := wslenvWith(in, plantEnv); got != want {
			t.Errorf("wslenvWith(%q) = %q, want %q", in, got, want)
		}
	}
}
