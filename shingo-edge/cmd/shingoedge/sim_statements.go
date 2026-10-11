//go:build sim

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	_ "sync" // the plain lock inlines only where sync is imported (protocol/lockcheck)
	"time"

	"shingo/protocol/lockcheck"
	"shingoedge/internal/lockorder"
	"shingoedge/store"
)

// openDatabase, in a sim build, opens the store through the statement counter
// (store.OpenCounting, the same driver-seam instrument the budget pins use),
// names every statement by its principal table and tags it by the part of the
// Edge that sent it, and writes the running totals to edge-statements.tsv
// beside the database every 30 s. It exists to answer "what does the Edge do
// per PLC tick" in a sim pair. A production build opens the store unchanged
// (sim_disabled.go) and cannot set the store's statement observer (its setter
// is in store/query_count_observe_sim.go), so the Pi gets nothing added.
func openDatabase(path string) (*store.DB, error) {
	db, counter, err := store.OpenCounting(path)
	if err != nil {
		return nil, err
	}
	obs := newStmtObserver()
	store.SetStatementObserver(obs.observe)
	out := filepath.Join(filepath.Dir(path), "edge-statements.tsv")
	go dumpStatements(obs, counter, out)
	return db, nil
}

// stackDepth is how many frames a statement's stack is read to. The deepest
// Edge paths sit a few dozen frames above SQLite (database/sql alone is about
// eight); a stack cut short loses its entry point and reads "other", which the
// output shows.
const stackDepth = 96

// stmtRow is one line of edge-statements.tsv.
type stmtRow struct {
	tag, verb, table, path string
}

type callerClass struct{ tag, path string }

type stmtName struct{ verb, table string }

// stmtObserver tags and names each statement as it is sent.
//
// COST, per statement, in a sim build only: one runtime.Callers of up to
// stackDepth frames and two map lookups (the stack's program counters -> its
// tag and path; the SQL text -> its name), under one mutex. Frames are
// resolved and the SQL tokenised only the first time a stack or a text is
// seen. A few microseconds a statement; at the Edge's few hundred statements a
// second, well under one percent of a core. Memory is one entry per distinct
// stack and per distinct SQL text.
type stmtObserver struct {
	mu     lockcheck.Mutex[lockorder.SimStatements]
	stacks map[[stackDepth]uintptr]callerClass
	names  map[string]stmtName
	totals map[stmtRow]int64
}

func newStmtObserver() *stmtObserver {
	return &stmtObserver{
		stacks: map[[stackDepth]uintptr]callerClass{},
		names:  map[string]stmtName{},
		totals: map[stmtRow]int64{},
	}
}

// observe runs synchronously inside the store's counting driver, on the
// goroutine that sent the statement, so the stack is the sender's.
func (o *stmtObserver) observe(query string) {
	var pcs [stackDepth]uintptr
	n := runtime.Callers(2, pcs[:])
	o.mu.Lock()
	defer o.mu.Unlock()
	cls, ok := o.stacks[pcs]
	if !ok {
		tag, path := classifyFrames(stackFrames(pcs[:n]))
		cls = callerClass{tag, path}
		o.stacks[pcs] = cls
	}
	nm, ok := o.names[query]
	if !ok {
		v, t := statementName(query)
		nm = stmtName{v, t}
		o.names[query] = nm
	}
	o.totals[stmtRow{cls.tag, nm.verb, nm.table, cls.path}]++
}

// stmtHeader is the first line of edge-statements.tsv. soakstat tells this
// format from the older "VERB table<TAB>count" by it.
const stmtHeader = "tag\tverb\ttable\tstatements\tpath\n"

// render writes the running totals: one row per (tag, verb, table, path),
// sorted by tag, verb, table, then count descending.
func (o *stmtObserver) render() []byte {
	o.mu.Lock()
	rows := make([]stmtRow, 0, len(o.totals))
	counts := make(map[stmtRow]int64, len(o.totals))
	for r, n := range o.totals {
		rows = append(rows, r)
		counts[r] = n
	}
	o.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.tag != b.tag {
			return a.tag < b.tag
		}
		if a.verb != b.verb {
			return a.verb < b.verb
		}
		if a.table != b.table {
			return a.table < b.table
		}
		if counts[a] != counts[b] {
			return counts[a] > counts[b]
		}
		return a.path < b.path
	})
	b := []byte(stmtHeader)
	for _, r := range rows {
		b = fmt.Appendf(b, "%s\t%s\t%s\t%d\t%s\n", r.tag, r.verb, r.table, counts[r], r.path)
	}
	return b
}

func dumpStatements(obs *stmtObserver, counter *store.QueryCounter, out string) {
	for range time.Tick(30 * time.Second) {
		// The counter keeps every statement's text until drained; the
		// observer has already counted each one, so drain to bound memory.
		counter.Drain()
		if err := os.WriteFile(out, obs.render(), 0o644); err != nil {
			log.Printf("[sim] statement count: write %s: %v", out, err)
		}
	}
}
