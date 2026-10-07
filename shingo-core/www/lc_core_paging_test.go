package www

import (
	"fmt"
	"testing"
	"time"

	"shingo/protocol/debuglog"
)

// TestPageLogEntries pins the Core Logs paging (R25, LC11): the newest 100,
// Older / Newer, and which rows of how many. Before, the page rendered every
// entry in the ring (1,000 on a busy Core).
func TestPageLogEntries(t *testing.T) {
	mk := func(n int) []debuglog.Entry {
		out := make([]debuglog.Entry, n)
		for i := range out {
			out[i] = debuglog.Entry{Time: time.Unix(int64(i), 0), Message: fmt.Sprint(i)}
		}
		return out
	}
	all := mk(250) // oldest first, messages "0" … "249"

	cases := []struct {
		page                  int
		wantPage, from, to, n int
		older, newer          int
		firstMsg, lastMsg     string
	}{
		{0, 1, 1, 100, 100, 2, 0, "150", "249"},  // no page given: the newest 100
		{1, 1, 1, 100, 100, 2, 0, "150", "249"},  //
		{2, 2, 101, 200, 100, 3, 1, "50", "149"}, //
		{3, 3, 201, 250, 50, 0, 2, "0", "49"},    // the oldest, short page
		{9, 3, 201, 250, 50, 0, 2, "0", "49"},    // past the end: the oldest page
		{-4, 1, 1, 100, 100, 2, 0, "150", "249"}, //
	}
	for _, tc := range cases {
		rows, p := pageLogEntries(all, tc.page)
		if p.Page != tc.wantPage || p.From != tc.from || p.To != tc.to || p.Total != 250 || p.Older != tc.older || p.Newer != tc.newer {
			t.Errorf("page %d: got %+v", tc.page, p)
		}
		if len(rows) != tc.n || rows[0].Message != tc.firstMsg || rows[len(rows)-1].Message != tc.lastMsg {
			t.Errorf("page %d: %d rows %q…%q, want %d rows %q…%q", tc.page, len(rows), rows[0].Message, rows[len(rows)-1].Message, tc.n, tc.firstMsg, tc.lastMsg)
		}
	}

	rows, p := pageLogEntries(nil, 1)
	if len(rows) != 0 || p.Total != 0 || p.Older != 0 || p.Newer != 0 {
		t.Errorf("empty log: %d rows, %+v", len(rows), p)
	}
	rows, p = pageLogEntries(mk(100), 1)
	if len(rows) != 100 || p.Older != 0 || p.To != 100 {
		t.Errorf("exactly one page: %d rows, %+v", len(rows), p)
	}
}

// TestPingWithin: a fleet check that does not answer is answered for it
// (LC10). Before, the Dashboard waited out the fleet client's own timeout,
// twice per render.
func TestPingWithin(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	start := time.Now()
	err := pingWithin(func() error { <-block; return nil }, 50*time.Millisecond)
	if err == nil {
		t.Fatal("a ping that never answers: want an error")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("answered after %v, want about 50ms", el)
	}
	if err := pingWithin(func() error { return nil }, time.Second); err != nil {
		t.Errorf("a ping that answers ok: got %v", err)
	}
	want := fmt.Errorf("refused")
	if err := pingWithin(func() error { return want }, time.Second); err != want {
		t.Errorf("a ping that fails: got %v, want %v", err, want)
	}
}
