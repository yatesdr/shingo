package canary

// The reuse wrapper's canary. The gate runs this package through the wrapper
// once per gate, before any reuse, and reuse.CanaryMain compares what the
// wrapper recorded with what the test below is known to do. If the record
// differs, reuse is off for that gate run: the wrapper's eyes are checked on
// a known input before its verdicts are trusted.
//
// One of each thing the wrapper must see:
//   - a package-level var reads a file before m.Run (testdata/pre.txt): only
//     the Linux trace can see it, and CanaryMain requires it there;
//   - an env var read (SHINGO_REUSE_CANARY);
//   - a file read inside the module (testdata/in.txt) and one outside it (the
//     repo root's go.work, in no module);
//   - a directory listed (testdata);
//   - a child process started (this test binary again), which reads
//     testdata/child.txt.
//
// Run with no wrapper (the dev gate's unit step) it is an ordinary passing
// test.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

var pre = read(filepath.Join("testdata", "pre.txt"))

func read(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

func TestCanary(t *testing.T) {
	if os.Getenv("SHINGO_REUSE_CANARY_CHILD") == "1" {
		if read(filepath.Join("testdata", "child.txt")) == "" {
			t.Fatal("the child could not read testdata/child.txt")
		}
		return
	}
	if pre == "" {
		t.Fatal("the package var did not read testdata/pre.txt")
	}
	_ = os.Getenv("SHINGO_REUSE_CANARY")
	if read(filepath.Join("testdata", "in.txt")) == "" {
		t.Fatal("testdata/in.txt is unreadable")
	}
	if read(filepath.Join("..", "..", "..", "..", "..", "go.work")) == "" {
		t.Fatal("the repo root's go.work is unreadable")
	}
	if ents, err := os.ReadDir("testdata"); err != nil || len(ents) != 3 {
		t.Fatalf("testdata lists %d entries (%v), want 3", len(ents), err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCanary$")
	cmd.Env = append(os.Environ(), "SHINGO_REUSE_CANARY_CHILD=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the child failed: %v\n%s", err, out)
	}
}
