package www

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// undeclared_carrier_watcher_drift_test.go — the inventory page's half of the
// carrier-rule watcher, held as source text.
//
// ── WHY THIS IS A TEST AT ALL ───────────────────────────────────────────────
//
// The carrier rule stopped refusing a produce finalize. A bin loaded into a
// carrier its payload does not declare LANDS now, counts as stock on every
// inventory surface, and is refused by every sourcing reader — so the only
// thing between "that bin can never be fetched" and nobody knowing is the count
// on this page and the list on /material-flags. Law 8: a flag nobody sees is
// error 5, and the refusal it replaced was at least loud.
//
// The failure mode that would silently disable it is a JSON key: the Go field
// tag and the JS property are two spellings of one name, they are in different
// languages in different directories, and `anomalySummary.undeclared_carrier_bins`
// evaluating to undefined renders as "no findings" — the reassuring reading and
// the wrong one. Nothing else in the build relates them.
//
// The same shape as the clock-globals and inline-onclick guards on this page:
// a claim about source text, no database, no browser.
func TestInventoryPageReadsTheUndeclaredCarrierCount(t *testing.T) {
	t.Parallel()

	const key = "undeclared_carrier_bins"

	goSrc := mustReadRepoFile(t, filepath.FromSlash("../uop/applier.go"))
	if !strings.Contains(goSrc, `json:"`+key+`"`) {
		t.Fatalf("uop.AnomalyDeltaSummary no longer publishes %q.\n"+
			"If the field was renamed, rename it in inventory.js in the same commit; "+
			"if it was deleted, the inventory page has lost its only count of carriers "+
			"nothing will ever fetch.", key)
	}

	js := mustReadRepoFile(t, filepath.FromSlash("static/pages/inventory.js"))
	if !strings.Contains(js, "anomalySummary."+key) {
		t.Errorf("inventory.js does not read anomalySummary.%s.\n"+
			"An undefined property renders as zero findings, which is the reassuring "+
			"reading and the wrong one — the produce door lands these bins rather than "+
			"refusing them, so this count is what replaced the refusal.", key)
	}
	if !strings.Contains(js, "/material-flags") {
		t.Error("the count on /inventory does not link to /material-flags. A bare " +
			"number names no carrier and no fix; the list is on the other page.")
	}
}

func mustReadRepoFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (if the file moved, repoint this guard rather than "+
			"deleting it — a guard that cannot find its subject is a guard that "+
			"passes on nothing)", path, err)
	}
	return string(b)
}
