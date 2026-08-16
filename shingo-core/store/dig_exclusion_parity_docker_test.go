//go:build docker

package store_test

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// dig_exclusion_parity_docker_test.go — the guarantee the drift test cannot make.
//
// TestDigExclusionHasExactlyOneSQLSpelling proves nobody WROTE a second spelling.
// It cannot prove the one spelling means the same thing on both sides of the
// database boundary: DigExclusionSQL renders a fragment Postgres evaluates, and
// ExcludedBy is Go, and "$3 <> order_id" agreeing with "digOwner != a.OrderID"
// is an assumption until something runs both.
//
// That assumption is exactly what broke. Admission's Go answer and sourcing's
// SQL answer were both "correct" in isolation and disagreed about the owner, and
// nothing in the tree compared them. So the matrix below is run through BOTH
// readers and the verdicts must match case for case.

// digRole names the three orders the matrix needs. Real rows, because
// reservations.order_id carries a foreign key to orders — a fixture that
// invented ids would test nothing that can happen.
//
//	parent  — holds a dig, and is the lane owner of child
//	child   — a compound child of parent, holding no dig of its own
//	foreign — an unrelated order
type digRole int

const (
	roleNone digRole = iota // no dig at all
	roleParent
	roleChild
	roleForeign
)

// digMatrix is every combination of (who holds the lane, who is asking) that the
// arrest turned on. Named cases rather than a loop over ids, because the point
// of each row is which real situation it is.
var digMatrix = []struct {
	name         string
	holder       digRole // who owns the dig mouth row
	askerSelf    digRole // the asking order
	askerOwner   digRole // its lane owner (its compound parent, or itself)
	noAsker      bool    // ask with reservations.Anyone instead
	wantExcluded bool
	why          string
}{
	{
		name: "no dig at all", holder: roleNone, askerSelf: roleParent, askerOwner: roleParent,
		wantExcluded: false, why: "an unheld lane keeps nobody out",
	},
	{
		name: "foreign dig", holder: roleForeign, askerSelf: roleParent, askerOwner: roleParent,
		wantExcluded: true, why: "the ordinary case: somebody else is excavating, stay out",
	},
	{
		name: "own dig, asked by self", holder: roleParent, askerSelf: roleParent, askerOwner: roleParent,
		wantExcluded: false,
		why: "THE ARREST. An expose dig transfers its lock to the complex parent to " +
			"protect the bin it uncovered; the parent then re-resolves and must SEE that lane. " +
			"Excluding it here is what sent order 1 back to a buried bin to dig again",
	},
	{
		name: "own dig, asked by a child of the holder", holder: roleParent, askerSelf: roleChild, askerOwner: roleParent,
		wantExcluded: false,
		why: "a compound child works inside the lane its parent locked — the arm ownsDig " +
			"had and the sourcing query did not",
	},
	{
		name: "child's dig, asked by the parent", holder: roleChild, askerSelf: roleParent, askerOwner: roleParent,
		wantExcluded: true,
		why: "NOT symmetric, and deliberately so: laneOwnerFor resolves upward only. A parent " +
			"does not inherit an exemption from a lock a child took on its own account",
	},
	{
		name: "no asker", holder: roleForeign, noAsker: true,
		wantExcluded: true,
		why: "reservations.Anyone reproduces the owner-blind behaviour exactly, which is what " +
			"makes every un-migrated call site unchanged rather than wrong",
	},
}

func TestDigExclusion_GoAndSQLAgree(t *testing.T) {
	db := testdb.Open(t)

	group, lane := seedGroupWithOneLane(t, db, "DIGPAR")

	parent := testdb.CreateOrder(t, db)
	child := testdb.CreateOrder(t, db, func(o *orders.Order) { o.ParentOrderID = &parent.ID })
	foreign := testdb.CreateOrder(t, db)
	id := map[digRole]int64{roleNone: 0, roleParent: parent.ID, roleChild: child.ID, roleForeign: foreign.ID}

	for _, c := range digMatrix {
		t.Run(c.name, func(t *testing.T) {
			clearDigHolds(t, db, lane.ID)
			holder := id[c.holder]
			if holder != 0 {
				if err := reservations.AcquireLanes(db.DB, holder, reservations.ModeDig, "paritytest", lane.ID); err != nil {
					t.Fatalf("fixture: could not give order %d the dig on lane %d: %v", holder, lane.ID, err)
				}
			}
			asker := reservations.Anyone
			if !c.noAsker {
				asker = reservations.AskerFor(id[c.askerSelf], id[c.askerOwner])
			}

			// Reader 1: the Go predicate, as admission asks it.
			goSays := asker.ExcludedBy(holder)

			// Reader 2: the rendered SQL, as the sourcing scan asks it. The lane is
			// absent from the candidate list exactly when the dig excludes the asker.
			children, err := db.ListChildNodesUnlocked(group.ID, asker)
			if err != nil {
				t.Fatalf("ListChildNodesUnlocked: %v", err)
			}
			sqlSays := !containsNode(children, lane.ID)

			if goSays != c.wantExcluded {
				t.Errorf("Go predicate says excluded=%v, want %v — %s", goSays, c.wantExcluded, c.why)
			}
			if sqlSays != c.wantExcluded {
				t.Errorf("SQL candidate scan says excluded=%v, want %v — %s", sqlSays, c.wantExcluded, c.why)
			}
			if goSays != sqlSays {
				t.Fatalf("THE READERS DISAGREE (Go=%v, SQL=%v). This is the exact failure the "+
					"predicate exists to prevent: admission and sourcing answering one question two "+
					"ways. Fix reservations.DigExclusionSQL or ExcludedBy so they are one rule again, "+
					"not the test.", goSays, sqlSays)
			}
		})
	}
}

// TestDigExclusion_UnexemptedSourcingReproducesTheArrest is the mutation, kept
// as a test rather than as a note in a commit message.
//
// It drives the pre-fix behaviour through the post-fix query by asking with
// reservations.Anyone where production now asks with the parent, and asserts the
// lane vanishes. If someone "simplifies" ListChildNodesUnlocked back to an
// asker-less filter, TestDigExclusion_GoAndSQLAgree fails; if someone leaves the
// parameter in place but a CALLER stops threading it, only this shows what that
// costs.
func TestDigExclusion_UnexemptedSourcingReproducesTheArrest(t *testing.T) {
	db := testdb.Open(t)
	group, lane := seedGroupWithOneLane(t, db, "DIGARREST")

	parent := testdb.CreateOrder(t, db)
	if err := reservations.AcquireLanes(db.DB, parent.ID, reservations.ModeDig, "paritytest", lane.ID); err != nil {
		t.Fatalf("fixture: dig hold for parent: %v", err)
	}

	withAsker, err := db.ListChildNodesUnlocked(group.ID, reservations.AskerFor(parent.ID, parent.ID))
	if err != nil {
		t.Fatalf("ListChildNodesUnlocked(asker): %v", err)
	}
	if !containsNode(withAsker, lane.ID) {
		t.Fatalf("the parent cannot see the lane its own expose dig holds — this is the arrest, " +
			"not a regression in the test")
	}

	blind, err := db.ListChildNodesUnlocked(group.ID, reservations.Anyone)
	if err != nil {
		t.Fatalf("ListChildNodesUnlocked(Anyone): %v", err)
	}
	if containsNode(blind, lane.ID) {
		t.Fatal("reservations.Anyone was supposed to reproduce the owner-blind behaviour and did " +
			"not. That sentinel is what makes every un-migrated call site provably unchanged; if it " +
			"has stopped excluding, that claim is void and the migration needs re-auditing")
	}
}

func seedGroupWithOneLane(t *testing.T, db *store.DB, prefix string) (group, lane *nodes.Node) {
	t.Helper()
	groupID, err := db.CreateNodeGroup(prefix + "_GRP")
	if err != nil {
		t.Fatalf("create node group: %v", err)
	}
	group, err = db.GetNode(groupID)
	if err != nil || group == nil {
		t.Fatalf("reload node group %d: %v", groupID, err)
	}
	lane = &nodes.Node{Name: prefix + "_LANE", NodeTypeCode: protocol.NodeClassLANE, ParentID: &groupID, Enabled: true}
	if err := db.CreateNode(lane); err != nil {
		t.Fatalf("create lane: %v", err)
	}
	// Depth 2+, because AcquireLanes exempts depth-1 lanes from taking a mouth row
	// at all (a single-slot lane is already serialized by its slot reservation) —
	// a one-slot fixture would take no dig hold and every row would read "not
	// excluded" for the wrong reason.
	for i := 1; i <= 2; i++ {
		d := i
		slot := &nodes.Node{
			Name: prefix + "_SLOT" + string(rune('0'+i)), NodeTypeCode: protocol.NodeClassSTOR,
			ParentID: &lane.ID, Depth: &d, Enabled: true,
		}
		if err := db.CreateNode(slot); err != nil {
			t.Fatalf("create slot %d: %v", i, err)
		}
	}
	return group, lane
}

func clearDigHolds(t *testing.T, db *store.DB, laneID int64) {
	t.Helper()
	if _, err := db.DB.Exec(
		`DELETE FROM reservations WHERE resource_kind='mouth' AND node_id=$1`, laneID); err != nil {
		t.Fatalf("clear dig holds on lane %d: %v", laneID, err)
	}
}

func containsNode(list []*nodes.Node, id int64) bool {
	for _, n := range list {
		if n != nil && n.ID == id {
			return true
		}
	}
	return false
}
