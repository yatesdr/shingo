// Decision 55's record half, in both books: a cart the line draws below 0 is
// released at the count the Edge's seat reads, and Core records the overpack.
// The release declares the seat's count as the Edge sends it (the clamp to 0
// is gone); Core writes 0 and its ledger row carries the declared count, the
// bins page's timeline "released at 0 (overpack N)". RED before: Core refused
// the negative count.
//
//go:build docker

package scenarios

import (
	"strings"
	"testing"

	"shingo/protocol"
)

func TestScenario_H_R6_ACartDrawnPastZeroIsReleasedAsAnOverpack(t *testing.T) {
	t.Parallel()
	w := newTwoBooks(t, "SCN-H-R6", 1)
	w.tick()
	w.tick()
	w.deliverCoreAdjustments()
	declared := w.edgeRuntime().RemainingUOPCached
	if declared != -1 {
		t.Fatalf("the Edge's seat after two strokes on a cart of 1 = %d, want -1", declared)
	}
	if got, _ := w.coreBin(); got != -1 {
		t.Fatalf("Core's count after the two strokes = %d, want -1", got)
	}

	if err := w.manifest.SyncOrClearForReleasedFenced(w.binID, w.order.ID, &declared,
		protocol.DispositionReleasePartial, "operator", w.fence()); err != nil {
		t.Fatalf("the release declaring %d: %v (want it written as 0 and recorded)", declared, err)
	}
	if got, _ := w.coreBin(); got != 0 {
		t.Errorf("Core uop_remaining after the release = %d, want 0", got)
	}
	var (
		op       string
		before   int
		after    int
		declSeen int
	)
	if err := w.core.QueryRow(`SELECT op, before_uop, after_uop, (detail->>'declared')::int FROM bin_uop_ledger
		WHERE bin_id=$1 AND op LIKE 'released%' ORDER BY id DESC LIMIT 1`, w.binID).Scan(&op, &before, &after, &declSeen); err != nil {
		t.Fatalf("the release's ledger row: %v", err)
	}
	if op != "released_overpack" || before != -1 || after != 0 || declSeen != -1 {
		t.Errorf("the release's row = %s %d -> %d declared %d, want released_overpack -1 -> 0 declared -1", op, before, after, declSeen)
	}
	entries, err := w.core.ListEntityAudit("bin", w.binID)
	if err != nil {
		t.Fatalf("the bin's audit: %v", err)
	}
	line := ""
	for _, e := range entries {
		if strings.HasPrefix(e.Action, "released") {
			line = e.Action + ": " + e.NewValue
			break
		}
	}
	if !strings.Contains(line, "released at 0 (overpack 1)") {
		t.Errorf("the bins page's timeline reads %q, want \"released at 0 (overpack 1)\"", line)
	}
	if got := w.staleDrops(); got != 0 {
		t.Errorf("stale_epoch_dropped = %d, want 0", got)
	}
}
