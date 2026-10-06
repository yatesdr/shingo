package www

import (
	"html/template"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/domain"
)

// The threshold is what the whole design turns on: under it an operator is told
// the order is replanning, over it that it faulted. These render the two states
// and assert what the page says.

func faultRow(t *testing.T, elapsed time.Duration, ref protocol.TermRef) (string, protocol.FaultLine) {
	t.Helper()
	const noticeAfter = 60 * time.Second
	now := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
	since := now.Add(-elapsed)
	line := protocol.BuildFaultLine(ref, since, since.Add(45*time.Minute), now, noticeAfter)

	order := &domain.Order{ID: 4711, EdgeUUID: "e-4711", StationID: "plant-a.line-1", Status: protocol.StatusFaulted}
	data := map[string]any{
		"Page":       "orders",
		"Orders":     []*domain.Order{order},
		"FaultLines": map[int64]template.HTML{order.ID: template.HTML(line.HTML())},
	}
	if line.Notice {
		data["FaultedNoticeCount"] = 1
	}
	namer := &fakeNamer{byUID: map[string]string{"plant-a.line-1": "SPRINGFIELD / LINE 1"}}
	return renderPageWithNamer(t, "orders.html", namer, data), line
}

// A 14-second fault is a replan. 706 of 730 faults over 30 days recovered on
// their own with a median of 20s; announcing all of them as faults is how the
// word stops being read.
func TestOrdersPage_FaultUnderThresholdReadsAsReplanning(t *testing.T) {
	t.Parallel()
	html, line := faultRow(t, 14*time.Second, protocol.TermRef{
		Node: "ALN_003", VendorCode: 60011, VendorDesc: "cannot replan",
	})

	if line.Notice {
		t.Fatal("14s must be under the 60s threshold")
	}
	// The VISIBLE text — the over-threshold wording also ships, as a data
	// attribute, so the client can swap it when the clock crosses without
	// another request. What matters is which one is rendered as the element's
	// content.
	if !strings.Contains(html, ">Replanning</span>") {
		t.Error("a fault under the threshold must read Replanning")
	}
	// The vendor reason is withheld with the word: "cannot replan" beside
	// "Replanning" is a contradiction at 14 seconds.
	if strings.Contains(html, ">Fault · cannot replan (60011)</span>") {
		t.Error("the fleet reason must not be shown under the threshold")
	}
	// No chip: a replan is not something to walk toward.
	if strings.Contains(html, "chip-warn") {
		t.Error("a replan must not raise the faulted chip")
	}
	// The clock is present and server-rendered, so the line is complete before
	// any script runs.
	if !strings.Contains(html, `data-since="2026-08-22T13:59:46Z"`) {
		t.Error("the fault line must carry data-since for the live clock")
	}
	if !strings.Contains(html, `data-notice-after="60"`) {
		t.Error("the threshold must be echoed so the client can cross it on its own")
	}
}

func TestOrdersPage_FaultOverThresholdReadsAsFaultWithTheFleetsReason(t *testing.T) {
	t.Parallel()
	html, line := faultRow(t, 3*time.Minute+12*time.Second, protocol.TermRef{
		Node: "ALN_003", VendorCode: 60011, VendorDesc: "cannot replan",
	})

	if !line.Notice {
		t.Fatal("3m12s must be over the 60s threshold")
	}
	if !strings.Contains(html, ">Fault · cannot replan (60011)</span>") {
		t.Error("over the threshold the line must show the fleet's reason")
	}
	if !strings.Contains(html, "chip-warn") || !strings.Contains(html, "faulted:") {
		t.Error("a notice fault must raise the faulted chip")
	}
	if !strings.Contains(html, "gives up in") {
		t.Error("over the threshold the grace countdown must be shown")
	}
}

// The wording rule, at the surface an operator actually reads.
func TestOrdersPage_ALiveFaultNeverSaysFail(t *testing.T) {
	t.Parallel()
	for _, elapsed := range []time.Duration{14 * time.Second, 3 * time.Minute} {
		html, _ := faultRow(t, elapsed, protocol.TermRef{Node: "ALN_003"})
		// Scope to the fault line: the page's own markup legitimately contains
		// "failed" elsewhere (the status filter pill, the badge namespace).
		i := strings.Index(html, "orders-reason")
		if i < 0 {
			t.Fatalf("elapsed=%s: no fault line rendered", elapsed)
		}
		end := strings.Index(html[i:], "</td>")
		if end < 0 {
			end = len(html) - i
		}
		cell := strings.ToLower(html[i : i+end])
		if strings.Contains(cell, "fail") {
			t.Errorf("elapsed=%s: the fault line says \"fail\" about a live order: %s", elapsed, cell)
		}
	}
}

// A faulted order whose history row could not be read still renders its
// sentence — less information rather than a blank cell or a 2026-year duration.
func TestOrdersPage_FaultWithNoClockStillRendersItsSentence(t *testing.T) {
	t.Parallel()
	line := protocol.BuildFaultLine(protocol.TermRef{}, time.Time{}, time.Time{},
		time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC), 60*time.Second)

	order := &domain.Order{ID: 4712, EdgeUUID: "e-4712", StationID: "plant-a.line-1", Status: protocol.StatusFaulted}
	namer := &fakeNamer{byUID: map[string]string{"plant-a.line-1": "SPRINGFIELD / LINE 1"}}
	html := renderPageWithNamer(t, "orders.html", namer, map[string]any{
		"Page":       "orders",
		"Orders":     []*domain.Order{order},
		"FaultLines": map[int64]template.HTML{order.ID: template.HTML(line.HTML())},
	})

	if !strings.Contains(html, "Replanning") {
		t.Error("a fault with no readable clock must still say what it is")
	}
	if strings.Contains(html, "data-since") {
		t.Error("no clock must be rendered when the faulted row could not be read")
	}
}

// The Faulted filter pill exists — the status had no way to be filtered for.
func TestOrdersPage_HasAFaultedFilterPill(t *testing.T) {
	t.Parallel()
	namer := &fakeNamer{byUID: map[string]string{}}
	html := renderPageWithNamer(t, "orders.html", namer, map[string]any{
		"Page": "orders", "Orders": []*domain.Order{},
	})
	if !strings.Contains(html, `/orders?status=faulted`) {
		t.Error("the status filter row must offer Faulted")
	}
}

// TestOrderStatusFilters_CoverEveryStatusCoreWrites: the orders board has a
// chip for every status an order can hold, and only for those.
//
// The board had none for staged, cancelled or skipped (U4 audit, 2026-10-05):
// the badge was in the table and there was no way to narrow the board to it. A
// gate-staged parent sits at `staged` for the whole of a dig it owns, so a
// reshuffle in progress could be found by no chip at all.
//
// The two statuses left out have no writer in Core. `submitted`: no
// shingo-core code writes it. `acknowledged`: LifecycleService.Acknowledge has
// one caller (engine/wiring_vendor_status.go), an arm its own comment records as
// dead because fleet.MapState never yields it. Expiry: when Core writes either,
// it moves out of this list and gets a chip.
func TestOrderStatusFilters_CoverEveryStatusCoreWrites(t *testing.T) {
	t.Parallel()
	noCoreWriter := map[protocol.Status]bool{
		protocol.StatusSubmitted:    true,
		protocol.StatusAcknowledged: true,
	}
	chips := map[protocol.Status]int{}
	for _, f := range orderStatusFilters {
		chips[f.Status]++
		if f.Label == "" {
			t.Errorf("the %s chip has no label", f.Status)
		}
	}
	known := map[protocol.Status]bool{}
	for _, s := range protocol.AllStatuses() {
		known[s] = true
		switch n := chips[s]; {
		case noCoreWriter[s] && n > 0:
			t.Errorf("%s has a chip but Core never writes it; if it does now, drop it from noCoreWriter", s)
		case !noCoreWriter[s] && n == 0:
			t.Errorf("%s is a status an order can hold and the board has no chip for it", s)
		case n > 1:
			t.Errorf("%s has %d chips", s, n)
		}
	}
	for s := range chips {
		if !known[s] {
			t.Errorf("chip %q is not a protocol status", s)
		}
	}

	namer := &fakeNamer{byUID: map[string]string{}}
	html := renderPageWithNamer(t, "orders.html", namer, map[string]any{
		"Page": "orders", "Orders": []*domain.Order{},
	})
	for _, f := range orderStatusFilters {
		if !strings.Contains(html, `href="/orders?status=`+string(f.Status)+`"`) {
			t.Errorf("the %s chip is in the list but not on the page", f.Status)
		}
	}
}
