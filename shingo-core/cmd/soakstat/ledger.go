package main

// ── THE BOOKS-OVER-TIME LEDGER ────────────────────────────────────────────────
//
// `soakstat -ledger FILE -sample N` appends ONE sample to FILE and exits. The rig
// calls it every 30 s; the report above keeps its own cadence. It replaces the
// rig's psql probe, which rendered a copy of orders.HoldsAtRestSQL.
//
// FORMAT: JSON lines, one object per line, appended. Every object has "rec" and
// "n" (the sample number the rig passes). These columns are fixed for every
// phase from B0 on; a new column is added, never renamed or removed.
//
// rec "sample": one per sample, first.
//   n           sample number
//   wall        wall clock, RFC3339 UTC with milliseconds
//   sim_s       sim seconds since the run's anchor (clock.ResolveAnchor, as Core
//               builds its clock); null unless the sim clock is anchored-running
//   statements  statements this sample cost against Core's database
//   dispatches  order_history rows into `dispatched`, re-dispatches included
//               (soakstat's PAIR count, sampled at the ledger's cadence)
//
// rec "order": one per LIVE order (status not terminal).
//   id, kind (order_type), status, queue_code, queue_cause, parent (null for a
//   top-level order), source, delivery, coordinated
//   fleet              the order has a fleet order id (vendor_order_id != '')
//   reserved           the SOFT book: PENDING reservation rows, by resource_kind
//                      {bin, slot, mouth, occupancy}
//   committed          the HARD book while the fleet is not yet moving (fleet
//                      false); claimed is the same book once it is (fleet true).
//                      The other of the two is null. Fields:
//     bins             bins.claimed_by = the order
//     nodes            nodes.claimed_by = the order
//     bin, slot, occupancy  CONFIRMED reservation rows of that kind
//     mouth            CONFIRMED mouth rows that are not a dig lock
//     dig_lock         CONFIRMED mouth rows tagged as a dig lock
//                      (orders.DigLockTags)
//   claimed            as committed
//   holds_at_rest      the orders.HoldsAtRestSQL predicate on this row:
//                      pre-dispatch, top-level, and bins+nodes+slot+mouth+
//                      occupancy > 0 in whichever book holds them (the SQL does
//                      not read the fleet id, so neither does this)
//   dig_lock_at_rest   the orders.DigLocksAtRestSQL predicate: pre-dispatch,
//                      top-level, dig_lock > 0
//   no_shuffle_slot    queue_cause is dispatch.CauseNoShuffleSlot
//
// rec "slot": one per storage slot: a node whose parent is a LANE-type node that
// has a parent (the group), which is soakstat's laneShapes spelling.
//   group, lane, node, depth
//   bin               label of the bin stored at the slot, "" if none
//   lifted            label of a bin that is in _TRANSIT and whose last audited
//                     move ("moved", node=<id>) set it down in this slot: the
//                     lift itself is not audited, so this is the bin a dig has
//                     in the air from here; "" if none
//   claimed_by        nodes.claimed_by, null if none
//   res_pending       order ids with a PENDING slot reservation on the slot
//   res_confirmed     order ids with a CONFIRMED slot reservation on the slot
//   bare_dest         live order ids whose delivery_node is this slot
//   spoken_for        the slot is empty (no bin stored) and has a claim, an
//                     active slot reservation or a bare destination: the three
//                     spellings of helpers.EntombsASpokenForSlotSQL, owner-blind
//   entombing         a spoken-for slot sits deeper in the same lane (same
//                     parent, strictly greater depth), so a bin parked here would
//                     seal it: store.SlotsThatWouldEntombASpokenForSlot's rule
//   entombs           the names of those deeper spoken-for slots
//   lane_dig_lock     order ids holding a confirmed dig-lock mouth on the lane
//   lane_occupancy    order ids with an occupancy row on the lane, any state
//
// COST: one statement per table per sample, never one per order or per slot:
// orders, bins, nodes, reservations, the audit tail (the last move of each bin
// now in _TRANSIT) and order_history (the dispatch count). Six in all, counted
// on the "sample" record as they run. Every book is a reading of those rows in
// Go; the docker tests hold the readings equal to the SQL they restate
// (ListHoldsAtRest, ListDigLocksAtRest, SlotsThatWouldEntombASpokenForSlot).
// It reads only, through the same OpenWithoutMigrate handle as the report.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/config"
	"shingocore/dispatch"
	"shingocore/engine"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

type ledgerSample struct {
	Rec        string   `json:"rec"`
	N          int      `json:"n"`
	Wall       string   `json:"wall"`
	SimS       *float64 `json:"sim_s"`
	Statements int      `json:"statements"`
	Dispatches int      `json:"dispatches"`
}

type ledgerKinds struct {
	Bin       int `json:"bin"`
	Slot      int `json:"slot"`
	Mouth     int `json:"mouth"`
	Occupancy int `json:"occupancy"`
}

type ledgerHard struct {
	Bins      int `json:"bins"`
	Nodes     int `json:"nodes"`
	Bin       int `json:"bin"`
	Slot      int `json:"slot"`
	Mouth     int `json:"mouth"`
	Occupancy int `json:"occupancy"`
	DigLock   int `json:"dig_lock"`
}

// restHolds is what holds-at-rest counts: everything hard except a confirmed
// bin reservation (the claim column is the bin's hard hold) and a dig lock.
func (h ledgerHard) restHolds() int { return h.Bins + h.Nodes + h.Slot + h.Mouth + h.Occupancy }

type ledgerOrder struct {
	Rec           string      `json:"rec"`
	N             int         `json:"n"`
	ID            int64       `json:"id"`
	Kind          string      `json:"kind"`
	Status        string      `json:"status"`
	QueueCode     string      `json:"queue_code"`
	QueueCause    string      `json:"queue_cause"`
	Parent        *int64      `json:"parent"`
	Source        string      `json:"source"`
	Delivery      string      `json:"delivery"`
	Coordinated   bool        `json:"coordinated"`
	Fleet         bool        `json:"fleet"`
	Reserved      ledgerKinds `json:"reserved"`
	Committed     *ledgerHard `json:"committed"`
	Claimed       *ledgerHard `json:"claimed"`
	HoldsAtRest   bool        `json:"holds_at_rest"`
	DigLockAtRest bool        `json:"dig_lock_at_rest"`
	NoShuffleSlot bool        `json:"no_shuffle_slot"`
	hard          ledgerHard
}

type ledgerSlot struct {
	Rec           string   `json:"rec"`
	N             int      `json:"n"`
	Group         string   `json:"group"`
	Lane          string   `json:"lane"`
	Node          string   `json:"node"`
	Depth         *int     `json:"depth"`
	Bin           string   `json:"bin"`
	Lifted        string   `json:"lifted"`
	ClaimedBy     *int64   `json:"claimed_by"`
	ResPending    []int64  `json:"res_pending"`
	ResConfirmed  []int64  `json:"res_confirmed"`
	BareDest      []int64  `json:"bare_dest"`
	SpokenFor     bool     `json:"spoken_for"`
	Entombing     bool     `json:"entombing"`
	Entombs       []string `json:"entombs"`
	LaneDigLock   []int64  `json:"lane_dig_lock"`
	LaneOccupancy []int64  `json:"lane_occupancy"`
	id, laneID    int64
}

// ledgerRows is one sample: the header, then every live order and every slot.
type ledgerRows struct {
	sample ledgerSample
	orders []ledgerOrder
	slots  []ledgerSlot
}

// ledgerReader holds one sample while it is read: the query door, which counts
// every statement so the sample record's count is the count of what ran, and
// the rows each table read leaves for the readings after it.
type ledgerReader struct {
	db   *sql.DB
	n    int // statements sent
	out  *ledgerRows
	byID map[int64]*ledgerOrder

	nodes     map[int64]*ledgerNode
	nodeOrder []int64
	transitID int64
	storedAt  map[int64]string // node id -> label of the bin stored there
	binLabel  map[int64]string
	res       []ledgerRes
	lifted    map[int64]string // slot id -> label of the bin in the air from it
}

type ledgerNode struct {
	id       int64
	name     string
	parent   *int64
	depth    *int
	claimed  *int64
	typeCode string
}

type ledgerRes struct {
	order      int64
	kind       string
	state      string
	reservedBy string
	node       *int64
}

func (r *ledgerReader) query(q string, args ...any) (*sql.Rows, error) {
	r.n++
	return r.db.Query(q, args...)
}

var movedNode = regexp.MustCompile(`node=(\d+)`)

// takeLedgerSample reads one sample. It returns an error rather than a partial
// sample: a ledger row that silently lost a table is worse than a missing one.
func takeLedgerSample(db *sql.DB, n int, wall time.Time, simS *float64) (*ledgerRows, error) {
	r := &ledgerReader{
		db:       db,
		out:      &ledgerRows{sample: ledgerSample{Rec: "sample", N: n, Wall: wall.UTC().Format("2006-01-02T15:04:05.000Z"), SimS: simS}},
		byID:     map[int64]*ledgerOrder{},
		nodes:    map[int64]*ledgerNode{},
		storedAt: map[int64]string{},
		binLabel: map[int64]string{},
		lifted:   map[int64]string{},
	}
	for _, read := range []func() error{r.readOrders, r.readNodes, r.readBins, r.readReservations, r.readAuditTail, r.readDispatches} {
		if err := read(); err != nil {
			return nil, err
		}
	}
	r.out.sample.Statements = r.n
	r.orderBooks()
	r.slotRows()
	return r.out, nil
}

// readOrders: every live order.
func (r *ledgerReader) readOrders() error {
	rows, err := r.query(fmt.Sprintf(`
		SELECT id, order_type, status, COALESCE(queue_code, ''), COALESCE(queue_cause, ''), parent_order_id,
		       source_node, delivery_node, vendor_order_id <> '', coordinated
		FROM orders WHERE status NOT IN (%s) ORDER BY id`, protocol.TerminalStatusSQLList()))
	if err != nil {
		return fmt.Errorf("ledger orders: %w", err)
	}
	for rows.Next() {
		o := ledgerOrder{Rec: "order", N: r.out.sample.N}
		if err := rows.Scan(&o.ID, &o.Kind, &o.Status, &o.QueueCode, &o.QueueCause, &o.Parent,
			&o.Source, &o.Delivery, &o.Fleet, &o.Coordinated); err != nil {
			rows.Close()
			return fmt.Errorf("ledger orders scan: %w", err)
		}
		r.out.orders = append(r.out.orders, o)
	}
	if err := closeRows(rows, "orders"); err != nil {
		return err
	}
	for i := range r.out.orders {
		r.byID[r.out.orders[i].ID] = &r.out.orders[i]
	}
	return nil
}

// readNodes: the whole table, so a slot's lane and group, a claim anywhere and
// _TRANSIT all come from one read.
func (r *ledgerReader) readNodes() error {
	rows, err := r.query(`
		SELECT n.id, n.name, n.parent_id, n.depth, n.claimed_by, COALESCE(t.code, '')
		FROM nodes n LEFT JOIN node_types t ON t.id = n.node_type_id ORDER BY n.id`)
	if err != nil {
		return fmt.Errorf("ledger nodes: %w", err)
	}
	for rows.Next() {
		var nd ledgerNode
		if err := rows.Scan(&nd.id, &nd.name, &nd.parent, &nd.depth, &nd.claimed, &nd.typeCode); err != nil {
			rows.Close()
			return fmt.Errorf("ledger nodes scan: %w", err)
		}
		r.nodes[nd.id] = &nd
		r.nodeOrder = append(r.nodeOrder, nd.id)
		if nd.name == "_TRANSIT" {
			r.transitID = nd.id
		}
		if nd.claimed != nil {
			if o := r.byID[*nd.claimed]; o != nil {
				o.hard.Nodes++
			}
		}
	}
	return closeRows(rows, "nodes")
}

// readBins: every bin, for the claim column, the stored bin per slot, and the
// labels of the bins in the air.
func (r *ledgerReader) readBins() error {
	rows, err := r.query(`SELECT id, label, node_id, claimed_by FROM bins ORDER BY id`)
	if err != nil {
		return fmt.Errorf("ledger bins: %w", err)
	}
	for rows.Next() {
		var id int64
		var label string
		var node, claimed *int64
		if err := rows.Scan(&id, &label, &node, &claimed); err != nil {
			rows.Close()
			return fmt.Errorf("ledger bins scan: %w", err)
		}
		r.binLabel[id] = label
		if node != nil {
			r.storedAt[*node] = label
		}
		if claimed != nil {
			if o := r.byID[*claimed]; o != nil {
				o.hard.Bins++
			}
		}
	}
	return closeRows(rows, "bins")
}

// readReservations: every row. The live orders' soft and hard counts are
// booked here; the slot and lane holders are read off r.res by slotHolders.
func (r *ledgerReader) readReservations() error {
	rows, err := r.query(`SELECT order_id, resource_kind, state, COALESCE(reserved_by, ''), node_id FROM reservations ORDER BY id`)
	if err != nil {
		return fmt.Errorf("ledger reservations: %w", err)
	}
	for rows.Next() {
		var x ledgerRes
		if err := rows.Scan(&x.order, &x.kind, &x.state, &x.reservedBy, &x.node); err != nil {
			rows.Close()
			return fmt.Errorf("ledger reservations scan: %w", err)
		}
		r.res = append(r.res, x)
		if o := r.byID[x.order]; o != nil {
			bookReservation(o, x)
		}
	}
	return closeRows(rows, "reservations")
}

// digLockTag is orders.DigLockTags as a set.
var digLockTag = func() map[string]bool {
	m := map[string]bool{}
	for _, t := range orders.DigLockTags {
		m[t] = true
	}
	return m
}()

// bookReservation counts one reservation row into its order's books: pending
// is soft, confirmed is hard, and a confirmed mouth tagged as a dig lock is
// counted apart from the other mouths.
func bookReservation(o *ledgerOrder, x ledgerRes) {
	pending := x.state == string(reservations.StatePending)
	switch reservations.Kind(x.kind) {
	case reservations.KindBin:
		if pending {
			o.Reserved.Bin++
		} else {
			o.hard.Bin++
		}
	case reservations.KindSlot:
		if pending {
			o.Reserved.Slot++
		} else {
			o.hard.Slot++
		}
	case reservations.KindMouth:
		switch {
		case pending:
			o.Reserved.Mouth++
		case digLockTag[x.reservedBy]:
			o.hard.DigLock++
		default:
			o.hard.Mouth++
		}
	case reservations.KindOccupancy:
		if pending {
			o.Reserved.Occupancy++
		} else {
			o.hard.Occupancy++
		}
	}
}

// readAuditTail: the last move of each bin now in _TRANSIT, which names the
// slot it was lifted from.
func (r *ledgerReader) readAuditTail() error {
	rows, err := r.query(`
		SELECT DISTINCT ON (a.entity_id) a.entity_id, a.new_value
		FROM audit_log a
		WHERE a.entity_type = 'bin' AND a.action = $1
		  AND a.entity_id IN (SELECT b.id FROM bins b WHERE b.node_id = $2)
		ORDER BY a.entity_id, a.id DESC`, string(engine.BinActionMoved), r.transitID)
	if err != nil {
		return fmt.Errorf("ledger audit tail: %w", err)
	}
	for rows.Next() {
		var bin int64
		var v string
		if err := rows.Scan(&bin, &v); err != nil {
			rows.Close()
			return fmt.Errorf("ledger audit tail scan: %w", err)
		}
		if m := movedNode.FindStringSubmatch(v); m != nil {
			if slot, err := strconv.ParseInt(m[1], 10, 64); err == nil {
				r.lifted[slot] = r.binLabel[bin]
			}
		}
	}
	return closeRows(rows, "audit tail")
}

// readDispatches: order_history's dispatch count.
func (r *ledgerReader) readDispatches() error {
	rows, err := r.query(`SELECT COUNT(*) FROM order_history WHERE status = $1`, string(protocol.StatusDispatched))
	if err != nil {
		return fmt.Errorf("ledger dispatches: %w", err)
	}
	for rows.Next() {
		if err := rows.Scan(&r.out.sample.Dispatches); err != nil {
			rows.Close()
			return fmt.Errorf("ledger dispatches scan: %w", err)
		}
	}
	return closeRows(rows, "dispatches")
}

// orderBooks files each order's hard holds in its book and reads the at-rest
// predicates off them.
func (r *ledgerReader) orderBooks() {
	for i := range r.out.orders {
		o := &r.out.orders[i]
		h := o.hard
		if o.Fleet {
			o.Claimed = &h
		} else {
			o.Committed = &h
		}
		atRest := protocol.IsPreDispatch(protocol.Status(o.Status)) && o.Parent == nil
		o.HoldsAtRest = atRest && h.restHolds() > 0
		o.DigLockAtRest = atRest && h.DigLock > 0
		o.NoShuffleSlot = o.QueueCause == string(dispatch.CauseNoShuffleSlot)
	}
}

// slotRows builds one row per storage slot, then its holders, then the
// spoken-for and entombing readings.
func (r *ledgerReader) slotRows() {
	bareDest := map[string][]int64{}
	for _, o := range r.out.orders {
		if o.Delivery != "" {
			bareDest[o.Delivery] = append(bareDest[o.Delivery], o.ID)
		}
	}
	for _, id := range r.nodeOrder {
		s := r.nodes[id]
		if s.parent == nil {
			continue
		}
		lane := r.nodes[*s.parent]
		if lane == nil || lane.typeCode != "LANE" || lane.parent == nil || r.nodes[*lane.parent] == nil {
			continue
		}
		r.out.slots = append(r.out.slots, ledgerSlot{
			Rec: "slot", N: r.out.sample.N, Group: r.nodes[*lane.parent].name, Lane: lane.name, Node: s.name,
			Depth: s.depth, Bin: r.storedAt[s.id], Lifted: r.lifted[s.id], ClaimedBy: s.claimed,
			BareDest: bareDest[s.name], id: s.id, laneID: lane.id,
		})
	}
	r.slotHolders()
	laneSlots := map[int64][]*ledgerSlot{}
	for i := range r.out.slots {
		s := &r.out.slots[i]
		s.SpokenFor = s.Bin == "" && (s.ClaimedBy != nil || len(s.ResPending)+len(s.ResConfirmed) > 0 || len(s.BareDest) > 0)
		laneSlots[s.laneID] = append(laneSlots[s.laneID], s)
	}
	// Entombing: a spoken-for slot deeper in the same lane. The SQL compares
	// siblings by parent_id; a slot's siblings are its lane's slots.
	for i := range r.out.slots {
		s := &r.out.slots[i]
		if s.Depth == nil {
			continue
		}
		for _, deep := range laneSlots[s.laneID] {
			if deep.id != s.id && deep.SpokenFor && deep.Depth != nil && *s.Depth < *deep.Depth {
				s.Entombs = append(s.Entombs, deep.Node)
			}
		}
		sort.Strings(s.Entombs)
		s.Entombing = len(s.Entombs) > 0
	}
}

// slotHolders reads the reservation rows onto the slots: slot reservations on
// the slot, dig-lock mouths and occupancy rows on its lane.
func (r *ledgerReader) slotHolders() {
	slotIdx := map[int64]*ledgerSlot{}
	for i := range r.out.slots {
		slotIdx[r.out.slots[i].id] = &r.out.slots[i]
	}
	laneDig, laneOcc := map[int64][]int64{}, map[int64][]int64{}
	for _, x := range r.res {
		if x.node == nil {
			continue
		}
		switch reservations.Kind(x.kind) {
		case reservations.KindSlot:
			if s := slotIdx[*x.node]; s != nil {
				if x.state == string(reservations.StatePending) {
					s.ResPending = append(s.ResPending, x.order)
				} else {
					s.ResConfirmed = append(s.ResConfirmed, x.order)
				}
			}
		case reservations.KindMouth:
			if x.state == string(reservations.StateConfirmed) && digLockTag[x.reservedBy] {
				laneDig[*x.node] = append(laneDig[*x.node], x.order)
			}
		case reservations.KindOccupancy:
			laneOcc[*x.node] = append(laneOcc[*x.node], x.order)
		}
	}
	for i := range r.out.slots {
		s := &r.out.slots[i]
		s.LaneDigLock = laneDig[s.laneID]
		s.LaneOccupancy = laneOcc[s.laneID]
	}
}

func closeRows(rows *sql.Rows, what string) error {
	err := rows.Err()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("ledger %s: %w", what, err)
	}
	return nil
}

// write appends the sample as JSON lines: the sample record, the orders, the
// slots.
func (l *ledgerRows) write(w io.Writer) error {
	enc := json.NewEncoder(w)
	if err := enc.Encode(l.sample); err != nil {
		return err
	}
	for _, o := range l.orders {
		if err := enc.Encode(o); err != nil {
			return err
		}
	}
	for _, s := range l.slots {
		if err := enc.Encode(s); err != nil {
			return err
		}
	}
	return nil
}

// simSeconds is sim seconds since the run's anchor, built the way Core builds
// its clock (cmd/shingocore: ResolveAnchor, then BuildSimClock). nil unless the
// clock is anchored and running, the rig's mode: in any other mode "since the
// anchor" has no meaning.
func simSeconds(cfg *config.Config) *float64 {
	if !cfg.Sim.Enabled {
		return nil
	}
	epoch, anchor, _, err := clock.ResolveAnchor(cfg.Sim.Epoch, cfg.Sim.AnchorWall)
	if err != nil {
		return nil
	}
	clk, mode := clock.BuildSimClock(epoch, anchor, cfg.Sim.Speed, cfg.Sim.MaxSpeed)
	if mode != clock.SimSyncedRunning {
		return nil
	}
	s := clk.Now().Sub(epoch).Seconds()
	return &s
}

// appendLedger takes one sample now and appends it to path.
func appendLedger(db *sql.DB, path string, n int, simS *float64) error {
	l, err := takeLedgerSample(db, n, time.Now(), simS)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := l.write(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
