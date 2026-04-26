//go:build docker

package store

import (
	"testing"

	"shingocore/store/nodes"
)

func TestNodeProperty_SetGetUpsert(t *testing.T) {
	db := testDB(t)

	node := &nodes.Node{Name: "PROP-NODE-1", Enabled: true}
	if err := db.CreateNode(node); err != nil {
		t.Fatalf("create node: %v", err)
	}

	// Miss: GetNodeProperty returns "" when key not set
	if v := db.GetNodeProperty(node.ID, "missing"); v != "" {
		t.Errorf("miss returned %q, want empty string", v)
	}

	// Insert
	if err := db.SetNodeProperty(node.ID, "role", "source"); err != nil {
		t.Fatalf("set role=source: %v", err)
	}
	if v := db.GetNodeProperty(node.ID, "role"); v != "source" {
		t.Errorf("role = %q, want %q", v, "source")
	}

	// Upsert: same key, new value should overwrite
	if err := db.SetNodeProperty(node.ID, "role", "sink"); err != nil {
		t.Fatalf("set role=sink (overwrite): %v", err)
	}
	if v := db.GetNodeProperty(node.ID, "role"); v != "sink" {
		t.Errorf("role after overwrite = %q, want %q", v, "sink")
	}

	// Only one row per (node, key) — list should still have a single role entry
	props, err := db.ListNodeProperties(node.ID)
	if err != nil {
		t.Fatalf("ListNodeProperties: %v", err)
	}
	if len(props) != 1 {
		t.Fatalf("props after overwrite len = %d, want 1", len(props))
	}
	if props[0].Key != "role" || props[0].Value != "sink" {
		t.Errorf("props[0] = (%q, %q), want (role, sink)", props[0].Key, props[0].Value)
	}
}

func TestNodeProperty_MultipleKeysAndDelete(t *testing.T) {
	db := testDB(t)

	node := &nodes.Node{Name: "PROP-NODE-2", Enabled: true}
	db.CreateNode(node)

	db.SetNodeProperty(node.ID, "role", "source")
	db.SetNodeProperty(node.ID, "capacity", "10")
	db.SetNodeProperty(node.ID, "direction", "forward")

	props, err := db.ListNodeProperties(node.ID)
	if err != nil {
		t.Fatalf("ListNodeProperties: %v", err)
	}
	if len(props) != 3 {
		t.Fatalf("props len = %d, want 3", len(props))
	}

	// Props are ordered by key per properties.go: capacity, direction, role
	seen := map[string]string{}
	for _, p := range props {
		seen[p.Key] = p.Value
	}
	if seen["role"] != "source" {
		t.Errorf("role = %q, want %q", seen["role"], "source")
	}
	if seen["capacity"] != "10" {
		t.Errorf("capacity = %q, want %q", seen["capacity"], "10")
	}
	if seen["direction"] != "forward" {
		t.Errorf("direction = %q, want %q", seen["direction"], "forward")
	}

	// Delete one key, verify the others remain
	if err := db.DeleteNodeProperty(node.ID, "capacity"); err != nil {
		t.Fatalf("DeleteNodeProperty: %v", err)
	}
	if v := db.GetNodeProperty(node.ID, "capacity"); v != "" {
		t.Errorf("capacity after delete = %q, want empty", v)
	}
	remaining, _ := db.ListNodeProperties(node.ID)
	if len(remaining) != 2 {
		t.Errorf("props after delete len = %d, want 2", len(remaining))
	}

	// The remaining keys are role and direction
	stillThere := map[string]bool{}
	for _, p := range remaining {
		stillThere[p.Key] = true
	}
	if !stillThere["role"] || !stillThere["direction"] {
		t.Errorf("remaining keys = %+v, want role+direction", stillThere)
	}
}
