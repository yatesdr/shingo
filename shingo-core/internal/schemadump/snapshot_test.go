//go:build docker

package schemadump_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"shingocore/internal/schemadump"
	"shingocore/store"
)

// moduleRoot is shingo-core/, two levels up from internal/schemadump.
func moduleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// One Postgres for the whole test binary, not one per test. Container startup
// was a fixed cost each test paid alone; the convergence test's vintages and
// the package's other tests can share an instance because every database they
// build is created empty and named uniquely — nothing crosses between them.
var sharedInst *schemadump.Instance

// startErr is why sharedInst is nil: the tests that need Postgres skip on it.
var startErr error

// releaseWork holds the release binaries (schemadump.BuildRelease) this binary
// boots, built or copied from the release cache once each, for every test
// that boots a release; removed when the binary exits.
var releaseWork string

// releaseCacheForTests keeps the release builds across runs in the user's
// cache directory (schemadump.ReleaseCache). Its key is content, not a
// worktree, so every worktree on the host shares it. The gate's cert tier and
// GitHub never read an entry: they build every release, and a fresh build
// that differs from the entry under its key fails.
func releaseCacheForTests() schemadump.ReleaseCache {
	dir, err := os.UserCacheDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "schemadump: no user cache directory (%v): every release is built\n", err)
		return schemadump.ReleaseCache{}
	}
	c := schemadump.ReleaseCache{Dir: filepath.Join(dir, "shingo", "schemadump-boot")}
	switch {
	case os.Getenv("GATE_TIER") == "cert":
		c.Rebuild = "GATE_TIER=cert: certification never reuses"
	case os.Getenv("GITHUB_ACTIONS") == "true":
		c.Rebuild = "GITHUB_ACTIONS=true: GitHub always builds"
	}
	return c
}

// TestMain starts the one Postgres (or, with $SHINGO_TEST_PG set, takes the
// server it names: schemadump.Start). When it cannot, the binary still runs: the
// package's untagged tests (baseline_test.go, release_cache_test.go) need no
// database, and in the gate's docker step this binary is the only place they
// run, so exiting here would drop them along with the snapshot tests. The line
// printed carries the SHINGO-DOCKER-DOWN prefix scripts/gate.sh greps for, and
// under SHINGO_TEST_REQUIRE_DOCKER a failed start is a failure, as in testdb.
func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	inst, err := schemadump.Start(ctx)
	if err != nil {
		if os.Getenv("SHINGO_TEST_REQUIRE_DOCKER") != "" {
			fmt.Fprintf(os.Stderr, "SHINGO-DOCKER-DOWN (required): schemadump: start postgres: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "SHINGO-DOCKER-DOWN: skipping integration tests: schemadump: start postgres: %v\n", err)
		startErr = err
		os.Exit(m.Run())
	}
	sharedInst = inst
	if releaseWork, err = os.MkdirTemp("", "schemadump-release-"); err != nil {
		fmt.Fprintf(os.Stderr, "schemadump: release work dir: %v\n", err)
		_ = inst.Close(context.Background())
		os.Exit(1)
	}
	schemadump.UseReleaseCache(releaseCacheForTests())
	code := m.Run()
	cancel()
	_ = inst.Close(context.Background())
	_ = os.RemoveAll(releaseWork)
	os.Exit(code)
}

// TestStart_UsesTheServerSHINGO_TEST_PGNames pins Start's rule for the gate's
// docker step and CI's docker jobs: with $SHINGO_TEST_PG set, the instance is
// that server and Start started no container of its own (Start says why).
// Without it Start starts its own, the path cmd/schemasnapshot and
// cmd/schemabaseline take, and there is nothing here to check.
func TestStart_UsesTheServerSHINGO_TEST_PGNames(t *testing.T) {
	t.Parallel()
	addr := os.Getenv("SHINGO_TEST_PG")
	if addr == "" {
		t.Skip("SHINGO_TEST_PG unset: Start starts a container of its own")
	}
	inst := start(context.Background(), t)
	if got := inst.AddrForTest(); got != addr {
		t.Fatalf("the instance is at %s: with SHINGO_TEST_PG=%s set, Start must use that server and start no container", got, addr)
	}
}

// TestSchemaSnapshotIsCurrent builds the schema from scratch and compares it
// to the committed snapshot.
//
// The snapshot is the only file in this repository that IS the schema. It is
// worth nothing if it drifts, and nobody will remember to regenerate it — so
// this test remembers, and it fails with the exact command that fixes it.
func TestSchemaSnapshotIsCurrent(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	path := filepath.Join(root, filepath.FromSlash(schemadump.SnapshotPath))

	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v\n\nrun `%s` and commit the result", schemadump.SnapshotPath, err, schemadump.RegenCommand)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	inst := start(ctx, t)
	dbName, err := inst.BuildFresh(ctx)
	if err != nil {
		t.Fatalf("build fresh database: %v", err)
	}
	built, err := inst.Dump(ctx, dbName)
	if err != nil {
		t.Fatalf("dump fresh database: %v", err)
	}

	want := strings.ReplaceAll(string(committed), "\r\n", "\n")
	if built == want {
		return
	}
	t.Fatalf("schema snapshot is stale — run `%s` and commit the result\n\nfirst difference:\n%s",
		schemadump.RegenCommand, firstDiff(want, built, "committed", "built"))
}

// TestSchemaConvergesAcrossVintages is the one that catches the bug class.
//
// It builds databases the ways the plants' were built and asserts that each
// one, brought up by this build, has the SAME SHAPE as a database born on the
// baseline:
//
//	fresh   — an empty database, born on the baseline (store.migrate)
//	vintage — a database made by a released binary built from git
//	          (schemadump.BuildRelease): that release's own store.Open, the
//	          code the plant ran; or born on an old baseline DDL; or a
//	          current release's carrying a plant's leftovers (Vintage.Drift)
//
// This build refuses a vintage below the retired chain, by design: the test
// asserts that refusal, and that it wrote nothing, then walks the vintage up
// with store.RetiredChainRelease, the release that carries the chain, and
// boots this build again. That is the upgrade a plant on an older release
// makes.
//
// Shapes are compared by Normalize + Canonical, partition children excluded
// (ExcludePartitions): column ORDER legitimately differs (a fresh database
// gets a column where its CREATE TABLE put it, an upgraded one where the ALTER
// appended it), and runtime partitions are made by the code that writes them.
//
// A vintage that fails for a reason you cannot explain is a FINDING, not an
// obstacle: the upgrade path from that vintage does not converge, and a plant
// at that vintage would get a different database from a new one.
func TestSchemaConvergesAcrossVintages(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	inst := start(ctx, t)

	freshDB, err := inst.BuildFresh(ctx)
	if err != nil {
		t.Fatalf("build fresh database: %v", err)
	}
	fresh, err := inst.Dump(ctx, freshDB)
	if err != nil {
		t.Fatalf("dump fresh database: %v", err)
	}
	freshShape := schemadump.Canonical(schemadump.ExcludePartitions(fresh))

	for _, v := range schemadump.Vintages {
		t.Run(v.Name, func(t *testing.T) {
			t.Parallel()
			// Own context, not the parent's: the parent function returns (and
			// defers cancel) as soon as these subtests are SCHEDULED, which
			// would cancel every build still queued behind t.Parallel().
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()

			name, err := inst.CreateDatabase("vintage")
			if err != nil {
				t.Fatal(err)
			}
			if v.DDLRev != "" {
				ddl, err := schemadump.BaselineFromGit(root, v.DDLRev)
				if err != nil {
					t.Fatalf("read the baseline DDL at %s: %v\n(%s)", v.DDLRev, err, v.Why)
				}
				if err := inst.ApplyRaw(ctx, name, ddl); err != nil {
					t.Fatalf("apply %s's baseline DDL: %v\n(%s)", v.DDLRev, err, v.Why)
				}
			}
			if v.BuiltBy != "" {
				bootRelease(ctx, t, inst, root, v.BuiltBy, name, v.Why)
			}
			if v.BuiltBy != store.RetiredChainRelease && !v.Current {
				assertRefusedWritingNothing(ctx, t, inst, name, v)
				bootRelease(ctx, t, inst, root, store.RetiredChainRelease, name, v.Why)
			}
			if v.Drift != "" {
				if err := inst.ApplyRaw(ctx, name, v.Drift); err != nil {
					t.Fatalf("apply the vintage's leftovers: %v\n(%s)", err, v.Why)
				}
			}
			if err := inst.Reboot(ctx, name); err != nil {
				t.Fatalf("this build's boot of the vintage: %v\n(%s)", err, v.Why)
			}
			aged, err := inst.Dump(ctx, name)
			if err != nil {
				t.Fatalf("dump the vintage: %v", err)
			}
			if drift, err := inst.DriftAfterBootForTest(name); err != nil || len(drift) != 0 {
				t.Errorf("the boot's drift report on the vintage (err %v):\n  %s", err, strings.Join(drift, "\n  "))
			}
			agedShape := schemadump.Canonical(schemadump.ExcludePartitions(aged))
			if agedShape == freshShape {
				return
			}
			t.Fatalf("a database %s and brought up by this build does NOT match one born on the baseline.\n"+
				"vintage: %s\n\n"+
				"This is the shape that has already broken two plant deploys: the upgrade path\n"+
				"produces a different database from the one every test builds.\n\n%s",
				v.Name, v.Why, shapeDiff(freshShape, agedShape))
		})
	}
}

// bootRelease builds rev (once per test binary) and boots it on dbName. The
// log line says whether the binary was cached or rebuilt, and why.
func bootRelease(ctx context.Context, t *testing.T, inst *schemadump.Instance, root, rev, dbName, why string) {
	t.Helper()
	rel, err := schemadump.BuildRelease(ctx, root, rev, releaseWork)
	if err != nil {
		t.Fatalf("build %s: %v\n(%s)", rev, err, why)
	}
	t.Logf("%s's boot binary: %s, sha256 %s", rev, rel.Source, rel.Sum)
	if _, err := inst.BootRelease(ctx, rel, dbName); err != nil {
		t.Fatalf("boot by %s: %v\n(%s)", rev, err, why)
	}
}

// assertRefusedWritingNothing boots this build on a vintage below the retired
// chain and requires the refusal, by its words, and the schema and rows unchanged.
func assertRefusedWritingNothing(ctx context.Context, t *testing.T, inst *schemadump.Instance, dbName string, v schemadump.Vintage) {
	t.Helper()
	before, err := inst.DumpWithRowsForTest(ctx, dbName)
	if err != nil {
		t.Fatalf("dump before the refused boot: %v", err)
	}
	want := "start " + store.RetiredChainRelease + " on this database first"
	if v.BuiltBy == "" {
		want = "objects in schema public but no schema_migrations: restore the whole dump"
	}
	err = inst.Reboot(ctx, dbName)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("this build's boot of the vintage before %s: err %v, want the refusal saying %q\n(%s)",
			store.RetiredChainRelease, err, want, v.Why)
	}
	t.Logf("refused as it should be: %v", err)
	after, err := inst.DumpWithRowsForTest(ctx, dbName)
	if err != nil {
		t.Fatalf("dump after the refused boot: %v", err)
	}
	if after != before {
		t.Fatalf("the refused boot changed the database (a refusal writes nothing):\n%s",
			firstDiff(before, after, "before", "after"))
	}
}

func start(ctx context.Context, t *testing.T) *schemadump.Instance {
	t.Helper()
	if startErr != nil {
		t.Skipf("no postgres: %v", startErr)
	}
	if sharedInst == nil {
		t.Fatal("TestMain did not start the shared instance")
	}
	return sharedInst
}

// shapeDiff reports which lines exist in one canonical form and not the other.
//
// A positional diff of two sorted 1,400-line dumps reads as a wall of noise
// where a single missing column shifts everything after it. The question the
// convergence test actually asks is a set question — "what does the aged
// database not have" — so the answer is given as one.
func shapeDiff(fresh, aged string) string {
	inFresh := map[string]int{}
	for l := range strings.SplitSeq(fresh, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			inFresh[t]++
		}
	}
	var onlyAged []string
	for l := range strings.SplitSeq(aged, "\n") {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if inFresh[t] > 0 {
			inFresh[t]--
			continue
		}
		onlyAged = append(onlyAged, t)
	}
	var onlyFresh []string
	for l, n := range inFresh {
		for range n {
			onlyFresh = append(onlyFresh, l)
		}
	}
	sort.Strings(onlyFresh)
	sort.Strings(onlyAged)

	var sb strings.Builder
	sb.WriteString("in FRESH but not in the upgraded database (the plant would be missing these):\n")
	sb.WriteString(bullets(onlyFresh))
	sb.WriteString("\nin the UPGRADED database but not in FRESH (the plant carries these and a new install would not):\n")
	sb.WriteString(bullets(onlyAged))
	return sb.String()
}

func bullets(lines []string) string {
	if len(lines) == 0 {
		return "  (none)\n"
	}
	var sb strings.Builder
	const maxShown = 40
	for i, l := range lines {
		if i == maxShown {
			sb.WriteString("  ... and " + itoa(len(lines)-maxShown) + " more\n")
			break
		}
		sb.WriteString("  " + l + "\n")
	}
	return sb.String()
}

// firstDiff renders the first divergence with a little context. A 49KB
// side-by-side is unreadable; the first difference is almost always the whole
// story, and the regen command is right there in the message.
func firstDiff(a, b, aLabel, bLabel string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	n := min(len(al), len(bl))
	for i := range n {
		if al[i] == bl[i] {
			continue
		}
		var sb strings.Builder
		for j := max(0, i-3); j < i; j++ {
			sb.WriteString("  " + al[j] + "\n")
		}
		sb.WriteString("- " + aLabel + ": " + al[i] + "\n")
		sb.WriteString("+ " + bLabel + ": " + bl[i] + "\n")
		sb.WriteString("\n(" + aLabel + " has " + itoa(len(al)) + " lines, " + bLabel + " has " + itoa(len(bl)) + ")")
		return sb.String()
	}
	return "(identical for " + itoa(n) + " lines; " + aLabel + " has " + itoa(len(al)) +
		" lines, " + bLabel + " has " + itoa(len(bl)) + " — one is a prefix of the other)"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
