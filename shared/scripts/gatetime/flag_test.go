package main

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestFlagDir_MirrorHoldsTheClonesFlag: in a checkout the flag is
// <common dir>/gate.timing; in a gate mirror, whose config records the Windows
// clone's flag as gate.timingdir, it is that one; GATE_TIMING_DIR wins over both.
func TestFlagDir_MirrorHoldsTheClonesFlag(t *testing.T) {
	t.Setenv(flagEnvDir, "")
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	common, err := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := flagDir(repo), filepath.Join(common, "gate.timing"); !sameDir(t, got, want) {
		t.Errorf("a checkout's flag: %q, want %q", got, want)
	}
	git("config", "gate.timingdir", "/mnt/c/clone/.git/gate.timing")
	if got := flagDir(repo); got != "/mnt/c/clone/.git/gate.timing" {
		t.Errorf("a mirror's flag: %q, want the clone's it records", got)
	}
	t.Setenv(flagEnvDir, "/private/flag")
	if got := flagDir(repo); got != "/private/flag" {
		t.Errorf("with GATE_TIMING_DIR set: %q", got)
	}
}

// sameDir compares two paths as the filesystem does (git may print a short or
// a long form of a temp path on Windows).
func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	ea, err1 := filepath.EvalSymlinks(filepath.Dir(a))
	eb, err2 := filepath.EvalSymlinks(filepath.Dir(b))
	return err1 == nil && err2 == nil && ea == eb && filepath.Base(a) == filepath.Base(b)
}
