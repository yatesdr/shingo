package material

import (
	"errors"

	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// fakeStore is an in-memory Store used by the material unit tests.
// It is intentionally dumb: every method reads a map and returns.
//
// Kept tiny on purpose. If a test needs a condition the fake does
// not model, add it inline in that test rather than growing this
// struct.
type fakeStore struct {
	nodes map[int64]*nodes.Node
	props map[int64]map[string]string
	bins  map[int64]*bins.Bin

	// totals[boundaryID] -> map[catID]qty, returned verbatim by
	// SumCatIDsAtBoundary.
	totals map[int64]map[string]int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		nodes:  map[int64]*nodes.Node{},
		props:  map[int64]map[string]string{},
		bins:   map[int64]*bins.Bin{},
		totals: map[int64]map[string]int64{},
	}
}

// setProp seeds a node property (log_cms_transactions in practice).
func (f *fakeStore) setProp(nodeID int64, key, value string) {
	if f.props[nodeID] == nil {
		f.props[nodeID] = map[string]string{}
	}
	f.props[nodeID][key] = value
}

// --- Store interface ---------------------------------------------

func (f *fakeStore) GetNode(id int64) (*nodes.Node, error) {
	n, ok := f.nodes[id]
	if !ok {
		return nil, errors.New("node not found")
	}
	return n, nil
}

func (f *fakeStore) GetNodeProperty(nodeID int64, key string) string {
	return f.props[nodeID][key]
}

func (f *fakeStore) GetBin(id int64) (*bins.Bin, error) {
	b, ok := f.bins[id]
	if !ok {
		return nil, errors.New("bin not found")
	}
	return b, nil
}

func (f *fakeStore) SumCatIDsAtBoundary(boundaryID int64) map[string]int64 {
	out := make(map[string]int64, len(f.totals[boundaryID]))
	for k, v := range f.totals[boundaryID] {
		out[k] = v
	}
	return out
}
