// loader_service.go — CRUD for the Core-owned bin_loaders aggregate (loader
// refactor: Core authors loader config; the Nodes-page Create-Loader UI and the
// per-payload UOP-threshold editors call this). Every mutating call re-derives
// demand_registry from the aggregate and fires the threshold monitor, so a
// config edit engages immediately; the Edge re-pulls the new LoaderInfos on its
// next node-list sync (config_gen bumps on each write).

package service

import (
	"fmt"

	"shingocore/store"
	"shingocore/store/demands"
	"shingocore/store/loaders"
)

// ThresholdNotifier is the monitor hook the service fires after a re-derive.
// engine.ThresholdMonitor satisfies it; declared here so service does not import
// engine (which would be a cycle).
type ThresholdNotifier interface {
	OnThresholdChanges(changes []demands.RegistryChange)
}

// LoaderService wraps the bin_loaders store CRUD with the demand re-derive.
type LoaderService struct {
	db       *store.DB
	notifier ThresholdNotifier
}

// NewLoaderService constructs the service. notifier may be nil (re-derive still
// rewrites demand_registry; it just skips the immediate monitor nudge).
func NewLoaderService(db *store.DB, notifier ThresholdNotifier) *LoaderService {
	return &LoaderService{db: db, notifier: notifier}
}

// ── Reads (no re-derive) ──────────────────────────────────────────────

func (s *LoaderService) List() ([]loaders.Loader, error) { return s.db.ListLoaders() }
func (s *LoaderService) Get(id int64) (*loaders.Loader, error) {
	return s.db.GetLoader(id)
}
func (s *LoaderService) Payloads(loaderID int64) ([]loaders.Payload, error) {
	return s.db.ListLoaderPayloads(loaderID)
}
func (s *LoaderService) Homes(loaderID int64) ([]loaders.Home, error) {
	return s.db.ListLoaderHomes(loaderID)
}

// ── Writes (re-derive after each) ─────────────────────────────────────

// Create persists a new loader and re-derives. Takes primitives (not a store
// type) so www handlers can call it without importing the store. Empty
// layout/replenishment default to shared_window/auto. The loader's identity is the
// surrogate id returned here; member nodes are dragged in afterward. Returns the new id.
func (s *LoaderService) Create(name, role, layout, replenishment, outboundDest, inboundSource, bufferDest string) (int64, error) {
	if layout == "" {
		layout = loaders.LayoutSharedWindow
	}
	if replenishment == "" {
		replenishment = loaders.ReplenishmentAuto
	}
	id, err := s.db.CreateLoader(loaders.Loader{
		Name: name, Role: role, Layout: layout,
		Replenishment: replenishment, OutboundDest: outboundDest,
		InboundSource: inboundSource, BufferDest: bufferDest,
	})
	if err != nil {
		return 0, err
	}
	s.rederive()
	return id, nil
}

// Update changes a loader's editable fields and re-derives. role + the surrogate id
// are the identity and stay fixed; layout/replenishment default to
// the current value when blank. The shared_window flow endpoints
// (inbound/outbound/buffer) are passed through verbatim — a dedicated_positions
// loader sends them empty (each position is its own in/out).
func (s *LoaderService) Update(id int64, name, layout, replenishment, outboundDest, inboundSource, bufferDest string) error {
	cur, err := s.db.GetLoader(id)
	if err != nil {
		return err
	}
	if cur == nil {
		return fmt.Errorf("loader %d not found", id)
	}
	if layout == "" {
		layout = cur.Layout
	}
	if replenishment == "" {
		replenishment = cur.Replenishment
	}
	cur.Name = name
	cur.Layout = layout
	cur.Replenishment = replenishment
	cur.OutboundDest = outboundDest
	cur.InboundSource = inboundSource
	cur.BufferDest = bufferDest
	if err := s.db.UpdateLoader(*cur); err != nil {
		return err
	}
	s.rederive()
	return nil
}

// Delete removes a loader (cascades its homes + payloads) and re-derives.
func (s *LoaderService) Delete(id int64) error {
	if err := s.db.DeleteLoader(id); err != nil {
		return err
	}
	s.rederive()
	return nil
}

// SetPayload assigns (or updates) a shared_window payload binding + threshold.
func (s *LoaderService) SetPayload(loaderID int64, payloadCode string, minStock, uopThreshold int) error {
	if err := s.db.UpsertLoaderPayload(loaders.Payload{
		LoaderID: loaderID, PayloadCode: payloadCode, MinStock: minStock, UOPThreshold: uopThreshold,
	}); err != nil {
		return err
	}
	s.rederive()
	return nil
}

// SetHome assigns (or updates) a dedicated-position binding + threshold. A new
// position is appended at the end of the loader's order; re-setting an existing
// one preserves its place (the store ignores sort_order on conflict). payloadCode
// may be empty — the grid-drag drops a node first, then the operator assigns its
// payload via the inline picker.
func (s *LoaderService) SetHome(loaderID, positionNodeID int64, payloadCode string, minStock, uopThreshold int) error {
	existing, err := s.db.ListLoaderHomes(loaderID)
	if err != nil {
		return err
	}
	if err := s.db.UpsertLoaderHome(loaders.Home{
		LoaderID: loaderID, PositionNodeID: positionNodeID, PayloadCode: payloadCode,
		MinStock: minStock, UOPThreshold: uopThreshold, SortOrder: len(existing),
	}); err != nil {
		return err
	}
	s.rederive()
	return nil
}

// RemoveHome clears a dedicated position from a loader.
func (s *LoaderService) RemoveHome(loaderID, positionNodeID int64) error {
	if err := s.db.RemoveLoaderHome(loaderID, positionNodeID); err != nil {
		return err
	}
	s.rederive()
	return nil
}

// ReorderHomes rewrites the position order to match orderedNodeIDs (the
// grid-drag sequence).
func (s *LoaderService) ReorderHomes(loaderID int64, orderedNodeIDs []int64) error {
	if err := s.db.SetLoaderHomeOrder(loaderID, orderedNodeIDs); err != nil {
		return err
	}
	s.rederive()
	return nil
}

// RemovePayload drops a shared_window payload binding.
func (s *LoaderService) RemovePayload(loaderID int64, payloadCode string) error {
	if err := s.db.RemoveLoaderPayload(loaderID, payloadCode); err != nil {
		return err
	}
	s.rederive()
	return nil
}

// rederive rebuilds demand_registry from the aggregate for every station that
// carries loader-driven demand and nudges the threshold monitor. v1 re-derives
// each distinct station already present in the registry (the dev sim has one);
// per-station scoping by node_stations is a documented refinement.
func (s *LoaderService) rederive() {
	stations, err := s.db.DemandRegistryStations()
	if err != nil {
		return
	}
	for _, st := range stations {
		entries, err := s.db.BuildDemandRegistryFromAggregate(st)
		if err != nil {
			continue
		}
		changes, err := s.db.SyncDemandRegistry(st, entries)
		if err != nil {
			continue
		}
		if s.notifier != nil && len(changes) > 0 {
			s.notifier.OnThresholdChanges(changes)
		}
	}
}
