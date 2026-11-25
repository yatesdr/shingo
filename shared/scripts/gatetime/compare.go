package main

// `gatetime compare [-a NAME -b NAME] A1 B1 B2 A2` judges one A B B A pair of
// `gatetime run` directories. It refuses, naming why, when:
//   - a run is not quotable: its exit is not 0, its verdict is not clean, or
//     the gate's own total line says the gate failed or was off AC, in
//     standby, or under foreign load (the stamps gate.sh writes);
//   - the runs did not choose the same work (selection, below);
//   - the runs were judged by different foreign-load lines (run -foreign-cores);
//   - an arm's two runs differ by more than 10% in CPU-seconds (the larger
//     over the smaller): that arm did not repeat itself, so its range says
//     nothing.
// Otherwise it prints each arm's range and mean, and calls a difference of
// the means inside the larger arm's range (max less min) "no difference". An
// A A A A run of one tree is the noise floor. Walls are the runs' own walls;
// nothing here adds test or step seconds into a wall.
//
// SELECTION, like with like:
//   - all four runs carry the gate's `selection` line (steps.tsv): those lines
//     must be equal, sha and run time aside;
//   - otherwise, all four carry a gate sentence (cmd.log; every gate since the
//     sentence existed prints one, b5949e26 included): the selection is
//     derived from it, the tier, the steps, docker ran (scoped or unscoped)
//     or skipped, the module scope, the scripts account (N of M and what was
//     skipped), the unit step's mode (-count=1, or cached with its reuse
//     count) and the sim mode, and must be equal;
//   - otherwise, no run is a gate: the command lines must be equal;
//   - a mix is refused, never passed.
// In every mode the steps and modules the runs logged (steps.tsv: a step, a
// module's tests, a guard) must be equal too. Package-level differences (a
// test package one tree has and the other does not) are listed, not refused:
// they are the code under comparison, not the gate's choice.

import (
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// timedRun is one `gatetime run` directory as compare reads it.
type timedRun struct {
	dir      string
	res      resultLine
	wall     float64
	cpu      float64
	selLines []string // the gate's selection lines, sha and run removed
	sentence string   // the last gate sentence in cmd.log, "" when none
	totals   []string // the gate's total lines
	steps    map[string]bool
	pkgs     map[string]bool
}

func compareMain(args []string) int {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	an := fs.String("a", "A", "name of the A arm")
	bn := fs.String("b", "B", "name of the B arm")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 4 {
		fmt.Fprintln(os.Stderr, "usage: gatetime compare [-a NAME -b NAME] A1 B1 B2 A2   (gatetime run directories, in the order they ran)")
		return 2
	}
	var runs []*timedRun
	for _, d := range fs.Args() {
		r, err := readTimedRun(d)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gatetime compare:", err)
			return 2
		}
		runs = append(runs, r)
	}
	if !comparePair(os.Stdout, *an, *bn, runs) {
		return 1
	}
	return 0
}

func readTimedRun(dir string) (*timedRun, error) {
	r := &timedRun{dir: dir, steps: map[string]bool{}, pkgs: map[string]bool{}}
	data, err := os.ReadFile(filepath.Join(dir, "result.tsv"))
	if err != nil {
		return nil, err
	}
	r.res = parseResultLine(strings.SplitN(string(data), "\n", 2)[0])
	if r.wall, err = strconv.ParseFloat(r.res["wall_s"], 64); err != nil {
		return nil, fmt.Errorf("%s: result.tsv wall_s %q: %w", dir, r.res["wall_s"], err)
	}
	if r.cpu, err = strconv.ParseFloat(r.res["job_cpu_s"], 64); err != nil {
		return nil, fmt.Errorf("%s: result.tsv job_cpu_s %q: %w", dir, r.res["job_cpu_s"], err)
	}
	if steps, err := os.ReadFile(filepath.Join(dir, "steps.tsv")); err == nil {
		r.readSteps(string(steps))
	}
	if log, err := os.ReadFile(filepath.Join(dir, "cmd.log")); err == nil {
		r.sentence = lastSentence(string(log))
	}
	return r, nil
}

// readSteps reads the gate's steps.log lines: `sha mode step seconds run
// [key=value...]`.
func (r *timedRun) readSteps(s string) {
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r", ""), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		step := f[2]
		switch {
		case step == "selection":
			r.selLines = append(r.selLines, strings.Join(f[5:], " "))
		case step == "total" || step == "total-FAILED":
			r.totals = append(r.totals, strings.Join(append([]string{step}, f[5:]...), " "))
		default:
			name, unit, ok := strings.Cut(step, ":")
			if !ok {
				r.steps[name] = true
				continue
			}
			switch name {
			case "test", "docker", "sim", "race":
				r.steps[name+":"+moduleOf(unit)] = true
				r.pkgs[step] = true
			default: // modbuild:<module>, lint:<module>, scripts:<guard>
				r.steps[step] = true
			}
		}
	}
}

// moduleOf is the module directory an import path belongs to.
func moduleOf(pkg string) string {
	first, rest, _ := strings.Cut(pkg, "/")
	switch first {
	case "shingo":
		m, _, _ := strings.Cut(rest, "/")
		return m
	case "shingocore":
		return "shingo-core"
	case "shingoedge":
		return "shingo-edge"
	}
	return first
}

var sentenceRE = regexp.MustCompile(`^Gate( \((dev|push)\))?: `)

func lastSentence(log string) string {
	last := ""
	for _, line := range strings.Split(strings.ReplaceAll(log, "\r", ""), "\n") {
		if sentenceRE.MatchString(line) {
			last = line
		}
	}
	return last
}

// sentenceSelection derives what a gate chose from its sentence.
func sentenceSelection(s string) string {
	m := sentenceRE.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	tier := "cert"
	if m[2] != "" {
		tier = m[2]
	}
	body := s[len(m[0]):]
	head, tail := splitTop(body, ". ")
	head = strings.TrimSuffix(head, ".")
	docker := "not-run"
	if i := strings.Index(head, " AND the docker suites"); i >= 0 || strings.HasPrefix(head, "the docker suites") {
		rest := head
		if i >= 0 {
			rest, head = head[i:], head[:i]
		} else {
			head = ""
		}
		docker = "ran"
		switch {
		case strings.Contains(rest, "(unscoped)"):
			docker = "ran(unscoped)"
		case strings.Contains(rest, "(scoped against"):
			docker = "ran(scoped)"
		}
	}
	if strings.Contains(tail, "Docker suites SKIPPED by scope") {
		docker = "skipped(scope)"
	}
	modules := "all"
	if i := strings.Index(head, " (scoped to: "); i >= 0 && strings.HasSuffix(head, ")") {
		modules, head = strings.TrimSuffix(head[i+len(" (scoped to: "):], ")"), head[:i]
	}
	var steps []string
	fields := map[string]string{}
	for _, item := range splitList(head) {
		name, paren := item, ""
		if i := strings.Index(item, " ("); i >= 0 && strings.HasSuffix(item, ")") {
			name, paren = item[:i], item[i+2:len(item)-1]
		}
		steps = append(steps, name)
		switch name {
		case "scripts":
			fields["scripts"] = paren
		case "unit":
			fields["unit"] = unitMode(paren)
		case "sim":
			fields["sim"] = map[bool]string{true: "no-race", false: "race"}[paren == "no -race"]
		}
	}
	out := []string{"tier=" + tier, "steps=" + strings.Join(steps, ","), "docker=" + docker, "modules=" + modules}
	for _, k := range []string{"scripts", "unit", "sim"} {
		if v, ok := fields[k]; ok {
			out = append(out, k+"="+v)
		}
	}
	return strings.Join(out, " | ")
}

var cachedRE = regexp.MustCompile(`cached: ([0-9]+) of ([0-9]+) packages reused`)

// unitMode is the unit step's mode, whatever words a tree used for it: Go's
// test cache with its reuse count, or -count=1.
func unitMode(paren string) string {
	if m := cachedRE.FindStringSubmatch(paren); m != nil {
		return "cache(" + m[1] + " of " + m[2] + " reused)"
	}
	return "count1"
}

// splitTop splits s at the first sep outside parentheses.
func splitTop(s, sep string) (string, string) {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 && strings.HasPrefix(s[i:], sep) {
			return s[:i], s[i+len(sep):]
		}
	}
	return s, ""
}

// splitList splits a comma list at the commas outside parentheses.
func splitList(s string) []string {
	var out []string
	for s != "" {
		var item string
		item, s = splitTop(s, ", ")
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// selection is a run's selection in the given mode.
func (r *timedRun) selection(mode string) string {
	switch mode {
	case "selection line":
		return strings.Join(r.selLines, " || ")
	case "gate sentence":
		return sentenceSelection(r.sentence)
	}
	return r.res["cmd"]
}

// quotable is why a run cannot be quoted, or "".
func (r *timedRun) quotable() string {
	if r.res["exit"] != "0" {
		return "exit " + r.res["exit"]
	}
	if r.res["verdict"] != "clean" {
		return "verdict " + r.res["verdict"]
	}
	for _, t := range r.totals {
		f := strings.Fields(t)
		if f[0] == "total-FAILED" {
			return "the gate logged total-FAILED"
		}
		for _, kv := range f[1:] {
			k, v, _ := strings.Cut(kv, "=")
			switch {
			case k == "ac" && v != "on",
				k == "standby" && v != "0",
				k == "load" && v != "clean":
				return "the gate's total line stamps " + kv
			}
		}
	}
	return ""
}

// execStamp is the exec stamp on a gate's last total line, or "" for a gate
// that wrote none. A gate wrote one (on, off or -) only while it ran its Go
// tests under the record-only -exec wrapper, which the gate no longer has; it
// was the treatment an A/B of the wrapper compared, so it is shown for such
// a run, never required equal.
func (r *timedRun) execStamp() string {
	for i := len(r.totals) - 1; i >= 0; i-- {
		for _, kv := range strings.Fields(r.totals[i])[1:] {
			if strings.HasPrefix(kv, "exec=") {
				return ", " + kv
			}
		}
	}
	return ""
}

func comparePair(w io.Writer, an, bn string, runs []*timedRun) bool {
	ok := true
	refuse := func(format string, a ...any) {
		fmt.Fprintf(w, "REFUSED: "+format+"\n", a...)
		ok = false
	}
	names := []string{an + "1", bn + "1", bn + "2", an + "2"}
	for i, r := range runs {
		fmt.Fprintf(w, "%s %s: wall %.2f s, cpu %.2f s, %s%s\n", names[i], r.dir, r.wall, r.cpu, r.res["verdict"], r.execStamp())
		if why := r.quotable(); why != "" {
			refuse("%s (%s) is not quotable: %s", names[i], r.dir, why)
		}
	}
	// One void line for all four: a run judged by a raised -foreign-cores line is
	// not like a run judged by the designed one. Absent (a run from before the
	// field) is the default.
	lines := make([]string, 4)
	for i, r := range runs {
		if lines[i] = r.res["foreign_line"]; lines[i] == "" {
			lines[i] = fmt.Sprintf("%.2f", foreignCores)
		}
	}
	if lines[1] != lines[0] || lines[2] != lines[0] || lines[3] != lines[0] {
		refuse("foreign-load lines differ: %s: %s, %s: %s, %s: %s, %s: %s", names[0], lines[0], names[1], lines[1], names[2], lines[2], names[3], lines[3])
	}
	mode := selectionMode(runs)
	if mode == "" {
		refuse("the runs are not like with like: %s", describeKinds(names, runs))
	} else {
		fmt.Fprintf(w, "selection: by the %s\n", mode)
		for i := 1; i < 4; i++ {
			if a, b := runs[0].selection(mode), runs[i].selection(mode); a != b {
				refuse("selection differs, %s against %s:\n    %s: %s\n    %s: %s", names[0], names[i], names[0], a, names[i], b)
			}
		}
	}
	for i := 1; i < 4; i++ {
		if only := setDiff(runs[0].steps, runs[i].steps); only != "" {
			refuse("logged steps differ, %s against %s: %s", names[0], names[i], only)
		}
		if only := setDiff(runs[0].pkgs, runs[i].pkgs); only != "" {
			fmt.Fprintf(w, "note: test packages differ, %s against %s (the code, not the gate's choice): %s\n", names[0], names[i], only)
		}
	}
	armA, armB := []*timedRun{runs[0], runs[3]}, []*timedRun{runs[1], runs[2]}
	for _, arm := range []struct {
		name string
		rs   []*timedRun
	}{{an, armA}, {bn, armB}} {
		lo, hi := math.Min(arm.rs[0].cpu, arm.rs[1].cpu), math.Max(arm.rs[0].cpu, arm.rs[1].cpu)
		if lo <= 0 || (hi-lo)/lo > 0.10 {
			refuse("arm %s's two runs differ by more than 10%% in CPU-seconds: %.2f and %.2f", arm.name, arm.rs[0].cpu, arm.rs[1].cpu)
		}
	}
	if !ok {
		return false
	}
	report(w, an, bn, armA, armB)
	return true
}

func report(w io.Writer, an, bn string, armA, armB []*timedRun) {
	for _, q := range []struct {
		what string
		get  func(*timedRun) float64
	}{{"wall", func(r *timedRun) float64 { return r.wall }}, {"cpu", func(r *timedRun) float64 { return r.cpu }}} {
		a0, a1 := q.get(armA[0]), q.get(armA[1])
		b0, b1 := q.get(armB[0]), q.get(armB[1])
		ra, rb := math.Abs(a0-a1), math.Abs(b0-b1)
		ma, mb := (a0+a1)/2, (b0+b1)/2
		fmt.Fprintf(w, "%s: %s %.2f-%.2f s (mean %.2f, range %.2f); %s %.2f-%.2f s (mean %.2f, range %.2f)\n",
			q.what, an, math.Min(a0, a1), math.Max(a0, a1), ma, ra, bn, math.Min(b0, b1), math.Max(b0, b1), mb, rb)
		d := mb - ma
		if math.Abs(d) <= math.Max(ra, rb) {
			fmt.Fprintf(w, "%s: no difference (%s - %s = %+.2f s, inside the larger arm's range %.2f s)\n", q.what, bn, an, d, math.Max(ra, rb))
		} else {
			fmt.Fprintf(w, "%s: %s - %s = %+.2f s (%+.1f%% of %s), outside the larger arm's range %.2f s\n", q.what, bn, an, d, 100*d/ma, an, math.Max(ra, rb))
		}
	}
}

// selectionMode is the like-with-like mode all four runs share, or "".
func selectionMode(runs []*timedRun) string {
	lines, sentences, neither := 0, 0, 0
	for _, r := range runs {
		switch {
		case len(r.selLines) > 0 && r.sentence != "":
			lines++
			sentences++
		case len(r.selLines) > 0:
			lines++
		case r.sentence != "":
			sentences++
		default:
			neither++
		}
	}
	switch {
	case lines == len(runs):
		return "selection line"
	case sentences == len(runs):
		return "gate sentence"
	case neither == len(runs):
		return "command line"
	}
	return ""
}

func describeKinds(names []string, runs []*timedRun) string {
	var out []string
	for i, r := range runs {
		k := "no selection line and no gate sentence"
		switch {
		case len(r.selLines) > 0:
			k = "a selection line"
		case r.sentence != "":
			k = "a gate sentence, no selection line"
		}
		out = append(out, names[i]+" has "+k)
	}
	return strings.Join(out, "; ")
}

// setDiff names what one set has and the other lacks, or "".
func setDiff(a, b map[string]bool) string {
	var onlyA, onlyB []string
	for k := range a {
		if !b[k] {
			onlyA = append(onlyA, k)
		}
	}
	for k := range b {
		if !a[k] {
			onlyB = append(onlyB, k)
		}
	}
	if len(onlyA)+len(onlyB) == 0 {
		return ""
	}
	sort.Strings(onlyA)
	sort.Strings(onlyB)
	return fmt.Sprintf("only in the first: [%s]; only in the second: [%s]", strings.Join(onlyA, " "), strings.Join(onlyB, " "))
}
