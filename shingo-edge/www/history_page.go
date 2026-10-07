package www

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"shingo/protocol/debuglog"
)

// historyPageSize is how many rows a paged history view shows (R25: the
// Orders page's views other than Active, and the Logs page).
const historyPageSize = 100

// historyPage is the pager under a paged history view: which rows of how many
// this page shows, and the links either side. Page 1 is the newest.
type historyPage struct {
	Page     int
	From     int // 1-based row number of the first row shown (0 when none)
	To       int
	Total    int
	NewerURL string
	OlderURL string
}

// Summary is the pager's line, e.g. "rows 1–100 of 393".
func (p *historyPage) Summary() string {
	if p.Total == 0 {
		return "rows 0 of 0"
	}
	return fmt.Sprintf("rows %d–%d of %d", p.From, p.To, p.Total)
}

// requestedPage reads ?page= (1 = newest). Anything unreadable is page 1.
func requestedPage(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// clampPage keeps page inside 1..last for total rows, so a stale link past the
// end shows the oldest page rather than an empty one.
func clampPage(page, total int) int {
	last := (total + historyPageSize - 1) / historyPageSize
	if last < 1 {
		last = 1
	}
	if page > last {
		return last
	}
	if page < 1 {
		return 1
	}
	return page
}

// newHistoryPage builds the pager for page (already clamped) showing shown
// rows of total. path and query are the view's own URL; the links change only
// its page parameter.
func newHistoryPage(path string, query url.Values, page, shown, total int) *historyPage {
	p := &historyPage{Page: page, Total: total}
	if shown > 0 {
		p.From = (page-1)*historyPageSize + 1
		p.To = p.From + shown - 1
	}
	link := func(n int) string {
		q := url.Values{}
		for k, v := range query {
			q[k] = v
		}
		q.Set("page", strconv.Itoa(n))
		return path + "?" + q.Encode()
	}
	if page > 1 {
		p.NewerURL = link(page - 1)
	}
	if page*historyPageSize < total {
		p.OlderURL = link(page + 1)
	}
	return p
}

// logsPage is the Logs page's slice of the debug ring (R25). The ring answers
// oldest first; page 1 is the newest historyPageSize entries, still shown
// oldest first so the table reads down to the latest line.
func logsPage(all []debuglog.Entry, subsystem string, page int) ([]debuglog.Entry, *historyPage) {
	total := len(all)
	page = clampPage(page, total)
	end := total - (page-1)*historyPageSize
	start := end - historyPageSize
	if start < 0 {
		start = 0
	}
	q := url.Values{}
	if subsystem != "" {
		q.Set("subsystem", subsystem)
	}
	return all[start:end], newHistoryPage("/diagnostics", q, page, end-start, total)
}
