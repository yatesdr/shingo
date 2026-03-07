package nodestate

import (
	"shingocore/store"
)

// Manager provides node state management backed by SQL.
type Manager struct {
	db       *store.DB
	DebugLog func(string, ...any)
}

func (m *Manager) dbg(format string, args ...any) {
	if fn := m.DebugLog; fn != nil {
		fn(format, args...)
	}
}

func NewManager(db *store.DB) *Manager {
	return &Manager{db: db}
}

// MoveBin moves a bin between nodes in SQL and unclaims the payload on it.
func (m *Manager) MoveBin(binID, toNodeID int64) error {
	if err := m.db.MoveBin(binID, toNodeID); err != nil {
		return err
	}
	return nil
}

// GetAllNodeStates reads all node states from SQL.
func (m *Manager) GetAllNodeStates() (map[int64]*NodeState, error) {
	nodes, err := m.db.ListNodes()
	if err != nil {
		return nil, err
	}
	states := make(map[int64]*NodeState, len(nodes))
	for _, node := range nodes {
		state, err := m.getNodeStateFromSQL(node.ID)
		if err != nil {
			continue
		}
		states[node.ID] = state
	}
	return states, nil
}

func (m *Manager) getNodeStateFromSQL(nodeID int64) (*NodeState, error) {
	node, err := m.db.GetNode(nodeID)
	if err != nil {
		return nil, err
	}
	payloads, err := m.db.ListPayloadsByNode(nodeID)
	if err != nil {
		return nil, err
	}

	items := make([]PayloadItem, len(payloads))
	for i, p := range payloads {
		items[i] = PayloadItem{
			ID:                p.ID,
			BlueprintID:       p.BlueprintID,
			BlueprintCode:     p.BlueprintCode,
			BinID:             p.BinID,
			BinLabel:          p.BinLabel,
			ManifestConfirmed: p.ManifestConfirmed,
			Notes:             p.Notes,
			ClaimedBy:         p.ClaimedBy,
		}
	}

	return &NodeState{
		NodeID:    node.ID,
		NodeName:  node.Name,
		Zone:      node.Zone,
		Enabled:   node.Enabled,
		Items:     items,
		ItemCount: len(items),
	}, nil
}
