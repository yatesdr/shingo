package www

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"shingo/protocol"
)

// The Core UI's status label and colour tables are hand-written JS objects
// keyed on order statuses. Nothing served them from Go, so a status renamed or
// added in protocol/ left every one of them quietly keyed on a word the server
// no longer sends. These pins are the mission_state_vocabulary_drift_test shape
// applied to the rest of the pages: every key must be a real status, and where
// a table's purpose is a whole population, the population must be whole.

func readStaticPage(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("static", "pages", name)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// jsBlock returns the text between open and the next closeMark, failing loudly
// when either is missing so a renamed table is a red test, not a vacuous pass.
func jsBlock(t *testing.T, file, src, open, closeMark string) string {
	t.Helper()
	_, rest, found := strings.Cut(src, open)
	if !found {
		t.Fatalf("%s: marker %q not found — the test can no longer see what it claims to check", file, open)
	}
	body, _, found := strings.Cut(rest, closeMark)
	if !found {
		t.Fatalf("%s: no %q after %q", file, closeMark, open)
	}
	return body
}

func knownStatuses() map[string]bool {
	known := map[string]bool{}
	for _, s := range protocol.AllStatuses() {
		known[string(s)] = true
	}
	return known
}

// objectKeys pulls `key:` and `'key':` object keys out of a JS object body.
var objectKeyRe = regexp.MustCompile(`(?m)(?:^|[{,\s])'?([a-z_]*)'?\s*:`)

func objectKeys(body string) []string {
	var out []string
	for _, m := range objectKeyRe.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

// deadStatusKeys are keys a table carries for a status protocol/ does not have.
// "blocked" is not an order status (no protocol.Status spells it), so
// dashboard-map.js's STATUS_COLOR.blocked / statusLabel.blocked entries, and
// its `ord.status === 'blocked'` test (:219), can never match an order. Pinned
// as known-dead rather than deleted: removal is a UI change, and this work is
// no-semantic-change. Shrinking this list is how the clean-up lands; any NEW
// non-status key still fails.
var deadStatusKeys = map[string]bool{"blocked": true}

func TestJSStatusTablesAreKeyedOnRealStatuses(t *testing.T) {
	t.Parallel()
	known := knownStatuses()
	dash := readStaticPage(t, "dashboard-map.js")
	missions := readStaticPage(t, "missions.js")

	cases := []struct {
		file, name string
		body       string
		allowEmpty bool // '' is a documented key, not a status
	}{
		{"dashboard-map.js", "STATUS_COLOR", jsBlock(t, "dashboard-map.js", dash, "var STATUS_COLOR = {", "};"), false},
		{"dashboard-map.js", "statusLabel", jsBlock(t, "dashboard-map.js", dash, "var statusLabel = {", "};"), false},
		// '' is "Still faulted": the outcome of a fault that has not ended, which
		// is the absence of a status rather than a status.
		{"missions.js", "FAULT_OUTCOME_LABELS", jsBlock(t, "missions.js", missions, "const FAULT_OUTCOME_LABELS = {", "};"), true},
	}
	for _, tc := range cases {
		keys := objectKeys(tc.body)
		if len(keys) == 0 {
			t.Errorf("%s %s: found no keys — has the table's shape changed?", tc.file, tc.name)
			continue
		}
		for _, k := range keys {
			if k == "" && tc.allowEmpty {
				continue
			}
			if deadStatusKeys[k] {
				continue
			}
			if !known[k] {
				t.Errorf("%s %s key %q is not a protocol status (protocol.AllStatuses: %v). A table keyed "+
					"on a word the server never sends is dead or wrong.", tc.file, tc.name, k, protocol.AllStatuses())
			}
		}
	}
}

// isActiveKnownGap is the part of the non-terminal set test-orders.js's isActive
// does NOT list today. It gates the Cancel button on the test-orders page, so an
// order in any of these statuses shows no Cancel although Core would cancel it.
//
// Pinned as a known gap rather than fixed because the rule-copy work is
// no-semantic-change: closing it changes which orders offer Cancel, and that is
// a decision, not a copy. Shrinking this list is how the fix lands; growing it
// is how this test is meant to fail.
var isActiveKnownGap = []string{"acknowledged", "faulted", "queued", "reshuffling", "staged", "submitted"}

func TestTestOrdersIsActiveIsTheNonTerminalSet(t *testing.T) {
	t.Parallel()
	src := readStaticPage(t, "test-orders.js")
	m := regexp.MustCompile(`var isActive = \[([^\]]*)\]\.indexOf\(o\.status\)`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("test-orders.js: `var isActive = [...].indexOf(o.status)` not found — has the gate moved?")
	}
	listed := map[string]bool{}
	for _, v := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(m[1], -1) {
		listed[v[1]] = true
	}
	known := knownStatuses()
	for s := range listed {
		if !known[s] {
			t.Errorf("isActive lists %q, which is not a protocol status", s)
		}
		if protocol.IsTerminal(protocol.Status(s)) {
			t.Errorf("isActive lists terminal status %q — a terminal order offers no Cancel", s)
		}
	}
	gap := map[string]bool{}
	for _, s := range isActiveKnownGap {
		gap[s] = true
	}
	var missing []string
	for _, s := range protocol.AllStatuses() {
		if protocol.IsTerminal(s) || listed[string(s)] {
			continue
		}
		missing = append(missing, string(s))
	}
	sort.Strings(missing)
	for _, s := range missing {
		if !gap[s] {
			t.Errorf("isActive omits non-terminal status %q and it is not in isActiveKnownGap — a new "+
				"status the Cancel gate cannot see", s)
		}
	}
	for s := range gap {
		if listed[s] {
			t.Errorf("isActive now lists %q: remove it from isActiveKnownGap", s)
		}
	}
}
