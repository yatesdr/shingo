package www

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"shingo/protocol/debuglog"
	"shingoedge/domain"
)

// history_page_test.go — R25 (LC2, LC6): the paged history views.

// TestPinEdgeLogs_PageNewestHundred: the Logs page shows the newest 100 lines
// of the ring, oldest of those first, and pages back through the rest.
func TestPinEdgeLogs_PageNewestHundred(t *testing.T) {
	base := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	all := make([]debuglog.Entry, 1000)
	for i := range all {
		all[i] = debuglog.Entry{Time: base.Add(time.Duration(i) * time.Second), Subsystem: "s", Message: fmt.Sprint(i)}
	}
	for _, tc := range []struct {
		page, wantPage int
		first, last    string
		summary        string
		newer, older   bool
	}{
		{1, 1, "900", "999", "rows 1–100 of 1000", false, true},
		{2, 2, "800", "899", "rows 101–200 of 1000", true, true},
		{10, 10, "0", "99", "rows 901–1000 of 1000", true, false},
		{99, 10, "0", "99", "rows 901–1000 of 1000", true, false}, // past the end: the oldest page
	} {
		got, p := logsPage(all, "", tc.page)
		if len(got) != 100 || got[0].Message != tc.first || got[99].Message != tc.last {
			t.Errorf("page %d: %d rows %s..%s, want 100 rows %s..%s", tc.page, len(got),
				got[0].Message, got[len(got)-1].Message, tc.first, tc.last)
		}
		if p.Page != tc.wantPage || p.Summary() != tc.summary {
			t.Errorf("page %d: pager page %d %q, want %d %q", tc.page, p.Page, p.Summary(), tc.wantPage, tc.summary)
		}
		if (p.NewerURL != "") != tc.newer || (p.OlderURL != "") != tc.older {
			t.Errorf("page %d: newer %q older %q", tc.page, p.NewerURL, p.OlderURL)
		}
	}

	// A short ring is one page; the subsystem filter rides the links.
	got, p := logsPage(all[:30], "plc", 1)
	if len(got) != 30 || p.Summary() != "rows 1–30 of 30" || p.NewerURL != "" || p.OlderURL != "" {
		t.Errorf("short ring: %d rows, %q, newer %q older %q", len(got), p.Summary(), p.NewerURL, p.OlderURL)
	}
	_, p = logsPage(all, "plc", 1)
	if p.OlderURL != "/diagnostics?page=2&subsystem=plc" {
		t.Errorf("older link = %q, want the subsystem kept", p.OlderURL)
	}
	got, p = logsPage(nil, "", 1)
	if len(got) != 0 || p.Summary() != "rows 0 of 0" {
		t.Errorf("empty ring: %d rows, %q", len(got), p.Summary())
	}
}

// TestPinOrders_PagerLinksAndLine: the Orders pager keeps the view's status and
// process on its links and says which rows of how many.
func TestPinOrders_PagerLinksAndLine(t *testing.T) {
	q := url.Values{}
	q.Set("status", "confirmed")
	q.Set("process", "3")
	p := newHistoryPage("/orders", q, 2, 100, 393)
	if p.Summary() != "rows 101–200 of 393" {
		t.Errorf("summary = %q", p.Summary())
	}
	if p.NewerURL != "/orders?page=1&process=3&status=confirmed" || p.OlderURL != "/orders?page=3&process=3&status=confirmed" {
		t.Errorf("links: newer %q older %q", p.NewerURL, p.OlderURL)
	}
	if last := newHistoryPage("/orders", q, 4, 93, 393); last.OlderURL != "" || last.Summary() != "rows 301–393 of 393" {
		t.Errorf("last page: older %q, %q", last.OlderURL, last.Summary())
	}
}

// TestPinOrders_PagerSitsOutsideTheTable: the pager renders under the table,
// not as one of its rows, so the page's text filter (which walks
// #orders-table tbody tr) still filters only orders. The Active tab has no
// pager.
func TestPinOrders_PagerSitsOutsideTheTable(t *testing.T) {
	q := url.Values{}
	q.Set("status", "all")
	q.Set("process", "0")
	data := map[string]any{
		"ActiveOrders": []domain.Order{{ID: 1, UUID: "pager-row", Status: "confirmed"}},
		"Pager":        newHistoryPage("/orders", q, 1, 1, 150),
	}
	html := renderOrdersBody(t, data)
	end := strings.Index(html, "</table>")
	pager := strings.Index(html, "data-orders-pager")
	if end < 0 || pager < end {
		t.Fatalf("pager at %d, table ends at %d — the pager must sit after the table", pager, end)
	}
	if !strings.Contains(html, "rows 1–1 of 150") || !strings.Contains(html, `href="/orders?page=2&amp;process=0&amp;status=all"`) {
		t.Errorf("pager line or Older link missing:\n%s", html[pager:])
	}

	delete(data, "Pager")
	if strings.Contains(renderOrdersBody(t, data), "data-orders-pager") {
		t.Error("the Active tab (no pager) rendered a pager")
	}
}
