// catid_holds.go — counts held per CATID.
//
// A counted stroke Shingo cannot place by the PLC's part has its CART DRAW held
// in a ledger (store catid_holds); its production record still ships, under the
// point's style with the part on the row, and hourly counts it as today. What
// is held: a held tick's whole draw (node 0), and an off-plan tick's share at a
// node whose bound carrier is not the tick's style's part. Nothing here ever
// writes a seat hold (pending_uop_*), D12's one slot.
//
// A hold is released four ways, each latched before anything moves (a second
// release writes nothing) and each charging through the walk directly — never
// a re-emitted counter delta, which would count the hour twice:
//
//   - automatically, when a cutover or a switch lands the process on a style
//     whose set holds the CATID and no other changeover has started since the
//     hold opened (a cancelled one is not counted);
//   - automatically, when a cart lands at the hold's position (any position,
//     for a whole draw) while the active style runs the CATID, under the same
//     guards: the cart a forced cutover went ahead of, or the one a person
//     brought. Landing what waited, not ordering;
//   - a person's Charge, under the one style that runs the CATID;
//   - a person's Write off, which charges nothing.
//
// The landings and the Charge apply one rule. A share lands only on a cart of
// the style's part (tickRelease); a share at a cart of another part, or at no
// cart, is held again for a person. A release that would land nothing — the
// share's position deleted or not the style's, no cart of its part — is
// refused and the hold stays open, naming why (holdRows). The quantity
// charged is the quantity the latch resolved.
//
// The automatic releases add one rule a person's Charge does not: a share
// lands only when it fits the cart (decision 46). A share bigger than the
// cart's count did not come from that cart; the hold stays open for a person,
// its line saying "N held, the cart at X holds M", and Charge (with its
// preview) and Write off apply as before (decision 25: the quantity charged is
// the quantity latched).
//
// A hold that never resolves stays, with its age and its quantity.

package engine

import (
	"errors"
	"fmt"
	"log"

	"shingo/protocol/clock"
	"shingoedge/plc"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// The holds' reasons and resolutions.
const (
	holdReasonHeld    = "held"
	holdReasonOffPlan = "off-plan"
	holdReleased      = "released"
	holdCharged       = "charged"
	holdWrittenOff    = "written-off"
)

// tickRelease is the walk kind of a hold's release (chargeHold). Like an
// off-plan tick it charges only a cart of its style's part and holds every
// other share again (heldForItsPart); the poll never attributes it.
const tickRelease plc.TickKind = "release"

// holdCatidDraw adds a stroke's draw to the ledger: node 0 is the cell
// counter's held stroke's whole draw, a node a share there — an off-plan
// tick's share of part, a station's held share (part ""), or a release's
// share held again. The reason is off-plan for an off-plan tick, held
// otherwise.
func (e *Engine) holdCatidDraw(tick tickScope, nodeID int64, part string, qty int) {
	reason := holdReasonHeld
	if tick.kind == plc.TickOffPlan {
		reason = holdReasonOffPlan
	}
	opened, err := e.db.HoldCatidDraw(tick.processID, tick.catid, nodeID, part, reason, int64(qty), clock.Now())
	if err != nil {
		log.Printf("catid hold: process %d CATID %s node %d: %d not held: %v", tick.processID, tick.catid, nodeID, qty, err)
		return
	}
	log.Printf("catid hold: process %d CATID %s node %d part %q: %d held (%s, %s)", tick.processID, tick.catid, nodeID, part, qty, reason, tick.source)
	if opened {
		e.catidHealthChanged(tick.processID)
	}
}

// releaseHoldsOnLanding releases the process's open holds whose CATID the style
// a changeover just landed it on runs (releaseHolds, every position). The
// changeover's own completion is the landing; its row is written completed
// first, so releaseHolds reads it as the newest completed one to the style.
func (e *Engine) releaseHoldsOnLanding(processID int64) {
	e.releaseHolds(processID, nil)
}

// releaseHoldsOnArrival releases what a cart that just landed at node lands:
// the open holds at that position and the process's whole draws (node 0),
// whose CATID the active style runs (releaseHolds). A share there lands only
// on a cart of its part, so a landing of another part leaves it waiting; a
// whole draw lands what lands and is held again at its other positions
// (chargeHold). The landing changeover the guard excepts is the one that put
// the process on the style it runs (landedActiveBy): a cart that lands after
// a completion went ahead of it lands what that completion would have. +1
// statement per landing (the open holds); the rest only when one waits.
// Latched, so a release racing the door's charges once.
func (e *Engine) releaseHoldsOnArrival(node *processes.Node) {
	e.releaseHolds(node.ProcessID, node)
}

// releaseHolds releases the process's open holds whose CATID its active style
// runs — those at node and the whole draws, when node is given — when no
// changeover has started since the hold opened (the one that landed the active
// style excepted, landedActiveBy, and a cancelled one not counted), something
// would land (holdRows), and every share fits its cart (overTheCart); a hold
// that would land nothing, or more than a cart holds, waits for a person.
func (e *Engine) releaseHolds(processID int64, node *processes.Node) {
	holds, err := e.db.ListOpenCatidHolds(processID)
	if err != nil {
		return
	}
	if node != nil {
		holds = holdsLandingAt(holds, node.ID)
	}
	if len(holds) == 0 {
		return
	}
	proc, err := e.db.GetProcess(processID)
	if err != nil || proc == nil || proc.ActiveStyleID == nil {
		return
	}
	landing := e.landedActiveBy(processID, *proc.ActiveStyleID)
	var landed styleCATIDEntry
	for _, s := range e.processStyleCATIDSets(processID) {
		if s.ID == *proc.ActiveStyleID {
			landed = s
		}
	}
	on := "the landing on " + landed.Name
	if node != nil {
		on = "the cart that landed at " + node.Name
	}
	released := 0
	for i := range holds {
		h := &holds[i]
		if !catidSetHas(landed.Set, h.CATID) {
			continue
		}
		if n, err := e.db.CountChangeoversStartedSince(processID, h.FirstTickAt, landing); err != nil || n > 0 {
			log.Printf("catid hold %d: a changeover started since it opened — it waits for a person", h.ID)
			continue
		}
		rows, refusal, err := e.holdRows(h, landed.ID, landed.Name)
		if err == nil && refusal == "" {
			refusal = overTheCart(rows)
		}
		if err != nil || refusal != "" {
			log.Printf("catid hold %d: not released on %s — it waits for a person: %s%v", h.ID, on, refusal, err)
			continue
		}
		qty, ok, err := e.db.ResolveCatidHold(h.ID, holdReleased, "auto", clock.Now())
		if err != nil || !ok {
			continue
		}
		h.Qty = qty
		n := e.chargeHold(h, landed.ID)
		released++
		log.Printf("catid hold %d: released on %s: %d of CATID %s charged at %d position(s)", h.ID, on, qty, h.CATID, n)
	}
	if node == nil || released > 0 {
		e.catidHealthChanged(processID)
	}
}

// landedActiveBy is the changeover that landed the process on styleID: the
// newest completed row to it, one read; 0 when none is (or the read fails,
// which excepts nothing). The completion's own row and the row a later
// arrival finds are the same once the completed row is written.
func (e *Engine) landedActiveBy(processID, styleID int64) int64 {
	id, err := e.db.LatestCompletedChangeoverTo(processID, styleID)
	if err != nil {
		log.Printf("catid holds: process %d: the changeover that landed style %d: %v", processID, styleID, err)
		return 0
	}
	return id
}

// overTheCart is why an automatic release refuses a share bigger than the
// cart it would land on (decision 46): "N held, the cart at X holds M"; ""
// when every share fits. As holdRows read the carts: a stroke between the read
// and the latch can still add one.
func overTheCart(rows []CatidHoldChargeRow) string {
	for _, r := range rows {
		if r.BinID != 0 && !r.WrongPart && r.Qty > int64(r.CartCount) {
			return fmt.Sprintf("%d held, the cart at %s holds %d", r.Qty, r.Node, r.CartCount)
		}
	}
	return ""
}

// holdsLandingAt is the holds a landing at nodeID can release: the shares
// taken there and the whole draws.
func holdsLandingAt(holds []store.CatidHold, nodeID int64) []store.CatidHold {
	var out []store.CatidHold
	for _, h := range holds {
		if h.ProcessNodeID == 0 || h.ProcessNodeID == nodeID {
			out = append(out, h)
		}
	}
	return out
}

// chargeHold charges a hold's latched draw under styleID through the walk, as
// a release (tickRelease): every node the cell counter counts for a node-0
// draw, the one node for a share — walked as its own counter's (countedAt), so
// a share at a station-counted position reaches its cart. A share lands only
// on a cart of the style's part, read by the walk itself; any other is held
// again for a person, and a position the style does not claim is named in the
// walk's unattributed line. Returns the positions charged.
func (e *Engine) chargeHold(h *store.CatidHold, styleID int64) int {
	nodes, err := e.db.ListProcessNodesByProcess(h.ProcessID)
	if err != nil {
		log.Printf("catid hold %d: list nodes: %v", h.ID, err)
		return 0
	}
	if h.ProcessNodeID != 0 {
		nodes = onlyNode(nodes, h.ProcessNodeID)
	}
	process, claims, runtimes := e.counterDeltaRows(h.ProcessID, styleID, nodes)
	return e.walkTick(process, nodes, claims, runtimes, tickScope{
		processID: h.ProcessID, styleID: styleID, delta: int(h.Qty),
		source: fmt.Sprintf("hold=%d", h.ID), catid: h.CATID, kind: tickRelease,
		counter: holdCounter(h, process, nodes, claims, styleID),
	}).charged()
}

// holdCounter is the counter a hold's draw is walked as: a share's position's
// own (countedAt under the charge style's claim), the cell counter for a
// node-0 draw.
func holdCounter(h *store.CatidHold, process *processes.Process, nodes []processes.Node, claims *store.NodeClaimSet, styleID int64) int64 {
	if h.ProcessNodeID == 0 || len(nodes) != 1 {
		return 0
	}
	claim := claims.Resolve(process, &nodes[0], store.ExactStyle(styleID))
	if claim == nil {
		return 0
	}
	return countedAt(&nodes[0], claim)
}

func onlyNode(nodes []processes.Node, id int64) []processes.Node {
	for _, n := range nodes {
		if n.ID == id {
			return []processes.Node{n}
		}
	}
	return nil
}

// CatidHoldPreview is what a Charge would do: the style the CATID is charged
// under and, per node, the cart and the quantity.
type CatidHoldPreview struct {
	Hold      store.CatidHold      `json:"hold"`
	StyleID   int64                `json:"style_id"`
	StyleName string               `json:"style_name"`
	Rows      []CatidHoldChargeRow `json:"rows"`
	// Refusal is why the Charge is refused, "" when it is not.
	Refusal string `json:"refusal,omitempty"`
}

// CatidHoldChargeRow is one node a Charge draws from. CarrierSource is who
// recorded CarrierPart (domain.CarrierSource: delivery, landing, operator,
// flip, ...), "" when nobody could say.
type CatidHoldChargeRow struct {
	Node          string `json:"node"`
	BinID         int64  `json:"bin_id,omitempty"`
	CarrierPart   string `json:"carrier_part"`
	CarrierSource string `json:"carrier_source,omitempty"`
	ClaimPart     string `json:"claim_part"`
	Qty           int64  `json:"qty"`
	WrongPart     bool   `json:"wrong_part"`
	// CartCount is the cart's count as the seat reads it (0 for no cart).
	CartCount int `json:"cart_count"`
}

// ErrHoldResolved is a release of a hold already released.
var ErrHoldResolved = errors.New("the hold has already been released")

// PreviewCatidHold names the style, the nodes, the carts and the quantities a
// Charge of the hold would draw, and why it would be refused.
func (e *Engine) PreviewCatidHold(id int64) (*CatidHoldPreview, error) {
	h, err := e.db.GetCatidHold(id)
	if err != nil {
		return nil, err
	}
	if h == nil {
		return nil, fmt.Errorf("no hold %d", id)
	}
	p := &CatidHoldPreview{Hold: *h}
	if h.ResolvedAt != nil {
		p.Refusal = ErrHoldResolved.Error()
		return p, nil
	}
	var matches []styleCATIDEntry
	for _, s := range e.processStyleCATIDSets(h.ProcessID) {
		if catidSetHas(s.Set, h.CATID) {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 0:
		p.Refusal = fmt.Sprintf("CATID %s maps to no style: pin it to one first", h.CATID)
		return p, nil
	case 1:
		p.StyleID, p.StyleName = matches[0].ID, matches[0].Name
	default:
		p.Refusal = fmt.Sprintf("CATID %s is in %d styles: one CATID must name one style", h.CATID, len(matches))
		return p, nil
	}
	p.Rows, p.Refusal, err = e.holdRows(h, p.StyleID, p.StyleName)
	return p, err
}

// holdRows is what a release of h under a style would draw, the rule the
// landing and the Charge share: per position the cart and the quantity, and
// whether the cart is another part's (that share is held again); and why
// nothing would land, "" when something would. A share whose position is
// deleted, or not claimed by the style, lands nothing.
func (e *Engine) holdRows(h *store.CatidHold, styleID int64, styleName string) ([]CatidHoldChargeRow, string, error) {
	nodes, err := e.db.ListProcessNodesByProcess(h.ProcessID)
	if err != nil {
		return nil, "", err
	}
	if h.ProcessNodeID != 0 {
		if nodes = onlyNode(nodes, h.ProcessNodeID); nodes == nil {
			return nil, "the position this share was taken at is deleted: write it off", nil
		}
	}
	process, claims, runtimes := e.counterDeltaRows(h.ProcessID, styleID, nodes)
	var rows []CatidHoldChargeRow
	lands, refusal := false, ""
	for i := range nodes {
		claim := claims.Resolve(process, &nodes[i], store.ExactStyle(styleID))
		if claim == nil && h.ProcessNodeID != 0 {
			return nil, fmt.Sprintf("%s does not claim %s, where this share was taken: charge the cart there by hand or write it off",
				styleName, nodes[i].Name), nil
		}
		rt := runtimes[nodes[i].ID]
		if claim == nil || claim.IsLoaderNode() || rt == nil {
			continue
		}
		// The rows the release draws (chargeHold): a node-0 draw walks the
		// cell counter's positions, so a station-counted one is not its.
		if h.ProcessNodeID == 0 && countedAt(&nodes[i], claim) != 0 {
			continue
		}
		row := CatidHoldChargeRow{Node: nodes[i].Name, ClaimPart: claim.PayloadCode, Qty: h.Qty}
		switch {
		case rt.ActiveBinID == nil:
			if refusal == "" {
				refusal = fmt.Sprintf("no cart at %s", nodes[i].Name)
			}
		case !carrierIsPart(process, &nodes[i], rt, claim, claims):
			row.BinID, row.CarrierPart, row.CarrierSource, row.WrongPart = *rt.ActiveBinID, string(rt.LinesidePayloadCode), recordedBy(rt), true
			row.CartCount = rt.RemainingUOPCached
			if refusal == "" {
				refusal = e.wrongCartRefusal(&nodes[i], rt, styleID, styleName)
			}
		default:
			row.BinID, row.CarrierPart, row.CarrierSource = *rt.ActiveBinID, string(rt.LinesidePayloadCode), recordedBy(rt)
			row.CartCount = rt.RemainingUOPCached
			lands = true
		}
		rows = append(rows, row)
	}
	if lands {
		return rows, "", nil
	}
	if refusal == "" {
		refusal = fmt.Sprintf("%s has no position this hold draws from", styleName)
	}
	return rows, refusal, nil
}

// recordedBy is who recorded the seat's carrier, "" when it could not say.
func recordedBy(rt *processes.RuntimeState) string {
	if !rt.LinesidePayloadKnown {
		return ""
	}
	return rt.LinesideSource
}

// wrongCartRefusal is the refusal for a share at a cart that is not the
// style's part, naming what a person can fix: "the cart at X is recorded as P
// (from the switch off RUN), not S's part" — name the cart, or land one of the
// part. A cart that could not say is taken as the running style's part.
func (e *Engine) wrongCartRefusal(node *processes.Node, rt *processes.RuntimeState, styleID int64, styleName string) string {
	if !rt.LinesidePayloadKnown {
		return fmt.Sprintf("the cart at %s is not %s's part", node.Name, styleName)
	}
	return fmt.Sprintf("the cart at %s is %s, not %s's part", node.Name, recordedAs(rt, e.carrierRecordSaid(node, rt, styleID)), styleName)
}

// ChargeCatidHold is a person's Charge: the hold's draw is charged under the
// one style that runs its CATID, refused when the preview refuses (nothing
// would land). It charges the quantity its latch resolved. Latched: a second
// Charge is ErrHoldResolved.
func (e *Engine) ChargeCatidHold(id int64, by string) error {
	p, err := e.PreviewCatidHold(id)
	if err != nil {
		return err
	}
	if p.Hold.ResolvedAt != nil {
		return ErrHoldResolved
	}
	if p.Refusal != "" {
		return errors.New(p.Refusal)
	}
	qty, ok, err := e.db.ResolveCatidHold(id, holdCharged, by, clock.Now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrHoldResolved
	}
	h := p.Hold
	h.Qty = qty
	n := e.chargeHold(&h, p.StyleID)
	log.Printf("catid hold %d: charged by %s under style %s: %d of CATID %s at %d position(s)", id, by, p.StyleName, qty, h.CATID, n)
	e.catidHealthChanged(h.ProcessID)
	return nil
}

// WriteOffCatidHold is a person's Write off: the hold is released and charges
// nothing. Latched.
func (e *Engine) WriteOffCatidHold(id int64, by string) error {
	h, err := e.db.GetCatidHold(id)
	if err != nil {
		return err
	}
	if h == nil {
		return fmt.Errorf("no hold %d", id)
	}
	qty, ok, err := e.db.ResolveCatidHold(id, holdWrittenOff, by, clock.Now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrHoldResolved
	}
	log.Printf("catid hold %d: written off by %s: %d of CATID %s at node %d not charged", id, by, qty, h.CATID, h.ProcessNodeID)
	e.catidHealthChanged(h.ProcessID)
	return nil
}
