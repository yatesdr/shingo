package protocol

import (
	"fmt"
	"time"
)

// FormatDuration renders a duration compound, never as decimal hours, at the
// precision the measurement supports.
//
// Precision ladder, from docs/ui-style-guide.md: whole seconds under ten
// minutes, whole minutes above. The durations this renders are differences
// between two service clocks that are not synchronised to the millisecond, so
// sub-second digits would assert an accuracy the source cannot supply.
//
// A DAYS TIER WAS ADDED FOR 5.11 and it is a fix rather than an extension. The
// stale-binding candidates on /material-flags run to weeks — the longest binding
// in the Springfield dump is 22.99 days — and the hours tier rendered that as
// "551h 26m", which is compound, is not decimal hours, and is still a number
// nobody converts in their head. The ladder's own rule ("whole seconds under ten
// minutes, whole minutes above") generalises: at a day the minutes stop carrying
// anything a reader acts on, so the tier is days plus whole hours. Below 24h
// nothing changes.
//
// IT LIVES IN protocol RATHER THAN www because the fault sentence
// (FormatFaultSentence) is rendered here and crosses to the Edge, and a second
// Go ladder rendering the same concept differently is exactly the drift the
// style guide exists to prevent. www.FormatDuration delegates to it and its
// output is unchanged.
func FormatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	const day = 24 * time.Hour
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < 10*time.Minute:
		// Compound, zero-padded seconds so the column stays aligned under
		// tabular-nums — "4m 07s", not "4m 7s".
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < day:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %02dh", int(d/day), int(d.Hours())%24)
	}
}
