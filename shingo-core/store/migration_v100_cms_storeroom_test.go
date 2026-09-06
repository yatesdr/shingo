//go:build docker

package store_test

import (
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"shingocore/internal/testdb"
	"shingocore/material"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/schema"
)

// TestV100_ATaggedNodeStillWalksAsABoundary is the POSITIVE half of v100, and
// it was the missing one.
//
// The test above proves the retired key is deleted. Nothing proved that the key
// replacing it still works after the migration has run — and "the old thing is
// gone" plus "the new thing works" are different claims, of which only the
// first had a test. A v100 that deleted rather more than it meant to would have
// passed the suite.
//
// It walks through material.FindCMSBoundary rather than reading the property
// back, because reading it back only proves the row survived; the question is
// whether the boundary walk finds it.
func TestV100_ATaggedNodeStillWalksAsABoundary(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)

	root := &nodes.Node{Name: "V100-BOUNDARY-ROOT", IsSynthetic: true, Enabled: true}
	if err := db.CreateNode(root); err != nil {
		t.Fatalf("create root: %v", err)
	}
	if err := db.SetNodeProperty(root.ID, material.CMSStoreroomProperty, "SM07"); err != nil {
		t.Fatalf("tag boundary: %v", err)
	}
	// A slot beneath it, so the walk has somewhere to walk FROM.
	slot := &nodes.Node{Name: "V100-BOUNDARY-SLOT", Enabled: true, ParentID: &root.ID}
	if err := db.CreateNode(slot); err != nil {
		t.Fatalf("create slot: %v", err)
	}
	// And the retired key on the same node, so v100 has something to delete
	// beside the one that must survive.
	if err := db.SetNodeProperty(root.ID, "log_cms_transactions", "true"); err != nil {
		t.Fatalf("seed retired property: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 100`); err != nil {
		t.Fatalf("clear v100 row: %v", err)
	}

	migrated, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("re-open to apply v100: %v", err)
	}
	defer migrated.Close()

	node, code, err := material.FindCMSBoundary(migrated, slot.ID)
	if err != nil {
		t.Fatalf("FindCMSBoundary: %v", err)
	}
	if node == nil {
		t.Fatal("the walk found no boundary above a tagged root — v100 removed more than " +
			"the retired key, and the whole feed would go quiet with nothing saying why")
	}
	if code != "SM07" {
		t.Errorf("storeroom = %q, want SM07", code)
	}
	if node.ID != root.ID {
		t.Errorf("boundary = node %d, want the tagged root %d", node.ID, root.ID)
	}
}

// TestNodePropertyKeyAbsent_DoesNotReportAFailedQueryAsSuccess.
//
// This is v100's verify predicate, and a verify predicate answers "does the
// post-condition hold". Hand-rolled, it dropped the Scan error: a query that
// FAILED left `exists` false, `!exists` was true, and the migration was
// recorded as applied without anything having checked — absence of data
// rendering as absence of a problem, in the one place whose job is to notice.
//
// A closed database is the cheapest way to make the query fail for a reason
// that is not "the key is gone".
func TestNodePropertyKeyAbsent_DoesNotReportAFailedQueryAsSuccess(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	// Truthful answer first, so the test cannot pass by always returning false.
	if !schema.NodePropertyKeyAbsent(db.DB, "a-key-nothing-has") {
		t.Fatal("a key no node carries was reported present")
	}
	n := &nodes.Node{Name: "ABSENT-PRED", IsSynthetic: true, Enabled: true}
	if err := db.CreateNode(n); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if err := db.SetNodeProperty(n.ID, "a-key-something-has", "yes"); err != nil {
		t.Fatalf("set property: %v", err)
	}
	if schema.NodePropertyKeyAbsent(db.DB, "a-key-something-has") {
		t.Fatal("a key a node carries was reported absent")
	}

	// Now the question it could not previously answer.
	closed, err := sql.Open("pgx", "postgres://127.0.0.1:1/nothing?sslmode=disable")
	if err != nil {
		t.Fatalf("open a doomed handle: %v", err)
	}
	if err := closed.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if schema.NodePropertyKeyAbsent(closed, "log_cms_transactions") {
		t.Error("a query that could not run reported the post-condition as holding. The " +
			"migration would be recorded as applied without having verified anything.")
	}
}

// v100 deletes the retired log_cms_transactions node property. Nothing wrote it
// in production, so on every database this migration is a no-op — which means
// its verify predicate is trivially true and has never been exercised against a
// database that actually has the key. This test supplies that input.
func TestV100_RemovesTheRetiredBoundaryProperty(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)

	n := &nodes.Node{Name: "V100-AREA", IsSynthetic: true, Enabled: true}
	if err := db.CreateNode(n); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if err := db.SetNodeProperty(n.ID, "log_cms_transactions", "true"); err != nil {
		t.Fatalf("seed retired property: %v", err)
	}
	// A property that is NOT the retired one, to prove the DELETE is keyed.
	if err := db.SetNodeProperty(n.ID, "cms_storeroom", "SM01"); err != nil {
		t.Fatalf("seed cms_storeroom: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 100`); err != nil {
		t.Fatalf("clear v100 row: %v", err)
	}

	migrated, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("re-open to apply v100: %v", err)
	}
	defer migrated.Close()

	var stale bool
	if err := migrated.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM node_properties WHERE key = 'log_cms_transactions')`).Scan(&stale); err != nil {
		t.Fatalf("check retired property: %v", err)
	}
	if stale {
		t.Error("log_cms_transactions survived v100")
	}

	code, err := migrated.GetNodePropertyOrError(n.ID, "cms_storeroom")
	if err != nil {
		t.Fatalf("read cms_storeroom: %v", err)
	}
	if code != "SM01" {
		t.Errorf("cms_storeroom = %q, want SM01 — v100 must delete one key, not clear the table", code)
	}
}
