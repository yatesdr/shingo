package protocol_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"shingo/protocol"
)

// docs/order-state-machine/transitions.md deliberately does NOT copy the
// state machine out of the code — it is a table of POINTERS: "the statuses
// live in protocol/status.go", "the transitions live in validTransitions",
// and so on. The doc's own history (two statuses missing, thirteen
// transitions, seven methods out of date when it still carried tables) is
// why. But pointers rot too, in a different way: a rename or a move leaves
// the doc naming a symbol or file that no longer exists, and the next
// reader — arriving, per the forbidigo failure message, precisely when they
// need the machine — is sent to a dead reference.
//
// This test file pins the pointer table's factual claims against the code:
//
//   - every `symbol` the doc names in its "Where the machine actually lives"
//     table exists in the file the doc says it lives in;
//   - the predicate list is exactly the exported status-set predicates
//     protocol/status.go actually declares (missing AND extra both fail).
//
// The terminality row ("terminal iff no outgoing edges") is NOT pinned by a
// separate test: IsTerminal's body IS that derivation (types.go: "returns
// true if the status has no outgoing transitions"), so a test asserting one
// against the other would assert the code against itself — a check that
// cannot fail while the claim is false any other way. The row's file pointer
// and the predicate list are the load-bearing claims, and both are pinned
// above.
func TestTransitionsDocPointsAtLiveSymbols(t *testing.T) {
	doc := readTransitionsDoc(t)
	table := sectionAfter(t, doc, "Where the machine actually lives")

	type row struct{ what, where string }
	var rows []row
	for _, line := range strings.Split(table, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") || strings.HasPrefix(line, "|---") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 2 {
			continue
		}
		rows = append(rows, row{what: strings.TrimSpace(cells[0]), where: strings.TrimSpace(cells[1])})
	}
	if len(rows) == 0 {
		t.Fatalf("no rows parsed from the pointer table — the doc's structure changed; update this test alongside the doc")
	}

	for _, r := range rows {
		where := r.where
		// Split "path — symbols" on the em dash the doc uses.
		pathPart := where
		symPart := ""
		if i := strings.Index(where, " — "); i >= 0 {
			pathPart, symPart = strings.TrimSpace(where[:i]), where[i+3:]
		}
		if !strings.HasSuffix(pathPart, ".go") {
			continue // e.g. the diagram row, pointing at another doc
		}
		src := readRepoFile(t, pathPart)
		// Every backticked identifier in the symbol half must appear in the file.
		for _, sym := range backticks(symPart) {
			if sym == "const block" {
				continue // prose, not an identifier
			}
			if !strings.Contains(src, sym) {
				t.Errorf("transitions.md row %q: %s names %q, but %s does not declare or mention it",
					r.what, symPart, sym, pathPart)
			}
		}
	}
}

// TestTransitionsDocPredicateListIsComplete asserts the doc's predicate list
// equals the exported Is*/Blocks* predicates in protocol/status.go — both
// directions: a predicate missing from the doc fails, and a doc entry with no
// predicate fails.
func TestTransitionsDocPredicateListIsComplete(t *testing.T) {
	statusSrc := readRepoFile(t, "protocol/status.go")

	// Declared: every `func Name(` with the doc's naming shape.
	declared := map[string]bool{}
	for _, line := range strings.Split(statusSrc, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "func ") {
			continue
		}
		name := strings.TrimPrefix(line, "func ")
		if i := strings.Index(name, "("); i >= 0 {
			name = name[:i]
		}
		if (strings.HasPrefix(name, "Is") || strings.HasPrefix(name, "Blocks")) &&
			name != "IsTerminal" && // listed in its own row? no — same row; keep
			strings.Contains(line, "Status") {
			declared[name] = true
		}
	}
	// IsTerminal IS part of the doc's predicate row; add back deliberately.
	declared["IsTerminal"] = true

	doc := readTransitionsDoc(t)
	table := sectionAfter(t, doc, "Where the machine actually lives")
	listed := map[string]bool{}
	for _, sym := range backticks(table) {
		if strings.HasPrefix(sym, "Is") || strings.HasPrefix(sym, "Blocks") {
			listed[sym] = true
		}
	}

	for name := range declared {
		if !listed[name] {
			t.Errorf("protocol/status.go declares %s but transitions.md's predicate row does not list it — a reader hunting the status-set predicates will not find it", name)
		}
	}
	for name := range listed {
		if !declared[name] {
			t.Errorf("transitions.md lists %s as a predicate, but protocol/status.go declares no such exported function — dead pointer", name)
		}
	}
}

// readTransitionsDoc loads docs/order-state-machine/transitions.md.
func readTransitionsDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "docs", "order-state-machine", "transitions.md"))
	if err != nil {
		t.Fatalf("read transitions.md: %v", err)
	}
	return string(b)
}

// readRepoFile loads a repo file by worktree-relative path ("protocol/status.go").
func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", filepath.FromSlash(path)))
	if err != nil {
		t.Fatalf("read %s (named by transitions.md): %v", path, err)
	}
	return string(b)
}

// sectionAfter returns the body between the bare text line `marker` and the
// next blank-then-heading or table end. The pointer table sits under a
// plain sentence ("Where the machine actually lives:"), not a markdown
// heading, so docs_drift_test.go's heading-based section() cannot find it.
func sectionAfter(t *testing.T, doc, marker string) string {
	t.Helper()
	lines := strings.Split(doc, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), marker) {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("marker %q not found in transitions.md — the doc's structure changed; update this test alongside the doc", marker)
	}
	var out []string
	for _, l := range lines[start:] {
		// The table ends at the first non-table line after it starts.
		if strings.HasPrefix(l, "##") { // next section heading
			break
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// backticks extracts every `backticked` span from s — the odd-numbered parts
// of a split on the backtick. (It used to return every part, so the ", "
// between two symbols came back as the "symbol" "," and trivially matched any
// file: the pointer test could not fail on a row whose separators survived.)
func backticks(s string) []string {
	var out []string
	for i, part := range strings.Split(s, "`") {
		part = strings.TrimSpace(part)
		if i%2 == 1 && part != "" {
			out = append(out, part)
		}
	}
	return out
}

// TestTransitionsDocTableMatchesTheCode pins the doc's transition table to
// protocol's: the rows must be exactly AllStatuses(), each non-terminal row's
// targets exactly that status's validTransitions edges, and each terminal row
// (no key in the table) marked *(terminal)*. An edge or a status added or
// removed on EITHER side fails — the half-life that took the doc's last tables
// (thirteen transitions and two statuses stale) cannot recur silently.
func TestTransitionsDocTableMatchesTheCode(t *testing.T) {
	doc := readTransitionsDoc(t)
	table := sectionAfter(t, doc, "## The transition table")

	docEdges := map[string][]string{}
	docTerminal := map[string]bool{}
	var order []string
	for _, line := range strings.Split(table, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") || strings.HasPrefix(line, "|---") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 2 {
			t.Fatalf("transition table row %q does not have two cells", line)
		}
		fromSyms := backticks(cells[0])
		if len(fromSyms) == 0 {
			continue // the header row
		}
		if len(fromSyms) != 1 {
			t.Fatalf("transition table row %q names %d from-statuses, want 1", line, len(fromSyms))
		}
		from := fromSyms[0]
		if _, dup := docEdges[from]; dup || docTerminal[from] {
			t.Errorf("transition table lists %s twice", from)
		}
		order = append(order, from)
		if strings.Contains(cells[1], "(terminal)") {
			docTerminal[from] = true
			if tos := backticks(cells[1]); len(tos) > 0 {
				t.Errorf("transition table marks %s terminal but also lists targets %v", from, tos)
			}
			continue
		}
		docEdges[from] = backticks(cells[1])
	}
	if len(order) == 0 {
		t.Fatalf("no rows parsed from the transition table — the doc's structure changed; update this test alongside the doc")
	}

	code := protocol.AllValidTransitions()
	statuses := map[string]bool{}
	for _, st := range protocol.AllStatuses() {
		statuses[string(st)] = true
		_, docHas := docEdges[string(st)]
		if !docHas && !docTerminal[string(st)] {
			t.Errorf("protocol declares status %s but transitions.md's transition table has no row for it", st)
		}
	}
	for _, from := range order {
		if !statuses[from] {
			t.Errorf("transitions.md's transition table has a row for %s, which is not in protocol.AllStatuses() — dead row", from)
		}
	}
	for st := range docTerminal {
		if edges, ok := code[protocol.Status(st)]; ok {
			t.Errorf("transitions.md marks %s terminal, but validTransitions gives it edges %v", st, edges)
		}
	}
	for from, docTos := range docEdges {
		codeTos, ok := code[protocol.Status(from)]
		if !ok {
			t.Errorf("transitions.md gives %s outgoing edges %v, but validTransitions has none (it is terminal)", from, docTos)
			continue
		}
		want := make([]string, 0, len(codeTos))
		for _, to := range codeTos {
			want = append(want, string(to))
		}
		got := append([]string(nil), docTos...)
		sort.Strings(want)
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("transitions.md row %s → %v, but validTransitions says %s → %v", from, got, from, want)
		}
	}
}
