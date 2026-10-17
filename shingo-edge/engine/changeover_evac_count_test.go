package engine

import (
	"testing"

	"shingoedge/store/processes"
)

// TestChangeoverEvac_TheCountIsTheSeatsOrTheReleaseIsRefused: with no
// operator count, a changeover evac sends back the seat's count (the landing
// seeds it from Core's, the ticks count it down). When the seat cannot be
// read the count is unknown: the release is refused rather than sent as
// release_empty, which would clear a carrier that may be full.
func TestChangeoverEvac_TheCountIsTheSeatsOrTheReleaseIsRefused(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	_, nodeID, _, _ := seedConsumeNode(t, db, consumeNodeConfig{
		Prefix: "EVAC-COUNT", PayloadCode: "EVAC-PART", UOPCapacity: 20, InitialUOP: 13})
	eng := testEngine(t, db)

	got, err := evacDispositionForTask(eng, processes.NodeTask{ProcessNodeID: nodeID, NodeName: "EVAC-COUNT"}, ReleaseDisposition{})
	if err != nil || got.Mode != DispositionSendPartialBack || got.PartialCount == nil || *got.PartialCount != 13 {
		t.Errorf("a counted seat: %+v (%v), want send_partial_back with its 13", got, err)
	}
	got, err = evacDispositionForTask(eng, processes.NodeTask{ProcessNodeID: nodeID + 1000, NodeName: "UNREAD"}, ReleaseDisposition{})
	if err == nil {
		t.Errorf("an unread seat: %+v, want the release refused, not %s", got, got.Mode)
	}
}
