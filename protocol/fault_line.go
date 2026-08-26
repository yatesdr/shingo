package protocol

import (
	"html"
	"strconv"
	"strings"
	"time"
)

// FaultLine is the rendered fault sentence for one order, in the pieces a
// template needs. Core's board, the order modal, the robots page and the Edge
// board all build their markup from this, so the four cannot drift into saying
// different things about the same order.
//
// It carries BOTH wordings — the under-threshold one and the over-threshold one
// — because the page outlives the render. A faulted order sits on a board for
// minutes; the server decides which wording is correct at render time, and the
// browser swaps to the other one when the clock crosses the threshold the
// server also sent. Neither end owns the rule twice: the server owns the
// threshold and the words, the client owns only the comparison.
type FaultLine struct {
	// Under and Over are the two sentences: "Replanning", and "Fault" with the
	// fleet's reason when there is one.
	Under string
	Over  string
	// Notice is which of the two is correct right now, server-decided.
	Notice bool
	// SinceAttr and DeadlineAttr are RFC3339 instants for data-since /
	// data-until. Empty when not known.
	SinceAttr    string
	DeadlineAttr string
	// ElapsedText and RemainingText are the durations as they stood at render
	// time, so the line is complete before any script runs and stays readable
	// if none does.
	ElapsedText   string
	RemainingText string
	// NoticeAfterS is the threshold, echoed onto the node for the client's
	// comparison. Zero means no threshold was supplied and the client leaves
	// the server's wording alone.
	NoticeAfterS int
}

// BuildFaultLine assembles the fault line for a faulted order.
//
// since may be zero (the faulted row could not be read), in which case the line
// still renders its sentence and simply has no clock — less information rather
// than a duration measured from the zero time.
func BuildFaultLine(ref TermRef, since, deadline, now time.Time, noticeAfter time.Duration) FaultLine {
	fl := FaultLine{
		Under:        FormatFaultSentence(FaultPhaseLive, ref, since, now, false),
		Over:         FormatFaultSentence(FaultPhaseLive, ref, since, now, true),
		NoticeAfterS: int(noticeAfter.Seconds()),
	}
	if since.IsZero() {
		return fl
	}
	elapsed := faultElapsed(since, now)
	fl.Notice = noticeAfter > 0 && elapsed >= noticeAfter
	fl.SinceAttr = since.UTC().Format(time.RFC3339)
	fl.ElapsedText = FormatDuration(elapsed)
	if !deadline.IsZero() && deadline.After(now) {
		fl.DeadlineAttr = deadline.UTC().Format(time.RFC3339)
		fl.RemainingText = FormatDuration(deadline.Sub(now).Round(time.Second))
	}
	return fl
}

// Sentence is the wording correct at render time.
func (f FaultLine) Sentence() string {
	if f.Notice {
		return f.Over
	}
	return f.Under
}

// HTML renders the line as the markup every fault surface uses. Returned as a
// string; callers wrap it in their own template.HTML, since protocol must not
// depend on html/template's type just to hand back a fragment.
//
// The markup is the contract shared/utils.js installLiveDurations reads:
// data-since ticks the elapsed, data-until counts down to the give-up, and
// data-notice-after tells the client when to swap the word beside them. The
// grace countdown carries data-notice-only-over because it is noise beside a
// 14-second replan and the point beside a three-minute fault.
//
// Every interpolated value is escaped. VendorDesc is fleet-supplied text that
// reaches this function unmodified by design, so it is the one string here an
// attacker could influence.
func (f FaultLine) HTML() string {
	var b strings.Builder
	b.WriteString(`<span data-notice-word data-notice-under="`)
	b.WriteString(html.EscapeString(f.Under))
	b.WriteString(`" data-notice-over="`)
	b.WriteString(html.EscapeString(f.Over))
	b.WriteString(`">`)
	b.WriteString(html.EscapeString(f.Sentence()))
	b.WriteString(`</span>`)

	if f.SinceAttr != "" {
		b.WriteString(` · <span class="tnum" data-since="`)
		b.WriteString(html.EscapeString(f.SinceAttr))
		b.WriteString(`"`)
		if f.NoticeAfterS > 0 {
			b.WriteString(` data-notice-after="`)
			b.WriteString(strconv.Itoa(f.NoticeAfterS))
			b.WriteString(`"`)
		}
		b.WriteString(`>`)
		b.WriteString(html.EscapeString(f.ElapsedText))
		b.WriteString(`</span>`)
	}

	if f.DeadlineAttr != "" {
		b.WriteString(`<span data-notice-only-over`)
		if !f.Notice {
			b.WriteString(` hidden`)
		}
		b.WriteString(`> · gives up in <span class="tnum" data-until="`)
		b.WriteString(html.EscapeString(f.DeadlineAttr))
		b.WriteString(`">`)
		b.WriteString(html.EscapeString(f.RemainingText))
		b.WriteString(`</span></span>`)
	}
	return b.String()
}
