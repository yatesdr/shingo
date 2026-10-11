//go:build lockcheck || sim

package engine

import (
	"strings"
	"testing"

	"shingo/protocol/lockcheck"
)

// TestWithLoaderBudget_ASecondLoaderLockUnderOneIsRefused is the rule the
// converted TestWithLoaderBudget_EmitDuringReservation_NoDeadlock used to pin
// the other way: a reservation's subscriber re-entering the seam for another
// loader takes a second loader lock under the first, in no fixed order. The
// guard refuses it.
func TestWithLoaderBudget_ASecondLoaderLockUnderOneIsRefused(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	var report string
	_, _ = eng.withLoaderBudget(mustSharedLoader(t, "DLK-A", "P1"), "P1", 1, "", true, func([]string) (int, error) {
		report = lockcheck.Catch(func() {
			_, _ = eng.withLoaderBudget(mustSharedLoader(t, "DLK-B", "P1"), "P1", 1, "", true, func([]string) (int, error) {
				return 0, nil
			})
		})
		return 0, nil
	})
	if !strings.Contains(report, "order taken backwards") || !strings.Contains(report, "loaderBudgetLock") {
		t.Fatalf("a second loader lock under one was not refused:\n%s", report)
	}
}
