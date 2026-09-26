//go:build docker

package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/dispatch/plan"
	"shingocore/store"
)

// planner_zero_write_docker_test.go — THE PLANNER WRITES NOTHING, measured.
//
// B1's third proof of purity, beside the type (plan.Reader, reads only) and the
// lint (planner-purity depguard). Every planner golden case runs its engine,
// its picker and its resolver against a statement-counting store
// (store.OpenCounting), and every statement issued INSIDE a planner call is
// classified. A write, a row lock, an advisory lock or a transaction bracket
// inside one fails the case. A door around the planner writes plenty (it
// creates the order, takes the holds, calls the fleet), and that is not the
// planner's; only the statements between a planner call's entry and exit are
// judged.
//
// WHERE A PLANNER CALL BEGINS AND ENDS. A decider case calls the planner itself
// and brackets the call here. A door case cannot, because the door makes the
// call, so the planner reports its own entry and exit through plan.SetObserver.
// The observer is keyed by the reader the part reads through, which is this
// case's counting store, so cases running in parallel keep their own books.
//
// WHY OpenCounting'S COUNT IS NOT THE ASSERTION. It counts every statement pgx
// is handed, reads and BEGIN/COMMIT alike, so "0 writes" is a classification of
// its Statements(), not a number it keeps. And protocol/testutil's
// classifyStatement names a CTE by its LAST consumer, so a data-modifying CTE
// (`WITH x AS (UPDATE …) SELECT …`) reads as WITH/SELECT there. isPlannerSafe
// looks inside instead.
//
// THE BUDGET LINES ARE EVIDENCE, NOT PINS. Each door case logs the statements
// its planner calls issued and the door's whole count by kind, so a phase's
// before/after table can be read off a run (unification-B1 PREDICTIONS.md).

var (
	zwMu     sync.Mutex
	zwByDB   = map[any]*zeroWrite{}
	zwOnce   sync.Once
	zwAnyHit sync.Map // test name → true once a planner call was observed
)

// zeroWrite is one case's book of its planner calls.
type zeroWrite struct {
	name    string
	counter *store.QueryCounter

	mu         sync.Mutex
	depth      int
	startIndex int
	part       string
	statements int
	calls      map[string]int
	violations []string
}

func installPlannerObserver() {
	zwOnce.Do(func() {
		plan.SetObserver(func(db any, part string, enter bool) {
			zwMu.Lock()
			zw := zwByDB[db]
			zwMu.Unlock()
			if zw == nil {
				return
			}
			if enter {
				zw.enter(part)
			} else {
				zw.leave()
			}
		})
	})
}

// watchPlanner registers this case's counting store with the observer, and
// asserts at the end of the case that no planner call wrote.
func (g *goldenPlant) watchPlanner() {
	installPlannerObserver()
	zw := &zeroWrite{name: g.t.Name(), counter: g.counter, calls: map[string]int{}}
	g.zw = zw
	zwMu.Lock()
	zwByDB[g.cdb] = zw
	zwMu.Unlock()
	g.t.Cleanup(func() {
		zwMu.Lock()
		delete(zwByDB, g.cdb)
		zwMu.Unlock()
		zw.assertClean(g.t)
	})
}

// plannerCall brackets a planner call a case makes directly (a decider case).
func (g *goldenPlant) plannerCall(part string, call func()) {
	g.zw.enter(part)
	defer g.zw.leave()
	call()
}

func (zw *zeroWrite) enter(part string) {
	zw.mu.Lock()
	defer zw.mu.Unlock()
	if zw.depth == 0 {
		zw.startIndex = len(zw.counter.Statements())
		zw.part = part
		zw.calls[part]++
		zwAnyHit.Store(topLevel(zw.name), true)
	}
	zw.depth++
}

func (zw *zeroWrite) leave() {
	zw.mu.Lock()
	defer zw.mu.Unlock()
	zw.depth--
	if zw.depth > 0 {
		return
	}
	inside := zw.counter.Statements()[zw.startIndex:]
	zw.statements += len(inside)
	for _, s := range inside {
		if !isPlannerSafe(s) {
			zw.violations = append(zw.violations, fmt.Sprintf("%s: %s", zw.part, oneLine(s)))
		}
	}
}

func (zw *zeroWrite) assertClean(t *testing.T) {
	t.Helper()
	zw.mu.Lock()
	defer zw.mu.Unlock()
	for _, v := range zw.violations {
		t.Errorf("PLANNER WROTE: a statement inside a planner call writes, locks or brackets a transaction. "+
			"The planner reads; holds are acquiring's and writes are the applier's.\n  %s", v)
	}
	parts := make([]string, 0, len(zw.calls))
	for p, n := range zw.calls {
		parts = append(parts, fmt.Sprintf("%s×%d", p, n))
	}
	sort.Strings(parts)
	t.Logf("BUDGET planner %s: %d statements in %s, 0 writes asserted | case total: %s",
		zw.name, zw.statements, strings.Join(parts, " "), testutil.StatementBreakdown(zw.counter.Statements()))
}

func topLevel(name string) string {
	top, _, _ := strings.Cut(name, "/")
	return top
}

// requirePlannerSeen fails a golden test whose cases never reached the planner,
// which would make every zero-write assertion above pass vacuously.
func requirePlannerSeen(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if _, ok := zwAnyHit.Load(t.Name()); !ok {
			t.Errorf("%s: no planner call was observed in any case, so its zero-write assertions are vacuous. "+
				"Is plan.SetObserver still called from every planner entry point?", t.Name())
		}
	})
}

var (
	leadingSQLComment = regexp.MustCompile(`^(\s*--[^\n]*\n|\s*/\*.*?\*/)*\s*`)
	dataModifying     = regexp.MustCompile(`(?i)\b(INSERT\s+INTO|UPDATE\s+[\w.]+(\s+\w+)?\s+SET|DELETE\s+FROM|TRUNCATE|MERGE\s+INTO)\b`)
	rowLock           = regexp.MustCompile(`(?i)\bFOR\s+(UPDATE|SHARE|NO\s+KEY\s+UPDATE|KEY\s+SHARE)\b`)
	advisoryLock      = regexp.MustCompile(`(?i)\bpg_(try_)?advisory`)
)

// isPlannerSafe is true for a read and nothing else: a SELECT or a WITH that
// modifies no data, takes no row lock and no advisory lock. BEGIN, COMMIT and
// ROLLBACK are not safe either: a planner call has no business opening a
// transaction, because a transaction is where a hold is taken.
func isPlannerSafe(stmt string) bool {
	s := leadingSQLComment.ReplaceAllString(stmt, "")
	kind := strings.ToUpper(strings.TrimRight(firstWord(s), ";("))
	if kind != "SELECT" && kind != "WITH" {
		return false
	}
	return !dataModifying.MatchString(s) && !rowLock.MatchString(s) && !advisoryLock.MatchString(s)
}

func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return s
}

// TestPlannerSafe_Classifies pins the classifier on the shapes that matter,
// including the one protocol/testutil's breakdown names wrongly.
func TestPlannerSafe_Classifies(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		sql  string
		safe bool
	}{
		{"SELECT id FROM bins WHERE node_id = $1", true},
		{"-- note\nSELECT 1", true},
		{"WITH r AS (SELECT 1) SELECT * FROM r", true},
		{"SELECT b.updated_at FROM bins b", true},
		{"WITH x AS (UPDATE bins SET locked = true RETURNING id) SELECT * FROM x", false},
		{"SELECT id FROM nodes WHERE id = $1 FOR UPDATE", false},
		{"SELECT pg_advisory_xact_lock($1)", false},
		{"INSERT INTO outbox (topic) VALUES ($1)", false},
		{"UPDATE orders SET status = $1 WHERE id = $2", false},
		{"UPDATE bins b SET locked = true WHERE b.id = $1", false},
		{"DELETE FROM reservations WHERE id = $1", false},
		{"begin", false},
		{"commit", false},
	} {
		if got := isPlannerSafe(c.sql); got != c.safe {
			t.Errorf("isPlannerSafe(%q) = %v, want %v", c.sql, got, c.safe)
		}
	}
}
