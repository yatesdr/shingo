//go:build docker

package schemadump_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"shingocore/internal/schemadump"
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

// TestSchemaSnapshotIsCurrent builds the schema from scratch and compares it
// to the committed snapshot.
//
// The snapshot is the only file in this repository that IS the schema. It is
// worth nothing if it drifts, and nobody will remember to regenerate it — so
// this test remembers, and it fails with the exact command that fixes it.
func TestSchemaSnapshotIsCurrent(t *testing.T) {
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
// It builds the database two ways and asserts the results are IDENTICAL:
//
//	fresh — today's baseline + all migrations
//	aged  — an OLD baseline (from git) + all migrations
//
// schema.Apply runs CREATE ... IF NOT EXISTS, which does nothing to a table
// that already exists. So a column added to a baseline CREATE TABLE reaches a
// fresh database instantly and a plant database NEVER, and every test in this
// repository builds a fresh database. That asymmetry has cost us twice: the
// misplaced code/ref index (worked on a fresh DB, absent at the plant) and the
// five long-inert baseline indexes an old dump trips.
//
// With this test, anything that only works on a fresh database fails on the
// machine of whoever wrote it.
//
// A vintage that fails for a reason you cannot explain is a FINDING, not an
// obstacle — it means the upgrade path from that vintage genuinely does not
// converge, and a plant at that vintage would get a different database from a
// new one.
func TestSchemaConvergesAcrossVintages(t *testing.T) {
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

	for _, v := range schemadump.Vintages {
		t.Run(strings.NewReplacer("^", "-parent", "/", "-", ".", "_").Replace(v.Rev), func(t *testing.T) {
			baseline, err := schemadump.BaselineFromGit(root, v.Rev)
			if err != nil {
				t.Fatalf("read baseline at %s: %v\n(%s)", v.Rev, err, v.Why)
			}
			agedDB, err := inst.BuildAged(ctx, baseline)
			if err != nil {
				t.Fatalf("upgrade a %s database to today: %v\n(%s)", v.Rev, err, v.Why)
			}
			aged, err := inst.Dump(ctx, agedDB)
			if err != nil {
				t.Fatalf("dump aged database: %v", err)
			}
			// Canonical, not literal: column ORDER legitimately differs
			// (a fresh database gets the column where the baseline CREATE
			// TABLE puts it; an upgraded one gets it appended by an ALTER)
			// and order is not part of the shape. See schemadump.Canonical.
			if schemadump.Canonical(aged) == schemadump.Canonical(fresh) {
				return
			}
			t.Fatalf("a database created at %s and upgraded to today does NOT match a fresh one.\n"+
				"vintage: %s\n\n"+
				"This is the shape that has already broken two plant deploys: the upgrade path\n"+
				"produces a different database from the one every test builds.\n\n%s",
				v.Rev, v.Why, shapeDiff(schemadump.Canonical(fresh), schemadump.Canonical(aged)))
		})
	}
}

func start(ctx context.Context, t *testing.T) *schemadump.Instance {
	t.Helper()
	inst, err := schemadump.Start(ctx)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "docker") {
			t.Skipf("skipping: %v", err)
		}
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })
	return inst
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
