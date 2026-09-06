package material

import (
	"errors"

	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
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

	// propErrs[nodeID] makes GetNodePropertyOrError fail for that node.
	propErrs map[int64]bool

	// The payload templates, by code, and their manifest lines by payload id.
	payloads  map[string]*payloads.Payload
	templates map[int64][]*payloads.ManifestItem
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		nodes:     map[int64]*nodes.Node{},
		props:     map[int64]map[string]string{},
		bins:      map[int64]*bins.Bin{},
		payloads:  map[string]*payloads.Payload{},
		templates: map[int64][]*payloads.ManifestItem{},
	}
}

// setTemplate registers a payload and its per-cycle ratios in one call, since
// no test wants one without the other.
func (f *fakeStore) setTemplate(id int64, code string, capacity int, perCycle map[string]int64) {
	f.payloads[code] = &payloads.Payload{ID: id, Code: code, UOPCapacity: capacity}
	items := make([]*payloads.ManifestItem, 0, len(perCycle))
	for part, n := range perCycle {
		items = append(items, &payloads.ManifestItem{PayloadID: id, PartNumber: part, PartsPerCycle: n})
	}
	f.templates[id] = items
}

// setProp seeds a node property (cms_storeroom in practice).
func (f *fakeStore) setProp(nodeID int64, key, value string) {
	if f.props[nodeID] == nil {
		f.props[nodeID] = map[string]string{}
	}
	f.props[nodeID][key] = value
}

// failProps names node ids whose property reads fail, so a test can tell the
// walk apart from a store that could not answer it.
func (f *fakeStore) failProp(nodeID int64) {
	if f.propErrs == nil {
		f.propErrs = map[int64]bool{}
	}
	f.propErrs[nodeID] = true
}

// --- Store interface ---------------------------------------------

func (f *fakeStore) GetNode(id int64) (*nodes.Node, error) {
	n, ok := f.nodes[id]
	if !ok {
		return nil, errors.New("node not found")
	}
	return n, nil
}

func (f *fakeStore) GetNodePropertyOrError(nodeID int64, key string) (string, error) {
	if f.propErrs[nodeID] {
		return "", errors.New("property read failed")
	}
	return f.props[nodeID][key], nil
}

func (f *fakeStore) GetBin(id int64) (*bins.Bin, error) {
	b, ok := f.bins[id]
	if !ok {
		return nil, errors.New("bin not found")
	}
	return b, nil
}

func (f *fakeStore) GetPayloadByCode(code string) (*payloads.Payload, error) {
	p, ok := f.payloads[code]
	if !ok {
		return nil, errors.New("payload not found")
	}
	return p, nil
}

func (f *fakeStore) ListPayloadManifest(payloadID int64) ([]*payloads.ManifestItem, error) {
	return f.templates[payloadID], nil
}
