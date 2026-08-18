// loader_service.go — CRUD for the Core-owned bin_loaders aggregate (loader
// refactor: Core authors loader config; the Nodes-page Create-Loader UI and the
// per-payload UOP-threshold editors call this). Every mutating call re-derives
// demand_registry from the aggregate and fires the threshold monitor, so a
// config edit engages immediately; the Edge re-pulls the new LoaderInfos on its
// next node-list sync (config_gen bumps on each write).

package service

import (
	"errors"
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

// ErrConsumeThreshold refuses a consume loader set to threshold replenishment.
//
// The combination is storable — the CHECK constraint on replenishment is
// role-blind — and it produces a loader that does nothing at all. Core derives
// its thresholds into demand_registry and fires LoopBelowThresholdSignal at it;
// the Edge drops every one of those, because all three loader-resolution tiers
// on the threshold path ask for the produce role. Meanwhile the drain that IS a
// consume loader's job is skipped for exactly this replenishment value. So the
// unloader neither drains nor replenishes, and the existing misconfiguration
// warning stays silent, because that one only catches threshold mode with no
// threshold VALUE — and this loader has values.
//
// Nothing about that is visible from the outside. The loader appears configured,
// the boot log says nothing, and the operator finds out when bins stop moving.
// Refusing it at the door is the whole fix: a consume threshold mode may exist
// one day (the drain is a kanban problem too), and when it does, this error is
// what a reader will grep to find every place that has to change.
var ErrConsumeThreshold = errors.New("an unloader cannot be threshold-driven: consume loaders drain, and nothing acts on a consume threshold")

// checkReplenishment rejects the one role/replenishment pair that is storable
// but inert. Shared by Create and Update so a loader cannot be edited into a
// shape it could not have been created in.
func checkReplenishment(role, replenishment string) error {
	if role == loaders.RoleConsume && replenishment == loaders.ReplenishmentThreshold {
		return ErrConsumeThreshold
	}
	return nil
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

// Quotas is how many carriers of each type the loader wants on hand.
func (s *LoaderService) Quotas(loaderID int64) ([]loaders.Quota, error) {
	return s.db.ListLoaderQuotas(loaderID)
}

// WindowBinTypes is what each window can physically take, keyed by position
// node id. A window absent from the map takes anything.
func (s *LoaderService) WindowBinTypes(loaderID int64) (map[int64][]string, error) {
	return s.db.ListLoaderHomeBinTypes(loaderID)
}

// ── Writes (re-derive after each) ─────────────────────────────────────

// Create persists a new loader and re-derives. Takes primitives (not a store
// type) so www handlers can call it without importing the store. Empty layout
// defaults to shared_window; empty replenishment defaults role-aware
// (produce→threshold, consume→operator). The loader's identity is the surrogate
// id returned here; member nodes are dragged in afterward. Returns the new id.
//
// funnelWindows is accepted here because the KIND of loader is a property of
// the loader, not of its members — the same way layout is, which create has
// always taken. The form asks it first, as the question that decides which
// other questions appear, so a create that dropped it contradicted the screen
// that sent it.
func (s *LoaderService) Create(name, role, layout, replenishment, outboundDest, inboundSource string, funnelWindows bool) (int64, error) {
	if layout == "" {
		layout = loaders.LayoutSharedWindow
	}
	if replenishment == "" {
		// Role-aware default: a produce loader is threshold-driven (UOP kanban
		// autoreorder); a consume loader (unloader) is always operator (the
		// window-queue drain — no consume threshold mode today).
		if role == loaders.RoleConsume {
			replenishment = loaders.ReplenishmentOperator
		} else {
			replenishment = loaders.ReplenishmentThreshold
		}
	}
	if err := checkReplenishment(role, replenishment); err != nil {
		return 0, err
	}
	if err := s.checkInboundSource(inboundSource); err != nil {
		return 0, err
	}
	id, err := s.db.CreateLoader(loaders.Loader{
		Name: name, Role: role, Layout: layout,
		Replenishment: replenishment, OutboundDest: outboundDest,
		InboundSource: inboundSource,
		FunnelWindows: funnelWindows,
	})
	if err != nil {
		return 0, err
	}
	s.rederive()
	return id, nil
}

// Update changes a loader's editable fields and re-derives. role + the surrogate id
// are the identity and stay fixed; layout/replenishment default to
// the current value when blank. The flow endpoints (inbound/outbound) are
// passed through verbatim.
//
// funnelWindows is settable at Create as well as here. It used to be editable
// only on this path, on the reasoning that a loader is created before its
// windows are dragged in, so at creation there was nothing to funnel. That held
// while the control was a checkbox on a form about members. It stopped holding
// when the kind became the form's first question — the operator now answers it
// before anything else, and a create that ignored the answer silently produced
// a spread loader and then re-rendered the form showing it.
func (s *LoaderService) Update(id int64, name, layout, replenishment, outboundDest, inboundSource string, funnelWindows bool) error {
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
	// cur.Role, not a parameter: role is the identity and Update never changes it,
	// so this checks the pair that will actually be stored.
	if err := checkReplenishment(cur.Role, replenishment); err != nil {
		return err
	}
	cur.Name = name
	cur.Layout = layout
	cur.Replenishment = replenishment
	cur.OutboundDest = outboundDest
	if err := s.checkInboundSource(inboundSource); err != nil {
		return err
	}
	cur.InboundSource = inboundSource
	cur.FunnelWindows = funnelWindows
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
// (The bin-count floor / min_stock is retired — replenishment is operator or
// UOP-threshold, never bin-count.)
func (s *LoaderService) SetPayload(loaderID int64, payloadCode string, uopThreshold int) error {
	if err := s.db.UpsertLoaderPayload(loaders.Payload{
		LoaderID: loaderID, PayloadCode: payloadCode, UOPThreshold: uopThreshold,
	}); err != nil {
		return err
	}
	s.rederive()
	return nil
}

// SetQuota declares how many carriers of one bin type a loader wants on hand.
// want=0 is meaningful ("none of this type"); RemoveQuota drops the line.
//
// The mix is INTENT and belongs to the loader. It is a preference, not a cap:
// never-2N still bounds how many carriers exist, and this only decides which
// type is fetched next inside that bound.
func (s *LoaderService) SetQuota(loaderID, binTypeID int64, want int) error {
	if err := s.db.UpsertLoaderQuota(loaders.Quota{
		LoaderID: loaderID, BinTypeID: binTypeID, Want: want,
	}); err != nil {
		return err
	}
	s.rederive()
	return nil
}

// RemoveQuota drops one line of a loader's declared mix. Dropping every line
// returns the loader to first-come-first-served, which is where it started.
func (s *LoaderService) RemoveQuota(loaderID, binTypeID int64) error {
	if err := s.db.RemoveLoaderQuota(loaderID, binTypeID); err != nil {
		return err
	}
	s.rederive()
	return nil
}

// SetWindowBinTypes replaces what ONE window can physically take. An empty list
// means it takes anything, which is what every window does until somebody says
// otherwise.
//
// Physical, so it belongs to the window rather than the loader — a slot either
// fits a carrier or it does not. When the floor is rebuilt somebody edits this;
// Shingo does not model why.
func (s *LoaderService) SetWindowBinTypes(loaderID, positionNodeID int64, binTypeIDs []int64) error {
	if err := s.db.SetLoaderHomeBinTypes(loaderID, positionNodeID, binTypeIDs); err != nil {
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
func (s *LoaderService) SetHome(loaderID, positionNodeID int64, payloadCode, homeKind string, uopThreshold int) error {
	// homeKind is the home/buffer discriminator; "" normalises to home in the store.
	// A BUFFER slot pins no payload (it holds whatever partial parks there), so a
	// payload sent alongside a buffer kind is dropped rather than trusted.
	switch homeKind {
	case "", loaders.HomeKindHome:
	case loaders.HomeKindBuffer:
		payloadCode = ""
	default:
		return fmt.Errorf("invalid home_kind %q (want %q or %q)", homeKind, loaders.HomeKindHome, loaders.HomeKindBuffer)
	}
	// A loader window/position must be a real physical slot — a node a bin can
	// actually sit in. Reject synthetic container nodes (a node group or a
	// lane) and missing nodes. Without this guard, assigning an empty
	// lane/group as a window yields a loader that dispatches into a location
	// with no slots ("synthetic node has no children") — the failure behind
	// the Springfield "lane 14" loader-window incident.
	node, err := s.db.GetNode(positionNodeID)
	if err != nil {
		return fmt.Errorf("loader window node %d not found: %w", positionNodeID, err)
	}
	if node.IsSynthetic {
		return fmt.Errorf("loader window must be a physical slot, not a %s container (%s)", node.NodeTypeCode, node.Name)
	}
	existing, err := s.db.ListLoaderHomes(loaderID)
	if err != nil {
		return err
	}
	if err := s.db.UpsertLoaderHome(loaders.Home{
		LoaderID: loaderID, PositionNodeID: positionNodeID, PayloadCode: payloadCode,
		Kind: homeKind, UOPThreshold: uopThreshold, SortOrder: len(existing),
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
// could route this aggregate's demand, and nudges the threshold monitor. The
// target set is the UNION of stations already present in the registry AND every
// registered edge — keying only on existing registry rows can't bootstrap a
// station with zero rows yet, which is exactly what left a UI-authored loader
// with no demand routing until an edge reconnect/seed. Per-station scoping by
// node_stations is a documented refinement.
func (s *LoaderService) rederive() {
	stationSet := map[string]struct{}{}
	if stations, err := s.db.DemandRegistryStations(); err == nil {
		for _, st := range stations {
			stationSet[st] = struct{}{}
		}
	}
	if edges, err := s.db.ListEdges(); err == nil {
		for _, e := range edges {
			stationSet[e.StationID] = struct{}{}
		}
	}
	for st := range stationSet {
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

// ErrInboundSourceUnresolved is returned when a claim's inbound source names a
// node that does not exist.
var ErrInboundSourceUnresolved = errors.New("inbound source does not resolve to a node")

// checkInboundSource resolve-checks a claim's inbound source at SAVE TIME.
// MG3-3.
//
// ── IT WAS AN UNCHECKED STRING ──────────────────────────────────────────────
//
// inbound_source is typed by an operator, stored verbatim, and read months later
// by the source finder — which resolves it, gets nothing, and falls through to
// whatever its tier gates allow. A typo therefore produced no error at save, no
// error at read, and a silent change of sourcing behaviour that nobody could
// connect to the edit that caused it.
//
// It matters more now than it did. A maintained group is named by exactly this
// field, and phase 3 makes naming it consequential: the group either serves this
// claim or fences it, and both of those are decisions about a node. A field that
// might name nothing at all cannot carry either.
//
// SAVE TIME IS THE ONLY PLACE THIS IS CHEAP. The operator is standing there and
// knows what they meant; at read time the finder has an order to place and no
// way to ask.
//
// BLANK IS VALID and means "no scoped source" — the overwhelmingly common case,
// and the reason this is a resolve-check rather than a required field.
//
// IT CHECKS EXISTENCE, NOT SUITABILITY. Whether a node is a sensible source for
// this claim is a judgement that depends on plant layout and on config that may
// be edited in any order; refusing a save on it would make the form unusable
// during a reconfiguration. Existence is the part that is always knowable and
// always wrong when it fails.
func (s *LoaderService) checkInboundSource(inboundSource string) error {
	if inboundSource == "" {
		return nil
	}
	node, err := s.db.GetNodeByDotName(inboundSource)
	if err != nil || node == nil {
		return fmt.Errorf("%w: %q", ErrInboundSourceUnresolved, inboundSource)
	}
	return nil
}
