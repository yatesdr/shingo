package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The fixtures under testdata/b5949e26 are a real gate's output at the base
// the lead times against: cmd.log is the gate's stdout and steps.tsv the lines
// it added to .gate/steps.log, copied from the 2026-11-22 baseline's runs
// (raw/base/2.1/<tier>/pair-1/try-1-B: dev and push at b5949e26 plus one edit,
// cert at b5949e26 itself). That gate.sh writes no selection line and no load
// stamps.

// fakeRun makes one `gatetime run` directory from a fixture tier, with a
// result line of its own; edit may rewrite the gate's output first.
func fakeRun(t *testing.T, tier string, wall, cpu float64, verdict string, edit func(cmdlog, steps string) (string, string)) string {
	t.Helper()
	d := t.TempDir()
	cmdlog, err := os.ReadFile(filepath.Join("testdata", "b5949e26", tier, "cmd.log"))
	if err != nil {
		t.Fatal(err)
	}
	steps, err := os.ReadFile(filepath.Join("testdata", "b5949e26", tier, "steps.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	c, s := string(cmdlog), string(steps)
	if edit != nil {
		c, s = edit(c, s)
	}
	res := resultLine{"wall_s": fmt.Sprintf("%.2f", wall), "job_cpu_s": fmt.Sprintf("%.2f", cpu), "exit": "0", "verdict": verdict, "cmd": "bash scripts/gate.sh"}
	for name, body := range map[string]string{"cmd.log": c, "steps.tsv": s, "result.tsv": res.String() + "\n"} {
		if err := os.WriteFile(filepath.Join(d, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func compareDirs(t *testing.T, dirs ...string) (string, bool) {
	t.Helper()
	var runs []*timedRun
	for _, d := range dirs {
		r, err := readTimedRun(d)
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, r)
	}
	var b bytes.Buffer
	ok := comparePair(&b, "A", "B", runs)
	return b.String(), ok
}

func TestSentenceSelection_TheBaseTiers(t *testing.T) {
	want := map[string]string{
		"dev": "tier=dev | steps=fmt,modbuild,lint,scripts,sim compile,unit | docker=not-run | modules=all | " +
			"scripts=7 of 17; skipped: cache-scope, docker-order, docker-scope, gate-lock, gate-toolchain, golangci-run, install-sync-checkout, makefile-targets — inputs unchanged; push-only: tagged-vet, test-error-discard-ratchet | unit=count1",
		"push": "tier=push | steps=fmt,modbuild,lint,scripts,sim compile,unit,sim | docker=ran(scoped) | modules=all | " +
			"scripts=9 of 17; skipped: cache-scope, docker-order, docker-scope, gate-lock, gate-toolchain, golangci-run, install-sync-checkout, makefile-targets — inputs unchanged | unit=count1 | sim=no-race",
		"cert": "tier=cert | steps=fmt,modbuild,lint,scripts,unit,race,sim | docker=ran(unscoped) | modules=all | scripts= | unit=count1 | sim=race",
	}
	for tier, w := range want {
		b, err := os.ReadFile(filepath.Join("testdata", "b5949e26", tier, "cmd.log"))
		if err != nil {
			t.Fatal(err)
		}
		if got := sentenceSelection(lastSentence(string(b))); got != w {
			t.Errorf("%s:\n got %s\nwant %s", tier, got, w)
		}
	}
	for s, w := range map[string]string{
		"Gate (push): fmt, unit, sim (no -race). Docker suites SKIPPED by scope against origin/main @ abc (nothing in this diff can reach one). race: CI.": "docker=skipped(scope)",
		"Gate (dev): fmt, unit (cached: 12 of 40 packages reused) (scoped to: shingo-core shared).":                                                        "unit=cache(12 of 40 reused)",
		"Gate: the docker suites.": "steps= | docker=ran",
	} {
		if got := sentenceSelection(s); !strings.Contains(got, w) {
			t.Errorf("%q -> %q, want it to hold %q", s, got, w)
		}
	}
	if got := sentenceSelection("Gate (dev): fmt, unit (scoped to: shingo-core shared)."); !strings.Contains(got, "modules=shingo-core shared") {
		t.Errorf("module scope not read: %q", got)
	}
}

// TestCompare_OldTreeArms: four runs of the base's own gate compare by their
// sentences, and the verdict on the difference follows the larger range.
func TestCompare_OldTreeArms(t *testing.T) {
	a1 := fakeRun(t, "push", 300, 2000, "clean", nil)
	b1 := fakeRun(t, "push", 296, 1990, "clean", nil)
	b2 := fakeRun(t, "push", 300, 2010, "clean", nil)
	a2 := fakeRun(t, "push", 305, 2050, "clean", nil)
	out, ok := compareDirs(t, a1, b1, b2, a2)
	if !ok || !strings.Contains(out, "selection: by the gate sentence") || !strings.Contains(out, "wall: no difference") {
		t.Errorf("a like-with-like pair inside the range:\n%s", out)
	}
	b1 = fakeRun(t, "push", 200, 1950, "clean", nil)
	b2 = fakeRun(t, "push", 204, 1990, "clean", nil)
	out, ok = compareDirs(t, a1, b1, b2, a2)
	if !ok || !strings.Contains(out, "wall: B - A = -100.50 s") || !strings.Contains(out, "outside the larger arm's range 5.00 s") {
		t.Errorf("a pair 100 s apart:\n%s", out)
	}
}

// TestCompare_NewTreeAgainstTheBase: B carries the new gate's selection line
// and stamps, A (the base) has neither; the pair is compared by the sentence
// both print, and B's stamps are read.
func TestCompare_NewTreeAgainstTheBase(t *testing.T) {
	newTree := func(stamp string) func(c, s string) (string, string) {
		return func(c, s string) (string, string) {
			s = strings.Replace(s, "930b564f full total 284.6 2026-11-22T21:47:59Z",
				"abc12345 full selection - 2026-11-22T21:47:59Z tier=push docker=in modules=all p=shingo-core:4,integration:4 guards=a,b reused=0\n"+
					"abc12345 full total 284.6 2026-11-22T21:47:59Z "+stamp, 1)
			return c, s
		}
	}
	a1 := fakeRun(t, "push", 300, 2000, "clean", nil)
	a2 := fakeRun(t, "push", 305, 2050, "clean", nil)
	clean := newTree("exec=on ac=on samples=90 standby=0 load=clean foreign30=0.20")
	out, ok := compareDirs(t, a1, fakeRun(t, "push", 280, 1900, "clean", clean), fakeRun(t, "push", 282, 1920, "clean", clean), a2)
	if !ok || !strings.Contains(out, "selection: by the gate sentence") || !strings.Contains(out, "clean, exec=on\n") {
		t.Errorf("new B against the base A:\n%s", out)
	}
	for _, bad := range []string{"load=void", "ac=off", "standby=2"} {
		k, _, _ := strings.Cut(bad, "=")
		stamp := regexp.MustCompile(k+`=\S+`).ReplaceAllString("exec=on ac=on samples=90 standby=0 load=clean foreign30=1.90", bad)
		out, ok = compareDirs(t, a1, fakeRun(t, "push", 280, 1900, "clean", newTree(stamp)), fakeRun(t, "push", 282, 1920, "clean", clean), a2)
		if ok || !strings.Contains(out, "B1") || !strings.Contains(out, "stamps "+bad) {
			t.Errorf("a B run whose gate stamped %s was not refused:\n%s", bad, out)
		}
	}
}

func TestCompare_Refusals(t *testing.T) {
	a1 := fakeRun(t, "push", 300, 2000, "clean", nil)
	a2 := fakeRun(t, "push", 305, 2050, "clean", nil)
	b := fakeRun(t, "push", 290, 1950, "clean", nil)
	cases := []struct {
		name string
		b1   string
		want string
	}{
		{"a void run", fakeRun(t, "push", 290, 1950, "VOID: not-on-AC(2 of 90 samples)", nil), "B1 (" /* named */},
		{"docker skipped", fakeRun(t, "push", 290, 1950, "clean", func(c, s string) (string, string) {
			return strings.Replace(c, " AND the docker suites (scoped against origin/main @ b5949e26).",
				". Docker suites SKIPPED by scope against origin/main @ b5949e26 (nothing in this diff can reach one).", 1), s
		}), "docker=skipped(scope)"},
		{"no sentence among gates", fakeRun(t, "push", 290, 1950, "clean", func(c, s string) (string, string) {
			return strings.ReplaceAll(c, "Gate (push):", "Gat (push):"), s
		}), "not like with like: A1 has a gate sentence, no selection line; B1 has no selection line and no gate sentence"},
		{"an arm 15% apart in CPU", fakeRun(t, "push", 290, 1650, "clean", nil), "arm B's two runs differ by more than 10% in CPU-seconds: 1650.00 and 1950.00"},
		{"a module's docker tests missing", fakeRun(t, "push", 290, 1950, "clean", func(c, s string) (string, string) {
			var keep []string
			for _, l := range strings.Split(s, "\n") {
				if !strings.Contains(l, " docker:shingo/integration") {
					keep = append(keep, l)
				}
			}
			return c, strings.Join(keep, "\n")
		}), "logged steps differ"},
		{"a failed gate", fakeRun(t, "push", 290, 1950, "clean", func(c, s string) (string, string) {
			return c, strings.Replace(s, " full total 284.6", " full total-FAILED 284.6", 1)
		}), "total-FAILED"},
	}
	for _, c := range cases {
		out, ok := compareDirs(t, a1, c.b1, b, a2)
		if ok || !strings.Contains(out, "REFUSED") || !strings.Contains(out, c.want) {
			t.Errorf("%s: not refused, or refused without naming %q:\n%s", c.name, c.want, out)
		}
	}
	// A test package one arm has and the other lacks is the code, not the
	// gate's choice: listed, not refused.
	extra := fakeRun(t, "push", 290, 1950, "clean", func(c, s string) (string, string) {
		return c, s + "930b564f full docker:shingocore/newpkg 1.0 2026-11-22T21:47:59Z\n"
	})
	out, ok := compareDirs(t, a1, extra, b, a2)
	if !ok || !strings.Contains(out, "note: test packages differ") || !strings.Contains(out, "docker:shingocore/newpkg") {
		t.Errorf("a new package in B:\n%s", out)
	}
}

// TestCompare_NotGates: four plain commands compare by command line; a
// different command is refused.
func TestCompare_NotGates(t *testing.T) {
	mk := func(cmd string, cpu float64) string {
		d := t.TempDir()
		res := resultLine{"wall_s": "10.00", "job_cpu_s": fmt.Sprintf("%.2f", cpu), "exit": "0", "verdict": "clean", "cmd": cmd}
		if err := os.WriteFile(filepath.Join(d, "result.tsv"), []byte(res.String()+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return d
	}
	g := "go test -count=1 -exec x ./store/"
	if out, ok := compareDirs(t, mk(g, 30), mk(g, 31), mk(g, 30.5), mk(g, 30.2)); !ok || !strings.Contains(out, "selection: by the command line") {
		t.Errorf("one package four times:\n%s", out)
	}
	if out, ok := compareDirs(t, mk(g, 30), mk(g+" -run X", 31), mk(g, 30.5), mk(g, 30.2)); ok || !strings.Contains(out, "selection differs") {
		t.Errorf("a different command:\n%s", out)
	}
}

// TestCompare_RefusesMixedForeignLines: arms judged by different foreign-load
// lines (gatetime run -foreign-cores) are not like with like; a run from before
// the line was recorded reads as the default, 1.00.
func TestCompare_RefusesMixedForeignLines(t *testing.T) {
	withLine := func(d, line string) string {
		p := filepath.Join(d, "result.tsv")
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.TrimRight(string(b), "\n")+"\tforeign_line="+line+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return d
	}
	a1 := withLine(fakeRun(t, "push", 300, 2000, "clean", nil), "2.50")
	a2 := withLine(fakeRun(t, "push", 305, 2050, "clean", nil), "2.50")
	b1 := withLine(fakeRun(t, "push", 290, 1950, "clean", nil), "2.50")
	b2 := fakeRun(t, "push", 290, 1950, "clean", nil) // no field: the default line
	out, ok := compareDirs(t, a1, b1, b2, a2)
	if ok || !strings.Contains(out, "REFUSED: foreign-load lines differ") || !strings.Contains(out, "B2: 1.00") {
		t.Errorf("mixed lines not refused, or refused without naming them:\n%s", out)
	}
	b2 = withLine(fakeRun(t, "push", 290, 1950, "clean", nil), "2.50")
	if out, ok := compareDirs(t, a1, b1, b2, a2); !ok {
		t.Errorf("one line in all four runs refused:\n%s", out)
	}
}
