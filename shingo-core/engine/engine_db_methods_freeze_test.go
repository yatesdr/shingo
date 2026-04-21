package engine

import (
	"os"
	"regexp"
	"testing"
)

// TestEngineDbMethodsFrozen enforces a strict monotonic-decrease ratchet
// on the passthrough count. The constant MUST match the current file, and
// MUST only ever be updated downward.
//
// Rationale: engine_db_methods.go is a delegation layer that exists only
// so shingo-core/www handlers don't call *store.DB directly. The Stage-1
// refactor target is to drive this count to zero via service interfaces.
// New passthrough methods should never be added — if a handler needs a
// new store method, add it to a service interface instead.
//
// Enforcement:
//   - If current file count > constant: test FAILS (someone added a passthrough).
//   - If current file count < constant: test FAILS ("ratchet the constant
//     downward in this PR; do not leave it stale").
//   - Equality is the only passing state.
//
// This forces every PR that removes passthroughs to also update the constant,
// which makes the decrease visible in git history and cross-PR traceable.
// When the count hits zero, this test and engine_db_methods.go both get deleted.
const frozenPassthroughCount = 0

func TestEngineDbMethodsFrozen(t *testing.T) {
	src, err := os.ReadFile("engine_db_methods.go")
	if err != nil {
		t.Fatalf("read engine_db_methods.go: %v", err)
	}
	re := regexp.MustCompile(`(?m)^func \(e \*Engine\)`)
	got := len(re.FindAll(src, -1))
	switch {
	case got > frozenPassthroughCount:
		t.Fatalf("passthrough count grew: %d > %d (add to a service interface instead)", got, frozenPassthroughCount)
	case got < frozenPassthroughCount:
		t.Fatalf("passthrough count is %d but constant is %d — this PR removed passthroughs but did not ratchet the constant down; update frozenPassthroughCount to %d in this same PR", got, frozenPassthroughCount, got)
	}
}
