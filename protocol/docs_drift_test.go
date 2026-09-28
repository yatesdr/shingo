package protocol_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"shingo/protocol"
)

// docs/wire-protocol.md is the reference a new contributor reads before any
// protocol constant. Its "Known Subjects" table claimed to be the register of
// subjects while three existed only as constants (production.tick,
// production.ticks, inventory.lineside_level_report), it listed order.skipped
// — a message TYPE, not a subject — as one, and its TTL section said "two
// subjects carry NO expiry" when the set in protocol/expiry.go is five.
// Every one of those was found by audit on 2026-09-26, which is the point:
// the doc and the code are two definition sites for one fact, and nothing
// made them agree. This test does. Modelled on wire_vocabulary_drift_test.go,
// which exists for the same failure with a different pair of sites.
//
// The pins, so a failure here names its authority:
//   - subjects table ↔ protocol.AllSubjects() (types.go:386)
//   - `**none**` TTL rows ↔ protocol.DataTTLFor(s) == protocol.NoExpiry
//     (expiry.go:67-82)
//   - order.skipped ↔ TypeOrderSkipped belongs in the Type Registry table
//     (types.go:31), never the subjects table
func TestWireProtocolDocSubjectsMatchCode(t *testing.T) {
	doc := readDoc(t)

	docSubjects := parseSubjectsTable(t, doc)

	want := map[string]bool{}
	for _, s := range protocol.AllSubjects() {
		want[s] = true
	}

	missing := []string{}
	stale := []string{}
	for s := range want {
		if !docSubjects[s] {
			missing = append(missing, s)
		}
	}
	for s := range docSubjects {
		if !want[s] {
			stale = append(stale, s)
		}
	}
	if len(missing) > 0 || len(stale) > 0 {
		sort.Strings(missing)
		sort.Strings(stale)
		t.Errorf("docs/wire-protocol.md Known Subjects table drifted from protocol.AllSubjects():\n  missing from doc: %v\n  in doc but no Subject constant: %v",
			missing, stale)
	}

	// order.skipped is a message TYPE pinned by the Type Registry table, not a
	// subject. It sat in the subjects table for months because the string is
	// identical; the set check above would drop it as stale, but naming it
	// here makes that failure readable instead of a bare list entry.
	if docSubjects["order.skipped"] {
		t.Error("order.skipped is listed as a subject — it is TypeOrderSkipped, a message type (see the Complete Type Registry table)")
	}
}

// TestWireProtocolDocNoExpiryRowsMatchCode pins the `**none**` rows of the
// Subject-Specific TTL table against the NoExpiry set in expiry.go. The prose
// above the table ("N subjects carry NO expiry") is pinned by
// TestWireProtocolDocNoExpiryProseCountsTheTable below, which keeps the count
// honest rather than freezing the words.
func TestWireProtocolDocNoExpiryRowsMatchCode(t *testing.T) {
	doc := readDoc(t)

	docNone := parseNoExpiryRows(t, doc)

	want := map[string]bool{}
	for _, s := range protocol.AllSubjects() {
		if protocol.DataTTLFor(s) == protocol.NoExpiry {
			want[s] = true
		}
	}

	missing := []string{}
	stale := []string{}
	for s := range want {
		if !docNone[s] {
			missing = append(missing, s)
		}
	}
	for s := range docNone {
		if !want[s] {
			stale = append(stale, s)
		}
	}
	if len(missing) > 0 || len(stale) > 0 {
		sort.Strings(missing)
		sort.Strings(stale)
		t.Errorf("docs/wire-protocol.md TTL table `**none**` rows drifted from protocol/expiry.go NoExpiry set:\n  NoExpiry but not in table: %v\n  in table but TTL is not NoExpiry: %v",
			missing, stale)
	}
}

// TestWireProtocolDocNoExpiryProseCountsTheTable keeps the prose count ("N
// subjects carry NO expiry") in step with the table. It does not freeze the
// sentence: any count that matches the `**none**` rows passes.
func TestWireProtocolDocNoExpiryProseCountsTheTable(t *testing.T) {
	doc := readDoc(t)
	none := parseNoExpiryRows(t, doc)

	// The prose names the count once, in bold lead-in style.
	for _, line := range strings.Split(doc, "\n") {
		if !strings.Contains(line, "carry NO expiry at all") {
			continue
		}
		if !strings.Contains(line, countWord(len(none))) {
			t.Errorf("prose says %q but the TTL table has %d `**none**` rows — update the prose with the table (pinned against expiry.go by TestWireProtocolDocNoExpiryRowsMatchCode)",
				strings.TrimSpace(line), len(none))
		}
		return
	}
	t.Fatalf("the \"carry NO expiry at all\" prose sentence is gone from docs/wire-protocol.md — restore it or repoint this pin")
}

func countWord(n int) string {
	words := map[int]string{2: "Two", 3: "Three", 4: "Four", 5: "Five", 6: "Six", 7: "Seven", 8: "Eight", 9: "Nine"}
	if w, ok := words[n]; ok {
		return w
	}
	// Outside 2–9 the prose should spell the set out instead of counting; the
	// digit string still lets the test fail loudly rather than silently pass.
	return fmt.Sprintf("%d", n)
}

func readDoc(t *testing.T) string {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, "docs", "wire-protocol.md"))
	if err != nil {
		t.Fatalf("read docs/wire-protocol.md: %v", err)
	}
	return string(b)
}

// parseSubjectsTable extracts the first-column subject names from the table
// under "### Known Subjects" up to the next "###" heading.
func parseSubjectsTable(t *testing.T, doc string) map[string]bool {
	t.Helper()
	body := section(t, doc, "### Known Subjects")
	return firstColumn(t, body)
}

// parseNoExpiryRows extracts the subjects whose TTL column reads `**none**`
// from the table under "### Subject-Specific TTLs".
func parseNoExpiryRows(t *testing.T, doc string) map[string]bool {
	t.Helper()
	body := section(t, doc, "### Subject-Specific TTLs")
	out := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		cols := strings.Split(line, "|")
		if len(cols) < 3 {
			continue
		}
		subject := strings.TrimSpace(cols[1])
		ttl := strings.TrimSpace(cols[2])
		subject = strings.TrimSuffix(strings.TrimPrefix(subject, "`"), "`")
		if ttl == "**none**" {
			out[subject] = true
		}
	}
	if len(out) == 0 {
		t.Fatal("no `**none**` TTL rows found — the parser is looking at the wrong place, or the table lost its rows")
	}
	return out
}

// section returns the doc body between `heading` and the next heading of any
// level, excluding the heading lines themselves.
func section(t *testing.T, doc, heading string) string {
	t.Helper()
	lines := strings.Split(doc, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == heading {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("heading %q not found in docs/wire-protocol.md", heading)
	}
	var out []string
	for _, l := range lines[start:] {
		if strings.HasPrefix(l, "###") || strings.HasPrefix(l, "## ") {
			break
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// firstColumn reads `| `name` | ... |` rows, keyed by the backticked first
// cell. Rows with an empty or non-backticked first cell (separator rows,
// prose) are skipped.
func firstColumn(t *testing.T, body string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		cols := strings.Split(line, "|")
		if len(cols) < 2 {
			continue
		}
		name := strings.TrimSpace(cols[1])
		name = strings.TrimSuffix(strings.TrimPrefix(name, "`"), "`")
		if name == "" {
			continue
		}
		// Retired rows (strikethrough) are history, not register; skip them.
		if strings.HasPrefix(name, "~~") {
			continue
		}
		out[name] = true
	}
	if len(out) == 0 {
		t.Fatal("table under the heading has no backticked first-column rows — the parser is looking at the wrong place")
	}
	return out
}
