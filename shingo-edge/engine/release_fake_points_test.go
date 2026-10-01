package engine

import (
	"shingo/protocol"
	"shingoedge/engine/releasefake"
	"shingoedge/store"
)

// release_fake_points_test.go — the harness's Core answer to releasePoints
// (engine/releasefake), with the harness's own lifts and its statement
// accounting.
func (h *relHarness) fakePoints(req protocol.ReleasePointsRequest) protocol.ReleasePointsResponse {
	r0, w0 := h.counter.Reads(), h.counter.Writes()
	resp := releasefake.Points(h.db, h.lifted, req)
	h.coreReads += h.counter.Reads() - r0
	h.coreWrites += h.counter.Writes() - w0
	return resp
}

// dbPoints is the fake as a test engine's point source: no lift is ever
// reported to it, so every node holds its bin.
type dbPoints struct{ db *store.DB }

func (p dbPoints) ReleasePoints(station string, uuids []string) ([]protocol.ReleasePoint, error) {
	return releasefake.DB{DB: p.db}.ReleasePoints(station, uuids)
}
