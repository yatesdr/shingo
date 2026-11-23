package main

import (
	"bytes"
	"strings"
	"testing"
)

func mani(pkg, bin string, exit, children int, env map[string]string, paths ...PathRead) *Manifest {
	m := &Manifest{Package: pkg, BinarySHA256: bin, Argv: []string{"-test.count=1"}, Exit: exit, Children: children,
		Testlog: "complete", IdentitySHA256: "id1", WallS: 2}
	for k, v := range env {
		m.Env = append(m.Env, EnvRead{Name: k, Value: v})
	}
	m.Paths = paths
	return m
}

// TestReport_HitRule: each way a later run must miss misses, the run-scoped
// Postgres names miss only under the strict rule, temp paths never decide,
// and the children and outside reads are listed.
func TestReport_HitRule(t *testing.T) {
	in := PathRead{Path: "/m/a.txt", Scope: "module", Hash: "file:1"}
	out := PathRead{Path: "/repo/docs/x.md", Scope: "outside", Hash: "file:2"}
	tmp1 := PathRead{Path: "/tmp/TestX001/f", Scope: "temp", Hash: "absent"}
	tmp2 := PathRead{Path: "/tmp/TestX777/f", Scope: "temp", Hash: "absent"}
	pg := map[string]string{"SHINGO_TEST_PG": "sha256:port1"}
	run1 := []*Manifest{
		mani("p/same", "b1", 0, 0, nil, in, tmp1),
		mani("p/binary", "b1", 0, 0, nil),
		mani("p/red", "b1", 1, 0, nil),
		mani("p/path", "b1", 0, 0, nil, out),
		mani("p/pg", "b1", 0, 0, pg),
		mani("p/child", "b1", 0, 2, nil),
	}
	out2 := out
	out2.Hash = "file:changed"
	run2 := []*Manifest{
		mani("p/same", "b1", 0, 0, nil, in, tmp2),
		mani("p/binary", "b2", 0, 0, nil),
		mani("p/red", "b1", 0, 0, nil),
		mani("p/path", "b1", 0, 0, nil, out2),
		mani("p/pg", "b1", 0, 0, map[string]string{"SHINGO_TEST_PG": "sha256:port2"}),
		mani("p/child", "b1", 0, 2, nil),
		mani("p/new", "b1", 0, 0, nil),
	}
	var b bytes.Buffer
	writeReport(&b, []string{"r1", "r2"}, [][]*Manifest{run1, run2})
	got := b.String()
	for _, want := range []string{
		"strict: 2 of 6 binary runs would hit",
		"; 1 cold; 1 of the hits started children; misses: binary 1, earlier-run-red 1, env 1, paths 1",
		"run-scoped PG names left out: 3 of 6 binary runs would hit",
		"children: p/child started 2",
		"outside the module: p/path read 1 paths (and 0 under temp):\n      /repo/docs/x.md",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
}
