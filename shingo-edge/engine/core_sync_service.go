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

// RequestOrderStatusSync asks Core for the authoritative state of every order
// this Edge is tracking — and, since the projection work, for any order Core
// authored for this station that the Edge does not know about at all.
//
// IT SENDS EVEN WITH NOTHING TO ASK ABOUT. It used to return early on an empty
// list, on the reasonable-looking grounds that there was nothing to reconcile.
// That is exactly backwards now: an Edge with no orders is the case most likely
// to be missing one. A fresh install, a wiped database, or a plant that dropped
// every projection would ask nothing, so Core would never get the chance to
// answer, and the hole would never close.
func (s *CoreSyncService) RequestOrderStatusSync() error {
	e := s.engine
	if e.sendFn == nil {
		return fmt.Errorf("send function not configured (messaging not connected)")
	}
	orders, err := e.db.ListActiveOrders()
	if err != nil {
		return err
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

// HandleUnlistedOrders takes the healing half of the reconcile: orders Core
// authored for this station that the Edge did not ask about, because it has no
// row for them.
//
// This is not a rare repair path. The Core → Edge outbox drops a message
// permanently once it exhausts its retries, so a projection that never arrived
// is an ordinary outcome rather than a fault — which is why the reconcile ships
// with the projection rather than after it, and why it is tested first.
func (s *CoreSyncService) HandleUnlistedOrders(items []protocol.OrderProjection) {
	for _, p := range items {
		created, err := s.engine.ApplyOrderProjection(p)
		if err != nil {
			s.engine.debugFn.Log("reconcile: heal unlisted order uuid=%s: %v", p.OrderUUID, err)
			continue
		}
		if created {
			s.engine.logFn("reconcile: order %s was authored by Core and missing here — row created from the reconcile, so its projection never landed",
				p.OrderUUID)
		}
	}
}
