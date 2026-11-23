package main

// `gatetime report DIR...` reads manifest directories (one per gate run, in
// the order the runs happened) and prints:
//   - per run: the binaries that started children, and the binaries that read
//     outside their module, with the paths (temp paths are counted, not
//     listed: they are the run's own output);
//   - the hit rate the manifests imply across successive runs. A binary run
//     "would hit" when the latest earlier run of the same slot (package and
//     argv) exited 0 with a complete test log, and its binary hash, argv, the
//     value of every env var read, the content hash of every path read and
//     the identity hash all equal this run's. Paths under the temp directory
//     are left out of that comparison: t.TempDir names a new directory every
//     run, so no run would ever equal another on them.
// Two rates are printed: strict, and with the gate's run-scoped Postgres
// names (SHINGO_TEST_PG, SHINGO_TEST_PG_TEMPLATE: a new port and template
// name every gate run) left out, which is what a key that replaces them with
// the identity hash would see. A would-hit binary that started children is
// counted apart: its read set is not the whole of its inputs. Record only:
// nothing here is reused.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// runScopedEnv are env vars the gate sets fresh every run.
var runScopedEnv = map[string]bool{"SHINGO_TEST_PG": true, "SHINGO_TEST_PG_TEMPLATE": true}

func reportMain(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: gatetime report MANIFEST_DIR...   (one directory per gate run, oldest first)")
		return 2
	}
	var runs [][]*Manifest
	for _, d := range args {
		ms, err := readManifestDir(d)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gatetime report:", err)
			return 2
		}
		runs = append(runs, ms)
	}
	writeReport(os.Stdout, args, runs)
	return 0
}

// readManifestDir reads every manifest in one directory, oldest first (the
// file names start with the write time).
func readManifestDir(dir string) ([]*Manifest, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*Manifest
	for _, e := range ents {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		m, err := readManifest(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func slotOf(m *Manifest) string { return m.Package + " " + strings.Join(m.Argv, " ") }

// missReason is why m would not reuse prev's verdict, or "" when it would.
// lenient leaves the run-scoped env names out.
func missReason(prev, m *Manifest, lenient bool) string {
	switch {
	case prev.Exit != 0:
		return "earlier-run-red"
	case prev.Testlog != "complete" || m.Testlog != "complete":
		return "testlog-incomplete"
	case prev.BinarySHA256 == "" || prev.BinarySHA256 != m.BinarySHA256:
		return "binary"
	case strings.Join(prev.Argv, " ") != strings.Join(m.Argv, " "):
		return "argv"
	case prev.IdentitySHA256 != m.IdentitySHA256:
		return "identity"
	case envKey(prev, lenient) != envKey(m, lenient):
		return "env"
	case pathKey(prev) != pathKey(m):
		return "paths"
	}
	return ""
}

func envKey(m *Manifest, lenient bool) string {
	var b strings.Builder
	for _, e := range m.Env {
		if lenient && runScopedEnv[e.Name] {
			continue
		}
		b.WriteString(e.Name + "=" + e.Value + "\n")
	}
	return b.String()
}

func pathKey(m *Manifest) string {
	var b strings.Builder
	for _, p := range m.Paths {
		if p.Scope == "temp" {
			continue
		}
		b.WriteString(p.Path + " " + p.Hash + "\n")
	}
	return b.String()
}

type hitCount struct {
	comparable, hits, cold, hitChildren int
	secs, hitSecs                       float64
	reasons                             map[string]int
}

func (h *hitCount) add(prev, m *Manifest, lenient bool) {
	if prev == nil {
		h.cold++
		return
	}
	h.comparable++
	h.secs += m.WallS
	r := missReason(prev, m, lenient)
	if r != "" {
		h.reasons[r]++
		return
	}
	h.hits++
	h.hitSecs += m.WallS
	if m.Children != 0 || prev.Children != 0 {
		h.hitChildren++
	}
}

func (h *hitCount) line(name string) string {
	pct := func(a, b float64) string {
		if b == 0 {
			return "-"
		}
		return fmt.Sprintf("%.0f%%", 100*a/b)
	}
	var rs []string
	for k, v := range h.reasons {
		rs = append(rs, fmt.Sprintf("%s %d", k, v))
	}
	sort.Strings(rs)
	return fmt.Sprintf("%s: %d of %d binary runs would hit (%s; %.1f of %.1f package-seconds, %s); %d cold; %d of the hits started children; misses: %s",
		name, h.hits, h.comparable, pct(float64(h.hits), float64(h.comparable)), h.hitSecs, h.secs, pct(h.hitSecs, h.secs),
		h.cold, h.hitChildren, strings.Join(rs, ", "))
}

func writeReport(w io.Writer, dirs []string, runs [][]*Manifest) {
	latest := map[string]*Manifest{}
	total := [2]*hitCount{{reasons: map[string]int{}}, {reasons: map[string]int{}}}
	for i, ms := range runs {
		fmt.Fprintf(w, "run %d %s: %d binary runs\n", i+1, dirs[i], len(ms))
		writeEscapes(w, ms)
		if i > 0 {
			per := [2]*hitCount{{reasons: map[string]int{}}, {reasons: map[string]int{}}}
			for _, m := range ms {
				for k, lenient := range []bool{false, true} {
					per[k].add(latest[slotOf(m)], m, lenient)
					total[k].add(latest[slotOf(m)], m, lenient)
				}
			}
			fmt.Fprintln(w, "  "+per[0].line("strict"))
			fmt.Fprintln(w, "  "+per[1].line("run-scoped PG names left out"))
		}
		for _, m := range ms {
			latest[slotOf(m)] = m
		}
	}
	if len(runs) > 1 {
		fmt.Fprintln(w, "all runs after the first:")
		fmt.Fprintln(w, "  "+total[0].line("strict"))
		fmt.Fprintln(w, "  "+total[1].line("run-scoped PG names left out"))
	}
}

// writeEscapes lists, for one run, the binaries that started children and
// the binaries that read outside their module.
func writeEscapes(w io.Writer, ms []*Manifest) {
	sorted := append([]*Manifest(nil), ms...)
	sort.SliceStable(sorted, func(a, b int) bool { return sorted[a].Package < sorted[b].Package })
	var errs int
	for _, m := range sorted {
		errs += len(m.Errors)
		if m.Children != 0 {
			fmt.Fprintf(w, "  children: %s started %d (job %s; testlog %s)\n", m.Package, m.Children, m.Job, m.Testlog)
		}
		var out []string
		temp := 0
		for _, p := range m.Paths {
			switch p.Scope {
			case "outside":
				out = append(out, p.Path)
			case "temp":
				temp++
			}
		}
		if len(out) > 0 {
			fmt.Fprintf(w, "  outside the module: %s read %d paths (and %d under temp):\n", m.Package, len(out), temp)
			for _, p := range out {
				fmt.Fprintf(w, "      %s\n", p)
			}
		}
		if m.Testlog != "complete" {
			fmt.Fprintf(w, "  testlog %s: %s (exit %d)\n", m.Testlog, m.Package, m.Exit)
		}
	}
	if errs > 0 {
		fmt.Fprintf(w, "  %d manifest errors (see the manifests' errors field)\n", errs)
	}
}
