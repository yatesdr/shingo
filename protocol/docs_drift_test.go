package protocol_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shingo/protocol"
)

// docs/wire-protocol.md's subject registry, TTL tables, type registry and
// payload field tables are GENERATED from this package (shared/scripts/docgen,
// wire.go), and docgen's TestGeneratedBlocksAreCurrent fails when a committed
// block is not what the code emits now. The subject and NoExpiry tables this
// file used to parse and pin are therefore pinned there.
//
// What the generator cannot see is the prose: the TTL section states the size
// of the NoExpiry set in words ("N subjects carry NO expiry at all"). That one
// sentence is pinned here, against the code rather than the table, so the
// count stays honest without freezing the words.
func TestWireProtocolDocNoExpiryProseCountsTheSet(t *testing.T) {
	n := 0
	for _, s := range protocol.AllSubjects() {
		if protocol.DataTTLFor(s) == protocol.NoExpiry {
			n++
		}
	}
	for _, line := range strings.Split(readDoc(t), "\n") {
		if !strings.Contains(line, "carry NO expiry at all") {
			continue
		}
		if !strings.Contains(line, countWord(n)) {
			t.Errorf("prose says %q but %d subjects are NoExpiry in protocol/expiry.go — update the prose (the table above it is generated)",
				strings.TrimSpace(line), n)
		}
		return
	}
	t.Fatalf("the \"carry NO expiry at all\" prose sentence is gone from docs/wire-protocol.md — restore it or repoint this pin")
}

func countWord(n int) string {
	words := map[int]string{2: "Two", 3: "Three", 4: "Four", 5: "Five", 6: "Six", 7: "Seven", 8: "Eight", 9: "Nine",
		10: "Ten", 11: "Eleven", 12: "Twelve"}
	if w, ok := words[n]; ok {
		return w
	}
	// Outside the words the prose should spell the set out instead of counting;
	// the digit string still lets the test fail loudly rather than silently pass.
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
