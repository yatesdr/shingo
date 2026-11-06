package www

import (
	"testing"
	"time"

	"shingocore/domain"
)

// TestOrderNotes pins the order page's Notes: audit rows that restate a status
// (received, cancelled, …) are dropped, decisions keep their own sentence, a
// recovery verb reads as its label and who did it, and the list is oldest
// first though the store lists newest first.
func TestOrderNotes(t *testing.T) {
	t.Parallel()
	at := func(m int) time.Time { return time.Date(2026, 11, 5, 9, m, 0, 0, time.UTC) }
	newestFirst := []*domain.AuditEntry{
		{Action: "recovery.reissue_terminate", NewValue: "alice", Actor: "alice", CreatedAt: at(4)},
		{Action: "fleet_still_running", NewValue: "RDS still RUNNING (AMR-06) after 3 terminates — stop it in RoboShop", CreatedAt: at(3)},
		{Action: "cancelled", NewValue: "fleet order stopped", CreatedAt: at(2)},
		{Action: "swap_half_completed", NewValue: "sibling 3166 terminal (cancelled) while this leg reached delivered", CreatedAt: at(1)},
		{Action: "received", NewValue: "complex X from line-1", CreatedAt: at(0)},
	}
	got := orderNotes(newestFirst)
	want := []string{
		"sibling 3166 terminal (cancelled) while this leg reached delivered",
		"RDS still RUNNING (AMR-06) after 3 terminates — stop it in RoboShop",
		"Terminate re-sent (by alice)",
	}
	if len(got) != len(want) {
		t.Fatalf("notes = %+v, want %d", got, len(want))
	}
	for i, n := range got {
		if n.Text != want[i] {
			t.Errorf("note %d = %q, want %q", i, n.Text, want[i])
		}
	}
	if !got[0].At.Before(got[2].At) {
		t.Errorf("notes are not oldest first: %+v", got)
	}
}
