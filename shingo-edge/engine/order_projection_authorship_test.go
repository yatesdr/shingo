package engine

import (
	"reflect"
	"strings"
	"testing"

	"shingo/protocol"
	"shingoedge/orders"
)

// order_projection_authorship_test.go — what a projection does to a row that
// was already here.
//
// The interesting collision is not the one the projection was designed for. A
// Core-authored order arriving at an Edge that has never heard of it is the easy
// case, and order_projection_test.go covers it. The collision is a projection
// arriving for an order THIS EDGE created and sent up: the Edge writes the row
// before it sends the request, Core admits it and projects it straight back
// (deliberately — see CreateInboundOrder's note about giving the projection real
// traffic), and the upsert then lands on a row that already exists and already
// has an author.
//
// These tests pin what that echo does to the two provenance columns the board
// reads: authored_by, and the origin pair.

// edgeAuthoredOrder writes a row the way this Edge's own create doors do: one
// INSERT, carrying the demand episode this station minted for the order.
//
// The origin goes in through CreateOrder rather than through a follow-up UPDATE,
// which is what this fixture had to do before the column list named it. That
// difference is the point — the row here is now the shape a real Edge-authored
// order has, not a simulation of one.
func edgeAuthoredOrder(t *testing.T, eng *Engine, uuid, originID, originClass string) int64 {
	t.Helper()
	id, err := eng.db.CreateOrder(uuid, orders.TypeRetrieve, nil, true, 1, "W-A", "", "EMPTY-MARKET", "", false, "PART-A", originID, originClass)
	if err != nil {
		t.Fatalf("create edge-authored order: %v", err)
	}
	return id
}

// TestProjection_OverAnEdgeAuthoredRow_Authorship pins who the board says
// created an order this station created itself.
//
// The row goes in as 'edge' — this Edge decided the order should exist and sent
// the request up. Core admits it, projects it back, and the upsert's DO UPDATE
// arm runs against the existing row.
//
// It is operator-visible and not bookkeeping. orders-body.html renders a "core"
// tag beside the uuid on exactly this column, titled "Core created this order;
// this station did not request it", so an unconditional re-stamp told the
// operator Core had decided something their own station had asked for.
func TestProjection_OverAnEdgeAuthoredRow_Authorship(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	edgeAuthoredOrder(t, eng, "echo-1", "", "")

	before, err := eng.db.GetOrderByUUID("echo-1")
	if err != nil || before == nil {
		t.Fatalf("fixture read: %v", err)
	}
	if before.AuthoredBy != "edge" {
		t.Fatalf("fixture: authored_by = %q, want \"edge\"; the test proves nothing", before.AuthoredBy)
	}

	created, err := eng.ApplyOrderProjection(projectionFixture("echo-1", "W-A"))
	if err != nil {
		t.Fatalf("apply the echo: %v", err)
	}
	if created {
		t.Error("the echo created a second row; the upsert is meant to be idempotent by uuid")
	}

	got, err := eng.db.GetOrderByUUID("echo-1")
	if err != nil || got == nil {
		t.Fatalf("read back: %v", err)
	}
	if got.AuthoredBy != "edge" {
		t.Errorf("authored_by = %q, want \"edge\" — the conflict arm leaves the existing author alone; this station asked for the order", got.AuthoredBy)
	}
}

// TestProjection_OverAnEdgeAuthoredRow_Origin pins what the echo does to an
// attribution the Edge minted for itself.
//
// The two origins are different values on purpose. An Edge episode id and a
// Core episode id are separate facts about the same order, and the question this
// test asks is which of them the row ends up holding.
func TestProjection_OverAnEdgeAuthoredRow_Origin(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	edgeAuthoredOrder(t, eng, "echo-2", "edge-episode-2222", protocol.OriginClassAttached)

	p := projectionFixture("echo-2", "W-A")
	p.OriginID = "core-episode-3333"
	p.OriginClass = protocol.OriginClassAttached
	if _, err := eng.ApplyOrderProjection(p); err != nil {
		t.Fatalf("apply the echo: %v", err)
	}

	got, err := eng.db.GetOrderByUUID("echo-2")
	if err != nil || got == nil {
		t.Fatalf("read back: %v", err)
	}
	if got.OriginID != "core-episode-3333" {
		t.Errorf("origin_id = %q, want \"core-episode-3333\" — a NON-BLANK incoming value still wins; it is Core's newer statement", got.OriginID)
	}
	if got.OriginClass != protocol.OriginClassAttached {
		t.Errorf("origin_class = %q, want %q", got.OriginClass, protocol.OriginClassAttached)
	}
}

// TestProjection_BlankOriginOverAStoredOne is the case the reconcile makes
// ordinary rather than exotic.
//
// Blank on the wire MEANS "not recorded" — ProjectionRow's own doc says so.
// Core sends blank for an order it never classified, and the reconcile re-sends
// every row it thinks the Edge is missing, so a blank arrival over a row that
// holds a value is a thing that happens on a healthy plant rather than a thing
// that happens when something is wrong.
//
// Two shapes, because the value got onto the row two different ways: one from an
// earlier projection, one minted by this Edge for an order of its own. Both are
// attributions somebody recorded, and a blank arrival is not a statement that
// either should go.
func TestProjection_BlankOriginOverAStoredOne(t *testing.T) {
	t.Parallel()

	t.Run("over a previously projected origin", func(t *testing.T) {
		t.Parallel()
		eng := newCoverageEngine(t)

		p := projectionFixture("blank-1", "W-B")
		p.OriginID = "core-episode-4444"
		p.OriginClass = protocol.OriginClassAttached
		if _, err := eng.ApplyOrderProjection(p); err != nil {
			t.Fatalf("first apply: %v", err)
		}

		p.OriginID = ""
		p.OriginClass = ""
		if _, err := eng.ApplyOrderProjection(p); err != nil {
			t.Fatalf("re-apply with a blank origin: %v", err)
		}

		got, err := eng.db.GetOrderByUUID("blank-1")
		if err != nil || got == nil {
			t.Fatalf("read back: %v", err)
		}
		if got.OriginID != "core-episode-4444" {
			t.Errorf("origin_id = %q, want \"core-episode-4444\" — blank on the wire means \"not recorded\", not \"erase it\"", got.OriginID)
		}
		if got.OriginClass != protocol.OriginClassAttached {
			t.Errorf("origin_class = %q, want %q", got.OriginClass, protocol.OriginClassAttached)
		}
	})

	t.Run("over an edge-authored origin", func(t *testing.T) {
		t.Parallel()
		eng := newCoverageEngine(t)
		edgeAuthoredOrder(t, eng, "blank-2", "edge-episode-5555", protocol.OriginClassAttached)

		if _, err := eng.ApplyOrderProjection(projectionFixture("blank-2", "W-C")); err != nil {
			t.Fatalf("apply the echo: %v", err)
		}

		got, err := eng.db.GetOrderByUUID("blank-2")
		if err != nil || got == nil {
			t.Fatalf("read back: %v", err)
		}
		if got.OriginID != "edge-episode-5555" {
			t.Errorf("origin_id = %q, want \"edge-episode-5555\" — the echo carries no origin and must not take the Edge's own away", got.OriginID)
		}
		if got.OriginClass != protocol.OriginClassAttached {
			t.Errorf("origin_class = %q, want %q", got.OriginClass, protocol.OriginClassAttached)
		}
	})
}

// TestProjection_InsertArmStampsCore guards the half of the statement that must
// NOT move. A row that genuinely arrives by projection was authored by Core, and
// the INSERT arm is the only place that fact is established.
func TestProjection_InsertArmStampsCore(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)

	created, err := eng.ApplyOrderProjection(projectionFixture("insert-arm-1", "W-D"))
	if err != nil || !created {
		t.Fatalf("apply: created=%v err=%v, want a new row", created, err)
	}
	got, err := eng.db.GetOrderByUUID("insert-arm-1")
	if err != nil || got == nil {
		t.Fatalf("read back: %v", err)
	}
	if got.AuthoredBy != "core" {
		t.Errorf("authored_by = %q, want \"core\" — nobody on this Edge asked for this order", got.AuthoredBy)
	}
}

// TestProjection_CarriesNoParentMarker pins what the Edge CANNOT know about a
// projected order, which is the fact S3's Edge close has to be built around.
//
// A Core order that is a step of another order — a reshuffle leg — inherits its
// parent's station_id AND its origin_id and origin_class (compound.go copies
// both forward on purpose, so a dig move counts against the episode that needed
// the buried bin). The reconcile's unlisted heal is scoped by station_id and
// status with no parent filter, so such a leg is sent down and lands here as an
// ordinary row. dispatcher.go's checkOwnership already says so in as many words.
//
// Nothing on the wire distinguishes it. protocol.OrderProjection carries no
// parent_order_id and no sequence, so this applier cannot tell a leg from the
// order the episode was opened for, and neither can anything reading the Edge's
// orders table afterwards. "Every Edge row with an origin_id is a root" is
// therefore NOT true today.
//
// This test fires the moment the wire type grows a parent marker — which is the
// moment the Edge could start telling them apart, and should.
func TestProjection_CarriesNoParentMarker(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf(protocol.OrderProjection{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		lower := strings.ToLower(name)
		if strings.Contains(lower, "parent") || lower == "sequence" {
			t.Errorf(`protocol.OrderProjection now carries %q.

The Edge can tell a compound leg from a root order for the first time. Anything
that reads origin_id off an Edge orders row and treats it as a root's — the S3
close above all — should be reading this field instead of assuming.`, name)
		}
	}
}

// TestProjection_ALegLandsAsAnOrdinaryRow is the behavioural half of the same
// fact. A projection carrying an episode id produces a row that is
// indistinguishable from a root's: same columns, same values, nothing marking it
// derivative.
func TestProjection_ALegLandsAsAnOrdinaryRow(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)

	// What a reshuffle leg's projection looks like on the wire: the parent's
	// episode, the parent's class, a move.
	leg := projectionFixture("leg-1", "W-E")
	leg.OrderType = protocol.OrderTypeMove
	leg.OriginID = "parent-episode-6666"
	leg.OriginClass = protocol.OriginClassAttached

	if _, err := eng.ApplyOrderProjection(leg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got, err := eng.db.GetOrderByUUID("leg-1")
	if err != nil || got == nil {
		t.Fatalf("read back: %v", err)
	}
	if got.OriginID != "parent-episode-6666" {
		t.Errorf("origin_id = %q, want the parent's episode — the leg carries it verbatim", got.OriginID)
	}
	if got.OriginClass != protocol.OriginClassAttached {
		t.Errorf("origin_class = %q, want %q", got.OriginClass, protocol.OriginClassAttached)
	}
	if got.AuthoredBy != "core" {
		t.Errorf("authored_by = %q, want \"core\"", got.AuthoredBy)
	}
	// Nothing on the row says "this is a step of something else". The two
	// columns that could have — sibling_order_id and steps_json — are about
	// swap pairing and Edge-authored plans, and a projection writes neither.
	if got.SiblingOrderID != nil {
		t.Errorf("sibling_order_id = %v, want nil; a projection does not pair legs", got.SiblingOrderID)
	}
}
