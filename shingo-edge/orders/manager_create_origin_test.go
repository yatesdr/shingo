package orders

import (
	"testing"

	"shingo/protocol"
)

// manager_create_origin_test.go — what an order this Edge authored records
// about the demand that asked for it.
//
// Every create door takes an Origin and is compile-forced to name one (see the
// Origin type). Until the column list named them, what a door did with it was
// send it up the wire on the OrderRequest and nothing else, so the Edge asked
// Core to remember the attribution and did not remember it itself.
//
// The gap was invisible on the simple types, because Core projects those back
// and the projection carries the origin home — the Edge looked attributed
// because Core told it what it had just told Core. It was never invisible on
// complex orders: they are not projected, they are 69% of Springfield's Edge
// orders, and their attribution lived on Core alone.
//
// The three doors are three signatures over ONE insert, which is why these run
// as a table rather than as one test for the path somebody happened to be
// looking at.

// createDoor is one of the three ways this Edge brings an order row into
// existence. Named by what a caller does, not by the function, because the
// three functions are three signatures over one INSERT.
type createDoor struct {
	name   string
	create func(t *testing.T, m *Manager, origin Origin) (int64, string)
}

func createDoors() []createDoor {
	return []createDoor{
		{
			name: "retrieve",
			create: func(t *testing.T, m *Manager, origin Origin) (int64, string) {
				t.Helper()
				o, err := m.CreateRetrieveOrder(nil, true, 1, "LINE-1", "EMPTY-MARKET", "", "", "PART-A", false, false, origin)
				if err != nil {
					t.Fatalf("create retrieve order: %v", err)
				}
				return o.ID, o.UUID
			},
		},
		{
			name: "move",
			create: func(t *testing.T, m *Manager, origin Origin) (int64, string) {
				t.Helper()
				o, err := m.CreateMoveOrder(nil, 1, "HOLD-1", "LINE-1", false, origin)
				if err != nil {
					t.Fatalf("create move order: %v", err)
				}
				return o.ID, o.UUID
			},
		},
		{
			name: "complex",
			create: func(t *testing.T, m *Manager, origin Origin) (int64, string) {
				t.Helper()
				steps := []protocol.ComplexOrderStep{
					{Action: protocol.ActionPickup, Node: "LINE-1"},
					{Action: protocol.ActionDropoff, Node: "EMPTY-MARKET"},
				}
				o, err := m.CreateComplexOrder(nil, 1, "EMPTY-MARKET", "LINE-1", steps, origin)
				if err != nil {
					t.Fatalf("create complex order: %v", err)
				}
				return o.ID, o.UUID
			},
		},
	}
}

// TestCreate_AuthoredByIsEdgeOnEveryDoor pins the half that is already right.
// An order this Edge decided to create is labelled 'edge', which is the column
// default and what the board renders as no tag at all.
func TestCreate_AuthoredByIsEdgeOnEveryDoor(t *testing.T) {
	t.Parallel()
	for _, door := range createDoors() {
		t.Run(door.name, func(t *testing.T) {
			t.Parallel()
			db := testManagerDB(t)
			mgr := NewManager(db, testEmitter{}, "edge.station")

			id, _ := door.create(t, mgr, Attached("episode-authored-by"))

			row, err := db.GetOrder(id)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			if row.AuthoredBy != "edge" {
				t.Errorf("authored_by = %q, want \"edge\" — this station created the order", row.AuthoredBy)
			}
		})
	}
}

// TestCreate_AttachedOriginOnTheRow pins the demand attribution an
// Attached order carries on its own row.
//
// The origin reaches the OrderRequest envelope on every one of these doors, so
// Core learns it. The assertion below is about the LOCAL row and nothing else.
func TestCreate_AttachedOriginOnTheRow(t *testing.T) {
	t.Parallel()
	const episode = "11111111-2222-3333-4444-555555555555"
	for _, door := range createDoors() {
		t.Run(door.name, func(t *testing.T) {
			t.Parallel()
			db := testManagerDB(t)
			mgr := NewManager(db, testEmitter{}, "edge.station")

			id, _ := door.create(t, mgr, Attached(episode))

			row, err := db.GetOrder(id)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			if row.OriginID != episode {
				t.Errorf("origin_id = %q, want %q — the order was created to serve that episode", row.OriginID, episode)
			}
			if row.OriginClass != protocol.OriginClassAttached {
				t.Errorf("origin_class = %q, want %q", row.OriginClass, protocol.OriginClassAttached)
			}
		})
	}
}

// TestCreate_NoDemandOriginOnTheRow is the other constructor, and the one whose
// loss is the more expensive of the two.
//
// no_demand is an ANSWER: it says this order belongs to no demand episode by
// construction, and it is what keeps the orphan bucket meaning "should have had
// an episode and lost it". A row that stores blank for it is indistinguishable
// from a row whose attribution was dropped, which is the exact confusion the
// class was introduced to end.
func TestCreate_NoDemandOriginOnTheRow(t *testing.T) {
	t.Parallel()
	for _, door := range createDoors() {
		t.Run(door.name, func(t *testing.T) {
			t.Parallel()
			db := testManagerDB(t)
			mgr := NewManager(db, testEmitter{}, "edge.station")

			id, _ := door.create(t, mgr, NoDemand())

			row, err := db.GetOrder(id)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			if row.OriginID != "" {
				t.Errorf("origin_id = %q, want \"\" — a no_demand order has no episode", row.OriginID)
			}
			if row.OriginClass != protocol.OriginClassNoDemand {
				t.Errorf("origin_class = %q, want %q — blank here is indistinguishable from a dropped attribution", row.OriginClass, protocol.OriginClassNoDemand)
			}
		})
	}
}
