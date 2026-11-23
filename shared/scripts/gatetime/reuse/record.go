package reuse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Decision is one line of $GATETIME_REUSE_LOG: what the wrapper did with one
// binary. The gate counts these lines for its sentence; the probes read them.
type Decision struct {
	Time     string `json:"time"`
	Package  string `json:"package"`
	Cwd      string `json:"cwd"`
	Mode     string `json:"mode"`
	Decision string `json:"decision"` // "reuse" or "run"
	Exit     int    `json:"exit"`
	Signal   string `json:"signal,omitempty"`
	// Stored is whether this run's verdict went into the store; Refused says
	// why a passing run's did not.
	Stored  bool   `json:"stored"`
	Refused string `json:"refused,omitempty"`
	// Miss is why no stored entry was served (reuse and audit only).
	Miss         string  `json:"miss,omitempty"`
	Entries      int     `json:"entries"`
	Unverifiable int     `json:"unverifiable"`
	From         *Origin `json:"from,omitempty"`
	Audit        string  `json:"audit,omitempty"`
	Processes    int     `json:"processes"`
	SilentForks  int     `json:"silent_forks,omitempty"`
	ChildHow     string  `json:"child_how,omitempty"`
	Traced       bool    `json:"traced"`
	WholeTree    bool    `json:"whole_tree"`
	WholeTreeWhy string  `json:"whole_tree_why,omitempty"`
	Paths        int     `json:"paths"`
	KeyID        string  `json:"key_id"`
	Note         string  `json:"note,omitempty"`
}

// record works out what the run touched and stores it when nothing refuses.
func (inv *invocation) record(r RunResult) {
	d := &inv.dec
	d.Decision, d.Exit, d.KeyID = "run", r.Code, inv.key.ID()
	d.Processes, d.SilentForks, d.ChildHow, d.Traced = r.Processes, r.SilentForks, r.ChildHow, r.Traced
	if r.Signaled {
		d.Signal = r.Signal.String()
	}
	if inv.looked {
		d.Miss, d.Entries, d.Unverifiable = inv.lookup.Miss, inv.lookup.Entries, inv.lookup.Unverifiable
		if inv.lookup.Hit != nil {
			d.Miss = ""
		}
	}
	if r.Code != 0 || r.Signaled {
		return // never stored; the exit code says why
	}
	e, refused := inv.entry(r)
	if refused == "" {
		if _, err := writeEntry(inv.cfg.store, e); err != nil {
			refused = "store write: " + err.Error()
		}
	}
	d.Stored, d.Refused = refused == "", refused
	if e != nil {
		d.WholeTree, d.WholeTreeWhy, d.Paths = e.WholeTree, e.WholeTreeWhy, len(e.Paths)
	}
}

// entry builds the store entry for a passing run, or says why there is none.
// The checks run cheapest first; the first refusal is the one reported.
func (inv *invocation) entry(r RunResult) (*Entry, string) {
	if inv.keyErr != "" {
		return nil, inv.keyErr
	}
	if f := filtered(inv.argv); f != "" {
		return nil, f
	}
	if bytes.Contains(inv.out.Bytes(), []byte("SHINGO-DOCKER-DOWN")) {
		return nil, "the run reported docker down (SHINGO-DOCKER-DOWN)"
	}
	if r.Unobservable != "" {
		return nil, "unobservable: " + r.Unobservable
	}
	tl := parseTestlog(inv.tlog, inv.cwd)
	if tl.state != "complete" {
		return nil, "test log " + tl.state
	}
	set := pathSet{}
	for p, op := range tl.paths {
		set.add(p, op, false)
	}
	for p, rec := range r.Parent {
		set.add(p, rec.Op, false)
	}
	for p, rec := range r.Children {
		set.add(p, rec.Op, true)
	}
	// The files the TestMain answer was read from are inputs of this record:
	// an edit to one during the run refuses it (stateRecs).
	testMain, testMainWhy, scanned := hasTestMain(inv.cwd)
	for _, p := range scanned {
		set.add(p, OpOpen, false)
	}
	recs := set.recs(newExclusions(inv.work, inv.binary, inv.self))
	if why := inv.stateRecs(recs); why != "" {
		return nil, why
	}
	e := &Entry{Version: entryVersion, Key: inv.key, KeyID: inv.key.ID(), Paths: recs, Getenv: tl.env,
		Processes: r.Processes, SilentForks: r.SilentForks, ChildHow: r.ChildHow, Traced: r.Traced, Root: inv.root, TestMain: testMain,
		Origin: Origin{Tier: inv.cfg.tier, Run: inv.cfg.run, SHA: inv.cfg.sha, Package: inv.pkg,
			Start: inv.start.UTC().Format(time.RFC3339Nano), WallS: r.Wall.Seconds()},
		Output: append([]byte{}, inv.out.Bytes()...)}
	var why []string
	switch {
	case r.Processes > 0:
		why = append(why, fmt.Sprintf("started %d process(es), by %s", r.Processes, r.ChildHow))
	case r.Processes < 0:
		why = append(why, "children unknown: "+r.ChildHow)
	}
	if testMain {
		why = append(why, testMainWhy)
	}
	e.WholeTreeWhy = strings.Join(why, "; ")
	e.WholeTree = e.WholeTreeWhy != ""
	if e.WholeTree {
		ts, err := hashTree(inv.root, inv.start)
		switch {
		case err != nil:
			return e, "whole-tree key: " + err.Error()
		case len(ts.Changed) > 0:
			return e, fmt.Sprintf("changed during the run: %s (tree, %d path(s))", ts.Changed[0], len(ts.Changed))
		}
		e.Tree = ts.Hash
	}
	return e, ""
}

// stateRecs fills each record's state, or says why the run cannot be stored.
func (inv *invocation) stateRecs(recs []PathRec) string {
	for i := range recs {
		s, err := stateOf(recs[i].Path, recs[i].Op)
		if err != nil {
			return "unhashable: " + filepath.ToSlash(recs[i].Path) + ": " + err.Error()
		}
		if changedSince(recs[i].Path, recs[i].Op, inv.start) {
			return "changed during the run: " + filepath.ToSlash(recs[i].Path)
		}
		recs[i].State = s
	}
	return ""
}

func (inv *invocation) writeDecision() {
	if inv.cfg.log == "" {
		return
	}
	inv.dec.Time = time.Now().UTC().Format(time.RFC3339Nano)
	b, err := json.Marshal(inv.dec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(inv.cfg.log, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gatetime: reuse log not written:", err)
		return
	}
	defer f.Close() // a lost close error loses at most this line, which is then short
	if _, err := f.Write(append(b, '\n')); err != nil {
		fmt.Fprintln(os.Stderr, "gatetime: reuse log not written:", err)
	}
}

// testlog is what one test log says the binary read.
type testlog struct {
	state string            // complete, absent, empty, truncated, malformed
	env   []string          // getenv names, sorted
	paths map[string]string // absolute path -> strongest op
}

// parseTestlog reads the testing package's log as cmd/go's computeTestInputsID
// does: "op name" per line after the "# test log" header, a relative name
// resolved against the last chdir (starting at cwd). Every path is kept, in
// the module or out of it. A chdir is recorded as a stat of the directory.
func parseTestlog(path, cwd string) testlog {
	tl := testlog{paths: map[string]string{}}
	data, err := os.ReadFile(path)
	const magic = "# test log\n"
	switch {
	case err != nil:
		tl.state = "absent"
		return tl
	case len(data) == 0:
		tl.state = "empty"
		return tl
	case !bytes.HasPrefix(data, []byte(magic)):
		tl.state = "malformed"
		return tl
	case data[len(data)-1] != '\n':
		tl.state = "truncated"
		return tl
	}
	tl.state = "complete"
	env := map[string]bool{}
	pwd := cwd
	for _, line := range strings.Split(strings.TrimSuffix(string(data[len(magic):]), "\n"), "\n") {
		if line == "" {
			continue
		}
		op, name, ok := strings.Cut(line, " ")
		if !ok {
			tl.state = "malformed"
			continue
		}
		if op != "getenv" && !filepath.IsAbs(name) {
			name = filepath.Join(pwd, name)
		}
		switch op {
		case "getenv":
			env[name] = true
		case "chdir":
			pwd = filepath.Clean(name)
			tl.add(pwd, OpStat)
		case "open":
			tl.add(name, OpOpen)
		case "stat":
			tl.add(name, OpStat)
		default:
			tl.state = "malformed"
		}
	}
	for n := range env {
		tl.env = append(tl.env, n)
	}
	sort.Strings(tl.env)
	return tl
}

func (tl *testlog) add(p, op string) {
	p = filepath.Clean(p)
	if old, ok := tl.paths[p]; !ok || opRank(op) > opRank(old) {
		tl.paths[p] = op
	}
}
