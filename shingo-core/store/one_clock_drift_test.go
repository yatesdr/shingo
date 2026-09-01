package store

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// one_clock_drift_test.go — EVERYTHING FOLLOWS THE SAME CLOCK.
//
// ── THE TWO CLOCKS, AND WHY ONE OF THEM IS A TRAP ─────────────────────────
//
// Application code stamps timestamps from clock.Now(), which under the sim is a
// FAST-FORWARD clock. A column that instead takes the schema's DEFAULT now() is
// stamped by POSTGRES, from wall time. On a live plant the two agree closely
// enough that nothing shows. In the sim they are months apart — measured on a
// running plant, 2026-08-30:
//
//	orders.created_at             2027-11-21 13:52   (app clock)
//	order_history.created_at      2027-11-21 13:53   (app clock)
//	bins.updated_at               2027-11-21 13:53   (app clock)
//	reservations.created_at       2026-08-30 20:26   (WALL — 15 months adrift)
//	mission_telemetry.created_at  2026-08-30 20:26   (WALL — 15 months adrift)
//
// A duration taken across the two is not merely wrong, it is NEGATIVE, and the
// clamps that exist downstream ("a hold stamped in the future is not a negative
// age") turn it silently into zero. That is the worst failure shape available:
// an instrument that reads a confident 0 forever, on a plant where nothing looks
// broken.
//
// The class already has scar tissue. The reconciliation stuck-order detector
// carried it and was fixed in §R.98 stage D, whose comment says it plainly — "a
// wall-NOW() comparison never fires once the sim clock outruns wall time (10x →
// immediately)". mission_events carries the same warning one function above
// mission_telemetry, which did not have it. This test is what stops the next one
// being written.
//
// ── WHAT IS ASSERTED ──────────────────────────────────────────────────────
//
// An INSERT into a timeline table must write created_at explicitly. A source
// scan and not a runtime check, on purpose: the failure is invisible at runtime
// on a live plant, which is the property that let it survive this long.
//
// MUTATION (verified): drop created_at from the reservations INSERT in
// reservations.go. This names the file and the table.
func TestTimelineInsertsStampTheAppClock(t *testing.T) {
	t.Parallel()

	// Tables whose rows are read BESIDE app-clock rows — compared, ordered, or
	// subtracted against orders/order_history. A row here stamped by Postgres is
	// a row in a different year from the thing it describes.
	timeline := []string{
		"reservations",
		"mission_telemetry",
		"mission_events",
		"orders",
		"order_history",
	}

	insert := regexp.MustCompile(`INSERT INTO (\w+)`)
	var offenders []string

	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") {
			return err
		}
		// migrations.go writes DDL and seed rows, not timeline rows.
		if strings.HasSuffix(path, "migrations.go") {
			return nil
		}
		src, rErr := os.ReadFile(filepath.Clean(path))
		if rErr != nil {
			return rErr
		}
		body := string(src)
		for _, m := range insert.FindAllStringSubmatchIndex(body, -1) {
			table := body[m[2]:m[3]]
			if !slices.Contains(timeline, table) {
				continue
			}
			// PROSE IS NOT CODE. orders.go's own doc says "exactly one INSERT
			// INTO orders statement", and a scanner that cannot tell that
			// sentence from the statement it describes reports the file
			// documenting the rule as the file breaking it. (10 is LF.)
			lineStart := strings.LastIndexByte(body[:m[0]], 10) + 1
			if strings.HasPrefix(strings.TrimSpace(body[lineStart:m[0]]), "//") {
				continue
			}
			// The statement runs to the close of its SQL literal. m[0] is already
			// inside that literal, so the next backtick is the closing one.
			end := strings.IndexByte(body[m[0]:], '`')
			if end < 0 {
				end = len(body) - m[0]
			}
			if !strings.Contains(body[m[0]:m[0]+end], "created_at") {
				offenders = append(offenders,
					path+" -> INSERT INTO "+table+" (no explicit created_at)")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the store package: %v", err)
	}

	for _, o := range offenders {
		t.Errorf("%s\n\tThis row is stamped by the DATABASE's wall clock while every row it is read "+
			"beside carries the app clock. On a live plant the two agree and nothing shows; in the "+
			"sim they are months apart, durations across the pair go negative, and the clamps "+
			"downstream turn that into a confident, permanent 0. Write created_at from clock.Now().", o)
	}
}
