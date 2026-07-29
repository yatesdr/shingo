package www

import (
	"fmt"
	"time"

	"shingo/protocol"
	"shingocore/config"
	"shingocore/domain"
)

// demand_episode_detail_view.go — origin-indexed forensics (Stage 5.12, N2).
//
// ONE DEMAND, EVERY ORDER IT SPAWNED. The list page answers "which demands cost
// the most"; this answers "what did THIS one actually do", which is the question
// an engineer has when a row on that page looks wrong. Each child links to its
// existing mission detail, so the transitions, legs and logs are one click away
// rather than duplicated here — /missions/{orderID} already renders them and a
// second copy would be a second thing to keep true.
//
// Same discipline as demand_episodes_view.go, and it applies harder here:
//
//  1. EVERYTHING IS A PURE FUNCTION OF (episode, children, read outcome, clock,
//     constants). No database, no time.Now, no template.
//
//  2. THE THREE ABSENCE STATES STAY APART. Reused verbatim — Cell, Value, NoData
//     and NA are defined once in demand_episodes_view.go and not re-invented,
//     because two implementations of "render nothing" is how two of the three
//     collapse into each other.
//
//  3. A READ FAILURE IS NOT AN EMPTY RESULT, AND ON A DETAIL PAGE THAT IS THE
//     WHOLE RISK. An episode with zero orders against it is the worst thing the
//     demand grain can show — a place asked for material and nothing was ever
//     created. An episode whose child query failed looks EXACTLY THE SAME if the
//     error is dropped: an empty table under a real demand. One sends someone to
//     the floor, the other sends them to the wrong floor. BuildEpisodeDetail
//     takes the read error as a PARAMETER so it cannot be lost on the way in.
//
//  4. NO NEW DISPLAY CONSTANT. Every threshold this page renders against
//     (worry, concern, ramp steps, the minimum denominator) already exists in
//     config.DisplayConfig with a provenance record. The only number this page
//     introduces is a query cap, which is not a display constant in the
//     provenance sense — see childOrderLimit in handlers_demand_episode_detail.go.

// ChildOrderRow is one order this episode spawned, rendered.
//
// The template prints these fields and makes no decisions, same contract as
// EpisodeRow.
type ChildOrderRow struct {
	OrderID int64

	// MissionHref is the existing per-order detail page. Built here rather than
	// in the template so the one place that knows the route shape is Go, and so a
	// route rename fails a test instead of producing a dead link nobody clicks
	// until they need it.
	MissionHref string

	EdgeUUID  string
	OrderType string
	Status    string

	// Terminal says the order has no outgoing transitions left. It is what makes
	// "no completion time" readable: on a live order that is a question not yet
	// asked, on a finished one it is a missing answer.
	Terminal bool

	// OriginClass is the attribution stamped on the order AT CREATE TIME, printed
	// rather than assumed. Every order reachable from this page was found BY its
	// origin_id, so it should read `attached`; anything else means the two columns
	// disagree about the same order, which is a finding and not a display detail.
	OriginClass      string
	OriginClassLabel string
	// OriginClassDisagrees flags exactly that: found by origin_id, but not
	// classed as attached.
	OriginClassDisagrees bool

	SourceNode   string
	DeliveryNode string
	PayloadCode  string

	// Robot carries all three states. See buildChildOrderRow.
	Robot Cell

	CreatedAt time.Time

	// SinceOpen is how long after the demand opened this order was created — the
	// column that makes the page a timeline rather than a list. Signed, because a
	// negative offset is a real thing that can happen and rounding it to zero
	// would hide it.
	SinceOpen Cell

	// Took is the order's own created→completed duration. NA while it is still
	// running, NoData when it finished without recording one.
	Took Cell

	ErrorDetail string
}

// EpisodeDetail is one episode's header plus its children.
//
// The header fields are COPIED OUT of the list page's row builder rather than
// embedding it, deliberately: when the child read fails, the row builder's
// order-count and ratio fields hold values computed from a count of zero that
// nobody measured. Copying only the child-count-INDEPENDENT fields makes those
// two unreachable from the template, so the lie is not merely unrendered — it is
// not present.
type EpisodeDetail struct {
	OriginID   string
	EpisodeKey string
	Kind       string
	KindLabel  string
	Direction  string
	Station    string
	Payload    string
	CoreNode   string
	ProcessID  string

	// Trigger and TriggerRef are what asked. Absent on Core-minted threshold
	// episodes, which are not triggered by anything — a monitor noticed a level.
	Trigger    string
	TriggerRef Cell

	OpenedAt time.Time
	ClosedAt *time.Time
	Open     bool

	Duration     time.Duration
	DurationText string
	Band         DurationBand
	BandLabel    string
	RampStep     int

	OpenedTotal   int
	Threshold     Cell
	Rerequests    int
	Discretionary bool

	// Orders is the child count AS A CELL, which is the difference between this
	// page and the list. On the list the count is always measured, because a
	// failed list query takes the whole page. Here it is measured OR unknown, and
	// unknown must not print as zero.
	Orders Cell

	Expected Cell
	Ratio    Cell

	CloseReason Cell
	ClosedBy    Cell

	Children []ChildOrderRow

	// ChildrenError is the child read's failure, verbatim. When it is non-empty
	// the page must NOT render an empty-state — there is no empty result to
	// report, only an unread one.
	ChildrenError string

	// ChildrenTruncated says the cap bit and more orders exist than are listed.
	ChildrenTruncated bool
	ChildrenLimit     int
}

// BuildEpisodeDetail renders one episode and its children into display form.
//
// childErr IS A PARAMETER, NOT AN EARLY RETURN AT THE CALL SITE. A handler that
// bailed on the child error would lose the header — and the header is exactly
// what tells you whether the missing children matter, because it carries the
// expectation. So the page renders with the header intact and says plainly that
// the child list is unknown.
//
// `now` is a parameter for the same reason it is one on BuildEpisodeRow: an open
// episode's duration depends on it, and a function that reads the clock cannot
// be tested at a boundary.
func BuildEpisodeDetail(
	o domain.DemandOrigin,
	children []*domain.Order,
	childrenTruncated bool,
	childErr error,
	now time.Time,
	c config.DisplayConfig,
) EpisodeDetail {
	// The header reuses the LIST's row builder for everything that does not
	// depend on the child count, so the two surfaces cannot drift about a
	// duration, a band, a close reason or a denominator. The count handed in
	// here is used ONLY for the ratio, and only in the arm below where it was
	// actually read.
	row := BuildEpisodeRow(domain.DemandEpisode{DemandOrigin: o, Children: len(children)}, now, c)

	d := EpisodeDetail{
		OriginID:      row.OriginID,
		EpisodeKey:    row.EpisodeKey,
		Kind:          row.Kind,
		KindLabel:     row.KindLabel,
		Direction:     row.Direction,
		Station:       row.Station,
		Payload:       row.Payload,
		CoreNode:      row.CoreNode,
		ProcessID:     o.ProcessID,
		Trigger:       o.Trigger,
		OpenedAt:      row.OpenedAt,
		ClosedAt:      o.ClosedAt,
		Open:          row.Open,
		Duration:      row.Duration,
		DurationText:  row.DurationText,
		Band:          row.Band,
		BandLabel:     row.BandLabel,
		RampStep:      row.RampStep,
		OpenedTotal:   o.OpenedTotal,
		Rerequests:    o.RerequestCount,
		Discretionary: o.Discretionary,
		Expected:      row.Expected,
		CloseReason:   row.CloseReason,
		ClosedBy:      row.ClosedBy,
	}

	// trigger_ref is the Edge-side identity of whatever asked — a claim id, a
	// changeover id. A Core-minted threshold episode has none because nothing
	// asked: a monitor noticed a level crossed. That is not-applicable, not
	// missing.
	if o.TriggerRef != "" {
		d.TriggerRef = Value(o.TriggerRef)
	} else if o.Kind == "threshold" {
		d.TriggerRef = NA("Core-minted from a threshold crossing — nothing asked, so there is no reference")
	} else {
		d.TriggerRef = NoData("no trigger reference recorded for this episode")
	}

	// threshold is the level the monitor was watching. Zero is meaningful only
	// for the kind that has one; on a cell or changeover episode there is no
	// threshold at all and a printed 0 would read as "the threshold is zero".
	if o.Kind == "threshold" {
		d.Threshold = Value(FormatCount(o.Threshold))
	} else {
		d.Threshold = NA("not a threshold episode — no level was being watched")
	}

	// ── The rule this page exists for ────────────────────────────────────────
	if childErr != nil {
		// NOT ZERO. The number of orders this demand caused is UNKNOWN, and the
		// most reassuring rendering available — an empty table — is the one thing
		// that must not happen here.
		d.ChildrenError = childErr.Error()
		d.Children = nil
		d.Orders = NoData("the child-order query failed, so the number of orders this demand " +
			"caused is unknown. This is NOT zero orders — nothing has been counted.")
		d.Ratio = NoData("no ratio: the numerator could not be read")
		return d
	}

	d.Orders = Value(FormatCount(len(children)))
	d.Ratio = row.Ratio
	d.ChildrenTruncated = childrenTruncated

	d.Children = make([]ChildOrderRow, 0, len(children))
	for _, child := range children {
		d.Children = append(d.Children, buildChildOrderRow(child, o.OpenedAt))
	}
	return d
}

// buildChildOrderRow renders one child order relative to the episode that
// caused it.
func buildChildOrderRow(o *domain.Order, episodeOpened time.Time) ChildOrderRow {
	status := string(o.Status)
	terminal := protocol.IsTerminal(o.Status)

	r := ChildOrderRow{
		OrderID:      o.ID,
		MissionHref:  MissionHref(o.ID),
		EdgeUUID:     o.EdgeUUID,
		OrderType:    string(o.OrderType),
		Status:       status,
		Terminal:     terminal,
		OriginClass:  o.OriginClass,
		SourceNode:   o.SourceNode,
		DeliveryNode: o.DeliveryNode,
		PayloadCode:  o.PayloadCode,
		CreatedAt:    o.CreatedAt,
		ErrorDetail:  o.ErrorDetail,
	}

	r.OriginClassLabel = OriginClassLabel(o.OriginClass)
	// Every order on this page was found BY its origin_id. If its origin_class
	// says anything other than "attached", the two columns disagree about the
	// same row, and that is worth showing rather than normalising away.
	r.OriginClassDisagrees = o.OriginClass != protocol.OriginClassAttached

	// ── Robot: all three states, and the discriminator is a FACT, not a guess ──
	//
	// vendor_order_id is written when the order is handed to the fleet. Keying on
	// it rather than on the status set means the answer stays right for an order
	// that has already run to completion, where "is it currently vendor-active"
	// is false and tells you nothing.
	switch {
	case o.RobotID != "":
		r.Robot = Value(o.RobotID)
	case o.VendorOrderID == "":
		r.Robot = NA("never handed to the fleet — there was no robot to assign")
	default:
		r.Robot = NoData(fmt.Sprintf(
			"handed to the fleet as %s, but no robot was recorded against it", o.VendorOrderID))
	}

	// ── Offset from the demand opening ───────────────────────────────────────
	//
	// SIGNED. An order created before its own episode opened is a real
	// possibility — Edge and Core clocks are independent, and an order stamped
	// forward from a parent can carry an origin minted after it. Clamping the
	// negative to zero, which FormatDuration does for display durations, would
	// erase exactly the anomaly someone opened this page to find.
	switch delta := o.CreatedAt.Sub(episodeOpened); {
	case episodeOpened.IsZero():
		r.SinceOpen = NoData("the episode has no opening time to measure from")
	case delta < 0:
		r.SinceOpen = Cell{
			Kind:  CellValue,
			Text:  "−" + FormatDuration(-delta),
			Title: "created BEFORE the episode opened — clock skew between services, or an origin stamped forward from a parent",
		}
	default:
		r.SinceOpen = Value("+" + FormatDuration(delta))
	}

	// ── How long the order itself took ───────────────────────────────────────
	switch {
	case o.CompletedAt != nil:
		r.Took = Value(FormatDuration(o.CompletedAt.Sub(o.CreatedAt)))
	case !terminal:
		// A question not yet asked. The order is still running; it has not failed
		// to record a completion, it has not completed.
		r.Took = NA("still running — it has not finished yet")
	default:
		// It reached a status with no way out and recorded no completion time.
		// That is a missing answer, and a different fact from the one above.
		r.Took = NoData(fmt.Sprintf(
			"reached %s with no completion time recorded", status))
	}

	return r
}

// MissionHref is the route to one order's existing mission-detail page.
//
// Exported and given its own function so the route shape has ONE home that a
// test can pin against the router. A link built inline in a template is a link
// no test can see, and this page's entire value is that the child rows are
// clickable — a dead href turns the forensics surface back into a list of
// numbers.
func MissionHref(orderID int64) string {
	return fmt.Sprintf("/missions/%d", orderID)
}

// EpisodeDetailHref is the route to one episode's detail page. Same reasoning;
// the list page links here.
func EpisodeDetailHref(originID string) string {
	return "/demand-episodes/" + originID
}

// OriginClassLabel renders orders.origin_class for display.
//
// SAME DEFAULT RULE AS CloseReasonLabel AND FOR THE SAME REASON: an unrecognised
// value renders as itself. This vocabulary is three values today and it is the
// subject of an open plan item (5.7, the orphan lane), so it is a vocabulary
// actively expected to move. A default of "Unknown" would turn its next addition
// into a silent data-loss bug on the one page built to notice.
func OriginClassLabel(class string) string {
	switch class {
	case "":
		// The column is NOT NULL DEFAULT '', so empty is what a row written before
		// the grain existed carries. It is not a class; it is the absence of one.
		return ""
	case protocol.OriginClassAttached:
		return "Attached"
	case protocol.OriginClassNoDemand:
		return "No demand"
	case protocol.OriginClassOrphan:
		return "Orphan"
	default:
		return humanizeUnknown(class)
	}
}
