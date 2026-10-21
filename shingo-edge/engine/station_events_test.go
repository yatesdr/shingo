package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// station_events_test.go — the events a station board rebuilds on that the
// Edge did not emit before: the refusal table changing, and a sourcing report
// arriving. A station rebuilds on each when it names something the station
// draws (operator-state.js eventConcerns).

// refusalEvents records every EventSupplyRefusalChanged as "node/part".
func refusalEvents(eng *Engine) *[]string {
	var got []string
	eng.Events.Subscribe(func(evt Event) {
		if p, ok := evt.Payload.(SupplyRefusalChangedEvent); ok && evt.Type == EventSupplyRefusalChanged {
			got = append(got, p.LoaderNode+"/"+p.PayloadCode)
		}
	})
	return &got
}

// TestSupplyRefusalChanged_EveryDoorThatWritesTheTable: refuse, an ack, an
// undo that removed a row, a broadcast applied from another Edge, the clear a
// LOAD makes, and the apply of Core's snapshot each emit one event; an undo of
// nothing emits none.
func TestSupplyRefusalChanged_EveryDoorThatWritesTheTable(t *testing.T) {
	t.Parallel()
	f := seedLoaderCard(t)
	got := refusalEvents(f.eng)
	f.call(t, "PART-A")

	testutil.MustNoErr(t, f.eng.RefuseSupply(f.nodeID, "PART-A", "Bin Loader"), "refuse")
	testutil.MustNoErr(t, f.eng.AckSupplyRefusal(f.nodeID, f.core, "PART-A", protocol.SupplyRefusalChoiceWait), "ack")
	testutil.MustNoErr(t, f.eng.UndoSupplyRefusal(f.nodeID, "PART-A"), "undo")
	testutil.MustNoErr(t, f.eng.UndoSupplyRefusal(f.nodeID, "PART-A"), "undo of nothing")
	f.eng.HandleSupplyRefusalState(protocol.SupplyRefusalState{
		Action: protocol.SupplyRefusalOpened, LoaderNode: "SMN_099", PayloadCode: "PART-Z", RefusedBy: "Another Edge",
	})
	testutil.MustNoErr(t, f.eng.RefuseSupply(f.nodeID, "PART-B", "Bin Loader"), "refuse PART-B")
	f.eng.clearRefusalOnLoad(f.core, "PART-B")
	testutil.MustNoErr(t, f.eng.ApplySupplyRefusalSnapshot(protocol.SupplyRefusalSnapshot{Digest: "d"}), "snapshot")

	want := []string{"SMN_014/PART-A", "SMN_014/PART-A", "SMN_014/PART-A", "SMN_099/PART-Z",
		"SMN_014/PART-B", "SMN_014/PART-B", "/"}
	if strings.Join(*got, " ") != strings.Join(want, " ") {
		t.Errorf("refusal events %v\nwant %v (refuse, ack, undo; none for an undo of nothing; the "+
			"broadcast; refuse and the LOAD's clear; the snapshot, naming nothing)", *got, want)
	}
}

// TestSourcingStateChanged_EveryReport: every sourcing report Core sends is
// announced once it is stored, the periodic full one included, naming the
// processes it carries; nothing is read to tell whether a verdict moved. An
// empty delta stores nothing and announces nothing.
func TestSourcingStateChanged_EveryReport(t *testing.T) {
	t.Parallel()
	eng := testEngine(t, testEngineDB(t))
	var got []string
	eng.Events.Subscribe(func(evt Event) {
		if p, ok := evt.Payload.(SourcingStateChangedEvent); ok && evt.Type == EventSourcingStateChanged {
			got = append(got, "["+strings.Join(p.Processes, ",")+"]")
		}
	})
	eng.HandleSourcingState(protocol.SourcingStateReport{Snapshot: true, States: []protocol.SourcingState{
		{ProcessID: "PRESS-1", StyleID: "A", Status: "green"}, {ProcessID: "PRESS-2", StyleID: "B", Status: "red"},
		{ProcessID: "PRESS-1", StyleID: "C", Status: "green"},
	}})
	eng.HandleSourcingState(protocol.SourcingStateReport{Snapshot: true, States: []protocol.SourcingState{
		{ProcessID: "PRESS-1", StyleID: "A", Status: "green"}, {ProcessID: "PRESS-2", StyleID: "B", Status: "red"},
		{ProcessID: "PRESS-1", StyleID: "C", Status: "green"},
	}})
	eng.HandleSourcingState(protocol.SourcingStateReport{States: []protocol.SourcingState{
		{ProcessID: "PRESS-3", StyleID: "D", Status: "red"},
	}})
	eng.HandleSourcingState(protocol.SourcingStateReport{})
	eng.HandleSourcingState(protocol.SourcingStateReport{Snapshot: true})

	want := []string{"[PRESS-1,PRESS-2]", "[PRESS-1,PRESS-2]", "[PRESS-3]", "[]"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("sourcing events %v\nwant %v (each stored report, the same snapshot twice too; "+
			"none for an empty delta; an empty snapshot names nothing)", got, want)
	}
}
