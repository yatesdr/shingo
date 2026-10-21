package resend

import (
	"testing"
	"time"
)

// The rule both sides apply, on one record: a new digest sends, the same one
// inside Guard is held back, the guard elapsed sends again, FlagAfter sends in
// a row mark it not converging, and the receiver quoting the last send ends
// the streak.
func TestRecord_GuardStreakAndConverge(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	var r Record
	steps := []struct {
		name     string
		at       time.Duration
		digest   string
		wantSend bool
		streak   int
	}{
		{"never sent: due", 0, "d1", true, 1},
		{"same digest inside the guard: held back", Guard - time.Second, "d1", false, 1},
		{"guard elapsed: due", Guard, "d1", true, 2},
		{"a new digest is not held back", Guard + time.Second, "d2", true, 3},
	}
	for _, s := range steps {
		if got := r.Send(s.digest, t0.Add(s.at)); got != s.wantSend {
			t.Errorf("%s: send %v, want %v", s.name, got, s.wantSend)
		}
		if r.Streak() != s.streak {
			t.Errorf("%s: streak %d, want %d", s.name, r.Streak(), s.streak)
		}
	}
	if !r.NotConverging() {
		t.Error("three sends in a row: want not converging")
	}
	if r.IsLast("d1") || !r.IsLast("d2") {
		t.Error("IsLast: want d2 only")
	}
	if d, at := r.Last(); d != "d2" || !at.Equal(t0.Add(Guard+time.Second)) {
		t.Errorf("Last = %s at %v", d, at)
	}
	r.Converged()
	if r.Streak() != 0 || r.NotConverging() {
		t.Errorf("after Converged: streak %d", r.Streak())
	}
}
