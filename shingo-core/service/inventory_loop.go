package service

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"shingo/protocol"
	"shingocore/store/inventory"
	"shingocore/store/loaders"
	"shingocore/store/orders"
)

// inventory_loop.go — the /inventory page's read: a row per (loader position,
// part) the threshold monitor watches (supermarket positions), plus a row per
// claim of the running styles at a line, consume and produce (Lines; a line
// is an Edge process).
//
// EIGHT STATEMENTS, NONE PER ROW: the monitored bindings (LoopBindings), the
// in-loop sum (SystemUOPForPayload, two), the bins behind the sums (LoopBins),
// the running claims (LoopClaims), sourceability's node rates (LoopNodeRates),
// the live orders not yet delivered (LoopLiveOrders) and the calculator's
// inputs for every part at once (LeadTimesByPayload).

// Loop row states (a supermarket position's).
const (
	LoopBelow             = "below"               // in-loop UoP under the UoP threshold
	LoopAbove             = "above"               // at or over it
	LoopRefillsWhenEmpty  = "refills_when_empty"  // a one-bin home at a threshold of a pack or more
	LoopAboveHomeCapacity = "above_home_capacity" // a threshold over what a multi-bin home can hold
)

// LoopLeadDays is the calculator's default window (CalculateDays), so the
// page's suggestion and the modal's agree for the same cycle.
const LoopLeadDays = 14

// LoopView is the /inventory page's one read.
type LoopView struct {
	Rows     []LoopRow  `json:"rows"`
	Doubt    []DoubtBin `json:"doubt"`
	LeadDays int        `json:"lead_days"`
	Safety   float64    `json:"safety"`
}

// LoopRow is one row of either table. Pointers are absent values (rule 4: no
// data is not zero).
type LoopRow struct {
	Key  string `json:"key"`
	Kind string `json:"kind"` // "loader" | "line"
	// Group is a position's lines (the processes whose running style consumes
	// the part), or a line row's process.
	Group          string `json:"group"`
	PayloadCode    string `json:"payload_code"`
	Description    string `json:"description,omitempty"`
	Node           string `json:"node"`
	LoaderID       int64  `json:"loader_id,omitempty"`
	LoaderName     string `json:"loader_name,omitempty"`
	Layout         string `json:"layout,omitempty"`
	PositionNodeID int64  `json:"position_node_id,omitempty"`
	Pack           int    `json:"pack"`
	State          string `json:"state"`

	// Loader rows: the in-loop sum and its two terms, and the UoP threshold.
	InLoop    *int `json:"in_loop,omitempty"`
	BinUOP    int  `json:"bin_uop"`
	BucketUOP int  `json:"bucket_uop"`
	Threshold int  `json:"threshold"`
	// HomeBins is how many bins the position (or a shared loader's windows)
	// can hold; HomeCapacity that times the pack.
	HomeBins     int `json:"home_bins"`
	HomeCapacity int `json:"home_capacity"`

	// Line rows: the claim's role, the UoP in the bin at the line (absent when
	// no bin of the part stands there) and the claim's cell reorder point
	// (read-only; the Edge sets it).
	Role         string `json:"role,omitempty"`
	InBin        *int   `json:"in_bin,omitempty"`
	ReorderPoint int    `json:"reorder_point"`

	// CoverSeconds is the time left: a position's time to empty; a line's
	// until its bin is empty (consume) or full (produce). Absent when the
	// cycle is unknown.
	CycleSeconds *float64 `json:"cycle_seconds"`
	CoverSeconds *float64 `json:"cover_seconds"`
	LeadSeconds  *float64 `json:"lead_seconds"`

	Inputs     *LoopInputs `json:"inputs,omitempty"`
	Suggested  *int        `json:"suggested"`
	Confidence string      `json:"confidence,omitempty"`
	// CompletedL1 is the position's completed threshold L1 cycles in the
	// calculator's window. Until it is 1 the row carries no suggestion: a lead
	// time from an order that never completed is not a lead time.
	CompletedL1 int `json:"completed_l1"`
	// Station names the Edge whose process setup holds a line's cell reorder
	// point (the mirror's display name, else its station id).
	Station string `json:"station,omitempty"`

	// Coming is the bins on the way: to a position, orders to it queued or in
	// transit, not yet delivered; to a line, live orders not yet delivered
	// that set a bin down at its node (protocol.OrderPlacesBinAt): a full of
	// the part at a consume spot, an empty at a produce spot.
	Coming     int         `json:"coming"`
	OffHome    []OffHome   `json:"off_home,omitempty"`
	Provenance *Provenance `json:"provenance"`

	// Beyond says the row is past its threshold: a position's in-loop UoP at
	// or under its UoP threshold, or for a home that refills when it empties,
	// that home empty; a consume line's bin at or under its cell reorder
	// point; a produce line's bin at or over the level its cell decides at
	// (protocol.ClaimDemandLevel), or over its pack.
	Beyond bool `json:"beyond"`
	// Look says the row shows in the table's default view: beyond, above home
	// capacity, a doubtful count, or a bin on the way. The rest fold.
	Look bool `json:"look"`
}

// LoopInputs are the calculator's inputs for the row's part, as the modal
// shows them.
type LoopInputs struct {
	L1QueueSeconds      float64 `json:"l1_queue_seconds"`
	L1TransitSeconds    float64 `json:"l1_transit_seconds"`
	L2LoadSeconds       float64 `json:"l2_load_seconds"`
	L2TransitSeconds    float64 `json:"l2_transit_seconds"`
	MarketToCellSeconds float64 `json:"market_to_cell_seconds"`
	SamplesL1           int     `json:"samples_l1"`
	SamplesRetrieve     int     `json:"samples_retrieve"`
}

// OffHome is the part's bins standing somewhere other than the row's position.
type OffHome struct {
	Node string `json:"node"` // "" = on a robot / at no node
	Bins int    `json:"bins"`
}

// Provenance is the last recorded change of a UoP threshold (audit_log).
type Provenance struct {
	Value   int       `json:"value"`
	Old     *int      `json:"old"`
	Source  string    `json:"source"` // manual | calculated
	By      string    `json:"by"`
	At      time.Time `json:"at"`
	Current bool      `json:"current"` // the recorded value is the one in force
}

// DoubtBin is a bin whose count makes its part's sum untrustworthy: below zero,
// or standing in the loop with its contents not confirmed (manifest_confirmed
// false) anywhere but a produce seat, where the bin is filling: its count climbs
// from the Edge's ticks and is confirmed when the bin is released.
type DoubtBin struct {
	BinID       int64  `json:"bin_id"`
	Label       string `json:"label"`
	Node        string `json:"node"`
	PayloadCode string `json:"payload_code"`
	UOP         int    `json:"uop"`
	Reason      string `json:"reason"` // below_zero | unconfirmed
}

// Loop reads the page. now is Core's clock (the calculator's window end);
// rateWindow is sourceability's rate look-back, for a line's cycle.
func (s *InventoryService) Loop(ctx context.Context, now time.Time, rateWindow time.Duration) (LoopView, error) {
	st := s.db
	leadWindow := orders.LeadTimeRange{Start: now.AddDate(0, 0, -LoopLeadDays), End: now}
	bindings, err := st.LoopBindings(leadWindow.Start, leadWindow.End)
	if err != nil {
		return LoopView{}, err
	}
	payloads := make([]string, 0, len(bindings))
	seen := map[string]bool{}
	for _, b := range bindings {
		if b.PayloadCode != "" && !seen[b.PayloadCode] {
			seen[b.PayloadCode] = true
			payloads = append(payloads, b.PayloadCode)
		}
	}
	sums, err := s.SystemUOPForPayload(ctx, payloads)
	if err != nil {
		return LoopView{}, err
	}
	bins, err := st.LoopBins(outOfLoopFilterSQL)
	if err != nil {
		return LoopView{}, err
	}
	claims, err := st.LoopClaims()
	if err != nil {
		return LoopView{}, err
	}
	rates, err := st.LoopNodeRates(rateWindow)
	if err != nil {
		return LoopView{}, err
	}
	lineRates := make(map[string]float64, len(claims))
	for _, c := range claims {
		r := rates.Consume(c.CoreNodeName, c.PayloadCode)
		if c.Role == loaders.RoleProduce {
			r = rates.Produce(c.CoreNodeName, c.PayloadCode)
		}
		lineRates[lineKey(c)] = r
	}
	live, err := st.LoopLiveOrders()
	if err != nil {
		return LoopView{}, err
	}
	leads, err := st.LeadTimesByPayload(leadWindow)
	if err != nil {
		return LoopView{}, err
	}
	return BuildLoopView(LoopInputsRead{
		Bindings: bindings, Sums: sums.Counts, Bins: bins, Claims: claims, LineRates: lineRates, Live: live, Leads: leads,
	}), nil
}

// LoopInputsRead is everything Loop read, for the pure assembly.
type LoopInputsRead struct {
	Bindings []inventory.Binding
	Sums     []PayloadSystemUOP
	Bins     []inventory.LoopBin
	Claims   []inventory.LoopClaim
	// LineRates is each claim's node rate over the rate window, UoP per
	// second, keyed by lineKey: drawn at a consume spot, made at a produce
	// spot. Absent or 0 is a line that did not run.
	LineRates map[string]float64
	Live      []inventory.LiveOrder
	Leads     map[string]orders.LeadTimeInputs
}

// BuildLoopView assembles the page from its reads. Pure.
func BuildLoopView(in LoopInputsRead) LoopView {
	safety := CalculateThresholds(ThresholdCalculatorInputs{}).SafetyApplied
	idx := indexLoop(in)
	view := LoopView{Rows: []LoopRow{}, Doubt: idx.doubt, LeadDays: LoopLeadDays, Safety: safety}
	for _, b := range in.Bindings {
		view.Rows = append(view.Rows, loaderRow(b, idx, in.Leads))
	}
	for _, c := range in.Claims {
		if c.LoaderMember {
			continue // a loader's home or window is a supermarket position, not a line
		}
		view.Rows = append(view.Rows, lineRow(c, idx, in.Live, in.LineRates[lineKey(c)]))
	}
	return view
}

// loopIndex is what the rows look up: the sums by part, the bins by part, the
// doubt line (and the parts and (node, part) pairs it names), and each part's
// consuming lines.
type loopIndex struct {
	sums        map[string]PayloadSystemUOP
	binsByPart  map[string][]inventory.LoopBin
	doubt       []DoubtBin
	doubtPart   map[string]bool
	doubtAtNode map[string]bool // "<node>|<part>"
	lines       map[string]map[string]bool
}

func indexLoop(in LoopInputsRead) loopIndex {
	idx := loopIndex{sums: map[string]PayloadSystemUOP{}, binsByPart: map[string][]inventory.LoopBin{}, doubt: []DoubtBin{},
		doubtPart: map[string]bool{}, doubtAtNode: map[string]bool{}, lines: map[string]map[string]bool{}}
	for _, c := range in.Sums {
		idx.sums[c.PayloadCode] = c
	}
	// A bin at a produce seat (a line, or a loader's window) is filling: its
	// count climbs from the Edge's ticks and is confirmed when the bin is
	// released, so an unconfirmed manifest there is the fill in progress.
	filling := map[string]bool{}
	for _, c := range in.Claims {
		if c.Role == loaders.RoleProduce {
			filling[c.CoreNodeName] = true
		}
	}
	for _, b := range in.Bins {
		idx.binsByPart[b.PayloadCode] = append(idx.binsByPart[b.PayloadCode], b)
		reason := ""
		switch {
		case b.UOP < 0:
			reason = "below_zero"
		case !b.Confirmed && !filling[b.Node]:
			reason = "unconfirmed"
		}
		if reason != "" {
			idx.doubt = append(idx.doubt, DoubtBin{BinID: b.ID, Label: b.Label, Node: b.Node, PayloadCode: b.PayloadCode, UOP: b.UOP, Reason: reason})
			idx.doubtPart[b.PayloadCode] = true
			idx.doubtAtNode[b.Node+"|"+b.PayloadCode] = true
		}
	}
	sort.SliceStable(idx.doubt, func(i, j int) bool {
		if idx.doubt[i].Node != idx.doubt[j].Node {
			return idx.doubt[i].Node < idx.doubt[j].Node
		}
		return idx.doubt[i].Label < idx.doubt[j].Label
	})
	for _, c := range in.Claims {
		if c.Role != loaders.RoleConsume {
			continue
		}
		if idx.lines[c.PayloadCode] == nil {
			idx.lines[c.PayloadCode] = map[string]bool{}
		}
		idx.lines[c.PayloadCode][c.ProcessID] = true
	}
	return idx
}

// loaderRow is one monitored (position, part).
func loaderRow(b inventory.Binding, idx loopIndex, leads map[string]orders.LeadTimeInputs) LoopRow {
	row := LoopRow{
		Key: "loader|" + b.CoreNodeName + "|" + b.PayloadCode, Kind: "loader",
		Group: groupOf(idx.lines[b.PayloadCode]), PayloadCode: b.PayloadCode, Description: b.Description,
		Node: b.CoreNodeName, LoaderID: b.LoaderID, LoaderName: b.LoaderName, Layout: b.Layout,
		PositionNodeID: b.PositionNodeID, Pack: b.Pack, Threshold: b.Threshold, Coming: b.Coming,
		CompletedL1: b.CompletedL1,
	}
	sum := idx.sums[b.PayloadCode]
	total := sum.TotalUOP
	row.InLoop, row.BinUOP, row.BucketUOP = &total, sum.BinUOP, sum.BucketUOP
	home := map[string]bool{b.CoreNodeName: true}
	row.HomeBins = 1
	if b.Layout == loaders.LayoutSharedWindow {
		row.HomeBins = max(b.Windows, 1)
		for _, m := range strings.Split(b.Members, ",") {
			if m != "" {
				home[m] = true
			}
		}
	}
	row.HomeCapacity = row.HomeBins * b.Pack
	row.State = loaderState(b, total, row.HomeCapacity)
	row.OffHome = offHome(idx.binsByPart[b.PayloadCode], home)
	row.Provenance = provenanceOf(b)
	lt, ok := leads[b.PayloadCode]
	if ok && lt.RatePerSec > 0 {
		cyc := 1 / lt.RatePerSec
		row.CycleSeconds = &cyc
	}
	fillTimes(&row, lt, ok, float64(max(total, 0)))
	if b.CompletedL1 == 0 {
		row.Suggested, row.Confidence = nil, "" // no completed L1 cycle at the position: no suggestion
	}
	// Past the threshold. A home that refills when it empties (one bin, a
	// threshold of a pack or more) sits at or under its threshold whenever its
	// one bin is all the part has, so the threshold says nothing about it: it
	// is past when its home is empty, no UoP of the part there.
	if row.State == LoopRefillsWhenEmpty {
		atHome := 0
		for _, x := range idx.binsByPart[b.PayloadCode] {
			if home[x.Node] {
				atHome += x.UOP
			}
		}
		row.Beyond = atHome <= 0
	} else {
		row.Beyond = total <= b.Threshold
	}
	row.Look = row.Beyond || row.State == LoopAboveHomeCapacity || idx.doubtPart[b.PayloadCode] || row.Coming > 0
	return row
}

// lineKey names a claim's row and its rate: process, node, part, role.
func lineKey(c inventory.LoopClaim) string {
	return "line|" + c.ProcessID + "|" + c.CoreNodeName + "|" + c.PayloadCode + "|" + c.Role
}

// lineRow is one claim of a running style at a line, consume or produce: the
// bin at the line against its pack, the time left at the node's rate, the cell
// reorder point and the bins on the way. Listed, not watched: the threshold
// monitor watches loader bindings only, and the cell calls its own swaps.
func lineRow(c inventory.LoopClaim, idx loopIndex, live []inventory.LiveOrder, rate float64) LoopRow {
	row := LoopRow{
		Key: lineKey(c), Kind: "line",
		Group: c.ProcessID, PayloadCode: c.PayloadCode, Node: c.CoreNodeName, Pack: c.Pack, Role: c.Role,
		ReorderPoint: c.ReorderPoint, Station: c.Station, Coming: lineComing(c, live),
	}
	fill, staged := 0, false
	for _, b := range idx.binsByPart[c.PayloadCode] {
		if b.Node == c.CoreNodeName {
			fill += b.UOP
			staged = true
		}
	}
	if staged {
		row.InBin = &fill
	}
	if rate > 0 {
		cyc := 1 / rate
		row.CycleSeconds = &cyc
	}
	zero := 0.0
	if c.Role == loaders.RoleProduce {
		// Until the bin is full. No bin, or no pack: nothing to fill. A full
		// bin has nothing left whatever the rate: it stops the line, so its
		// rate reads nothing while it waits for its swap.
		if staged && c.Pack > 0 {
			switch left := max(c.Pack-fill, 0); {
			case left == 0:
				row.CoverSeconds = &zero
			case row.CycleSeconds != nil:
				cover := float64(left) * *row.CycleSeconds
				row.CoverSeconds = &cover
			}
		}
		level := protocol.ClaimDemandLevel(protocol.ClaimRoleProduce, c.ReorderPoint, c.Pack)
		row.Beyond = staged && ((level > 0 && fill >= level) || (c.Pack > 0 && fill > c.Pack))
	} else {
		// Until the bin is empty. No bin at a consume spot, or an empty one, is
		// nothing left whatever the rate, and at or under any reorder point.
		switch {
		case fill <= 0:
			row.CoverSeconds = &zero
		case row.CycleSeconds != nil:
			cover := float64(fill) * *row.CycleSeconds
			row.CoverSeconds = &cover
		}
		row.Beyond = fill <= c.ReorderPoint
	}
	row.Look = row.Beyond || idx.doubtAtNode[c.CoreNodeName+"|"+c.PayloadCode] || row.Coming > 0
	return row
}

// lineComing counts the live orders that set a bin down at the claim's node
// (protocol.OrderPlacesBinAt, the one spelling): at a consume spot a full of
// the claim's part; at a produce spot an empty, whatever part it is meant for.
// A complex order whose steps do not decode is not counted: nothing says where
// it sets its bin down.
func lineComing(c inventory.LoopClaim, live []inventory.LiveOrder) int {
	n := 0
	for _, o := range live {
		if c.Role != loaders.RoleProduce && o.PayloadCode != c.PayloadCode {
			continue
		}
		var steps []protocol.ComplexOrderStep
		if o.OrderType == protocol.OrderTypeComplex && o.StepsJSON != "" {
			if err := json.Unmarshal([]byte(o.StepsJSON), &steps); err != nil {
				continue
			}
		}
		if protocol.OrderPlacesBinAt(o.OrderType, steps, o.DeliveryNode, c.CoreNodeName) {
			n++
		}
	}
	return n
}

// fillTimes derives a position's cover (on hand x cycle, when the cycle is
// known: time to empty), and from the part's calculator inputs (haveLeads) the
// lead time and the suggestion the calculator opens with.
func fillTimes(row *LoopRow, lt orders.LeadTimeInputs, haveLeads bool, onHand float64) {
	if row.CycleSeconds != nil {
		cover := onHand * *row.CycleSeconds
		row.CoverSeconds = &cover
	}
	if !haveLeads {
		return
	}
	row.Inputs = &LoopInputs{
		L1QueueSeconds: lt.L1QueueSeconds, L1TransitSeconds: lt.L1TransitSeconds, L2LoadSeconds: lt.L2LoadSeconds,
		MarketToCellSeconds: lt.MarketToCellSeconds, SamplesL1: lt.SamplesL1, SamplesRetrieve: lt.SamplesRetrieve,
	}
	lead := lt.L1QueueSeconds + lt.L1TransitSeconds + lt.L2LoadSeconds // + L2 transit, 0: no store orders exist
	if lead <= 0 || row.CoverSeconds == nil {
		if lead > 0 {
			row.LeadSeconds = &lead
		}
		return
	}
	row.LeadSeconds = &lead
	out := CalculateThresholds(ThresholdCalculatorInputs{
		CycleSeconds: *row.CycleSeconds, L1QueueSeconds: lt.L1QueueSeconds, L1TransitSeconds: lt.L1TransitSeconds,
		L2LoadSeconds: lt.L2LoadSeconds, MarketToCellSeconds: lt.MarketToCellSeconds,
	})
	sug := out.L1Threshold
	row.Suggested = &sug
	row.Confidence = scoreConfidence(LoopLeadDays, lt.SamplesL1, 0, lt.SamplesRetrieve)
}

// loaderState classifies a monitored row. A one-bin home at a threshold of a
// pack or more can never be met by the home alone: it refills the home when
// it empties, and is not read as below. A threshold over what a multi-bin
// home can hold is flagged and is not read as below either.
func loaderState(b inventory.Binding, inLoop, homeCapacity int) string {
	if b.Pack > 0 {
		if b.Layout == loaders.LayoutDedicatedPositions && b.Threshold >= b.Pack {
			return LoopRefillsWhenEmpty
		}
		if b.Layout == loaders.LayoutSharedWindow && b.Threshold > homeCapacity {
			return LoopAboveHomeCapacity
		}
	}
	if inLoop < b.Threshold {
		return LoopBelow
	}
	return LoopAbove
}

// groupOf names a loader row's line: the processes whose running style
// consumes the part.
func groupOf(lines map[string]bool) string {
	if len(lines) == 0 {
		return ""
	}
	names := make([]string, 0, len(lines))
	for l := range lines {
		names = append(names, l)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// offHome counts a part's bins standing anywhere but the row's home.
func offHome(bins []inventory.LoopBin, home map[string]bool) []OffHome {
	by := map[string]int{}
	var order []string
	for _, b := range bins {
		if home[b.Node] {
			continue
		}
		if _, ok := by[b.Node]; !ok {
			order = append(order, b.Node)
		}
		by[b.Node]++
	}
	sort.Strings(order)
	out := make([]OffHome, 0, len(order))
	for _, n := range order {
		out = append(out, OffHome{Node: n, Bins: by[n]})
	}
	return out
}

// provenanceOf parses the binding's last uop_threshold audit row:
// old "<payload> <n>", new "<payload> <n> <source>".
func provenanceOf(b inventory.Binding) *Provenance {
	if !b.ProvNew.Valid {
		return nil
	}
	f := strings.Fields(strings.TrimPrefix(b.ProvNew.String, b.PayloadCode+" "))
	if len(f) < 2 {
		return nil
	}
	v, err := strconv.Atoi(f[0])
	if err != nil {
		return nil
	}
	p := &Provenance{Value: v, Source: f[1], By: b.ProvActor.String, Current: v == b.Threshold}
	if b.ProvAt.Valid {
		p.At = b.ProvAt.Time
	}
	if of := strings.Fields(strings.TrimPrefix(b.ProvOld.String, b.PayloadCode+" ")); len(of) > 0 {
		if o, err := strconv.Atoi(of[0]); err == nil {
			p.Old = &o
		}
	}
	return p
}

// ThresholdAuditValues are a threshold change's audit_log old and new values.
func ThresholdAuditValues(payloadCode string, old, new int, source string) (string, string) {
	return payloadCode + " " + strconv.Itoa(old), payloadCode + " " + strconv.Itoa(new) + " " + source
}
