package schemadump_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"shingoedge/internal/schemadump"
	"shingoedge/store"
)

// plant opens path raw and runs stmts.
func plant(t *testing.T, path string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// startThisBuild opens and closes path as a start does.
func startThisBuild(path string) error {
	db, err := store.Open(path)
	if err != nil {
		return err
	}
	return db.Close()
}

// TestTheRefusalsRemedy: what each refusal tells a person to do works. Started
// once on the refused file, the retired chain's release refills it and this
// build takes it; a file missing v1 it cannot refill, which that refusal says.
func TestTheRefusalsRemedy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rel := retiredRelease(ctx, t)
	for _, c := range []struct {
		name, plant string
		refills     bool
	}{
		{"a hole", `DELETE FROM schema_migrations WHERE version = 30`, true},
		{"a head below the last", `DELETE FROM schema_migrations WHERE version > 40`, true},
		{"an empty version table", `DELETE FROM schema_migrations`, true},
		{"no version table", `DROP TABLE schema_migrations`, true},
		{"v1 gone", `DELETE FROM schema_migrations WHERE version = 1`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			path, err := schemadump.BuildFresh(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			plant(t, path, c.plant)
			refusedWhole(t, path)
			if _, err := rel.Boot(ctx, path); err != nil {
				t.Fatal(err)
			}
			if err := startThisBuild(path); (err == nil) != c.refills {
				t.Errorf("this build after %s started once: %v, refills want %t", rel.Rev, err, c.refills)
			}
		})
	}
}
