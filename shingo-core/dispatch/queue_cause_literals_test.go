package dispatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// queueCauseScanDirs are the packages that SET queue causes. Both are scanned
// whole: fulfillment/ is included because it writes causes into the same column
// through the same type, so a literal over there is exactly as invisible to
// causeReleasers as one in here.
var queueCauseScanDirs = []string{
	filepath.Join("shingo-core", "dispatch"),
	filepath.Join("shingo-core", "fulfillment"),
}

// TestNoLiteralQueueCauseConversions keeps the cause vocabulary ENUMERABLE.
//
// The type's doc used to say the un-named causes were "not forgotten — they are
// VISIBLE", because setQueueReason takes a QueueCause and an un-named value had
// to appear at its call site as an explicit QueueCause("…") conversion. That was
// true and it was a to-do list, not a guard: nothing stopped the list growing,
// and it did — twelve conversions across two packages by the time it was worked.
//
// It has to stay worked now for a reason it did not have then. causeReleasers
// pairs every cause with the event that ends its wait and the floor that
// backstops it, and TestEveryQueueCauseHasAReleaser asserts that mapping is
// TOTAL. Totality is only meaningful over a set you can enumerate — and a
// literal conversion is a cause that is in no constant block, so the totality
// test cannot see it, cannot miss it, and passes while a wait class has no
// documented way out. That is the F-22 shape one level up: an assertion that
// cannot fail over the population it was written for.
//
// So the conversion stops being the to-do list and becomes the error. Add the
// constant to queue_cause.go — where writing it forces you to write the
// causeReleasers row beside it — and use that.
//
// MUTATION: restore any one of the twelve literals (e.g.
// QueueCause("swap-hold") at its call site) and this names the file and line.
func TestNoLiteralQueueCauseConversions(t *testing.T) {
	t.Parallel()
	root := repoRootFor(t)

	// The declaration site is the one legitimate place the string form appears:
	// the constant block IS the naming, and the type doc quotes the literals it
	// is explaining.
	allowed := map[string]bool{
		filepath.Join("shingo-core", "dispatch", "queue_cause.go"):               true,
		filepath.Join("shingo-core", "dispatch", "queue_releasers.go"):           true,
		filepath.Join("shingo-core", "dispatch", "queue_cause_literals_test.go"): true,
	}

	var offenders []string
	for _, dir := range queueCauseScanDirs {
		abs := filepath.Join(root, dir)
		entries, err := os.ReadDir(abs)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			// PRODUCTION FILES ONLY. The guard exists so no production wait can
			// carry a cause causeReleasers cannot enumerate; a test that writes a
			// literal creates no wait class, and scanning tests would trip on the
			// ones that quote the form in prose to explain it.
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			rel := filepath.Join(dir, e.Name())
			if allowed[rel] {
				continue
			}
			body, err := os.ReadFile(filepath.Join(abs, e.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			for i, line := range strings.Split(string(body), "\n") {
				// Both spellings: in-package `QueueCause("…")` and the
				// cross-package `dispatch.QueueCause("…")`.
				if strings.Contains(line, `QueueCause("`) {
					offenders = append(offenders,
						filepath.ToSlash(rel)+":"+itoa(i+1)+": "+strings.TrimSpace(line))
				}
			}
		}
	}

	if len(offenders) > 0 {
		t.Errorf("literal QueueCause conversion(s) found — a cause that is not a constant is a "+
			"cause causeReleasers cannot enumerate, so TestEveryQueueCauseHasAReleaser passes "+
			"while this wait class has no documented releaser and no floor on record.\n"+
			"Name it in queue_cause.go and add its causeReleasers row:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}
