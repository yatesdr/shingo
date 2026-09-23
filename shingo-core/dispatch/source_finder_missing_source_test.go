package dispatch

import (
	"database/sql"
	"errors"
	"testing"

	"shingo/protocol"

	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// A NAMED SOURCE THAT NO LONGER EXISTS WAITS, AND SAYS SO.
//
// The source name is looked up once, at the top of FindSourceForNeed. The
// store answers a name matching no node with sql.ErrNoRows (nodes.ScanNode
// off row.Scan) and a failed read with a wrapped error; readFailed splits the
// two. No such node queues as finder-source-missing, naming it, and never
// widens; a failed read keeps the loader-source-unreadable wait tier 2 always
// gave it; a blank source stays plant-wide.
//
// Pinned at e83cccd1 (c82c8654), when the lookup discarded its error and tier 2
// looked the name up a second time and filed the miss as a read failure:
// full-with-a-part and empty parked loader-source-unreadable, a part-less full
// reached FindSourceBinFIFO, and a move read finder-plant-empty.

type missingSourceDB struct {
	*fakeFinderDB
	nameReads map[string]int
	readErr   error // a real read failure for the source name, when set
}

func (m *missingSourceDB) GetNodeByDotName(name string) (*nodes.Node, error) {
	m.nameReads[name]++
	if n, ok := m.nodesByName[name]; ok {
		return n, nil
	}
	if m.readErr != nil && name == "GONE-NODE" {
		return nil, m.readErr
	}
	return nil, sql.ErrNoRows
}

func missingSourceFixture() *missingSourceDB {
	db := namedSourceFixture() // the P5 fixture: a full of PART-A in an unrelated supermarket
	return &missingSourceDB{fakeFinderDB: db, nameReads: map[string]int{}}
}

func wantSourceMissing(t *testing.T, db *missingSourceDB, res SourceResult) {
	t.Helper()
	if res.Outcome != OutcomeWait || res.QueueCause != CauseFinderSourceMissing {
		t.Fatalf("outcome=%v cause=%q bin=%v, want Wait %q", res.Outcome, res.QueueCause, res.Bin, CauseFinderSourceMissing)
	}
	if !res.QueueParams.SourceMissing || res.QueueParams.Group != "GONE-NODE" {
		t.Errorf("params = %+v, want SourceMissing naming GONE-NODE", res.QueueParams)
	}
	want := "Source GONE-NODE no longer exists — waiting for its configuration to be fixed"
	if got := FormatQueueSentence(res.QueueCode, res.QueueParams); got != want {
		t.Errorf("sentence = %q, want %q", got, want)
	}
	if db.fifoCalls != 0 || db.globalEmptyCalls != 0 || db.typedGlobalCalls != 0 {
		t.Errorf("plant-wide searches fifo/empty/typed = %d/%d/%d, want 0 — a named source never widens",
			db.fifoCalls, db.globalEmptyCalls, db.typedGlobalCalls)
	}
	if got := db.nameReads["GONE-NODE"]; got != 1 {
		t.Errorf("source name read %d times, want 1 (it was 2: tier 2 read it again)", got)
	}
}

func TestMissingSource_FullWithAPart_WaitsNamingIt(t *testing.T) {
	t.Parallel()
	db := missingSourceFixture()
	wantSourceMissing(t, db, NewSourceFinder(db, nil, nil).FindSource(u1Order("GONE-NODE"), IntentFull))
}

func TestMissingSource_Empty_WaitsNamingIt(t *testing.T) {
	t.Parallel()
	db := missingSourceFixture()
	res := NewSourceFinder(db, nil, nil).FindSource(&orders.Order{
		OrderType: OrderTypeRetrieveEmpty, PayloadCode: "PART-A", SourceNode: "GONE-NODE",
		DeliveryNode: "SMN_001", SourceIntent: SourceIntentForType(OrderTypeRetrieveEmpty),
	}, IntentEmpty)
	if res.QueueParams.Kind != "empty" {
		t.Errorf("Kind = %q, want empty", res.QueueParams.Kind)
	}
	wantSourceMissing(t, db, res)
}

func TestMissingSource_FullWithoutAPart_WaitsNamingIt(t *testing.T) {
	t.Parallel()
	db := missingSourceFixture()
	o := u1Order("GONE-NODE")
	o.PayloadCode = ""
	wantSourceMissing(t, db, NewSourceFinder(db, nil, nil).FindSource(o, IntentFull))
}

func TestMissingSource_Move_WaitsNamingIt(t *testing.T) {
	t.Parallel()
	db := missingSourceFixture()
	wantSourceMissing(t, db, NewSourceFinder(db, nil, nil).FindSource(&orders.Order{
		OrderType: OrderTypeMove, SourceNode: "GONE-NODE", DeliveryNode: "SMN_001",
		SourceIntent: SourceIntentForType(OrderTypeMove),
	}, IntentFull))
}

// A READ FAILURE on the source name keeps the loader-source-unreadable wait
// tier 2 gave it, whatever the need's shape. For a move or a part-less full it
// used to read as an empty plant, which reported a failed read as a shortage.
func TestMissingSource_ReadFailureIsNotMissing(t *testing.T) {
	t.Parallel()
	cases := map[string]*orders.Order{
		"full with a part": u1Order("GONE-NODE"),
		"move": {OrderType: OrderTypeMove, SourceNode: "GONE-NODE", DeliveryNode: "SMN_001",
			SourceIntent: SourceIntentForType(OrderTypeMove)},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db := missingSourceFixture()
			db.readErr = errors.New("connection reset")
			res := NewSourceFinder(db, nil, nil).FindSource(o, IntentFull)
			if res.Outcome != OutcomeWait || res.QueueCause != CauseLoaderSourceUnreadable {
				t.Fatalf("outcome=%v cause=%q, want Wait %q", res.Outcome, res.QueueCause, CauseLoaderSourceUnreadable)
			}
			if res.QueueCode != protocol.QueueWaitingForMaterial || res.QueueParams.Group != "GONE-NODE" {
				t.Errorf("code=%q params=%+v, want waiting_for_material naming GONE-NODE", res.QueueCode, res.QueueParams)
			}
			if db.fifoCalls != 0 {
				t.Errorf("FindSourceBinFIFO calls = %d, want 0", db.fifoCalls)
			}
		})
	}
}

// A BLANK source names nothing and stays plant-wide.
func TestMissingSource_BlankSourceStaysPlantWide(t *testing.T) {
	t.Parallel()
	db := missingSourceFixture()
	res := NewSourceFinder(db, nil, nil).FindSource(u1Order(""), IntentFull)
	if res.Outcome != OutcomeFound || res.Bin == nil || res.Bin.ID != 901 || db.fifoCalls != 1 {
		t.Fatalf("outcome=%v bin=%v fifo=%d, want the plant-wide full 901", res.Outcome, res.Bin, db.fifoCalls)
	}
}
