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

// MoveInstance moves a payload instance between nodes in SQL and unclaims it.
func (m *Manager) MoveInstance(instanceID, toNodeID int64) error {
	if err := m.db.MoveInstance(instanceID, toNodeID); err != nil {
		return err
	}
	if err := m.db.UnclaimInstance(instanceID); err != nil {
		m.dbg("MoveInstance: unclaim instance %d error (silently dropped): %v", instanceID, err)
	}
	return nil
}

// GetNodeState reads node state from SQL.
func (m *Manager) GetNodeState(nodeID int64) (*NodeState, error) {
	return m.getNodeStateFromSQL(nodeID)
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
	instances, err := m.db.ListInstancesByNode(nodeID)
	if err != nil {
		return nil, err
	}

	items := make([]PayloadItem, len(instances))
	for i, p := range instances {
		items[i] = PayloadItem{
			ID:          p.ID,
			StyleID:     p.StyleID,
			StyleName:   p.StyleName,
			FormFactor:  p.FormFactor,
			Status:      p.Status,
			DeliveredAt: p.DeliveredAt,
			Notes:       p.Notes,
			ClaimedBy:   p.ClaimedBy,
		}
	}

	return &NodeState{
		NodeID:    node.ID,
		NodeName:  node.Name,
		NodeType:  node.NodeType,
		Zone:      node.Zone,
		Capacity:  node.Capacity,
		Enabled:   node.Enabled,
		Items:     items,
		ItemCount: len(items),
	}, nil
}
