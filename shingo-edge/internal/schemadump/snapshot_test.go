package schemadump_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shingoedge/internal/schemadump"
	"shingoedge/store"
	"shingoedge/store/schema"
)

// moduleRoot is shingo-edge/, two levels up from internal/schemadump.
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
// The snapshot is the only file in this module that IS the schema. It is worth
// nothing if it drifts, and nobody will remember to regenerate it — so this
// test remembers, and it fails with the exact command that fixes it.
//
// Unlike Core's, this needs no Docker: SQLite is a file, so it runs in the
// default test suite where it will actually be seen.
func TestSchemaSnapshotIsCurrent(t *testing.T) {
	root := moduleRoot(t)
	path := filepath.Join(root, filepath.FromSlash(schemadump.SnapshotPath))

	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v\n\nrun `%s` and commit the result", schemadump.SnapshotPath, err, schemadump.RegenCommand)
	}

	built := buildFresh(t)
	want := strings.ReplaceAll(string(committed), "\r\n", "\n")
	if built == want {
		return
	}
	t.Fatalf("schema snapshot is stale — run `%s` and commit the result\n\nfirst difference:\n%s",
		schemadump.RegenCommand, firstDiff(want, built, "committed", "built"))
}

// releaseWork holds the release build this binary makes; TestMain removes it.
var releaseWork string

func TestMain(m *testing.M) {
	var err error
	if releaseWork, err = os.MkdirTemp("", "edge-schemadump-release-"); err != nil {
		fmt.Fprintf(os.Stderr, "schemadump: release work dir: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(releaseWork)
	os.Exit(code)
}

// retiredRelease is the retired chain's release, built once per binary.
func retiredRelease(ctx context.Context, t *testing.T) *schemadump.Release {
	t.Helper()
	rel, err := schemadump.BuildRelease(ctx, moduleRoot(t), store.RetiredChainRelease, releaseWork)
	if err != nil {
		t.Fatalf("build %s: %v", store.RetiredChainRelease, err)
	}
	return rel
}

// refusedWhole opens path as a start does and requires the refusal, with the
// file's bytes as they were.
func refusedWhole(t *testing.T, path string) string {
	t.Helper()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	db, err := store.Open(path)
	if err == nil {
		db.Close()
		t.Fatalf("a file that is not v1..v%d whole was opened", store.RetiredChain)
	}
	var refusal *store.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("open = %v, want a refusal", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("the refusal changed the file (%v)", err)
	}
	return refusal.Error()
}

// TestSchemaConvergesAcrossVintages builds databases the ways the plants' were
// built and asserts each, brought up by this build, has the shape of a
// database born on the baseline:
//
//	fresh   — an empty file, born on the baseline (store.Open)
//	vintage — an old baseline DDL from git
//
// This build refuses a vintage below the retired chain, by design: the test
// asserts that refusal, and that it wrote nothing, then walks the vintage up
// with store.RetiredChainRelease, the release that carries the chain, and
// opens it with this build. That is the upgrade a plant on an older release
// makes. The two plants' own shapes are store's TestPlantShapes_WhatAStartLeaves.
//
// A vintage that fails for a reason you cannot explain is a FINDING, not an
// obstacle: it means an edge at that vintage genuinely gets a different
// database from a new one.
func TestSchemaConvergesAcrossVintages(t *testing.T) {
	root := filepath.Dir(moduleRoot(t)) // repo root — git needs the work tree
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rel := retiredRelease(ctx, t)
	fresh := schema.ParseShape(buildFresh(t))

	for _, v := range schemadump.Vintages {
		t.Run(strings.NewReplacer("^", "-parent", "/", "-", ".", "_").Replace(v.Rev), func(t *testing.T) {
			ddl, err := schemadump.BaselineFromGit(root, v.Rev)
			if err != nil {
				t.Fatalf("read baseline at %s: %v\n(%s)", v.Rev, err, v.Why)
			}
			path, err := schemadump.BuildAged(t.TempDir(), ddl)
			if err != nil {
				t.Fatalf("build a %s edge database: %v\n(%s)", v.Rev, err, v.Why)
			}
			if why := refusedWhole(t, path); !strings.Contains(why, store.RetiredChainRelease) {
				t.Errorf("the refusal does not name %s: %s", store.RetiredChainRelease, why)
			}
			if _, err := rel.Boot(ctx, path); err != nil {
				t.Fatal(err)
			}
			db, err := store.Open(path)
			if err != nil {
				t.Fatalf("this build on a %s database %s brought up: %v", v.Rev, rel.Rev, err)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			aged, err := schemadump.Dump(path)
			if err != nil {
				t.Fatalf("dump aged database: %v", err)
			}
			var unexplained []string
			missing, differs := schema.Compare(fresh, schema.ParseShape(aged))
			for _, d := range append(missing, differs...) {
				if !isKnown(d) {
					unexplained = append(unexplained, d)
				}
			}
			if len(unexplained) > 0 {
				t.Fatalf("an edge database created at %s and brought up to today does NOT match a born one.\n"+
					"vintage: %s\n\n  %s", v.Rev, v.Why, strings.Join(unexplained, "\n  "))
			}
		})
	}
}

func isKnown(diff string) bool {
	for _, kd := range schemadump.KnownDivergences {
		if kd.Key == diff {
			return true
		}
	}
	return false
}

func buildFresh(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path, err := schemadump.BuildFresh(dir)
	if err != nil {
		t.Fatalf("build fresh database: %v", err)
	}
	dump, err := schemadump.Dump(path)
	schemadump.RemoveDB(path)
	if err != nil {
		t.Fatalf("dump fresh database: %v", err)
	}
	return dump
}

// firstDiff renders the first divergence with a little context.
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
		return sb.String()
	}
	return "(one is a prefix of the other)"
}
