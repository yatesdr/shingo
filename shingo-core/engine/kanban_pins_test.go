package engine

import (
	"encoding/json"
	"testing"
	"time"

	"shingocore/dispatch"
)

// kanban_pins_test.go — characterization pins with no database.

// TestKanbanPin_LandedReceiptConfirmsThreePrefixesOnly: Core confirms a landed move
// of its own two window pulls and of the surplus arm, by prefix, and no other order it
// asked for: every other ask waits for its receipt or the auto-confirm sweep.
func TestKanbanPin_LandedReceiptConfirmsThreePrefixesOnly(t *testing.T) {
	t.Parallel()
	for uuid, want := range map[string]string{
		dispatch.Stage2EdgeUUIDPrefix + "1":  stage2Receipt,
		dispatch.DrainEdgeUUIDPrefix + "1":   drainReceipt,
		dispatch.SurplusEdgeUUIDPrefix + "1": surplusReceipt,
		"core-mnt-1":                         "",
		"core-l1-1":                          "",
		"edge-order-1":                       "",
		"":                                   "",
	} {
		if got := landedReceipt(uuid); got != want {
			t.Errorf("landedReceipt(%q) = %q, want %q", uuid, got, want)
		}
	}
	if dispatch.Stage2EdgeUUIDPrefix != "core-s2-" || dispatch.DrainEdgeUUIDPrefix != "core-dr-" || dispatch.SurplusEdgeUUIDPrefix != "core-sp-" {
		t.Errorf("prefixes %q, %q and %q, want core-s2-, core-dr- and core-sp-",
			dispatch.Stage2EdgeUUIDPrefix, dispatch.DrainEdgeUUIDPrefix, dispatch.SurplusEdgeUUIDPrefix)
	}
}

// TestKanbanLineState_TheCardsJSON: what the card reads is on the wire, and the
// tick's own fields are not.
func TestKanbanLineState_TheCardsJSON(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(KanbanLineState{GroupNode: "K", BinTypeCode: "T", Want: 9, Base: 1, Created: 3,
		OriginID: "o", OpenedAt: time.Now(), HeldBy: "why", RunningProcesses: []string{"P"}, Slots: 8, TotalWant: 9, CannotCover: 1,
		NothingProcesses: []string{"N"}, FallbackProcesses: []string{"F"}})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"created", "origin_id", "opened_at", "Created", "OriginID", "OpenedAt"} {
		if _, ok := got[gone]; ok {
			t.Errorf("%s is on the wire: %s", gone, raw)
		}
	}
	for _, kept := range []string{"want", "base", "held_by", "running_processes", "slots", "total_want", "cannot_cover",
		"nothing_processes", "fallback_processes"} {
		if _, ok := got[kept]; !ok {
			t.Errorf("%s is not on the wire: %s", kept, raw)
		}
	}
}
