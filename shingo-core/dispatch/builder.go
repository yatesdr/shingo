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
// transaction of its own; issue issues on the caller's (a compound's legs, a
// home clear's parent and its first leg). The issue decides nothing a plan
// decides: a plan whose verdict is not satisfiable never reaches it.
// TestCensus_Issuers names every caller of the INSERT, and testdb.IssueDoors
// says what each door's order holds at its commit.

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
	// QualityHoldBy puts Cart on quality hold, attributed to this, in the
	// issue's transaction (protocol.OrderRequest.QualityHoldBy): a station's
	// containment move. "" for every other order.
	QualityHoldBy string
	// SourceIntent is the sourcing intent when the type does not decide it: a
	// carried-bin recovery's on_deck. Blank takes the type's (SourceIntentForType).
	SourceIntent string
	// Recovers is the cancelled order whose carried bin a Core-minted return
	// puts down (orders.recovers_order_id); nil for every other order. Written
	// once, at its issue.
	Recovers *int64
	// Robot is the one robot the order is for, in the INSERT (orders.robot_id):
	// a carried-bin recovery's pin, the robot whose deck the bin rides. "" for
	// every other order, whose robot the fleet assigns at dispatch.
	Robot string
	// Steps is a multi-leg order's steps as resolved at its intake
	// (orders.steps_json); "" for every other order.
	Steps string
	// Complex is what a coordinated order states beyond the plain row; nil for
	// every simple order, and its presence is what makes the row coordinated.
	Complex *ComplexSpec
	// Sibling is the order this one is paired with (orders.sibling_order_uuid),
	// written with the row so the pointer can never be lost by a failed link
	// step after it: a two-robot swap's evac naming its supply, a home clear's
	// trade leg naming the partial's move. "" for every other order.
	Sibling string
	// Leg is a compound's child: its parent, its turn, and for a dig's leg the
	// bin it moves. nil for every root order.
	Leg *Leg
	// Want is the height the empty is wanted at, as a station stated it on its
	// request (protocol.OrderRequest.Want, orders.want): every find of the order
	// reads it. None for every other order.
	Want domain.Height

	// WHAT THE ORDER HOLDS FROM ITS ISSUE, softly, in the issue's transaction,
	// or the issue is refused.
	//
	// Cart is the one carrier the order is for: its pending bin row is taken in
	// the issue's transaction, and bin_id and named_bin_id are stamped in the
	// INSERT ("an order for one carrier"). 0: the acquire finds one, and the
	// planner's pick rides the row as orders.plan.
	Cart int64
	// Window is the drop when it is one carrier's at a time (a loader window):
	// it IS the order's delivery, so a need names it or Delivery, never both.
	// It is held as a pending slot row taken in the issue's transaction, before
	// the cart: a full ask born after it chooses another window
	// (plan.LoaderWindows passes over a reserved one), and its dispatch reuses
	// the row. A window another order holds refuses the issue (errWindowTaken).
	Window *nodes.Node
	// By is who asked: Person(name) for a person (a station or a page), the zero
	// value for Core's own errand. It decides whether a stranger's soft hold on
	// the cart is taken.
	By Actor
}

// Leg is a compound's child as its issue writes it: its parent
// (orders.parent_order_id), its turn among the parent's legs (sequence), and,
// for a dig's leg, the bin it moves. A dig leg's bin is stamped as bin_id and
// held by its parent's lane, with no bin row: one bin may be two legs' (an
// unbury followed by a retrieve of the same bin) and the bin index admits one
// active row. Its claim is its hand's. A leg that holds its bin by a row (the
// home clear's) declares it as the need's Cart, and Bin is 0.
type Leg struct {
	Parent   int64
	Sequence int
	Bin      int64
}

// ComplexSpec is the part of a coordinated order's row only complex intake
// states: the process it serves and the claim's SEER routing hints, kept
// because dispatch happens later (domain.Order.KeyRoute). There is no
// coordinated flag: a need with a ComplexSpec is coordinated. Its swap sibling
// is the need's (Need.Sibling), as a home clear's trade leg's is.
type ComplexSpec struct {
	ProcessNode string
	KeyRoute    []string
	KeyTask     string
}

// Actor is who asked for an order. A person is authoritative over another
// order's soft hold, never over a robot (v4 §2.7 Rule 4); Core's own errand
// takes nothing. The zero value is Core's errand.
type Actor struct{ person string }

// Person is an order a person asked for: a station's or a page's actor.
func Person(name string) Actor { return Actor{person: name} }

// Taken is what a person's issue took from strangers (store.TakeSoftHolds):
// the orders for that one cart whose soft hold it took, which the caller ends
// by name once the issue has committed, and how many other holders it
// un-pointed, which re-find on their next pass.
type Taken struct {
	Named     []int64
	Unpointed int
}

// row is the order a need and a plan describe, born `pending` (v4 §2.6). It
// writes nothing.
func row(need Need, p plan.Plan) *orders.Order {
	o := &orders.Order{
		EdgeUUID:         need.Ref,
		StationID:        need.Station,
		OrderType:        need.Type,
		Status:           protocol.StatusPending,
		Quantity:         need.Quantity,
		SourceNode:       need.Source,
		DeliveryNode:     need.Delivery,
		Priority:         need.Priority,
		PayloadDesc:      need.Desc,
		PayloadCode:      need.PayloadCode,
		SkipAutoConfirm:  need.SkipAutoConfirm,
		OriginID:         need.OriginID,
		OriginClass:      need.OriginClass,
		SourceIntent:     need.SourceIntent,
		StationUnaware:   !need.StationKnows,
		ProjectsTo:       need.Projects,
		RecoversOrderID:  need.Recovers,
		StepsJSON:        need.Steps,
		SiblingOrderUUID: need.Sibling,
		RobotID:          need.Robot,
		Want:             need.Want,
	}
	if o.SourceIntent == "" {
		o.SourceIntent = SourceIntentForType(need.Type)
	}
	if c := need.Complex; c != nil {
		o.Coordinated = true
		o.ProcessNode, o.KeyRoute, o.KeyTask = c.ProcessNode, c.KeyRoute, c.KeyTask
	}
	if l := need.Leg; l != nil {
		parent := l.Parent
		o.ParentOrderID, o.Sequence = &parent, l.Sequence
		if l.Bin != 0 {
			bin := l.Bin
			o.BinID = &bin
		}
	}
	if p.Dest != nil {
		o.DeliveryNode = p.Dest.Name
	}
	if need.Window != nil {
		o.DeliveryNode = need.Window.Name
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
// with its creation history), the hold marker a station asked for
// (QualityHoldBy), its window (Window), then the order's own soft hold on its
// cart (reservations.Acquire). The take is who asked's: a person takes a
// stranger's pending hold out loud, Core's own errand takes nothing, and nobody
// takes a confirmed hold. A hold that cannot be taken refuses the issue: the
// error is returned and the caller's transaction rolls back.
//
// The wire door brings a plan whose pick may have found nothing (the gate was
// closed, or the pick waits, digs or fails): the row is issued either way, as it
// always was, and the order's disposition is the door's business after.
func issue(tx *sql.Tx, need Need, p plan.Plan) (*orders.Order, Taken, error) {
	if p.Refused != nil {
		return nil, Taken{}, errors.New("issue: the plan is not satisfiable")
	}
	if need.Cart != 0 && need.Source == "" && p.Node == nil {
		return nil, Taken{}, errors.New("issue: an order for a cart names its source or the node the cart stands on")
	}
	if need.QualityHoldBy != "" && need.Cart == 0 {
		return nil, Taken{}, errors.New("issue: a hold marker needs the cart it marks")
	}
	if need.Leg != nil && need.Leg.Bin != 0 && need.Cart != 0 {
		return nil, Taken{}, errors.New("issue: a leg holds its bin by its parent's lane or by its cart, never both")
	}
	if need.Window != nil && need.Delivery != "" {
		return nil, Taken{}, errors.New("issue: a declared window is the delivery; a need names one or the other")
	}
	o := row(need, p)
	if err := orders.Create(tx, o); err != nil {
		return nil, Taken{}, err
	}
	if need.QualityHoldBy != "" {
		if err := store.HoldBinTx(tx, need.Cart, need.QualityHoldBy); err != nil {
			return nil, Taken{}, fmt.Errorf("hold bin %d: %w", need.Cart, err)
		}
	}
	// The window, before the cart: a window lost to another order since the
	// door read it free is no issue, and the door asks at its next window.
	if need.Window != nil {
		err := reservations.AcquireSlot(tx, o.ID, need.Window.ID, "reserveSlot")
		if errors.Is(err, reservations.ErrReservationConflict) {
			return nil, Taken{}, errWindowTaken
		}
		if err != nil {
			return nil, Taken{}, fmt.Errorf("hold window %s: %w", need.Window.Name, err)
		}
	}
	if need.Cart == 0 {
		return o, Taken{}, nil
	}
	// The cart's hold, asked first: an issue that meets no stranger reads
	// nothing more. A root order owns its own lane rows; a leg's are its parent's.
	owner := o.ID
	if need.Leg != nil {
		owner = need.Leg.Parent
	}
	err := reservations.Acquire(tx, o.ID, owner, need.Cart, "reserveForDispatch")
	if !errors.Is(err, reservations.ErrReservationConflict) || need.By.person == "" {
		// Held, or refused: another order holds the cart and Core's own errand
		// takes nothing.
		if err != nil {
			return nil, Taken{}, err
		}
		return o, Taken{}, nil
	}
	// A PERSON ASKED, so a stranger's pending hold is taken out loud: the
	// holder is un-pointed and told, and one that is itself an order for this
	// cart is handed back to be ended by name (store.TakeSoftHolds). A
	// confirmed hold is a robot's and refuses the issue.
	note := store.HolderNote{
		Reason: FormatQueueSentence(protocol.QueueWaitingForMaterial, QueueParams{TakenByOrderID: o.ID}),
		Code:   protocol.QueueWaitingForMaterial,
		Cause:  string(CauseBinTaken),
		Actor:  need.By.person,
	}
	var taken Taken
	if taken.Named, taken.Unpointed, err = store.TakeSoftHolds(tx, need.Cart, o.ID, note); err != nil {
		return nil, Taken{}, err
	}
	if err := reservations.Acquire(tx, o.ID, owner, need.Cart, "reserveForDispatch"); err != nil {
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

// errWindowTaken is an issue refused because the window its need declares
// (Need.Window) is held by another order: nothing was written.
var errWindowTaken = errors.New("the window is held by another order")

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

// CartHeldError is a person's order refused at its issue because a robot's
// order holds the bin it is for: a confirmed hold, which nobody takes. Error is
// the sentence the station shows. Holder is 0 when the holder was not read.
type CartHeldError struct {
	BinID  int64
	Label  string
	Holder int64
}

func (e *CartHeldError) Error() string {
	bin := e.Label
	if bin == "" {
		bin = fmt.Sprintf("%d", e.BinID)
	}
	if e.Holder == 0 {
		return fmt.Sprintf("bin %s is held by another order", bin)
	}
	return fmt.Sprintf("bin %s is held by order %d", bin, e.Holder)
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
