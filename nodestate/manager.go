package nodestate

import (
	"context"
	"log"

	"shingocore/store"
)

// Manager provides write-through node state management: SQL first, then Redis.
type Manager struct {
	db    *store.DB
	redis *RedisStore
}

func NewManager(db *store.DB, redis *RedisStore) *Manager {
	return &Manager{db: db, redis: redis}
}

// AddPayload creates a payload in SQL and refreshes Redis for its node.
func (m *Manager) AddPayload(p *store.Payload) error {
	if err := m.db.CreatePayload(p); err != nil {
		return err
	}
	if p.NodeID != nil {
		m.refreshNodeRedis(*p.NodeID)
	}
	return nil
}

// RemovePayload deletes a payload from SQL and refreshes Redis.
func (m *Manager) RemovePayload(payloadID int64) error {
	p, err := m.db.GetPayload(payloadID)
	if err != nil {
		return err
	}
	if err := m.db.DeletePayload(payloadID); err != nil {
		return err
	}
	if p.NodeID != nil {
		m.refreshNodeRedis(*p.NodeID)
	}
	return nil
}

// MovePayload moves a payload between nodes in SQL, unclaims it, and refreshes Redis for both.
func (m *Manager) MovePayload(payloadID, toNodeID int64) error {
	p, err := m.db.GetPayload(payloadID)
	if err != nil {
		return err
	}
	fromNodeID := p.NodeID
	if err := m.db.MovePayload(payloadID, toNodeID); err != nil {
		return err
	}
	m.db.UnclaimPayload(payloadID)
	if fromNodeID != nil {
		m.refreshNodeRedis(*fromNodeID)
	}
	m.refreshNodeRedis(toNodeID)
	return nil
}

// GetNodeState reads node state from Redis, falls back to SQL.
func (m *Manager) GetNodeState(nodeID int64) (*NodeState, error) {
	ctx := context.Background()

	meta, err := m.redis.GetNodeMeta(ctx, nodeID)
	if err == nil && meta != nil {
		items, _ := m.redis.GetNodePayloads(ctx, nodeID)
		count, _ := m.redis.GetCount(ctx, nodeID)
		return &NodeState{
			NodeID:    meta.NodeID,
			NodeName:  meta.NodeName,
			NodeType:  meta.NodeType,
			Zone:      meta.Zone,
			Capacity:  meta.Capacity,
			Enabled:   meta.Enabled,
			Items:     items,
			ItemCount: count,
		}, nil
	}

	// Fall back to SQL
	return m.getNodeStateFromSQL(nodeID)
}

// GetAllNodeStates reads all node states, preferring Redis.
func (m *Manager) GetAllNodeStates() (map[int64]*NodeState, error) {
	ctx := context.Background()
	states := make(map[int64]*NodeState)

	nodeIDs, err := m.redis.GetAllNodeIDs(ctx)
	if err == nil && len(nodeIDs) > 0 {
		for _, id := range nodeIDs {
			state, err := m.GetNodeState(id)
			if err == nil {
				states[id] = state
			}
		}
		return states, nil
	}

	// Fall back to SQL
	nodes, err := m.db.ListNodes()
	if err != nil {
		return nil, err
	}
	for _, node := range nodes {
		state, err := m.getNodeStateFromSQL(node.ID)
		if err != nil {
			continue
		}
		states[node.ID] = state
	}
	return states, nil
}

// SyncRedisFromSQL rebuilds all Redis state from SQL. Called on startup.
func (m *Manager) SyncRedisFromSQL() error {
	ctx := context.Background()
	m.redis.FlushAll(ctx)

	nodes, err := m.db.ListNodes()
	if err != nil {
		return err
	}

	for _, node := range nodes {
		meta := &NodeMeta{
			NodeID:   node.ID,
			NodeName: node.Name,
			NodeType: node.NodeType,
			Zone:     node.Zone,
			Capacity: node.Capacity,
			Enabled:  node.Enabled,
		}
		if err := m.redis.UpdateNodeMeta(ctx, node.ID, meta); err != nil {
			log.Printf("nodestate: sync meta for node %d: %v", node.ID, err)
			continue
		}
		m.refreshNodeRedis(node.ID)
	}

	log.Printf("nodestate: synced %d nodes to redis", len(nodes))
	return nil
}

// RefreshNodeMeta updates the Redis meta for a node from its DB record.
func (m *Manager) RefreshNodeMeta(nodeID int64) {
	node, err := m.db.GetNode(nodeID)
	if err != nil {
		return
	}
	ctx := context.Background()
	meta := &NodeMeta{
		NodeID:   node.ID,
		NodeName: node.Name,
		NodeType: node.NodeType,
		Zone:     node.Zone,
		Capacity: node.Capacity,
		Enabled:  node.Enabled,
	}
	m.redis.UpdateNodeMeta(ctx, node.ID, meta)
}

func (m *Manager) refreshNodeRedis(nodeID int64) {
	ctx := context.Background()
	dbPayloads, err := m.db.ListPayloadsByNode(nodeID)
	if err != nil {
		log.Printf("nodestate: refresh redis for node %d: %v", nodeID, err)
		return
	}

	items := make([]PayloadItem, len(dbPayloads))
	for i, p := range dbPayloads {
		items[i] = PayloadItem{
			ID:              p.ID,
			PayloadTypeID:   p.PayloadTypeID,
			PayloadTypeName: p.PayloadTypeName,
			FormFactor:      p.FormFactor,
			Status:          p.Status,
			DeliveredAt:     p.DeliveredAt,
			Notes:           p.Notes,
			ClaimedBy:       p.ClaimedBy,
		}
	}

	m.redis.SetNodePayloads(ctx, nodeID, items)
	m.redis.SetCount(ctx, nodeID, len(items))
}

func (m *Manager) getNodeStateFromSQL(nodeID int64) (*NodeState, error) {
	node, err := m.db.GetNode(nodeID)
	if err != nil {
		return nil, err
	}
	dbPayloads, err := m.db.ListPayloadsByNode(nodeID)
	if err != nil {
		return nil, err
	}

	items := make([]PayloadItem, len(dbPayloads))
	for i, p := range dbPayloads {
		items[i] = PayloadItem{
			ID:              p.ID,
			PayloadTypeID:   p.PayloadTypeID,
			PayloadTypeName: p.PayloadTypeName,
			FormFactor:      p.FormFactor,
			Status:          p.Status,
			DeliveredAt:     p.DeliveredAt,
			Notes:           p.Notes,
			ClaimedBy:       p.ClaimedBy,
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
