package orders

import (
	"database/sql"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// dbCounter gives each openLeadTimeTestDB call a unique in-memory
// SQLite name so tests that build multiple fixtures in a single
// function don't collide on `cache=shared`.
var dbCounter int64

// orderHistoryRow lets the tests below insert rows in a readable way
// without repeating positional Exec calls.
type orderHistoryRow struct {
	orderID    int64
	oldStatus  string
	newStatus  string
	detail     string
	createdAt  string
}

// openLeadTimeTestDB builds a minimal in-memory SQLite with just the
// columns the lead-time queries touch. The full Edge migration runner
// is overkill here — we exercise the SQL paths directly with the
// smallest schema that satisfies them.
func openLeadTimeTestDB(t *testing.T) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("file:lead_time_test_%d?mode=memory&cache=shared&_journal_mode=MEMORY", atomic.AddInt64(&dbCounter, 1))
	db, err := sql.Open("sqlite", name)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, ddl := range []string{
		`CREATE TABLE orders (
			id INTEGER PRIMARY KEY,
			order_type TEXT NOT NULL,
			payload_code TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE order_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			order_id INTEGER NOT NULL,
			old_status TEXT NOT NULL,
			new_status TEXT NOT NULL,
			detail TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}
	return db
}

func insertOrder(t *testing.T, db *sql.DB, id int64, orderType, payloadCode string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO orders (id, order_type, payload_code) VALUES (?, ?, ?)`,
		id, orderType, payloadCode); err != nil {
		t.Fatalf("insert order: %v", err)
	}
}

func insertHistory(t *testing.T, db *sql.DB, rows ...orderHistoryRow) {
	t.Helper()
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO order_history (order_id, old_status, new_status, detail, created_at)
			VALUES (?, ?, ?, ?, ?)`, r.orderID, r.oldStatus, r.newStatus, r.detail, r.createdAt); err != nil {
			t.Fatalf("insert history: %v", err)
		}
	}
}

// TestMedianL2LoadSeconds_AbsorbsOutliers — operator-fill is exposed
// to long-tail outliers (end-of-shift confirms, weekend confirms,
// walked-away-from-station, reconciler auto-confirms). Median lets
// every outlier class fall out without filtering by a magic detail
// string. The dataset here has 10 normal samples near 720s (12 min)
// and 2 long-tail outliers at 10,800s (3 hours) — a mean would be
// pulled to ~2400s; a median should land near 720s.
func TestMedianL2LoadSeconds_AbsorbsOutliers(t *testing.T) {
	db := openLeadTimeTestDB(t)

	// 10 normal-time confirms with durations 700..790s (12 min ± a bit)
	// and 2 outliers at 10,800s. Auto-confirm detail string included
	// on one outlier to demonstrate that median doesn't need the
	// magic-string filter; the row counts but its weight is bounded.
	base := time.Date(2026, 5, 10, 8, 0, 0, 0, time.UTC)
	add := func(id int64, dur time.Duration, detail string) {
		insertOrder(t, db, id, "retrieve_empty", "WIDGET-A")
		start := base.Add(time.Duration(id) * time.Hour)
		end := start.Add(dur)
		insertHistory(t, db,
			orderHistoryRow{id, "in_transit", "delivered", "", start.UTC().Format("2006-01-02 15:04:05")},
			orderHistoryRow{id, "delivered", "confirmed", detail, end.UTC().Format("2006-01-02 15:04:05")},
		)
	}
	for i := 1; i <= 10; i++ {
		add(int64(i), time.Duration(700+i*10)*time.Second, "operator pressed CONFIRM")
	}
	add(11, 3*time.Hour, "auto-confirmed after 1h0m0s timeout")
	add(12, 3*time.Hour, "operator forgot to confirm before shift end")

	dr := DateRange{
		Start: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC),
	}
	med, err := MedianL2LoadSeconds(db, "WIDGET-A", dr)
	if err != nil {
		t.Fatalf("MedianL2LoadSeconds: %v", err)
	}
	// 12 sorted durations: [710, 720, ..., 800, 10800, 10800].
	// Median is the mean of the 6th and 7th: (750 + 760) / 2 = 755.
	// Without an outlier-robust estimator the mean would be ~2400s.
	if med < 740 || med > 770 {
		t.Errorf("median = %.2fs, want ~755s (mean would be ~2400s)", med)
	}
}

// TestMedianL2LoadSeconds_EvenAndOdd — median definition uses the
// midpoint of the two middle values for even-length samples and the
// single middle value for odd-length samples. Spot-check both shapes.
func TestMedianL2LoadSeconds_EvenAndOdd(t *testing.T) {
	dr := DateRange{
		Start: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC),
	}

	odd := openLeadTimeTestDB(t)
	base := time.Date(2026, 5, 10, 8, 0, 0, 0, time.UTC)
	addOdd := func(id int64, dur time.Duration) {
		insertOrder(t, odd, id, "retrieve_empty", "PART-A")
		start := base.Add(time.Duration(id) * time.Hour)
		end := start.Add(dur)
		insertHistory(t, odd,
			orderHistoryRow{id, "in_transit", "delivered", "", start.UTC().Format("2006-01-02 15:04:05")},
			orderHistoryRow{id, "delivered", "confirmed", "", end.UTC().Format("2006-01-02 15:04:05")},
		)
	}
	for i, d := range []time.Duration{10 * time.Second, 20 * time.Second, 30 * time.Second, 40 * time.Second, 50 * time.Second} {
		addOdd(int64(i+1), d)
	}
	got, err := MedianL2LoadSeconds(odd, "PART-A", dr)
	if err != nil {
		t.Fatalf("odd: %v", err)
	}
	if got < 29 || got > 31 {
		t.Errorf("odd median = %.2f, want ~30", got)
	}

	even := openLeadTimeTestDB(t)
	addEven := func(id int64, dur time.Duration) {
		insertOrder(t, even, id, "retrieve_empty", "PART-A")
		start := base.Add(time.Duration(id) * time.Hour)
		end := start.Add(dur)
		insertHistory(t, even,
			orderHistoryRow{id, "in_transit", "delivered", "", start.UTC().Format("2006-01-02 15:04:05")},
			orderHistoryRow{id, "delivered", "confirmed", "", end.UTC().Format("2006-01-02 15:04:05")},
		)
	}
	for i, d := range []time.Duration{10 * time.Second, 20 * time.Second, 30 * time.Second, 40 * time.Second} {
		addEven(int64(i+1), d)
	}
	got, err = MedianL2LoadSeconds(even, "PART-A", dr)
	if err != nil {
		t.Fatalf("even: %v", err)
	}
	// (20 + 30) / 2 = 25
	if got < 24 || got > 26 {
		t.Errorf("even median = %.2f, want ~25", got)
	}

	empty := openLeadTimeTestDB(t)
	got, err = MedianL2LoadSeconds(empty, "PART-A", dr)
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if got != 0 {
		t.Errorf("empty median = %.2f, want 0 (no-data signal)", got)
	}
}

// TestP95MarketToCellSeconds_ArgBinding — the v5 implementation had
// a wrong append() prepending an extra startISO, producing args that
// didn't line up with the query placeholders and silently returning
// the wrong row set. Regression-guard the corrected binding by feeding
// a known set of retrieve durations and checking p95 lands on the
// expected value (not zero, not the wrong sample).
func TestP95MarketToCellSeconds_ArgBinding(t *testing.T) {
	db := openLeadTimeTestDB(t)

	// 10 retrieves with linearly increasing durations: 10, 20, ..., 100 s.
	// p95 of 10 sorted samples lands at index int(0.95*10) = 9 ⇒ 100s.
	base := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 10; i++ {
		insertOrder(t, db, int64(i), "retrieve", "WIDGET-A")
		start := base.Add(time.Duration(i) * time.Hour)
		end := start.Add(time.Duration(i*10) * time.Second)
		insertHistory(t, db,
			orderHistoryRow{int64(i), "acknowledged", "in_transit", "", start.UTC().Format("2006-01-02 15:04:05")},
			orderHistoryRow{int64(i), "in_transit", "delivered", "", end.UTC().Format("2006-01-02 15:04:05")},
		)
	}

	dr := DateRange{
		Start: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC),
	}
	p95, err := P95MarketToCellSeconds(db, "WIDGET-A", dr)
	if err != nil {
		t.Fatalf("P95MarketToCellSeconds: %v", err)
	}
	if p95 < 95 || p95 > 105 {
		t.Errorf("p95 = %.2fs, want ~100s", p95)
	}

	// Filter by date range — a window that excludes all samples
	// should return 0 with no error (no-data signal, not failure).
	emptyRange := DateRange{
		Start: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2025, 1, 31, 0, 0, 0, 0, time.UTC),
	}
	p95Empty, err := P95MarketToCellSeconds(db, "WIDGET-A", emptyRange)
	if err != nil {
		t.Fatalf("P95MarketToCellSeconds(empty range): %v", err)
	}
	if p95Empty != 0 {
		t.Errorf("empty-range p95 = %.2f, want 0", p95Empty)
	}
}
