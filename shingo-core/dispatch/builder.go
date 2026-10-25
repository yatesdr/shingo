package dispatch

import (
	"database/sql"
	"errors"
	"fmt"

	"shingo/protocol"
	"shingocore/dispatch/binresolver"
	"shingocore/dispatch/plan"
	"shingocore/domain"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// builder.go — the one issue (v4 §2.5; B2, and the round-2 unification's K).
//
// An order comes to exist one way: issued, through the single INSERT
// (orders.Create), holding softly what its door already knows and will not
// re-choose, in the issue's transaction or nothing. IssueOrder issues in a
// transaction of its own; issue issues on the caller's (a compound parent and
// its children). The issue decides nothing a plan decides: a plan whose verdict
// is not satisfiable never reaches it. Complex intake and the compound children
// (a dig's legs, the home clear's moves) still write through the store
// (TestCensus_Issuers names every caller of the INSERT).

// Need is what a door declares about the order it issues: the facts the row
// records that no plan decides, and what the order holds from its issue. The
// door fills it; the issue writes it verbatim. A door difference lives here,
// never as a parameter on the issue (v4 §2.5).
type Need struct {
	Type        protocol.OrderType
	Station     string
	Ref         string // the order's reference number (edge_uuid)
	Quantity    int64
	Priority    int
	Desc        string
	PayloadCode string
	OriginID    string
	OriginClass string
	// Projects names the station the order projects to, and what it projects
	// there is its material notices alone: order.delivered and bin_picked_up,
	// which bind the bin at a node that station's Edge models. A stage-2 pull
	// names its window's Edge; a person's move names nobody (""), and then no
	// station hears anything of it. The door says it; the notifier only reads
	// it (the orc's ruling on Stop 1's delivery line, 2026-09-26, v4 §2.5).
	Projects string

	// StationKnows: the station the order names has its own row for it, because
	// it sent the order (the wire door) or the door projects the order there (a
	// Core ask). A door whose station has no row writes a station-unaware row:
	// no ack, no waybill, no bookkeeping notice (v145; the orc's Stop 1 and 2
	// rulings).
	StationKnows bool
	// Source and Delivery are the nodes as asked. The row's delivery is the
	// plan's Dest when the planner chose one, else what was asked; its source is
	// what was asked, or the cart's node when nothing was.
	Source   string
	Delivery string
	// SkipAutoConfirm is carried verbatim, as the sender stated it.
	SkipAutoConfirm bool
	// NamedBin is the one bin a station's move is for, as the station read it on
	// the source node (protocol.OrderRequest.BinID), on a move that does not yet
	// hold it from its issue (birthNamedMove); nil for every other order. The
	// hold and claim statements take that bin and no other
	// (reservations.NamedBinSQL).
	NamedBin *int64
	// QualityHoldBy puts NamedBin on quality hold, attributed to this, in the
	// issue's transaction (protocol.OrderRequest.QualityHoldBy): a station's
	// containment move. birthNamedMove writes it. "" for every other order.
	QualityHoldBy string
	// SourceIntent is the sourcing intent when the type does not decide it: a
	// carried-bin recovery's on_deck. Blank takes the type's (SourceIntentForType).
	SourceIntent string
	// Recovers is the cancelled order whose carried bin a Core-minted return
	// puts down (orders.recovers_order_id); nil for every other order. Written
	// once, at its issue.
	Recovers *int64

	// WHAT THE ORDER HOLDS FROM ITS ISSUE, softly, in the issue's transaction,
	// or the issue is refused.
	//
	// Cart is the one carrier the order is for: its pending bin row is taken in
	// the issue's transaction, and bin_id and named_bin_id are stamped in the
	// INSERT ("an order for one carrier"). 0: the acquire finds one, and the
	// planner's pick rides the row as orders.plan.
	Cart int64
	// By is who asked: Person(name) for a person (a station or a page), the zero
	// value for Core's own errand. It decides whether a stranger's soft hold on
	// the cart is taken.
	By Actor
}

// Actor is who asked for an order. A person is authoritative over another
// order's soft hold, never over a robot (v4 §2.7 Rule 4); Core's own errand
// takes nothing. The zero value is Core's errand.
type Actor struct{ person string }

// Person is an order a person asked for: a station's or a page's actor.
func Person(name string) Actor { return Actor{person: name} }

// Taken is what an issue took from strangers: the person's moves whose soft
// hold it took, which the caller ends by name once the issue has committed
// (store.TakeSoftHolds).
type Taken struct {
	Named []int64
}

// row is the order a need and a plan describe, born `pending` (v4 §2.6). It
// writes nothing.
func row(need Need, p plan.Plan) *orders.Order {
	o := &orders.Order{
		EdgeUUID:        need.Ref,
		StationID:       need.Station,
		OrderType:       need.Type,
		Status:          protocol.StatusPending,
		Quantity:        need.Quantity,
		SourceNode:      need.Source,
		DeliveryNode:    need.Delivery,
		Priority:        need.Priority,
		PayloadDesc:     need.Desc,
		PayloadCode:     need.PayloadCode,
		SkipAutoConfirm: need.SkipAutoConfirm,
		OriginID:        need.OriginID,
		OriginClass:     need.OriginClass,
		SourceIntent:    need.SourceIntent,
		StationUnaware:  !need.StationKnows,
		ProjectsTo:      need.Projects,
		NamedBinID:      need.NamedBin,
		RecoversOrderID: need.Recovers,
	}
	if o.SourceIntent == "" {
		o.SourceIntent = SourceIntentForType(need.Type)
	}
	if p.Dest != nil {
		o.DeliveryNode = p.Dest.Name
	}
	switch {
	case need.Cart != 0:
		// The cart is the order's from its issue, so the node it stands on is
		// the order's source. A pick that is not held never names the source:
		// the asked source (blank, a node or a group) scopes any re-pick.
		cart := need.Cart
		o.BinID, o.NamedBinID = &cart, &cart
		if o.SourceNode == "" {
			o.SourceNode = p.Node.Name
		}
	case p.Outcome == plan.OutcomeFound && p.Bin != nil && p.Node != nil:
		o.Plan = &domain.PlannedPick{BinID: p.Bin.ID, Node: p.Node.Name, Payload: p.Bin.PayloadCode,
			Status: string(p.Bin.Status), Confirmed: p.Bin.ManifestConfirmed}
	}
	return o
}

// issue writes the order a door's need and the planner's plan describe, on the
// caller's transaction, holding what the need declares: the row (orders.Create,
// with its creation history), then the cart, a stranger's pending hold on it
// taken first when a person asked and the planner found one, then the order's
// own soft hold (reservations.Acquire). A hold that cannot be taken refuses the
// issue: the error is returned and the caller's transaction rolls back.
//
// The wire door brings a plan whose pick may have found nothing (the gate was
// closed, or the pick waits, digs or fails): the row is issued either way, as it
// always was, and the order's disposition is the door's business after.
func issue(tx *sql.Tx, need Need, p plan.Plan) (*orders.Order, Taken, error) {
	if p.Refused != nil {
		return nil, Taken{}, errors.New("issue: the plan is not satisfiable")
	}
	if need.Cart != 0 && (p.Node == nil || p.Dest == nil) {
		return nil, Taken{}, errors.New("issue: an order for a cart needs a gathered plan")
	}
	o := row(need, p)
	if err := orders.Create(tx, o); err != nil {
		return nil, Taken{}, err
	}
	if need.Cart == 0 {
		return o, Taken{}, nil
	}
	var taken Taken
	if p.Take && need.By.person != "" {
		note := store.HolderNote{
			Reason: FormatQueueSentence(protocol.QueueWaitingForMaterial, QueueParams{TakenByOrderID: o.ID}),
			Code:   protocol.QueueWaitingForMaterial,
			Cause:  string(CauseBinTaken),
			Actor:  need.By.person,
		}
		var err error
		if taken.Named, err = store.TakeSoftHolds(tx, need.Cart, o.ID, note); err != nil {
			return nil, Taken{}, err
		}
	}
	// A root order owns its own lane rows.
	if err := reservations.Acquire(tx, o.ID, o.ID, need.Cart, "reserveForDispatch"); err != nil {
		return nil, Taken{}, err
	}
	return o, taken, nil
}

// IssueOrder issues an order in a transaction of its own: the row, its history
// and its holds, or none of them (issue).
func (d *Dispatcher) IssueOrder(need Need, p plan.Plan) (*orders.Order, Taken, error) {
	return issueOwnTx(d.db, need, p)
}

// issueOwnTx is IssueOrder's body, for admission, which issues without the
// Dispatcher (LifecycleService.issue).
func issueOwnTx(db *store.DB, need Need, p plan.Plan) (*orders.Order, Taken, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, Taken{}, err
	}
	defer tx.Rollback() //nolint:errcheck // committed below; rollback is the error close
	o, taken, err := issue(tx, need, p)
	if err != nil {
		return nil, Taken{}, err
	}
	if err := tx.Commit(); err != nil {
		return nil, Taken{}, err
	}
	return o, taken, nil
}

// NotAtSourceError is a move refused at its issue because the bin it names is
// not on its source. Error is the sentence the station shows.
type NotAtSourceError struct {
	BinID  int64
	Label  string // "" when no bin has the id
	Source string
}

func (e *NotAtSourceError) Error() string {
	bin := e.Label
	if bin == "" {
		bin = fmt.Sprintf("%d", e.BinID)
	}
	// OWNER-WORDS
	return fmt.Sprintf("bin %s is not at that source (%s)", bin, e.Source)
}

// birthNamedMove is the issue of a station's move that names its bin, in one
// transaction: the bin stands on the source (reservations.NamedBinAtSourceTx,
// under its row lock), the hold marker when the station asked for one
// (need.QualityHoldBy), and the row (issue). It holds nothing: the acquire
// takes the bin. A bin not on the source is a *NotAtSourceError and nothing is
// written.
func birthNamedMove(db *store.DB, need Need, p plan.Plan) (*orders.Order, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // committed below; rollback is the error close
	label, _, at, err := reservations.NamedBinAtSourceTx(tx, *need.NamedBin, need.Source)
	if err != nil {
		return nil, err
	}
	if !at {
		return nil, &NotAtSourceError{BinID: *need.NamedBin, Label: label, Source: need.Source}
	}
	if need.QualityHoldBy != "" {
		if err := store.HoldBinTx(tx, *need.NamedBin, need.QualityHoldBy); err != nil {
			return nil, fmt.Errorf("hold bin %d: %w", *need.NamedBin, err)
		}
	}
	o, _, err := issue(tx, need, p)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return o, nil
}

// PlanMaintainedSlot asks the planner for the slot one keeper ask fills
// (plan.MaintainedSlot).
func (d *Dispatcher) PlanMaintainedSlot(group *nodes.Node, binTypeID int64) (*binresolver.ResolveResult, error) {
	return d.pure.MaintainedSlot(group, binTypeID)
}

// PlanGroupStoreSlot asks the planner for the slot a carrier is stored in when it
// goes into a group (plan.GroupStoreSlot).
func (d *Dispatcher) PlanGroupStoreSlot(group *nodes.Node, payloadCode string, stated binresolver.BinTypeStatement) (*binresolver.ResolveResult, error) {
	return d.pure.GroupStoreSlot(group, payloadCode, stated)
}

// PlanMove asks the planner for a bin move: it validates a named bin (Given),
// or chooses one at the source when none is named.
func (d *Dispatcher) PlanMove(g plan.Given) plan.Plan {
	if g.Bin != nil {
		return d.pure.ValidateMove(g)
	}
	return d.pure.ChooseMove(g.Source, g.Dest)
}

// CapacitySentence is the operator sentence for a blocked capacity verdict, as
// the preview says it.
func CapacitySentence(v plan.CapacityVerdict) string {
	return FormatQueueSentence(protocol.QueueWaitingForSlot, queueParamsOf(v.Params))
}
