//go:build sim

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"shingoedge/store"
)

// openDatabase, in a sim build, opens the store through the statement counter
// (store.OpenCounting, the same driver-seam instrument the budget pins use) and
// writes what it counted, by verb and table, to edge-statements.tsv beside the
// database every 30 s. It exists to answer "what does the Edge do per completed
// order" in a sim pair (RULINGS-C2c-run-through §3.4); a production build opens
// the store unchanged (sim_disabled.go), so the Pi gets nothing added.
func openDatabase(path string) (*store.DB, error) {
	db, counter, err := store.OpenCounting(path)
	if err != nil {
		return nil, err
	}
	out := filepath.Join(filepath.Dir(path), "edge-statements.tsv")
	go dumpStatements(counter, out)
	return db, nil
}

var statementKey = regexp.MustCompile(`(?is)^\s*(?:WITH\b.*?\)\s*)?(SELECT\b.*?\bFROM\s+(\w+)|INSERT\s+(?:OR\s+\w+\s+)?INTO\s+(\w+)|UPDATE\s+(\w+)|DELETE\s+FROM\s+(\w+)|(\w+))`)

// keyOf names a statement by its verb and first table.
func keyOf(q string) string {
	m := statementKey.FindStringSubmatch(q)
	if m == nil {
		return "OTHER"
	}
	for i, verb := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
		if t := m[2+i]; t != "" {
			return verb + " " + t
		}
	}
	return "OTHER " + m[6]
}

func dumpStatements(counter *store.QueryCounter, out string) {
	var mu sync.Mutex
	totals := map[string]int64{}
	for range time.Tick(30 * time.Second) {
		stmts := counter.Drain()
		mu.Lock()
		for _, q := range stmts {
			totals[keyOf(q)]++
		}
		keys := make([]string, 0, len(totals))
		for k := range totals {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b []byte
		for _, k := range keys {
			b = fmt.Appendf(b, "%s\t%d\n", k, totals[k])
		}
		mu.Unlock()
		if err := os.WriteFile(out, b, 0o644); err != nil {
			log.Printf("[sim] statement count: write %s: %v", out, err)
		}
	}
}
