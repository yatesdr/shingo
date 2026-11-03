package store

import (
	"database/sql"
	"fmt"
	"log"
	"strings"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/store/schema"
)

// order_carts_migration.go — v173: an order's carts are order_bins rows, one per
// LIFT (the plan step that picks the cart up, the plan step that sets it down),
// for every order that carries a cart, kept after the order ends. orders.bin_id
// stays until v174, which drops it once no reader is left.
//
// THE SHAPE. step_index was always the pickup: it becomes pick_step, and
// drop_step is new. Both are NULL only on a row carried over from orders.bin_id
// for an order that ended before the rows existed; drop_step alone is NULL on
// an OPEN lift (picked, its set-down not planned yet: a dwelling dig leg until
// its release binds the tail), and pick_step is -1 for a cart already in the
// robot's hands when the plan starts (domain.InHand). The check holds a drop
// only with a pick, and the pick before the drop. (order_id, bin_id) stops
// being unique — a relayed cart is lifted twice in one order — and (order_id,
// pick_step) and (order_id, drop_step) become so. The by-bin index becomes
// (bin_id, order_id DESC): "the last order of this cart" is its first entry.
//
// THE DATA. A live order's rows come from the plan walk (domain.Lifts) the
// allocator writes them with: a multi-cart order's existing rows gain their
// drop steps and its relays gain rows; a live single-cart order gets its row
// from its one non-relay pickup (the implied plan's step 0 for a plain order).
// An ended order with a bin_id and no rows gets one row with no steps, so the
// last order of every cart stays on record.
//
// IT LISTS, IT NEVER DECIDES. What the walk cannot place is logged at boot as
// a census line ("store: v173 list <list>: ...") and left as it stands; a row
// the shape's step check cannot hold keeps the check NOT VALID until the list is
// empty. The lists:
//
//	live_pick_step_ambiguous  a live single-cart order whose plan has no one non-relay pickup: no row; it keeps working on bin_id until v174
//	live_row_without_drop     a live order's row whose pick the walk never sets down (a stale allocation): kept, drop_step NULL
//	live_claim_without_row    a cart a live order claims with no row of that order's
//	live_row_without_claim    a dispatched, undelivered order's row whose cart it does not claim
//	live_two_in_hand          a live order with two carts claimed on _TRANSIT or a robot (finding 6's state)
//	plan_final_drop_disagrees a live order whose plan's last dropoff is not its delivery_node (StepNode's premise, measured)
//	dest_node_drift           a row whose dest_node is not the walk's final drop for its cart (D2's population)
//	ended_row_without_drop    an ended order's row the end failed to delete, whose pick the walk never sets down: kept, drop_step NULL
//	ended_rows_carried_over   the count of ended orders given a row with no steps
//
// RE-RUNNABLE: each step finds its own work done (a renamed column, a filled
// drop, an existing row, an existing index).

// v173Finding is one census line.
type v173Finding struct {
	list    string
	orderID int64
	binID   int64
	detail  string
}

func v173OrderCarts(tx *sql.Tx) error {
	if err := v173Shape(tx); err != nil {
		return err
	}
	findings, err := v173LiveRows(tx)
	if err != nil {
		return err
	}
	carried, err := v173CarryEnded(tx)
	if err != nil {
		return err
	}
	more, err := v173BooksCensus(tx)
	if err != nil {
		return err
	}
	findings = append(findings, more...)
	if more, err = v173FinalDrops(tx); err != nil {
		return err
	}
	findings = append(findings, more...)
	if err := v173Constraints(tx); err != nil {
		return err
	}
	counts := map[string]int{}
	for _, f := range findings {
		counts[f.list]++
		log.Printf("store: v173 list %s: order %d bin %d — %s", f.list, f.orderID, f.binID, f.detail)
	}
	var summary []string
	for _, l := range v173Lists {
		summary = append(summary, fmt.Sprintf("%s=%d", l, counts[l]))
	}
	summary = append(summary, fmt.Sprintf("ended_rows_carried_over=%d", carried))
	log.Printf("store: v173 an order's carts: %s", strings.Join(summary, " "))
	return nil
}

// v173Lists is the census's lists in the order the summary prints them.
var v173Lists = []string{
	"live_pick_step_ambiguous", "live_row_without_drop", "live_claim_without_row",
	"live_row_without_claim", "live_two_in_hand", "plan_final_drop_disagrees", "dest_node_drift", "ended_row_without_drop",
}

// v173Shape is the DDL before the data: the rename, the nullable pick, the drop
// column, and the end of one-row-per-cart.
func v173Shape(tx *sql.Tx) error {
	if schema.ColumnExists(tx, "order_bins", "step_index") {
		if _, err := tx.Exec(`ALTER TABLE order_bins RENAME COLUMN step_index TO pick_step`); err != nil {
			return fmt.Errorf("v173: rename step_index: %w", err)
		}
	}
	for _, q := range []string{
		`ALTER TABLE order_bins ALTER COLUMN pick_step DROP NOT NULL`,
		`ALTER TABLE order_bins ADD COLUMN IF NOT EXISTS drop_step INT`,
		`DROP INDEX IF EXISTS order_bins_order_bin_uniq`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("v173: %s: %w", q, err)
		}
	}
	return nil
}

// v173Order is a live order as the walk reads it.
type v173Order struct {
	o     domain.Order
	bin   int64 // orders.bin_id as it stood (the column v174 drops), 0 for none
	rows  []v173Row
	claim map[int64]bool // carts the order claims
	live  bool           // the order has not ended
}

type v173Row struct {
	id         int64
	binID      int64
	pick, drop sql.NullInt64
	destNode   string
}

// v173LiveRows walks every live order that carries a cart (a bin_id or a row)
// and writes the rows the walk finds; what it cannot place it lists.
func v173LiveRows(tx *sql.Tx) ([]v173Finding, error) {
	live, err := v173ReadLive(tx)
	if err != nil {
		return nil, err
	}
	var findings []v173Finding
	for _, lo := range live {
		f, err := v173WalkOne(tx, lo)
		if err != nil {
			return nil, err
		}
		findings = append(findings, f...)
	}
	return findings, nil
}

// v173ReadLive reads the orders the walk writes rows for: every live order that
// carries a cart, and every ENDED order that kept a junction row the end failed
// to delete (a pick and no drop; real plant dumps carry them):
// its plan is as true as a live one's, so its drops come from the same walk.
func v173ReadLive(tx *sql.Tx) ([]*v173Order, error) {
	rows, err := tx.Query(fmt.Sprintf(`SELECT o.id, COALESCE(o.steps_json, ''), COALESCE(o.source_node, ''),
		       COALESCE(o.delivery_node, ''), o.bin_id, o.status NOT IN (%[1]s)
		  FROM orders o
		 WHERE (o.status NOT IN (%[1]s)
		        AND (o.bin_id IS NOT NULL OR EXISTS (SELECT 1 FROM order_bins ob WHERE ob.order_id = o.id)))
		    OR (o.status IN (%[1]s)
		        AND EXISTS (SELECT 1 FROM order_bins ob WHERE ob.order_id = o.id AND ob.pick_step IS NOT NULL AND ob.drop_step IS NULL))
		 ORDER BY o.id`, protocol.TerminalStatusSQLList()))
	if err != nil {
		return nil, fmt.Errorf("v173: read live orders: %w", err)
	}
	byID := map[int64]*v173Order{}
	var out []*v173Order
	for rows.Next() {
		lo := &v173Order{claim: map[int64]bool{}}
		var bin sql.NullInt64
		if err := rows.Scan(&lo.o.ID, &lo.o.StepsJSON, &lo.o.SourceNode, &lo.o.DeliveryNode, &bin, &lo.live); err != nil {
			rows.Close()
			return nil, fmt.Errorf("v173: scan a live order: %w", err)
		}
		lo.bin = bin.Int64
		byID[lo.o.ID] = lo
		out = append(out, lo)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("v173: read live orders: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	ids := make([]int64, len(out))
	for i, lo := range out {
		ids[i] = lo.o.ID
	}
	rr, err := tx.Query(`SELECT ob.order_id, ob.id, ob.bin_id, ob.pick_step, ob.drop_step, ob.dest_node
		  FROM order_bins ob WHERE ob.order_id = ANY($1::bigint[])
		 ORDER BY ob.order_id, ob.id`, ids)
	if err != nil {
		return nil, fmt.Errorf("v173: read live rows: %w", err)
	}
	for rr.Next() {
		var orderID int64
		var r v173Row
		if err := rr.Scan(&orderID, &r.id, &r.binID, &r.pick, &r.drop, &r.destNode); err != nil {
			rr.Close()
			return nil, fmt.Errorf("v173: scan a live row: %w", err)
		}
		if lo := byID[orderID]; lo != nil {
			lo.rows = append(lo.rows, r)
		}
	}
	rr.Close()
	if err := rr.Err(); err != nil {
		return nil, fmt.Errorf("v173: read live rows: %w", err)
	}
	cr, err := tx.Query(fmt.Sprintf(`SELECT b.claimed_by, b.id FROM bins b JOIN orders o ON o.id = b.claimed_by
		 WHERE o.status NOT IN (%s)`, protocol.TerminalStatusSQLList()))
	if err != nil {
		return nil, fmt.Errorf("v173: read live claims: %w", err)
	}
	for cr.Next() {
		var orderID, binID int64
		if err := cr.Scan(&orderID, &binID); err != nil {
			cr.Close()
			return nil, fmt.Errorf("v173: scan a live claim: %w", err)
		}
		if lo := byID[orderID]; lo != nil {
			lo.claim[binID] = true
		}
	}
	cr.Close()
	return out, cr.Err()
}

// v173WalkOne writes one live order's rows from the walk.
func v173WalkOne(tx *sql.Tx, lo *v173Order) ([]v173Finding, error) {
	steps := domain.OrderPlan(&lo.o)
	claimedAt := map[int]int64{}
	for _, r := range lo.rows {
		if r.pick.Valid {
			claimedAt[int(r.pick.Int64)] = r.binID
		}
	}
	var findings []v173Finding
	if len(lo.rows) == 0 {
		// A single-cart order: its one non-relay pickup lifts its cart.
		pick, ok := v173OnePick(steps)
		if !ok {
			return []v173Finding{{list: "live_pick_step_ambiguous", orderID: lo.o.ID, binID: lo.bin,
				detail: fmt.Sprintf("%d non-relay pickups in a plan of %d steps; keeps working on bin_id", pick, len(steps))}}, nil
		}
		claimedAt[pick] = lo.bin
	}
	lifts := domain.Lifts(steps, claimedAt)
	final := map[int64]string{}
	for _, c := range lifts {
		if c.DropStep != nil {
			final[c.BinID] = steps[*c.DropStep].Node
		}
	}
	for _, c := range lifts {
		if err := v173WriteLift(tx, lo, c, final[c.BinID]); err != nil {
			return nil, err
		}
	}
	lifted := map[int]bool{}
	for _, c := range lifts {
		lifted[*c.PickStep] = true
	}
	withoutDrop := "live_row_without_drop"
	if !lo.live {
		withoutDrop = "ended_row_without_drop"
	}
	for _, r := range lo.rows {
		switch {
		case !r.pick.Valid:
		case !lifted[int(r.pick.Int64)]:
			findings = append(findings, v173Finding{list: withoutDrop, orderID: lo.o.ID, binID: r.binID,
				detail: fmt.Sprintf("picked at step %d and never set down in the plan; drop_step stays NULL", r.pick.Int64)})
		case lo.live && r.destNode != "" && final[r.binID] != "" && r.destNode != final[r.binID]:
			findings = append(findings, v173Finding{list: "dest_node_drift", orderID: lo.o.ID, binID: r.binID,
				detail: fmt.Sprintf("dest_node %s, the plan sets it down last at %s", r.destNode, final[r.binID])})
		}
	}
	if len(lifts) == 0 && len(lo.rows) == 0 {
		findings = append(findings, v173Finding{list: "live_row_without_drop", orderID: lo.o.ID, binID: lo.bin,
			detail: "the plan never sets its cart down; no row"})
	}
	return findings, nil
}

// v173WriteLift fills an existing row's drop step, or inserts the lift's row
// (a relay, or a single-cart order's). A row already whole is left alone.
func v173WriteLift(tx *sql.Tx, lo *v173Order, c domain.Cart, final string) error {
	for _, r := range lo.rows {
		if !r.pick.Valid || int(r.pick.Int64) != *c.PickStep || r.binID != c.BinID {
			continue
		}
		if r.drop.Valid || c.DropStep == nil {
			return nil
		}
		if _, err := tx.Exec(`UPDATE order_bins SET drop_step = $2 WHERE id = $1 AND drop_step IS NULL`,
			r.id, *c.DropStep); err != nil {
			return fmt.Errorf("v173: order %d bin %d: set drop %d: %w", lo.o.ID, c.BinID, *c.DropStep, err)
		}
		return nil
	}
	if _, err := tx.Exec(`INSERT INTO order_bins (order_id, bin_id, pick_step, drop_step, action, node_name, dest_node)
		SELECT $1, $2, $3, $4, 'pickup', $5, $6
		 WHERE NOT EXISTS (SELECT 1 FROM order_bins WHERE order_id = $1 AND pick_step = $3)`,
		lo.o.ID, c.BinID, *c.PickStep, c.DropStep, domain.StepNode(&lo.o, *c.PickStep), final); err != nil {
		return fmt.Errorf("v173: order %d bin %d: write the lift picked at %d: %w", lo.o.ID, c.BinID, *c.PickStep, err)
	}
	return nil
}

// v173FinalDrops lists every live order with stored steps whose last dropoff is
// not its delivery_node: StepNode's premise (the order delivers where its plan
// last sets down), measured before anything relies on it. Read in Go so a
// malformed plan is skipped, not a failed migration.
func v173FinalDrops(tx *sql.Tx) ([]v173Finding, error) {
	rows, err := tx.Query(fmt.Sprintf(`SELECT id, steps_json, COALESCE(delivery_node, ''), COALESCE(bin_id, 0)
		  FROM orders WHERE status NOT IN (%s) AND steps_json LIKE '[%%' ORDER BY id`, protocol.TerminalStatusSQLList()))
	if err != nil {
		return nil, fmt.Errorf("v173: read live plans: %w", err)
	}
	defer rows.Close()
	var out []v173Finding
	for rows.Next() {
		var id, bin int64
		var plan, delivery string
		if err := rows.Scan(&id, &plan, &delivery, &bin); err != nil {
			return nil, fmt.Errorf("v173: scan a live plan: %w", err)
		}
		steps, ok := domain.PlanSteps(plan)
		if !ok {
			continue
		}
		last := ""
		for _, s := range steps {
			if s.Action == protocol.ActionDropoff {
				last = s.Node
			}
		}
		if last != delivery {
			out = append(out, v173Finding{list: "plan_final_drop_disagrees", orderID: id, binID: bin,
				detail: fmt.Sprintf("the plan's last dropoff is %q, delivery_node is %q", last, delivery)})
		}
	}
	return out, rows.Err()
}

// v173OnePick is a single-cart order's pickup: its plan's one pickup that does
// not re-lift something the plan set down there earlier. ok=false when there
// is not exactly one; the int is then how many there are.
func v173OnePick(steps []domain.PlanStep) (int, bool) {
	setDown := map[string]bool{}
	pick, n := -1, 0
	for i, s := range steps {
		switch s.Action {
		case protocol.ActionPickup:
			if s.Node != "" && setDown[s.Node] {
				continue
			}
			pick, n = i, n+1
		case protocol.ActionDropoff:
			setDown[s.Node] = true
		}
	}
	if n != 1 {
		return n, false
	}
	return pick, true
}

// v173CarryEnded gives every ended order with a bin_id and no row one row with
// no steps: the link is true, and it keeps "the last order of this cart" whole.
func v173CarryEnded(tx *sql.Tx) (int64, error) {
	res, err := tx.Exec(fmt.Sprintf(`INSERT INTO order_bins (order_id, bin_id, pick_step, drop_step, action, node_name, dest_node)
		SELECT o.id, o.bin_id, NULL, NULL, 'pickup', COALESCE(o.source_node, ''), COALESCE(o.delivery_node, '')
		  FROM orders o
		 WHERE o.status IN (%s) AND o.bin_id IS NOT NULL
		   AND EXISTS (SELECT 1 FROM bins b WHERE b.id = o.bin_id)
		   AND NOT EXISTS (SELECT 1 FROM order_bins ob WHERE ob.order_id = o.id AND ob.bin_id = o.bin_id)`,
		protocol.TerminalStatusSQLList()))
	if err != nil {
		return 0, fmt.Errorf("v173: carry ended orders' carts over: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// v173BooksCensus lists where the books disagree after the walk.
func v173BooksCensus(tx *sql.Tx) ([]v173Finding, error) {
	terminal := protocol.TerminalStatusSQLList()
	q := fmt.Sprintf(`
SELECT 'live_claim_without_row', b.claimed_by, b.id, 'claimed with no row of its order''s'
  FROM bins b JOIN orders o ON o.id = b.claimed_by
 WHERE o.status NOT IN (%[1]s)
   AND NOT EXISTS (SELECT 1 FROM order_bins ob WHERE ob.order_id = o.id AND ob.bin_id = b.id)
UNION ALL
SELECT DISTINCT 'live_row_without_claim', o.id, ob.bin_id, 'a dispatched order''s row; the cart is claimed by ' || COALESCE(b.claimed_by::text, 'nobody')
  FROM order_bins ob JOIN orders o ON o.id = ob.order_id JOIN bins b ON b.id = ob.bin_id
 WHERE o.status NOT IN (%[1]s) AND o.status <> 'delivered' AND o.vendor_order_id <> ''
   AND b.claimed_by IS DISTINCT FROM o.id
UNION ALL
SELECT 'live_two_in_hand', o.id, 0, count(*)::text || ' carts claimed on _TRANSIT or a robot'
  FROM orders o JOIN bins b ON b.claimed_by = o.id JOIN nodes n ON n.id = b.node_id
 WHERE o.status NOT IN (%[1]s) AND (n.name = '_TRANSIT' OR n.name LIKE '_ROBOT:%%')
 GROUP BY o.id HAVING count(*) > 1
ORDER BY 1, 2, 3`, terminal)
	rows, err := tx.Query(q)
	if err != nil {
		return nil, fmt.Errorf("v173: the books census: %w", err)
	}
	defer rows.Close()
	var out []v173Finding
	for rows.Next() {
		var f v173Finding
		if err := rows.Scan(&f.list, &f.orderID, &f.binID, &f.detail); err != nil {
			return nil, fmt.Errorf("v173: scan the books census: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// v173Constraints is the DDL after the data: the step check (NOT VALID while
// any row the census listed fails it, so the list is not decided by refusing
// the deploy), the per-step uniqueness, and the by-bin index newest first.
func v173Constraints(tx *sql.Tx) error {
	var exists bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'order_bins_steps')`).Scan(&exists); err != nil {
		return fmt.Errorf("v173: read the step check: %w", err)
	}
	if !exists {
		if _, err := tx.Exec(`ALTER TABLE order_bins ADD CONSTRAINT order_bins_steps
			CHECK ((pick_step IS NOT NULL OR drop_step IS NULL) AND (drop_step IS NULL OR pick_step < drop_step)) NOT VALID`); err != nil {
			return fmt.Errorf("v173: add the step check: %w", err)
		}
	}
	var failing int
	if err := tx.QueryRow(`SELECT count(*) FROM order_bins
		WHERE NOT ((pick_step IS NOT NULL OR drop_step IS NULL) AND (drop_step IS NULL OR pick_step < drop_step))`).Scan(&failing); err != nil {
		return fmt.Errorf("v173: count rows the step check fails: %w", err)
	}
	if failing == 0 {
		if _, err := tx.Exec(`ALTER TABLE order_bins VALIDATE CONSTRAINT order_bins_steps`); err != nil {
			return fmt.Errorf("v173: validate the step check: %w", err)
		}
	} else {
		log.Printf("store: v173 the step check stays NOT VALID: %d row(s) listed above fail it", failing)
	}
	for _, q := range []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS order_bins_pick ON order_bins (order_id, pick_step)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS order_bins_drop ON order_bins (order_id, drop_step)`,
		`DROP INDEX IF EXISTS idx_order_bins_bin`,
		`CREATE INDEX IF NOT EXISTS order_bins_by_bin ON order_bins (bin_id, order_id DESC)`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("v173: %s: %w", q, err)
		}
	}
	return nil
}

// v174DropOneCartColumns drops what the rows replaced: orders.bin_id (one
// column naming one cart, chosen by shape) and the junction's three node copies
// of the plan, order_bins.action (always 'pickup'), node_name (the pick's node)
// and dest_node (the cart's final drop). An order's carts are its rows (v173),
// and where a lift happens is its plan's (domain.StepNode, StepNodeSQL). No
// reader is left: the stream moved every one onto the rows before this ran.
//
// IF EXISTS on every column, so the migration is its own re-run.
//
// IT WAITS WHILE v173's live_pick_step_ambiguous LIST IS NOT EMPTY. Such an order is live and
// holds its cart only in orders.bin_id: its plan has no one non-relay pickup, so v173 gave it no
// row, and dropping the column would lose that cart's only record. So v174 logs each such order
// and cart, leaves orders.bin_id and the node columns in place, and returns: Core boots on v173's
// shape. The node columns get defaults ('pickup', ”): they were NOT NULL with none, and the
// writers insert a cart row without them. v174's verify (the columns gone) fails at the next boot,
// so the runner runs it again (migrate.Run's self-heal) until the list is empty. The list is read
// again at each run, not taken from v173's log; once it is empty, the listed orders that have
// ended since get v173's carry-over row, so each stays its cart's last order, and the columns go
// (MIGRATION-LIST, v174). Pin: TestV173_AnOrdersCartsBecomeRowsPerLift.
func v174DropOneCartColumns(tx *sql.Tx) error {
	left, err := v174AmbiguousLeft(tx)
	if err != nil {
		return fmt.Errorf("v174: read v173's live_pick_step_ambiguous list: %w", err)
	}
	if len(left) > 0 {
		for _, stmt := range []string{
			`ALTER TABLE order_bins ALTER COLUMN action SET DEFAULT 'pickup'`,
			`ALTER TABLE order_bins ALTER COLUMN node_name SET DEFAULT ''`,
			`ALTER TABLE order_bins ALTER COLUMN dest_node SET DEFAULT ''`,
		} {
			if _, err := tx.Exec(stmt); err != nil {
				return fmt.Errorf("v174 deferred: %s: %w", stmt, err)
			}
		}
		for _, f := range left {
			log.Printf("store: v174 deferred: order %d bin %d — live, its cart only in orders.bin_id (v173 list live_pick_step_ambiguous: %s)",
				f.orderID, f.binID, f.detail)
		}
		log.Printf("store: v174 deferred: %d live order(s) on v173's live_pick_step_ambiguous list; orders.bin_id and "+
			"order_bins' node columns stay, and v174 runs again at the next boot", len(left))
		return nil
	}
	// An order of the list that has ended since, its cart only in bin_id, gets v173's carry-over
	// row, so the last order of that cart survives the drop.
	if schema.ColumnExists(tx, "orders", "bin_id") {
		carried, err := v173CarryEnded(tx)
		if err != nil {
			return fmt.Errorf("v174: %w", err)
		}
		if carried > 0 {
			log.Printf("store: v174 carried %d ended order(s) over before the drop", carried)
		}
	}
	for _, stmt := range []string{
		`ALTER TABLE orders DROP COLUMN IF EXISTS bin_id`,
		`ALTER TABLE order_bins DROP COLUMN IF EXISTS action`,
		`ALTER TABLE order_bins DROP COLUMN IF EXISTS node_name`,
		`ALTER TABLE order_bins DROP COLUMN IF EXISTS dest_node`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("v174: %s: %w", stmt, err)
		}
	}
	return nil
}

// v174AmbiguousLeft is v173's live_pick_step_ambiguous list as it stands when v174 runs: each live
// order with a cart in orders.bin_id and no row, whose plan has no one non-relay pickup. Nothing
// once the column is gone.
func v174AmbiguousLeft(tx *sql.Tx) ([]v173Finding, error) {
	if !schema.ColumnExists(tx, "orders", "bin_id") {
		return nil, nil
	}
	live, err := v173ReadLive(tx)
	if err != nil {
		return nil, err
	}
	var left []v173Finding
	for _, lo := range live {
		if !lo.live || lo.bin == 0 || len(lo.rows) > 0 {
			continue
		}
		if n, ok := v173OnePick(domain.OrderPlan(&lo.o)); !ok {
			left = append(left, v173Finding{list: "live_pick_step_ambiguous", orderID: lo.o.ID, binID: lo.bin,
				detail: fmt.Sprintf("%d non-relay pickups", n)})
		}
	}
	return left, nil
}
