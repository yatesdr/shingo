package plc

import (
	"testing"

	"shingoedge/config"
)

// ---------------------------------------------------------------------------
// The tags a poll fetched must reach mp.Values.
//
// 076e1fee extracted reconcilePLC out of warlinkSync. In warlinkSync the fetch
// was `tags, err = m.fetchTags(...)` — a plain assignment onto the tags
// declared above it, reusing the enclosing err. Inside the extracted function
// err was no longer in scope, so it became `tags, err := m.fetchTags(...)`,
// which declares a SECOND tags local to the if-block. The fetched map was
// discarded when the block ended and applyTags was handed the outer nil.
//
// Every PLC then reported Connected with zero tags, and every reporting point
// failed "tag X not found on Y" on every poll. No counter climbs, so nothing
// produces, so no swap, changeover or loader loop runs. At a plant that is
// every reporting point going silent while the PLC page stays green.
//
// STATUS IS NOT THE ASSERTION. The whole reason ./plc passed with and without
// the bug is that the status path never broke — the connection tests here
// exercise exactly the transitions that stayed correct. The observable that
// moved is mp.Values, so that is what these pin.
// ---------------------------------------------------------------------------

// TestReconcilePLC_FetchedTagsReachValues is the regression pin: a connected
// PLC whose fetch returns tags must end the poll with those tags readable.
func TestReconcilePLC_FetchedTagsReachValues(t *testing.T) {
	t.Parallel()
	mgr := NewManager(nil, config.Defaults(), &mockEmitter{}, nil)
	mgr.wl = &mockWarlinkClient{
		plcs: []WarlinkPLC{{Name: "PRESS-1", Status: "Connected"}},
		tags: map[string]map[string]WarlinkTag{
			"PRESS-1": {
				"PRESS-1.PRESS-1_COUNTER": {
					PLC:   "PRESS-1",
					Name:  "PRESS-1.PRESS-1_COUNTER",
					Type:  "DINT",
					Value: 42,
				},
			},
		},
	}

	mgr.warlinkPollTick()

	if !mgr.IsConnected("PRESS-1") {
		t.Fatal("PRESS-1 should be connected — the fixture reports Connected with a clean tag")
	}

	// The value, through the same door a reporting point uses.
	v, err := mgr.ReadTag("PRESS-1", "PRESS-1_COUNTER")
	if err != nil {
		t.Fatalf("reading a tag the poll just fetched failed: %v\n"+
			"A connected PLC with zero values is the shadowed-tags shape: the fetch result "+
			"was discarded and applyTags emptied mp.Values, so every reporting point goes "+
			"silent while the PLC page still shows green.", err)
	}
	if v == nil {
		t.Fatal("tag read returned a nil value for a tag the fixture gave a value")
	}

	mp := mgr.GetPLC("PRESS-1")
	if mp == nil {
		t.Fatal("no managed PLC for PRESS-1 after a poll")
	}
	mp.mu.RLock()
	n := len(mp.Values)
	mp.mu.RUnlock()
	if n == 0 {
		t.Fatal("mp.Values is empty after a poll that fetched one tag")
	}
}

// TestReconcilePLC_ConnectionLevelTagErrorStillSkipsApplyTags is the other half
// of the restored semantics, and the half a naive "just return the tags"
// refactor breaks next.
//
// A connection-level tag error leaves tags POPULATED — connectionErrorFromTags
// reads them to make its decision — but flips effectiveStatus to Disconnected,
// and it is effectiveStatus, not the tags, that decides whether applyTags runs.
// So the poll must end with the values CLEARED even though a non-empty map was
// fetched. Anything that keys applyTags off "did we get tags" instead would
// publish the values of a PLC that is not talking.
func TestReconcilePLC_ConnectionLevelTagErrorStillSkipsApplyTags(t *testing.T) {
	t.Parallel()
	mgr := NewManager(nil, config.Defaults(), &mockEmitter{}, nil)
	mgr.wl = &mockWarlinkClient{
		plcs: []WarlinkPLC{{Name: "PRESS-2", Status: "Connected"}},
		tags: map[string]map[string]WarlinkTag{
			"PRESS-2": {
				"PRESS-2.PRESS-2_COUNTER": {
					PLC:   "PRESS-2",
					Name:  "PRESS-2.PRESS-2_COUNTER",
					Type:  "DINT",
					Value: 7,
					Error: "ReadMultiple: SendUnitDataTransaction: not connected",
				},
			},
		},
	}

	mgr.warlinkPollTick()

	if mgr.IsConnected("PRESS-2") {
		t.Fatal("a connection-level error on every tag must read as disconnected")
	}
	mp := mgr.GetPLC("PRESS-2")
	if mp == nil {
		t.Fatal("no managed PLC for PRESS-2 after a poll")
	}
	mp.mu.RLock()
	n := len(mp.Values)
	mp.mu.RUnlock()
	if n != 0 {
		t.Fatalf("values = %d, want 0: the fetch returned a non-empty map, but effectiveStatus "+
			"is Disconnected and that is what gates applyTags", n)
	}
}
