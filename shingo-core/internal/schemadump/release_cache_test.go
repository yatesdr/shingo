package schemadump

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// TestReleaseKey_EveryFieldReachesTheText changes each field of a releaseKey
// alone and requires the key's text and its entry name to change: a field
// text() leaves out is an input two builds can differ in while the cache
// serves one for the other.
func TestReleaseKey_EveryFieldReachesTheText(t *testing.T) {
	t.Parallel()
	var base releaseKey
	n := reflect.TypeOf(base).NumField()
	if n == 0 {
		t.Fatal("releaseKey has no fields")
	}
	for i := range n {
		k := base
		f := reflect.ValueOf(&k).Elem().Field(i)
		f.SetString("changed")
		name := reflect.TypeOf(base).Field(i).Name
		if k.text() == base.text() {
			t.Errorf("releaseKey.%s does not reach text(): a build that differs only in it would be served the other's binary", name)
		}
		if entryName("v0.17.1", k) == entryName("v0.17.1", base) {
			t.Errorf("releaseKey.%s does not reach entryName()", name)
		}
	}
}

// TestReleaseRecipe_IsTheSourceTheBuildRuns: the key's Recipe hashes
// release.go as compiled, which holds the boot main the build writes and the
// build itself.
func TestReleaseRecipe_IsTheSourceTheBuildRuns(t *testing.T) {
	t.Parallel()
	for _, want := range []string{bootMain, "func buildBoot(", `var buildEnv = `, `"-trimpath", "-buildvcs=false"`} {
		if !strings.Contains(releaseRecipe, want) {
			t.Errorf("releaseRecipe (the embedded release.go) does not carry %q, so the key does not cover it", firstLine(want))
		}
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// cacheFixture is a cache directory holding a planted entry, and a build that
// writes a known, different binary and counts its calls.
type cacheFixture struct {
	c                    ReleaseCache
	name, key            string
	planted, fresh       []byte
	plantedSum, freshSum string
	bin, manifest        string
	dst                  string
	builds               int
	buildFor             func(dst string) func() error
}

const testKey = releaseKeyVersion + "\nrecipe r\ncommit c\n"

func newCacheFixture(t *testing.T, rebuild string) *cacheFixture {
	t.Helper()
	dir := t.TempDir()
	f := &cacheFixture{
		c:       ReleaseCache{Dir: filepath.Join(dir, "cache"), Rebuild: rebuild},
		name:    "boot-test",
		key:     testKey,
		planted: []byte("the binary an earlier run stored"),
		fresh:   []byte("the binary this run's build makes"),
		dst:     filepath.Join(dir, "work", "boot"+exeSuffix()),
	}
	if err := os.MkdirAll(filepath.Dir(f.dst), 0o755); err != nil {
		t.Fatal(err)
	}
	f.bin, f.manifest = f.c.paths(f.name)
	f.plantedSum, f.freshSum = sha256Hex(f.planted), sha256Hex(f.fresh)
	f.buildFor = func(dst string) func() error {
		return func() error {
			f.builds++
			return os.WriteFile(dst, f.fresh, 0o755)
		}
	}
	src := filepath.Join(dir, "planted")
	if err := os.WriteFile(src, f.planted, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.c.store(f.name, f.key, src, f.plantedSum); err != nil {
		t.Fatalf("store the planted entry: %v", err)
	}
	return f
}

// TestReleaseCache_ServesOnlyAVerifiedEntry plants an entry, damages it the
// ways a killed writer, a race or a hand can, and requires each damaged entry
// to be rebuilt, never served, and replaced by an entry the next run is
// served.
func TestReleaseCache_ServesOnlyAVerifiedEntry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		damage     func(t *testing.T, f *cacheFixture)
		wantSource string
	}{
		{"a whole entry is served", func(*testing.T, *cacheFixture) {}, "cached"},
		{"no entry", func(t *testing.T, f *cacheFixture) {
			mustRemove(t, f.bin, f.manifest)
		}, "rebuilt (no entry)"},
		{"a stored hash that is not the binary's", func(t *testing.T, f *cacheFixture) {
			rewrite(t, f.manifest, f.plantedSum, sha256Hex([]byte("another binary")))
		}, "rebuilt (entry refused: its binary's sha256 is "},
		{"a manifest that names another key", func(t *testing.T, f *cacheFixture) {
			rewrite(t, f.manifest, "commit c\n", "commit d\n")
		}, "rebuilt (entry refused: its manifest names another key)"},
		{"a malformed sha256 line", func(t *testing.T, f *cacheFixture) {
			rewrite(t, f.manifest, f.plantedSum, f.plantedSum[:10])
		}, "rebuilt (entry refused: its manifest's sha256 line is malformed)"},
		{"a torn binary", func(t *testing.T, f *cacheFixture) {
			if err := os.WriteFile(f.bin, f.planted[:len(f.planted)/2], 0o644); err != nil {
				t.Fatal(err)
			}
		}, "rebuilt (entry refused: its binary's sha256 is "},
		{"a binary with no manifest (killed between the two renames)", func(t *testing.T, f *cacheFixture) {
			mustRemove(t, f.manifest)
		}, "rebuilt (no entry)"},
		{"a manifest with no binary", func(t *testing.T, f *cacheFixture) {
			mustRemove(t, f.bin)
		}, "rebuilt (entry refused: no binary beside its manifest)"},
		{"only the temporary file of a writer killed before its rename", func(t *testing.T, f *cacheFixture) {
			mustRemove(t, f.bin, f.manifest)
			if err := os.WriteFile(f.bin+".tmp-1234", f.planted, 0o644); err != nil {
				t.Fatal(err)
			}
		}, "rebuilt (no entry)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCacheFixture(t, "")
			tc.damage(t, f)
			source, sum, err := f.c.obtain(f.name, f.key, f.dst, f.buildFor(f.dst))
			if err != nil {
				t.Fatalf("obtain: %v", err)
			}
			if !strings.HasPrefix(source, tc.wantSource) {
				t.Fatalf("source %q, want it to start %q", source, tc.wantSource)
			}
			want, wantSum, wantBuilds := f.fresh, f.freshSum, 1
			if tc.wantSource == "cached" {
				want, wantSum, wantBuilds = f.planted, f.plantedSum, 0
			}
			assertBinary(t, f.dst, want)
			if sum != wantSum || f.builds != wantBuilds {
				t.Fatalf("sum %s after %d builds, want %s after %d", sum, f.builds, wantSum, wantBuilds)
			}
			// The next run is served what this one left.
			next := filepath.Join(filepath.Dir(f.dst), "next"+exeSuffix())
			source, _, err = f.c.obtain(f.name, f.key, next, f.buildFor(next))
			if err != nil || source != "cached" || f.builds != wantBuilds {
				t.Fatalf("the next obtain: source %q, err %v, builds %d; want cached with no build", source, err, f.builds)
			}
			assertBinary(t, next, want)
		})
	}
}

// TestReleaseCache_RebuildNeverReadsAnEntry: with Rebuild set (cert, GitHub)
// a whole entry is not served; the release is built and the entry replaced.
func TestReleaseCache_RebuildNeverReadsAnEntry(t *testing.T) {
	t.Parallel()
	f := newCacheFixture(t, "GATE_TIER=cert")
	// The planted entry holds what this build makes, so the audit passes.
	src := filepath.Join(t.TempDir(), "same")
	if err := os.WriteFile(src, f.fresh, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.c.store(f.name, f.key, src, f.freshSum); err != nil {
		t.Fatal(err)
	}
	source, sum, err := f.c.obtain(f.name, f.key, f.dst, f.buildFor(f.dst))
	if err != nil {
		t.Fatalf("obtain: %v", err)
	}
	if source != "rebuilt (forced: GATE_TIER=cert)" || f.builds != 1 || sum != f.freshSum {
		t.Fatalf("source %q, %d builds, sum %s; want a forced rebuild of the fresh binary", source, f.builds, sum)
	}
	assertBinary(t, f.dst, f.fresh)
}

// TestReleaseCache_RebuildFailsWhenTheEntryDiffers: under Rebuild, an entry
// that verifies but holds another binary than a fresh build of the same key
// means the key misses an input; the build fails naming that, keeps the old
// binary for the comparison, and leaves the fresh one as the entry.
func TestReleaseCache_RebuildFailsWhenTheEntryDiffers(t *testing.T) {
	t.Parallel()
	f := newCacheFixture(t, "GATE_TIER=cert")
	_, _, err := f.c.obtain(f.name, f.key, f.dst, f.buildFor(f.dst))
	if err == nil || !strings.Contains(err.Error(), "the key misses an input the build reads") {
		t.Fatalf("obtain err %v, want the build to fail naming the key's missing input", err)
	}
	kept := strings.TrimSuffix(f.bin, exeSuffix()) + ".differed" + exeSuffix()
	assertBinary(t, kept, f.planted)
	assertBinary(t, f.bin, f.fresh)
	if sum, miss := manifestSum(f.manifest, f.key); miss != "" || sum != f.freshSum {
		t.Fatalf("the entry after: sum %s, miss %q; want the fresh build's", sum, miss)
	}
}

// TestReleaseCache_AServedBinaryRunsAtOnce pins what obtain owes BootRelease:
// the binary it leaves at dst can be run the moment obtain returns, whatever
// else this process is starting. Eight workers each take a runnable entry
// from the cache and run their copy straight away, 100 times each: the shape
// of the convergence test's parallel vintages (BuildRelease, then
// BootRelease), where every worker's fork lands beside the others' copies.
//
// LINUX ONLY (scripts/gate-os-tests.txt names it). Linux refuses to run a
// file that any process holds open for writing (ETXTBSY, "text file busy"),
// and a child forked while a copy's handle is open holds that handle until
// its own exec. The entry here is a /bin/sh script, which that rule covers as
// it does a binary.
func TestReleaseCache_AServedBinaryRunsAtOnce(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux only: refusing to run a file that is open for writing (ETXTBSY) is Linux's rule, and the entry this test runs is a /bin/sh script")
	}
	t.Parallel()
	f := newCacheFixture(t, "")
	script := []byte("#!/bin/sh\nexit 0\n")
	src := filepath.Join(t.TempDir(), "runnable")
	if err := os.WriteFile(src, script, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.c.store(f.name, f.key, src, sha256Hex(script)); err != nil {
		t.Fatalf("store the runnable entry: %v", err)
	}
	notBuilt := func() error { return errors.New("the planted entry was not served, and this test builds nothing") }

	const workers, rounds = 8, 100
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	failed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return first != nil
	}
	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if first == nil {
			first = err
		}
	}
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < rounds && !failed(); r++ {
				dst := filepath.Join(filepath.Dir(f.dst), fmt.Sprintf("boot-%d-%d", w, r))
				source, _, err := f.c.obtain(f.name, f.key, dst, notBuilt)
				if err != nil || source != "cached" {
					fail(fmt.Errorf("worker %d, round %d: obtain gave source %q, err %v; want the planted entry served", w, r, source, err))
					return
				}
				if err := exec.Command(dst).Run(); err != nil {
					fail(fmt.Errorf("worker %d, round %d: run the binary obtain had just served: %w", w, r, err))
					return
				}
			}
		}()
	}
	wg.Wait()
	switch {
	case first == nil:
	case errors.Is(first, syscall.ETXTBSY):
		t.Fatalf("%v\n\nA child this process forked while copyHashing had the file open for writing still held that "+
			"handle when the file was run. Hold syscall.ForkLock.RLock() from the open of dst to its close "+
			"(copyHashing, release_cache.go), so that no fork lands in between; retrying the run would only hide it.", first)
	default:
		t.Fatal(first)
	}
}

func assertBinary(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s holds %q, want %q", path, got, want)
	}
}

func rewrite(t *testing.T, path, old, replacement string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), old) {
		t.Fatalf("%s does not contain %q", path, old)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(b), old, replacement, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustRemove(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
}
