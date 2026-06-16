package engine

import (
	"errors"
	"fmt"
	"sync/atomic"

	"shingoedge/domain"
	"shingoedge/store"
)

// loader_store.go — the LoaderStore: the consumer-defined interface the demand /
// reservation / board paths resolve loaders through, backed by the Core-owned
// loader aggregate (bin_loaders) projected into validated *domain.Loader values.
//
// Every lookup returns (*domain.Loader, error) with a SENTINEL discipline:
//   - (loader, nil)            — resolved.
//   - (nil, ErrLoaderNotFound) — a clean miss; the caller may take its fallback.
//   - (nil, other error)       — a real failure (malformed config); the caller
//                                FAILS CLOSED and must NOT fall open, or a transient
//                                flicker reroutes demand to the wrong loader.
//
// The store reads an immutable in-memory snapshot of the projected loaders,
// swapped atomically on each node-list sync (SetCoreLoaders → Refresh). Resolution
// therefore never touches the DB — a torn multi-statement read of the cache is
// impossible, and a DB flicker during a sync only keeps the last good snapshot
// rather than producing a wrong resolution.

// ErrLoaderNotFound is the clean-miss sentinel. Distinguish it from a real error
// with errors.Is so a fallback never fires on a DB flicker.
var ErrLoaderNotFound = errors.New("loader not found")

// LoaderStore resolves loaders for the demand / reservation / board paths.
// Defined at the consumer (engine) — idiomatic Go, and it keeps the engine from
// importing store internals.
type LoaderStore interface {
	// LoaderForPayload resolves a loader of the given role whose payload set
	// contains payload. activeOnly is vestigial — the Core-owned aggregate is
	// always current, so it is ignored; kept on the signature for callers that
	// still pass it.
	LoaderForPayload(payload domain.PayloadCode, role domain.LoaderRole, activeOnly bool) (*domain.Loader, error)
	// LoaderAt resolves the loader of the given role that contains coreNode
	// (its anchor or a member window/position).
	LoaderAt(coreNode domain.NodeID, role domain.LoaderRole) (*domain.Loader, error)
	// LoaderByKey resolves the loader of the given role whose identity token is key
	// (step-4 cutover: the threshold signal carries LoaderKey, so a synthetic loader
	// with no anchor node resolves by its token instead of core_node_name).
	LoaderByKey(key domain.LoaderID, role domain.LoaderRole) (*domain.Loader, error)
	// Loaders returns every loader of the given role.
	Loaders(role domain.LoaderRole) ([]*domain.Loader, error)
	// LoaderForNode resolves the loader (of ANY role) that contains coreNode as a
	// window/position. A node belongs to at most one loader (UNIQUE position node),
	// so the result is unambiguous and role-agnostic. This is what lets a Core-owned
	// loader's node be treated as a manual_swap loader node without a per-style claim.
	LoaderForNode(coreNode domain.NodeID) (*domain.Loader, error)
}

// ── Aggregate projection ────────────────────────────────────────────

// projectCoreLoader maps one cached Core loader to a validated *domain.Loader via
// the C0 constructors, so invalid config is rejected here rather than mis-served.
// A shared_window loader projects its window members as windows; one with no window
// members yet (admin-created, awaiting drag-in) fails closed in the constructor — it
// has no node to deliver to, so it must not resolve.
func projectCoreLoader(l store.CoreLoader) (*domain.Loader, error) {
	// Identity is the loader_key token — a loader has no node of its own, so this is
	// the one place identity lives and the "a loader is never a node" rule holds. Core
	// always mints it (BuildLoaderInfos), so an empty key is malformed config that
	// fails closed rather than silently borrowing a node name.
	if l.LoaderKey == "" {
		return nil, fmt.Errorf("loader %s/%s: empty loader_key", l.Name, l.Role)
	}
	id := domain.LoaderID(l.LoaderKey)
	role := domain.LoaderRole(l.Role)
	repl := domain.LoaderReplenishment(l.Replenishment)

	switch l.Layout {
	case string(domain.LayoutSharedWindow):
		windows := make([]domain.Window, 0, len(l.Positions))
		for _, p := range l.Positions {
			windows = append(windows, domain.Window{Node: domain.NodeID(p.PositionNode)})
		}
		payloadSet := make([]domain.PayloadCode, 0, len(l.Payloads))
		uopThreshold := make(map[domain.PayloadCode]int, len(l.Payloads))
		for _, p := range l.Payloads {
			payloadSet = append(payloadSet, domain.PayloadCode(p.PayloadCode))
			uopThreshold[domain.PayloadCode(p.PayloadCode)] = p.UOPThreshold
		}
		return domain.NewSharedWindowLoader(id, l.Name, role, repl, windows, payloadSet,
			domain.WithInboundSource(l.InboundSource),
			domain.WithUOPThreshold(uopThreshold),
			domain.WithOutboundDest(l.OutboundDest), domain.WithBufferDest(l.BufferDest))

	case string(domain.LayoutDedicatedPositions):
		positions := make([]domain.Position, 0, len(l.Positions))
		for _, p := range l.Positions {
			positions = append(positions, domain.Position{
				Node:         domain.NodeID(p.PositionNode),
				Payload:      domain.PayloadCode(p.PayloadCode),
				UOPThreshold: p.UOPThreshold,
			})
		}
		return domain.NewDedicatedPositionsLoader(id, l.Name, role, repl, positions,
			domain.WithInboundSource(l.InboundSource),
			domain.WithOutboundDest(l.OutboundDest), domain.WithBufferDest(l.BufferDest))

	default:
		return nil, fmt.Errorf("loader %s/%s: unknown layout %q", l.LoaderKey, l.Role, l.Layout)
	}
}

// ── Aggregate impl (Core-owned cache + immutable snapshot) ───────────

type aggregateLoaderStore struct {
	db    *store.DB
	logFn LogFunc
	snap  atomic.Pointer[[]*domain.Loader] // immutable; swapped whole on Refresh
}

func newAggregateLoaderStore(db *store.DB, logFn LogFunc) *aggregateLoaderStore {
	s := &aggregateLoaderStore{db: db, logFn: logFn}
	if err := s.Refresh(); err != nil && logFn != nil {
		logFn("loader store: initial refresh failed (reads tolerate an empty snapshot): %v", err)
	}
	return s
}

// Refresh rebuilds the projected-loader snapshot from the cache and swaps it in
// atomically. On a DB read error the previous snapshot is kept (last-known-good);
// individual loaders that fail to project are skipped with a log, never aborting
// the whole refresh. Safe to call concurrently with readers.
func (s *aggregateLoaderStore) Refresh() error {
	cls, err := s.db.ListCoreLoaders()
	if err != nil {
		return fmt.Errorf("loader store refresh: %w", err)
	}
	out := make([]*domain.Loader, 0, len(cls))
	for _, cl := range cls {
		l, perr := projectCoreLoader(cl)
		if perr != nil {
			if s.logFn != nil {
				s.logFn("loader store: skip malformed loader %s/%s: %v", cl.LoaderKey, cl.Role, perr)
			}
			continue
		}
		out = append(out, l)
	}
	s.snap.Store(&out)
	return nil
}

func (s *aggregateLoaderStore) snapshot() []*domain.Loader {
	if p := s.snap.Load(); p != nil {
		return *p
	}
	return nil
}

func (s *aggregateLoaderStore) LoaderForPayload(payload domain.PayloadCode, role domain.LoaderRole, _ bool) (*domain.Loader, error) {
	for _, l := range s.snapshot() {
		if l.Role() == role && l.ServesPayload(payload) {
			return l, nil
		}
	}
	return nil, ErrLoaderNotFound
}

func (s *aggregateLoaderStore) LoaderAt(coreNode domain.NodeID, role domain.LoaderRole) (*domain.Loader, error) {
	for _, l := range s.snapshot() {
		if l.Role() == role && l.Contains(coreNode) {
			return l, nil
		}
	}
	return nil, ErrLoaderNotFound
}

func (s *aggregateLoaderStore) LoaderByKey(key domain.LoaderID, role domain.LoaderRole) (*domain.Loader, error) {
	for _, l := range s.snapshot() {
		if l.Role() == role && l.ID() == key {
			return l, nil
		}
	}
	return nil, ErrLoaderNotFound
}

func (s *aggregateLoaderStore) Loaders(role domain.LoaderRole) ([]*domain.Loader, error) {
	var out []*domain.Loader
	for _, l := range s.snapshot() {
		if l.Role() == role {
			out = append(out, l)
		}
	}
	return out, nil
}

func (s *aggregateLoaderStore) LoaderForNode(coreNode domain.NodeID) (*domain.Loader, error) {
	for _, l := range s.snapshot() {
		if l.Contains(coreNode) {
			return l, nil
		}
	}
	return nil, ErrLoaderNotFound
}

// loaders returns the engine's LoaderStore, lazily constructing one if absent.
// engine.New and the test harness set e.loaderStore up front, so the lazy path is
// only taken by tests that build an Engine struct directly — single-threaded
// setup, so the unguarded init never races a concurrent reader (which always
// finds a non-nil store).
func (e *Engine) loaders() LoaderStore {
	if e.loaderStore == nil {
		e.loaderStore = newLoaderStore(e)
	}
	return e.loaderStore
}

// newLoaderStore builds the Core-owned aggregate loader store. Its snapshot is
// refreshed by SetCoreLoaders on each node-list sync.
func newLoaderStore(e *Engine) LoaderStore {
	return newAggregateLoaderStore(e.db, e.logFn)
}

// stationLoaderResolver adapts the engine's LoaderStore to the
// service.LoaderResolver consumer interface used by the operator view. It resolves
// at call time through loaders() so the lazy init is honoured, and maps the
// ErrLoaderNotFound sentinel to a clean (nil, nil) miss so BuildView keeps its
// claim-derived fields for nodes that aren't aggregate loaders. Satisfies
// service.LoaderResolver structurally — no service import, no cycle.
type stationLoaderResolver struct{ e *Engine }

func (r stationLoaderResolver) LoaderAt(coreNode domain.NodeID, role domain.LoaderRole) (*domain.Loader, error) {
	l, err := r.e.loaders().LoaderAt(coreNode, role)
	if errors.Is(err, ErrLoaderNotFound) {
		return nil, nil
	}
	return l, err
}

func (r stationLoaderResolver) LoaderForNode(coreNode domain.NodeID) (*domain.Loader, error) {
	l, err := r.e.loaders().LoaderForNode(coreNode)
	if errors.Is(err, ErrLoaderNotFound) {
		return nil, nil
	}
	return l, err
}
