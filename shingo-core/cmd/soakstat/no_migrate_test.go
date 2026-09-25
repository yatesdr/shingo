package main

import (
	"os"
	"strings"
	"testing"
)

// TestSoakstat_NeverMigratesTheCoreItMeasures holds soakstat to a read-only
// open: store.Open migrates, and a soakstat built newer than the Core under
// test rewrote that Core's schema mid-run.
func TestSoakstat_NeverMigratesTheCoreItMeasures(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "store.Open(") {
		t.Fatal("soakstat opens Core's database with store.Open, which migrates it; use store.OpenWithoutMigrate")
	}
	if !strings.Contains(string(src), "store.OpenWithoutMigrate(") {
		t.Fatal("soakstat no longer opens Core's database read-only; re-derive this guard")
	}
}
