//go:build docker

package store_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
)

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
