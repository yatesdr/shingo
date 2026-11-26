package resend

import (
	"strconv"
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
		if got := r.Send(s.digest, "", t0.Add(s.at)); got != s.wantSend {
			t.Errorf("%s: send %v, want %v", s.name, got, s.wantSend)
		}
		if r.Streak() != s.streak {
			t.Errorf("%s: streak %d, want %d", s.name, r.Streak(), s.streak)
		}
	}
	if !r.NotConverging() {
		t.Error("three sends in a row: want not converging")
	}
	if d, at := r.Last(); d != "d2" || !at.Equal(t0.Add(Guard+time.Second)) {
		t.Errorf("Last = %s at %v", d, at)
	}
	r.Converged()
	if r.Streak() != 0 || r.NotConverging() {
		t.Errorf("after Converged: streak %d", r.Streak())
	}
	if r.Quoted("d1") || !r.Quoted("d2") {
		t.Error("Quoted after Converged: want d2, the newest send, only")
	}
}

// A receiver that held the last send and now quotes something else lost it
// (it restarted): the same digest goes at once, inside the guard, as the first
// send of a new streak, and a repeat of THAT send is held while it is in
// flight. A receiver still quoting the send it holds (a sender re-asking for
// its own reasons) is not one that forgot, and the guard holds the repeat. A
// receiver that never quoted a send back is waiting on a copy in flight: held,
// and three sends never quoted back still flag the key.
func TestRecord_AForgottenSendGoesAtOnce(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

	var r Record
	if !r.Send("d1", "", t0) {
		t.Fatal("never sent: want due")
	}
	r.Converged() // the receiver quoted d1 back
	if r.Send("d1", "d1", t0.Add(20*time.Second)) {
		t.Error("the receiver still quotes d1: want the guard to hold a repeat")
	}
	if !r.Send("d1", "", t0.Add(30*time.Second)) {
		t.Error("held, then quoted as nothing inside the guard: want the same digest sent at once")
	}
	if r.Streak() != 1 || r.NotConverging() {
		t.Errorf("after the forgotten send: streak %d, want 1", r.Streak())
	}
	if r.Send("d1", "", t0.Add(40*time.Second)) {
		t.Error("that send not yet quoted back: want the guard to hold a repeat")
	}

	var s Record
	s.Send("d1", "", t0)
	if s.Send("d1", "", t0.Add(30*time.Second)) {
		t.Error("never converged: want the same digest held inside the guard")
	}
	for i := 1; i <= 2; i++ {
		s.Send("d1", "", t0.Add(time.Duration(i)*Guard))
	}
	if !s.NotConverging() {
		t.Errorf("three sends never quoted back: streak %d, want the not-converging flag", s.Streak())
	}
}

// A quote proves one send made since the last one proved, by either path, and
// what preceded it: at every position of the kept sends once, past the bound
// and for an empty quote never. The newest send outlives its proof.
func TestRecord_AQuoteProvesOneSendAndWhatPrecededIt(t *testing.T) {
	t.Parallel()
	if sentKept != 256 {
		t.Fatalf("sentKept = %d, want 256, which the Core and Edge pins assume", sentKept)
	}
	t0 := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	name := func(i int) string { return "p" + strconv.Itoa(i) }
	for _, n := range []int{1, 2, 22, sentKept, sentKept + 1, 300} {
		for p := 0; p <= n; p++ {
			var r Record
			for i := 1; i <= n; i++ {
				r.Pushed(name(i), t0.Add(time.Duration(i)*time.Second))
			}
			quote := ""
			if p > 0 {
				quote = name(p)
			}
			want := p > 0 && p > n-sentKept
			if got := r.Quoted(quote); got != want {
				t.Fatalf("%d sends, quote of %d: proved %v, want %v", n, p, got, want)
			}
			if r.Quoted(quote) {
				t.Fatalf("%d sends, quote of %d again: proved twice", n, p)
			}
			if p > 1 && want && r.Quoted(name(p-1)) {
				t.Fatalf("%d sends, %d proved: the older %d still proves", n, p, p-1)
			}
			if d, at := r.Last(); d != name(n) || !at.Equal(t0.Add(time.Duration(n)*time.Second)) {
				t.Fatalf("%d sends, quote of %d: Last = %s at %v, want the newest", n, p, d, at)
			}
		}
	}
}

// A push is neither counted in the streak nor held back by the guard, and only
// a quote of the last send, not of an older one, makes a repeat a lost copy.
func TestRecord_APushLeavesTheGuardAndTheStreakAlone(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	var r Record
	for _, d := range []string{"p1", "p2", "p3"} {
		r.Pushed(d, t0)
	}
	if r.Streak() != 0 || r.NotConverging() {
		t.Errorf("after three pushes: streak %d, want 0", r.Streak())
	}
	if !r.Send("p3", "", t0.Add(time.Second)) || r.Streak() != 1 {
		t.Errorf("the pushed digest inside the guard: want it sent as the streak's first, streak %d", r.Streak())
	}
	if !r.Quoted("p1") {
		t.Fatal("quote of the oldest push: want it proved")
	}
	if r.Send("p3", "p1", t0.Add(2*time.Second)) {
		t.Error("quote of an older copy, p3 in flight: want the guard to hold")
	}
	if !r.Quoted("p3") {
		t.Fatal("quote of the last send: want it proved")
	}
	if !r.Send("p3", "", t0.Add(3*time.Second)) {
		t.Error("held the last send, then quoted nothing: want it at once")
	}
}
