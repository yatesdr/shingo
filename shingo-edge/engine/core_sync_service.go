package engine

import (
	"fmt"

	"shingo/protocol"
)

type CoreSyncService struct {
	engine *Engine
}

func newCoreSyncService(e *Engine) *CoreSyncService {
	return &CoreSyncService{engine: e}
}

func (s *CoreSyncService) StartupReconcile() error {
	s.engine.RequestNodeSync()
	s.engine.RequestCatalogSync()
	// Bin/bucket reconciliation removed with the bin-ownership flip:
	// Edge owns the count for any bin physically at lineside, ships
	// deltas to Core via the outbox, and trusts the Kafka pipeline.
	// No reverse heal; FlushFailures + consumer-lag dashboards surface
	// pipeline health instead.
	return s.RequestOrderStatusSync()
}

func (s *CoreSyncService) RequestOrderStatusSync() error {
	e := s.engine
	if e.sendFn == nil {
		return fmt.Errorf("send function not configured (messaging not connected)")
	}
	orders, err := e.db.ListActiveOrders()
	if err != nil {
		return err
	}
	if len(orders) == 0 {
		return nil
	}
	uuids := make([]string, 0, len(orders))
	for _, order := range orders {
		uuids = append(uuids, order.UUID)
	}
	env, err := protocol.NewDataEnvelope(
		protocol.SubjectOrderStatusRequest,
		protocol.Address{Role: protocol.RoleEdge, Station: e.cfg.StationID()},
		protocol.Address{Role: protocol.RoleCore},
		&protocol.OrderStatusRequest{OrderUUIDs: uuids},
	)
	if err != nil {
		return err
	}
	return e.sendFn(env)
}

func (s *CoreSyncService) HandleOrderStatusSnapshots(items []protocol.OrderStatusSnapshot) {
	for _, item := range items {
		if err := s.engine.orderMgr.ApplyCoreStatusSnapshot(item); err != nil {
			s.engine.debugFn.Log("startup reconcile: uuid=%s err=%v", item.OrderUUID, err)
		}
	}
}
