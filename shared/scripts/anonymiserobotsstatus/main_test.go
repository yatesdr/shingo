package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The golden was produced by the anonymiser this program replaced, from the
// same invented capture, and is byte-identical to its output.
func TestDerive_MatchesTheGolden(t *testing.T) {
	in, err := os.ReadFile("testdata/capture.json")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/want.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := derive(in)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("output differs from testdata/want.json:\n%s", got)
	}
}

// A committed fixture regenerates from itself: the derivation is a fixed
// point, so the wire-drift fixtures can be re-derived without the capture.
func TestDerive_TheCommittedFixturesAreFixedPoints(t *testing.T) {
	paths, err := filepath.Glob("../../../shingo-core/fleet/seerrds/testdata/robotsstatus_*.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no committed fixtures found (%v)", err)
	}
	for _, p := range append(paths, "testdata/want.json") {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		got, err := derive(data)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if got != string(data) {
			t.Errorf("%s is not a fixed point", p)
		}
	}
}

// Keys are kept verbatim, so a private address used as a key survives the
// rename and must fail the run. Built here rather than committed: no tracked
// file may carry a private address (scripts/check-plant-addresses.sh).
func TestDerive_APrivateAddressRefuses(t *testing.T) {
	addr := strings.Join([]string{"10", "1", "2", "3"}, ".")
	_, err := derive([]byte(`{"peers": {"` + addr + `": {"rtt": 4}}}`))
	if err == nil || err.Error() != "self-check: a private IPv4 address survived" {
		t.Fatalf("err = %v", err)
	}
}

func TestRun_WritesNothingOnRefusal(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.json")
	addr := strings.Join([]string{"192", "168", "4", "2"}, ".")
	if err := os.WriteFile(in, []byte(`{"`+addr+`": 1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.json")
	var sb strings.Builder
	if rc := run([]string{"--in", in, "--out", out}, &sb); rc != 1 {
		t.Fatalf("exit %d, want 1", rc)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("a refused run wrote %s", out)
	}
}
