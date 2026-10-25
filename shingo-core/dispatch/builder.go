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

// builder.go — the one builder (v4 §2.5; B2).
//
// Build turns a door's need and the planner's plan into the order row, through
// the single INSERT (orders.Create). It decides nothing: a plan whose verdict is
// not satisfiable never reaches it. Every door is on it but complex intake,
// which writes through the store's CreateOrder, and the compound children (a
// dig's legs, the home clear's moves), which the store's CreateCompoundChildrenTx
// writes in the parent's transaction (TestCensus_Issuers names each caller of the
// INSERT).

// QueryRower is the write surface Build needs, satisfied by *sql.DB and *sql.Tx:
// the two real call shapes are a door's own transaction and a compound parent's
// (the B2 design note §2). It is store/internal/helpers.QueryRower's shape, which
// orders.Create takes.
type QueryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

// Need is what a door asks the builder to write about who asked: the facts the
// row records that no plan decides. The door fills it; Build writes it verbatim.
// A door difference lives here, never as a parameter on Build (v4 §2.5).
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
	// HoldsBin: the door takes the plan's bin in the birth's transaction (the
	// bin move), so the row is born holding it (bin_id). A door that does not
	// hold the bin at birth gets the pick as a plan instead (orders.plan), which
	// acquiring reads and may find stale (SYNTH F4).
	HoldsBin bool
	// Source and Delivery are the nodes as asked. The row's delivery is the
	// plan's Dest when the planner chose one, else what was asked; its source is
	// what was asked, or the held bin's node when nothing was.
	Source   string
	Delivery string
	// SkipAutoConfirm is carried verbatim, as the sender stated it.
	SkipAutoConfirm bool
	// NamedBin is the one bin a move is for, as the station read it on the
	// source node (protocol.OrderRequest.BinID); nil for every other order.
	// Written once, at birth (orders.named_bin_id); the hold and claim
	// statements take that bin and no other (reservations.NamedBinSQL).
	NamedBin *int64
	// QualityHoldBy puts NamedBin on quality hold, attributed to this, in the
	// birth's transaction (protocol.OrderRequest.QualityHoldBy): a station's
	// containment move. Build does not write it; birthNamedMove does. "" for
	// every other order.
	QualityHoldBy string
	// SourceIntent is the sourcing intent when the type does not decide it: a
	// carried-bin recovery's on_deck. Blank takes the type's (SourceIntentForType).
	SourceIntent string
	// Recovers is the cancelled order whose carried bin a Core-minted return
	// puts down (orders.recovers_order_id); nil for every other order. Written
	// once, at birth.
	Recovers *int64
}

// Build writes the order a plan describes, born `pending` (v4 §2.6), and returns
// it with its id. It decides nothing: a plan the planner refused never reaches
// it (Refused), and a door that holds its bin at birth must bring a gathered
// plan (the bin, the node it stands on, the node it goes to).
//
// The wire door brings a plan whose pick may have found nothing (the gate was
// closed, or the pick waits, digs or fails): the row is born either way, as it
// always was, and the order's disposition is the door's business after birth.
func Build(ex QueryRower, need Need, p plan.Plan) (*orders.Order, error) {
	if p.Refused != nil {
		return nil, errors.New("build: the plan is not satisfiable")
	}
	if need.HoldsBin && (p.Bin == nil || p.Node == nil || p.Dest == nil) {
		return nil, errors.New("build: a door holding its bin needs a gathered plan")
	}
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
	case need.HoldsBin:
		// The bin is the order's from birth, so the node it stands on is the
		// order's source. A pick that is not held never names the source: the
		// asked source (blank, a node or a group) scopes any re-pick.
		o.BinID = &p.Bin.ID
		if o.SourceNode == "" {
			o.SourceNode = p.Node.Name
		}
	case p.Outcome == plan.OutcomeFound && p.Bin != nil && p.Node != nil:
		o.Plan = &domain.PlannedPick{BinID: p.Bin.ID, Node: p.Node.Name, Payload: p.Bin.PayloadCode,
			Status: string(p.Bin.Status), Confirmed: p.Bin.ManifestConfirmed}
	}
	if err := orders.Create(ex, o); err != nil {
		return nil, err
	}
	return o, nil
}

// Birth writes an order through Build in its own transaction (birthOwnTx), for
// a door that holds nothing else in the birth's transaction (carried-bin
// recovery).
func (d *Dispatcher) Birth(need Need, p plan.Plan) (*orders.Order, error) {
	return birthOwnTx(d.db, need, p)
}

// birthOwnTx is Build in a transaction of its own: the row and its birth
// history, or neither (db.CreateOrder's shape).
func birthOwnTx(db *store.DB, need Need, p plan.Plan) (*orders.Order, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // committed below; rollback is the error close
	o, err := Build(tx, need, p)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return o, nil
}

// NotAtSourceError is a move refused at its birth because the bin it names is
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

// birthNamedMove is the birth of a move that names its bin, in one
// transaction: the bin stands on the source (reservations.NamedBinAtSourceTx,
// under its row lock), the hold marker when the station asked for one
// (need.QualityHoldBy), and the row (Build). A bin not on the source is a *NotAtSourceError and nothing
// is written.
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
	o, err := Build(tx, need, p)
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

// BirthMove writes a person's bin move in one transaction: the row (Build),
// Q1's take when the plan says so, and the move's own soft hold on its bin. The
// take and the hold land together or not at all, so no instant exists in which
// the bin is promised to nobody or to two orders.
//
// It returns the person's moves the take handed back (store.TakeSoftHolds):
// the caller ends each by name once this has committed.
func (d *Dispatcher) BirthMove(need Need, p plan.Plan, actor string) (*orders.Order, []int64, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return nil, nil, fmt.Errorf("begin bin move: %w", err)
	}
	defer tx.Rollback()
	o, err := Build(tx, need, p)
	if err != nil {
		return nil, nil, err
	}
	var handedBack []int64
	if p.Take {
		note := store.HolderNote{
			Reason: FormatQueueSentence(protocol.QueueWaitingForMaterial, QueueParams{TakenByOrderID: o.ID}),
			Code:   protocol.QueueWaitingForMaterial,
			Cause:  string(CauseBinTaken),
			Actor:  actor,
		}
		if handedBack, err = store.TakeSoftHolds(tx, p.Bin.ID, o.ID, note); err != nil {
			return nil, nil, err
		}
	}
	// A move has no compound parent, so it owns its own lane rows.
	if err := reservations.Acquire(tx, o.ID, o.ID, p.Bin.ID, "reserveForDispatch"); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit bin move: %w", err)
	}
	return o, handedBack, nil
}

// CapacitySentence is the operator sentence for a blocked capacity verdict, as
// the preview says it.
func CapacitySentence(v plan.CapacityVerdict) string {
	return FormatQueueSentence(protocol.QueueWaitingForSlot, queueParamsOf(v.Params))
}
