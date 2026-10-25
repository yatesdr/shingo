package testdb

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"shingocore/store"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// issue_census.go — the census of the ways an order comes to exist, and what
// each one's order holds at the commit of the transaction that issues it.
//
// ONE TABLE, THREE READERS. The static arm (dispatch's TestCensus_Issuers) finds
// every caller of the issue on go/ast and holds the list of Sites below to it;
// the docker arms (dispatch's and engine's TestIssueCensus_AtCommit) issue one
// order through each door, with no acquirer acting on it, and hold its row and
// its reservations at commit to At and Take. A phase that changes what a door
// holds flips its row here, by prediction, before its source moves.
//
// AT COMMIT, NOT AFTER THE DOOR RETURNS. A door that writes a hold after its
// transaction (the drain's window, the recovery's pin) and a scanner pass that
// takes a hold after it are the two things the census must not mistake for the
// issue's own, so the record is a deferred constraint trigger on orders
// (InstallIssueCensus): it fires as the inserting transaction commits and reads
// the row and its reservations as they stand then.

// IssueDoor is one way an order comes to exist.
type IssueDoor struct {
	Name string // what the door is, in the words someone would use out loud
	// Site is "file:Func" for the function that calls the issue, module-root
	// relative (the static arm's unit: a caller, not a file).
	Site string
	// Arm is the package whose docker test issues through this door.
	Arm string
	At  AtCommit
	// Take is what becomes of a stranger's pending hold on the door's cart.
	Take Take
	Why  string
}

// AtCommit is what an order holds at the commit that issues it.
type AtCommit struct {
	BinID    bool // orders.bin_id is stamped
	NamedBin bool // orders.named_bin_id is stamped
	Robot    bool // orders.robot_id is stamped
	BinRow   bool // a pending bin reservation of the order's own
	SlotRow  bool // a pending slot reservation of the order's own
}

// Take is what becomes of a stranger's pending hold on the cart an order is
// issued for, at the issue's commit.
type Take string

const (
	// TakeNoCart: the door names no cart, so it meets no stranger at its issue.
	TakeNoCart Take = "no cart"
	// TakeTaken: the stranger's hold is gone at commit.
	TakeTaken Take = "taken"
	// TakeRefused: nothing is issued.
	TakeRefused Take = "refused"
	// TakeLeft: the order is issued and the stranger still holds the cart.
	TakeLeft Take = "left"
)

// IssueDoors is the census. Its order is the order the static arm reports in.
var IssueDoors = []IssueDoor{
	{Name: "wire plain order", Site: "dispatch/lifecycle_service.go:LifecycleService.birth", Arm: "dispatch",
		At: AtCommit{}, Take: TakeNoCart,
		Why: "an Edge station sends a retrieve: its row is not whole at its INSERT (the apply writes after it) and its pick rides as orders.plan, held after the gate and the slot by the acquire"},
	{Name: "wire named move", Site: "dispatch/lifecycle_service.go:LifecycleService.birth", Arm: "dispatch",
		At: AtCommit{NamedBin: true}, Take: TakeLeft,
		Why: "an Edge station's move naming its bin (SEND BACK, W6's clear, a spare's return, a containment move): it names the cart and holds nothing until the acquire, so a stranger keeps its hold"},
	{Name: "complex intake", Site: "dispatch/complex_intake.go:Dispatcher.handleComplexOrderRequest", Arm: "dispatch",
		At: AtCommit{}, Take: TakeNoCart,
		Why: "an Edge station sends a multi-leg order: the row only; its holds are the reserve pass's, every pass"},
	{Name: "dig legs", Site: "dispatch/compound.go:Dispatcher.writeCompoundChildren", Arm: "dispatch",
		At: AtCommit{BinID: true, SlotRow: true}, Take: TakeTaken,
		Why: "a reshuffle's children in their parent's transaction: the blocker stamped and held by the parent's lane (no bin row), a stranger's pending hold on it stolen by rank, the parking pencilled"},
	{Name: "loader drain", Site: "dispatch/loader_drain.go:Dispatcher.drainMove", Arm: "dispatch",
		At: AtCommit{BinID: true, BinRow: true}, Take: TakeRefused,
		Why: "an unloader with a drain box ticked moves a cart of that state into a free window: the cart held from the issue, the window reserved after the commit; its read of carts passes over a held one (BinUnheldSQL), and its plan's take is refused for the race"},
	{Name: "loader replenish", Site: "dispatch/lifecycle_service.go:LifecycleService.birth", Arm: "dispatch",
		At: AtCommit{}, Take: TakeNoCart,
		Why: "a loader's windows are below level: Core asks for an empty at the window it chose, and holds no window"},
	{Name: "person's bin move", Site: "engine/bin_move.go:Engine.CreateBinMove", Arm: "engine",
		At: AtCommit{BinID: true, BinRow: true}, Take: TakeTaken,
		Why: "a person moves one bin from Core's pages: the cart held from the issue, a stranger's pending hold taken out loud"},
	{Name: "stage 2's pull", Site: "engine/bin_move.go:Engine.CreateBinMove", Arm: "engine",
		At: AtCommit{BinID: true, BinRow: true}, Take: TakeRefused,
		Why: "a two-stage loader's stage 2 pulls a bare cart into a free window through the person's bin move, holding the cart and no window; its read of carts passes over a held one (BinUnheldSQL), and the person's take it reaches is its own only in the race after that read"},
	{Name: "carried-bin recovery", Site: "engine/carried_bin_recovery.go:Engine.birthAndDispatchRecovery", Arm: "engine",
		At: AtCommit{BinID: true}, Take: TakeLeft,
		Why: "an operator presses Recover: the cart is stamped and unheld at the issue, the pin and the hold come after it (a stranger's hold then fails it bin_taken)"},
	{Name: "the cancel's return", Site: "engine/carried_bin_recovery.go:Engine.birthAndDispatchRecovery", Arm: "engine",
		At: AtCommit{BinID: true}, Take: TakeRefused,
		Why: "an order cancelled while its robot carries a bin: Core asks that robot to set it down, through the recovery's door, nobody pressing anything; a stranger on the cart is the bin's last order, and the return stands down before it issues"},
	{Name: "home clear", Site: "dispatch/clear_home.go:Dispatcher.BirthClearTx", Arm: "engine",
		At: AtCommit{}, Take: TakeNoCart,
		Why: "an Edge station clears a dedicated loader's home: the clear's parent row, delivering to the home"},
	{Name: "home clear's child", Site: "dispatch/clear_home.go:Dispatcher.BirthClearTx", Arm: "engine",
		At: AtCommit{BinID: true, BinRow: true}, Take: TakeRefused,
		Why: "the partial's move home, in the parent's transaction, holding its cart from the issue; the clear's read of buffer partials passes over a held one, so a stranger refuses the tap"},
	{Name: "home clear's spot move", Site: "dispatch/clear_home.go:Dispatcher.birthClearSpot", Arm: "engine",
		At: AtCommit{BinID: true, BinRow: true}, Take: TakeRefused,
		Why: "the clear's empty, moved at confirm to the buffer spot chosen then, holding its cart from the issue; the hold is the compound's ranked take, and a stranger whose demand outranks a housekeeping clear refuses it"},
	{Name: "home clear's trade", Site: "dispatch/clear_home.go:Dispatcher.birthClearTrade", Arm: "engine",
		At: AtCommit{BinID: true, BinRow: true}, Take: TakeRefused,
		Why: "the clear's trade at confirm: the empty's move into the partial's place, sibling-linked to the partial's, holding its cart; the same ranked take as the spot move"},
	{Name: "maintained-group level keeper", Site: "dispatch/lifecycle_service.go:LifecycleService.birth", Arm: "engine",
		At: AtCommit{}, Take: TakeNoCart,
		Why: "a maintained group is below its level: Core asks for an empty at the slot it pre-resolved, and holds no slot"},
}

// InstallIssueCensus installs the at-commit record in db: a table and a
// deferred constraint trigger on orders that, as each inserting transaction
// commits, writes what the inserted row holds. Only for a database of the
// test's own (Open), never a shared one: it alters the schema.
func InstallIssueCensus(t testing.TB, db *store.DB) {
	t.Helper()
	if _, err := db.Exec(issueCensusDDL); err != nil {
		t.Fatalf("install the issue census: %v", err)
	}
}

// The cart is the order's bin_id, else its named_bin_id. A stranger is any
// other order holding a pending bin reservation on it.
var issueCensusDDL = `
CREATE TABLE issue_census_at_commit (
	order_id      bigint PRIMARY KEY,
	bin_id        bigint,
	named_bin_id  bigint,
	robot_id      text NOT NULL,
	own_bin_rows  int NOT NULL,
	own_slot_rows int NOT NULL,
	stranger_rows int NOT NULL
);
CREATE FUNCTION issue_census_at_commit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	INSERT INTO issue_census_at_commit
	SELECT o.id, o.bin_id, o.named_bin_id, o.robot_id,
	       (SELECT count(*) FROM reservations r
	         WHERE r.order_id = o.id AND r.resource_kind = 'bin' AND ` + reservations.BlockingStateSQL("r.") + `),
	       (SELECT count(*) FROM reservations r
	         WHERE r.order_id = o.id AND r.resource_kind = 'slot' AND ` + reservations.BlockingStateSQL("r.") + `),
	       (SELECT count(*) FROM reservations r
	         WHERE r.resource_kind = 'bin' AND ` + reservations.BlockingStateSQL("r.") + ` AND r.order_id <> o.id
	           AND r.bin_id = COALESCE(o.bin_id, o.named_bin_id))
	  FROM orders o WHERE o.id = NEW.id;
	RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER issue_census_at_commit AFTER INSERT ON orders
	DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION issue_census_at_commit();
`

// IssuedAtCommit is what order held at the commit that issued it, and what
// became of a stranger's pending hold on its cart (TakeTaken or TakeLeft; the
// caller knows whether the door names a cart). ok is false when no row of
// that id was ever committed.
func IssuedAtCommit(t testing.TB, db *store.DB, orderID int64) (at AtCommit, take Take, ok bool) {
	t.Helper()
	var binID, named sql.NullInt64
	var robot string
	var ownBin, ownSlot, strangers int
	err := db.QueryRow(`SELECT bin_id, named_bin_id, robot_id, own_bin_rows, own_slot_rows, stranger_rows
		FROM issue_census_at_commit WHERE order_id = $1`, orderID).
		Scan(&binID, &named, &robot, &ownBin, &ownSlot, &strangers)
	if errors.Is(err, sql.ErrNoRows) {
		return AtCommit{}, TakeRefused, false
	}
	if err != nil {
		t.Fatalf("read the issue census for order %d: %v", orderID, err)
	}
	at = AtCommit{BinID: binID.Valid, NamedBin: named.Valid, Robot: robot != "",
		BinRow: ownBin > 0, SlotRow: ownSlot > 0}
	take = TakeTaken
	if strangers > 0 {
		take = TakeLeft
	}
	return at, take, true
}

// IssueCensusDB is a database of the test's own with the at-commit record
// installed.
func IssueCensusDB(t testing.TB) *store.DB {
	t.Helper()
	db := Open(t)
	InstallIssueCensus(t, db)
	return db
}

// StrangerHolds gives cart a stranger's pending hold: a queued retrieve that
// points at it and has promised it to itself.
func StrangerHolds(t testing.TB, db *store.DB, cart int64) {
	t.Helper()
	o := &orders.Order{EdgeUUID: fmt.Sprintf("stranger-%d", orderSeq.Add(1)), StationID: "test",
		OrderType: "retrieve", Status: "queued", Quantity: 1, BinID: &cart}
	if err := db.CreateOrder(o); err != nil {
		t.Fatalf("the stranger: %v", err)
	}
	if err := reservations.Acquire(db, o.ID, o.ID, cart, "test"); err != nil {
		t.Fatalf("the stranger's hold on bin %d: %v", cart, err)
	}
}

// IssueDriver issues one order through a census door, on a database of its own
// with the at-commit record installed before anything is issued
// (IssueCensusDB), and returns that database and the id of the order the
// census row describes (0 when nothing was issued). stranger asks it to give
// the door's cart a stranger's pending hold first (StrangerHolds).
type IssueDriver func(t *testing.T, stranger bool) (*store.DB, int64)

// RunIssueCensus is a docker arm's body: every census row of arm has a driver,
// every driver drives a row, and what each row's order held at commit, and what
// became of a stranger's hold on its cart, are the row's.
func RunIssueCensus(t *testing.T, arm string, drivers map[string]IssueDriver) {
	t.Helper()
	named := map[string]bool{}
	for _, door := range IssueDoors {
		if door.Arm != arm {
			continue
		}
		named[door.Name] = true
		drive, ok := drivers[door.Name]
		if !ok {
			t.Errorf("census door %q has no driver in the %s arm", door.Name, arm)
			continue
		}
		t.Run(door.Name, func(t *testing.T) {
			t.Parallel()
			db, id := drive(t, false)
			at, _, ok := IssuedAtCommit(t, db, id)
			if !ok {
				t.Fatalf("%s issued nothing (order %d)", door.Name, id)
			}
			if at != door.At {
				t.Errorf("%s: at commit the order held %s, the census says %s (%s)",
					door.Name, at, door.At, door.Why)
			}
			if door.Take == TakeNoCart {
				return
			}
			db, id = drive(t, true)
			if _, take, _ := IssuedAtCommit(t, db, id); take != door.Take {
				t.Errorf("%s: a stranger's pending hold on its cart was %s, the census says %s", door.Name, take, door.Take)
			}
		})
	}
	for name := range drivers {
		if !named[name] {
			t.Errorf("driver %q drives no census door of the %s arm", name, arm)
		}
	}
}

func (a AtCommit) String() string {
	return fmt.Sprintf("{bin_id %v, named_bin_id %v, robot_id %v, bin row %v, slot row %v}",
		a.BinID, a.NamedBin, a.Robot, a.BinRow, a.SlotRow)
}
