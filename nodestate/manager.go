package nodestate

import (
	"context"
	"log"

	"warpath/store"
)

// Manager provides write-through node state management: SQL first, then Redis.
type Manager struct {
	db    *store.DB
	redis *RedisStore
}

func NewManager(db *store.DB, redis *RedisStore) *Manager {
	return &Manager{db: db, redis: redis}
}

// AddInventory adds an inventory item to a node in SQL and updates Redis.
func (m *Manager) AddInventory(nodeID, materialID int64, quantity float64, isPartial bool, sourceOrderID *int64, notes string) (int64, error) {
	id, err := m.db.AddInventory(nodeID, materialID, quantity, isPartial, sourceOrderID, notes)
	if err != nil {
		return 0, err
	}
	m.refreshNodeRedis(nodeID)
	return id, nil
}

// RemoveInventory removes an inventory item from SQL and updates Redis.
func (m *Manager) RemoveInventory(inventoryID int64) error {
	item, err := m.db.GetInventoryItem(inventoryID)
	if err != nil {
		return err
	}
	nodeID := item.NodeID
	if err := m.db.RemoveInventory(inventoryID); err != nil {
		return err
	}
	m.refreshNodeRedis(nodeID)
	return nil
}

// MoveInventory moves an inventory item between nodes in SQL and updates Redis for both.
func (m *Manager) MoveInventory(inventoryID, toNodeID int64) error {
	item, err := m.db.GetInventoryItem(inventoryID)
	if err != nil {
		return err
	}
	fromNodeID := item.NodeID
	if err := m.db.MoveInventory(inventoryID, toNodeID); err != nil {
		return err
	}
	m.refreshNodeRedis(fromNodeID)
	m.refreshNodeRedis(toNodeID)
	return nil
}

// AdjustQuantity updates the quantity of an inventory item.
func (m *Manager) AdjustQuantity(inventoryID int64, newQty float64) error {
	item, err := m.db.GetInventoryItem(inventoryID)
	if err != nil {
		return err
	}
	if err := m.db.UpdateInventoryQuantity(inventoryID, newQty); err != nil {
		return err
	}
	m.refreshNodeRedis(item.NodeID)
	return nil
}

// GetNodeState reads node state from Redis, falls back to SQL.
func (m *Manager) GetNodeState(nodeID int64) (*NodeState, error) {
	ctx := context.Background()

	meta, err := m.redis.GetNodeMeta(ctx, nodeID)
	if err == nil && meta != nil {
		items, _ := m.redis.GetNodeInventory(ctx, nodeID)
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
	dbItems, err := m.db.ListNodeInventory(nodeID)
	if err != nil {
		log.Printf("nodestate: refresh redis for node %d: %v", nodeID, err)
		return
	}

	items := make([]InventoryItem, len(dbItems))
	for i, di := range dbItems {
		items[i] = InventoryItem{
			ID:           di.ID,
			MaterialID:   di.MaterialID,
			MaterialCode: di.MaterialCode,
			Quantity:     di.Quantity,
			IsPartial:    di.IsPartial,
			DeliveredAt:  di.DeliveredAt,
			Notes:        di.Notes,
			ClaimedBy:    di.ClaimedBy,
		}
	}

	m.redis.SetNodeInventory(ctx, nodeID, items)
	m.redis.SetCount(ctx, nodeID, len(items))
}

func (m *Manager) getNodeStateFromSQL(nodeID int64) (*NodeState, error) {
	node, err := m.db.GetNode(nodeID)
	if err != nil {
		return nil, err
	}
	dbItems, err := m.db.ListNodeInventory(nodeID)
	if err != nil {
		return nil, err
	}

	items := make([]InventoryItem, len(dbItems))
	for i, di := range dbItems {
		items[i] = InventoryItem{
			ID:           di.ID,
			MaterialID:   di.MaterialID,
			MaterialCode: di.MaterialCode,
			Quantity:     di.Quantity,
			IsPartial:    di.IsPartial,
			DeliveredAt:  di.DeliveredAt,
			Notes:        di.Notes,
			ClaimedBy:    di.ClaimedBy,
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
