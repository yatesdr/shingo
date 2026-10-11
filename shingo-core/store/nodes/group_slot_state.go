package nodes

import (
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"shingo/protocol"
	"shingocore/store/internal/helpers"
	"shingocore/store/reservations"
)

// group_slot_state.go — a node group's slot state, read ONCE per gate event
// (RULINGS-C2c-run-through §3.3: "the group's slot state is read once per
// event: the per-pass cache serves every waiter after the first at 0 reads").
//
// The store selector's questions — which slot of a lane may this order put a
// bin on (findStoreSlot), which slots are free but for a stranger's paper
// (PaperSlotsInGroup), which of the group's children does no dig keep this
// order out of (ListChildNodesUnlocked) — each name the ASKER, so a pass that
// asked them per waiter read the group once per waiter. What the asker changes
// is only who is exempt: its own claim, its own row, its own delivery, its own
// dig, and whether it is a robot the fleet has (the burial guard's committed
// arm). So the group is read once with every party named, and the questions
// are answered per asker in Go, by the same predicates spelled over the rows:
//
//	SlotTakeableSQL      no bin; no hard claim but the owner's; no active slot
//	                     row but the owner's; no live dispatched order but the
//	                     owner naming it as its delivery
//	ReachableSQL         nothing occupied in front of it
//	storeBurialSQL       no live party but the owner coming for a bin behind it
//	                     (only parties already moving, for a committed owner)
//	DigExclusionSQL      no dig the asker does not own holding the child
//
// A release inside the pass takes a slot; it moves no bin. NoteReleased writes
// that take into the rows, so the next waiter in the pass sees it without a
// read. The equality with the SQL selectors is pinned by
// TestGroupSlotState_AnswersAsTheSelectorsDo (store, docker).

// GroupSlotState is one read of a group: every child of it and every slot of
// its lanes, with every party that holds, names, digs or is coming for one.
type GroupSlotState struct {
	GroupID int64
	rows    []*slotStateRow
	byID    map[int64]*slotStateRow
}

// slotStateRow is one node of the group's read.
type slotStateRow struct {
	node *Node
	// digHolders are the orders whose active dig-mode mouth row holds the node
	// (a lane): ListChildNodesUnlocked's exclusion, asker-free.
	digHolders []int64
	bins       int
	// The node's one active slot row (uq_reservations_slot_active), if any.
	resOrder   int64
	resPending bool
	resTag     string
	// resSent says the row's holder is with the fleet (not AwaitingFleet); the
	// rank columns are the demand it presents (DemandRankColumnsSQL).
	resSent      bool
	resPriority  int
	resCreatedAt time.Time
	resDemand    int64
	// namers are the live dispatched orders naming the node as delivery_node.
	namers    []int64
	reachable bool
	// coming are the live orders coming for a bin deeper in the node's lane
	// (ComingForBinSQL); moving the same as a committed robot reads it
	// (ComingForBinCommittedSQL).
	coming []int64
	moving []int64
	// nameOrder is the node's place in ORDER BY name, as the database collates
	// it (ListChildNodesUnlocked's order, which DPTH's first lane and a
	// like-kind tie read).
	nameOrder int64
}

// ReadGroupSlotState reads groupID's slot state in one statement.
func ReadGroupSlotState(db *sql.DB, groupID int64) (*GroupSlotState, error) {
	agg := func(expr, from string) string {
		return `COALESCE((SELECT string_agg(DISTINCT (` + expr + `)::text, ',') ` + from + `), '')`
	}
	coming := func(arm string) string {
		return agg("holder.id", `FROM nodes held
			JOIN bins held_bin ON held_bin.node_id = held.id
			JOIN orders holder ON `+arm+`
			WHERE holder.status NOT IN (`+protocol.TerminalStatusSQLList()+`)
			  AND `+helpers.ShallowerInSameLane("n", "held"))
	}
	// The leading comment names the read for a statement counter
	// (TestBudget_C2cGateEventLikeForLike counts it once per event).
	q := fmt.Sprintf(`/* group-slot-state */ SELECT %s,
		%s,
		(SELECT COUNT(*) FROM bins sb WHERE sb.node_id = n.id),
		COALESCE(r.order_id, 0), COALESCE(r.state = '%s', false), COALESCE(r.reserved_by, ''),
		COALESCE(NOT (%s), false), COALESCE(%s, 0), COALESCE(%s, 'epoch'::timestamptz), COALESCE(%s, 0),
		%s,
		%s,
		%s,
		%s,
		ROW_NUMBER() OVER (ORDER BY n.name)
		%s
		LEFT JOIN reservations r ON r.node_id = n.id AND r.resource_kind = '%s' AND %s
		LEFT JOIN orders ho ON ho.id = r.order_id
		LEFT JOIN orders hp ON hp.id = ho.parent_order_id
		WHERE n.parent_id = $1 OR pn.parent_id = $1`,
		SelectCols,
		agg("dh.order_id", `FROM reservations dh WHERE dh.resource_kind = 'mouth' AND dh.node_id = n.id
			AND `+reservations.ActiveStateSQL("dh.")+` AND dh.mode = $2`),
		string(reservations.StatePending),
		helpers.AwaitingFleetSQL("ho"),
		"COALESCE(hp.priority, ho.priority)", "COALESCE(hp.created_at, ho.created_at)", "COALESCE(hp.id, ho.id)",
		agg("nm.id", `FROM orders nm WHERE nm.delivery_node = n.name
			AND nm.status NOT IN (`+protocol.TerminalStatusSQLList()+`, `+protocol.PreDispatchStatusSQLList()+`)`),
		helpers.ReachableSQL("n"),
		coming(helpers.ComingForBinSQL("holder", "held_bin", "held")),
		coming(helpers.ComingForBinCommittedSQL("holder", "held_bin", "held")),
		FromClause,
		string(reservations.KindSlot), reservations.ActiveStateSQL("r."))
	rows, err := db.Query(q, groupID, string(reservations.ModeDig))
	if err != nil {
		return nil, fmt.Errorf("group slot state %d: %w", groupID, err)
	}
	defer rows.Close()
	s := &GroupSlotState{GroupID: groupID, byID: map[int64]*slotStateRow{}}
	for rows.Next() {
		var r slotStateRow
		var dig, namers, coming, moving string
		n, err := ScanNode(withTrailingCols{rows, []any{&dig, &r.bins, &r.resOrder, &r.resPending, &r.resTag,
			&r.resSent, &r.resPriority, &r.resCreatedAt, &r.resDemand, &namers, &r.reachable, &coming, &moving, &r.nameOrder}})
		if err != nil {
			return nil, fmt.Errorf("scan group slot state %d: %w", groupID, err)
		}
		r.node = n
		for dst, src := range map[*[]int64]string{&r.digHolders: dig, &r.namers: namers, &r.coming: coming, &r.moving: moving} {
			if *dst, err = parseIDList(src); err != nil {
				return nil, fmt.Errorf("group slot state %d: %w", groupID, err)
			}
		}
		s.rows = append(s.rows, &r)
		s.byID[n.ID] = &r
	}
	return s, rows.Err()
}

// parseIDList reads string_agg's comma-joined ids ("" for none).
func parseIDList(s string) ([]int64, error) {
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		id, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("id list %q: %w", s, err)
		}
		out = append(out, id)
	}
	return out, nil
}

// onlyBy reports whether every id is owner (none at all is true).
func onlyBy(ids []int64, owner int64) bool {
	for _, id := range ids {
		if id != owner {
			return false
		}
	}
	return true
}

// HasLane reports whether laneID is a child of the group the state read.
func (s *GroupSlotState) HasLane(laneID int64) bool {
	r := s.byID[laneID]
	return r != nil && r.node.ParentID != nil && *r.node.ParentID == s.GroupID
}

// slotsOf is the rows whose parent is parentID.
func (s *GroupSlotState) slotsOf(parentID int64) []*slotStateRow {
	var out []*slotStateRow
	for _, r := range s.rows {
		if r.node.ParentID != nil && *r.node.ParentID == parentID {
			out = append(out, r)
		}
	}
	return out
}

// takeable is SlotTakeableSQL over the row, with except's pending row and
// delivery set aside (slotTakeable's pendingHolderExpr; 0 sets nothing aside).
func (r *slotStateRow) takeable(owner, except int64) bool {
	if r.bins > 0 || (r.node.ClaimedBy != nil && *r.node.ClaimedBy != owner) {
		return false
	}
	if r.resOrder != 0 && r.resOrder != owner && !(except != 0 && r.resOrder == except && r.resPending) {
		return false
	}
	for _, id := range r.namers {
		if id != owner && id != except {
			return false
		}
	}
	return true
}

// committed is storeBurialSQL's committed arm for owner: the fleet has it and
// it holds a slot row on a child or a lane slot of this group
// (reservations.SlotInGroupOfLaneSQL).
func (s *GroupSlotState) committed(owner int64) bool {
	if owner == 0 {
		return false
	}
	for _, r := range s.rows {
		if r.resOrder == owner {
			return r.resSent
		}
	}
	return false
}

// guarded is storeBurialSQL over the row for owner: true when a live party
// other than owner is coming for a bin behind it.
func (r *slotStateRow) guarded(owner int64, committed bool) bool {
	if committed {
		return !onlyBy(r.moving, owner)
	}
	return !onlyBy(r.coming, owner)
}

// StoreSlot answers FindStoreSlotInLaneExcluding(laneID, owner) from the read:
// the deepest slot owner may put a bin on, reachable, not in front of a bin a
// live party is coming for; nil with open true when the burial guard alone
// refused (ErrLaneClosedByClaim's question).
func (s *GroupSlotState) StoreSlot(laneID, owner int64) (slot *Node, open bool) {
	committed := s.committed(owner)
	var best *Node
	for _, r := range s.slotsOf(laneID) {
		if r.node.IsSynthetic || !r.node.Enabled || !r.takeable(owner, 0) || !r.reachable {
			continue
		}
		open = true
		if r.guarded(owner, committed) {
			continue
		}
		if best == nil || nodeDepthOf(r.node) > nodeDepthOf(best) {
			best = r.node
		}
	}
	return best, open
}

func nodeDepthOf(n *Node) int {
	if n.Depth == nil {
		return 0
	}
	return *n.Depth
}

// PaperSlots answers PaperSlotsInGroup(group, owner) from the read: every lane
// slot free but for one stranger's pending groupRoom row, fenced as a free slot
// is, by lane then deepest first.
func (s *GroupSlotState) PaperSlots(owner int64) []PaperSlot {
	committed := s.committed(owner)
	var out []PaperSlot
	for _, r := range s.rows {
		lane := s.parentLane(r)
		if lane == 0 || r.node.IsSynthetic || !r.node.Enabled || r.resOrder == 0 || r.resOrder == owner ||
			!r.resPending || r.resTag != reservations.GroupRoomTag || !r.takeable(owner, r.resOrder) ||
			!r.reachable || r.guarded(owner, committed) {
			continue
		}
		out = append(out, PaperSlot{Slot: r.node, LaneID: lane, Holder: r.resOrder, HolderSent: r.resSent,
			HolderPriority: r.resPriority, HolderCreatedAt: r.resCreatedAt, HolderDemand: r.resDemand})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].LaneID != out[j].LaneID {
			return out[i].LaneID < out[j].LaneID
		}
		return nodeDepthOf(out[i].Slot) > nodeDepthOf(out[j].Slot)
	})
	return out
}

// parentLane is the lane id r stands in when its parent is a child of the
// group (pn.parent_id = group), or 0.
func (s *GroupSlotState) parentLane(r *slotStateRow) int64 {
	if r.node.ParentID == nil || *r.node.ParentID == s.GroupID {
		return 0
	}
	if p := s.byID[*r.node.ParentID]; p != nil && p.node.ParentID != nil && *p.node.ParentID == s.GroupID {
		return p.node.ID
	}
	return 0
}

// ChildrenUnlocked answers ListChildNodesUnlocked(group, asker) from the read:
// the group's children no dig keeps asker out of, in the database's name order.
func (s *GroupSlotState) ChildrenUnlocked(asker reservations.DigAsker) []*Node {
	var rows []*slotStateRow
	for _, r := range s.slotsOf(s.GroupID) {
		shut := false
		for _, h := range r.digHolders {
			if asker.ExcludedBy(h) {
				shut = true
				break
			}
		}
		if !shut {
			rows = append(rows, r)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].nameOrder < rows[j].nameOrder })
	out := make([]*Node, len(rows))
	for i, r := range rows {
		out[i] = r.node
	}
	return out
}

// LaneSlots answers ListLaneSlots(laneID): the lane's slots, shallowest first.
func (s *GroupSlotState) LaneSlots(laneID int64) []*Node {
	var out []*Node
	for _, r := range s.slotsOf(laneID) {
		out = append(out, r.node)
	}
	sort.SliceStable(out, func(i, j int) bool { return nodeDepthOf(out[i]) < nodeDepthOf(out[j]) })
	return out
}

// LaneBins answers CountBinsInLane(laneID).
func (s *GroupSlotState) LaneBins(laneID int64) int {
	n := 0
	for _, r := range s.slotsOf(laneID) {
		n += r.bins
	}
	return n
}

// NoteReleased writes a release the pass made into the read: orderID took
// slotID as armor (its row confirmed there, its claim on it), taking it from
// any paper holder, and let go of leftID. final says the drop is the plan's
// last, so its delivery_node now names slotID and no longer the slot it left
// (patchChosenDrop's applyDeliveryNodeAtStep); an interior drop's delivery is
// the trip's end and does not move. No bin moved, so nothing else changes.
func (s *GroupSlotState) NoteReleased(orderID, slotID, leftID int64, final bool) {
	if final {
		for _, r := range s.rows {
			kept := r.namers[:0]
			for _, id := range r.namers {
				if id != orderID {
					kept = append(kept, id)
				}
			}
			r.namers = kept
		}
	}
	if left := s.byID[leftID]; left != nil && leftID != slotID {
		if left.node.ClaimedBy != nil && *left.node.ClaimedBy == orderID {
			left.node.ClaimedBy = nil
		}
		if left.resOrder == orderID {
			left.resOrder, left.resPending, left.resTag = 0, false, ""
		}
	}
	if r := s.byID[slotID]; r != nil {
		id := orderID
		r.node.ClaimedBy = &id
		r.resOrder, r.resPending, r.resSent = orderID, false, true
		if r.resTag == "" {
			r.resTag = reservations.GroupRoomTag
		}
		if final {
			r.namers = append(r.namers, orderID)
		}
	}
}

// NoteGivenBack writes a release's give-back into the read: orderID's armor on
// slotID is paper again (DB.GiveBackReleaseDrop).
func (s *GroupSlotState) NoteGivenBack(orderID, slotID int64) {
	if r := s.byID[slotID]; r != nil {
		if r.node.ClaimedBy != nil && *r.node.ClaimedBy == orderID {
			r.node.ClaimedBy = nil
		}
		if r.resOrder == orderID {
			r.resPending = true
		}
	}
}
