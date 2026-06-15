package store

// Loader → protocol projection for the downward config sync (loader refactor
// cutover). Lives in the outer store/ package because it crosses two
// aggregates: loaders (config) and nodes (position_node_id → name resolution,
// since Edge keys on node names). DORMANT until Core authors loaders + the
// node-list builder includes the result.

import (
	"fmt"

	"shingo/protocol"
	"shingocore/store/demands"
	"shingocore/store/loaders"
)

// WriteDerivedLoaders persists a migration's derived loader aggregate into Core:
// one bin_loaders row per loader with its payloads (shared_window) or homes
// (dedicated_positions — position node NAMES resolved to Core node ids via
// GetNodeByName). Idempotent — skips a (core_node, role) that already exists.
// Returns (created, skippedHomes): a home whose position node is absent from
// Core's topology is skipped (it can't FK to a missing node) rather than
// failing the whole migration. Run loaders.CheckHomeTripwire / GroupIntoLoaders
// (which enforces it) before calling.
func (db *DB) WriteDerivedLoaders(derived []loaders.DerivedLoader) (created, skippedHomes int, err error) {
	for _, d := range derived {
		existing, gerr := db.GetLoaderByName(d.Loader.Name, d.Loader.Role)
		if gerr != nil {
			return created, skippedHomes, fmt.Errorf("check loader %s/%s: %w", d.Loader.Name, d.Loader.Role, gerr)
		}
		if existing != nil {
			continue
		}
		id, cerr := db.CreateLoader(d.Loader)
		if cerr != nil {
			return created, skippedHomes, fmt.Errorf("create loader %s: %w", d.Loader.Name, cerr)
		}
		for _, p := range d.Payloads {
			p.LoaderID = id
			if perr := db.UpsertLoaderPayload(p); perr != nil {
				return created, skippedHomes, fmt.Errorf("write payload %s: %w", p.PayloadCode, perr)
			}
		}
		for _, h := range d.Homes {
			node, nerr := db.GetNodeByName(h.PositionNode)
			if nerr != nil || node == nil {
				skippedHomes++
				continue
			}
			if herr := db.UpsertLoaderHome(loaders.Home{
				LoaderID: id, PositionNodeID: node.ID, PayloadCode: h.PayloadCode,
				UOPThreshold: h.UOPThreshold,
			}); herr != nil {
				return created, skippedHomes, fmt.Errorf("write home %s: %w", h.PositionNode, herr)
			}
		}
		created++
	}
	return created, skippedHomes, nil
}

// DemandRegistryStations returns the distinct station_ids present in
// demand_registry — the stations the loader-config re-derive must refresh after
// an aggregate edit.
func (db *DB) DemandRegistryStations() ([]string, error) {
	rows, err := db.DB.Query(`SELECT DISTINCT station_id FROM demand_registry ORDER BY station_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// BuildLoaderInfos assembles every Core-owned loader into the protocol shape
// carried on NodeListResponse.Loaders. Each dedicated position's node id is
// resolved to its name; a position whose node has vanished is skipped (the sync
// stays best-effort rather than failing the whole node list).
func (db *DB) BuildLoaderInfos() ([]protocol.LoaderInfo, error) {
	ls, err := db.ListLoaders()
	if err != nil {
		return nil, err
	}
	out := make([]protocol.LoaderInfo, 0, len(ls))
	for _, l := range ls {
		info := protocol.LoaderInfo{
			Name:          l.Name,
			LoaderKey:     loaders.Key(l.ID),
			Role:          l.Role,
			Layout:        l.Layout,
			Replenishment: l.Replenishment,
			OutboundDest:  l.OutboundDest,
			InboundSource: l.InboundSource,
			BufferDest:    l.BufferDest,
			ConfigGen:     l.ConfigGen,
		}

		// A home's kind is fully determined by the parent loader's layout: a
		// shared_window loader's homes are physical WINDOWS (no per-position
		// payload — the shared set rides info.Payloads); a dedicated loader's
		// homes are dedicated positions. Deriving it here, at the single
		// projection point, keeps Layout the one source of truth and stamps the
		// wire so the Edge never sniffs empty payload to guess.
		positionKind := protocol.LoaderPositionKindDedicated
		if l.Layout == loaders.LayoutSharedWindow {
			positionKind = protocol.LoaderPositionKindWindow
		}

		homes, err := db.ListLoaderHomes(l.ID)
		if err != nil {
			return nil, err
		}
		for _, h := range homes {
			node, err := db.GetNode(h.PositionNodeID)
			if err != nil || node == nil {
				continue // position node vanished — skip rather than fail the sync
			}
			info.Positions = append(info.Positions, protocol.LoaderPosition{
				CoreNodeName: node.Name,
				PayloadCode:  h.PayloadCode,
				Kind:         positionKind,
				UOPThreshold: h.UOPThreshold,
			})
		}

		payloads, err := db.ListLoaderPayloads(l.ID)
		if err != nil {
			return nil, err
		}
		for _, p := range payloads {
			info.Payloads = append(info.Payloads, protocol.LoaderPayloadInfo{
				PayloadCode:  p.PayloadCode,
				UOPThreshold: p.UOPThreshold,
			})
		}

		out = append(out, info)
	}
	return out, nil
}

// BuildDemandRegistryFromAggregate derives the manual_swap demand_registry
// entries for stationID from the Core-owned bin_loaders aggregate — the
// Core-authored replacement for the Edge ClaimSync that used to populate the
// registry (loader refactor cutover, threshold-to-Core). The threshold_monitor
// consumes the ReplenishUOPThreshold values. CoreNodeName is the position node
// (dedicated_positions) or the loader's first window node (shared_window) — a real
// node, since the loader has no node identity of its own; the Edge resolves the loader
// by LoaderKey on the LoopBelowThresholdSignal. Callers pass
// the result to SyncDemandRegistry (and, at runtime, the monitor's
// OnThresholdChanges) so Core becomes the single writer of the registry.
func (db *DB) BuildDemandRegistryFromAggregate(stationID string) ([]demands.RegistryEntry, error) {
	ls, err := db.ListLoaders()
	if err != nil {
		return nil, err
	}
	var out []demands.RegistryEntry
	for _, l := range ls {
		role := protocol.ClaimRole(l.Role)

		homes, err := db.ListLoaderHomes(l.ID)
		if err != nil {
			return nil, err
		}
		for _, h := range homes {
			// Skip homes with no payload yet: a shared_window loader's homes are
			// physical WINDOWS (the payload set in bin_loader_payloads governs), and
			// a just-dropped dedicated position is unassigned until the operator
			// picks a payload. Either way it drives no demand — emitting an
			// empty-payload registry entry would be junk.
			if h.PayloadCode == "" {
				continue
			}
			node, err := db.GetNode(h.PositionNodeID)
			if err != nil || node == nil {
				continue
			}
			out = append(out, demands.RegistryEntry{
				StationID:             stationID,
				CoreNodeName:          node.Name,
				LoaderID:              l.ID,
				Role:                  role,
				PayloadCode:           h.PayloadCode,
				OutboundDest:          l.OutboundDest,
				ReplenishUOPThreshold: h.UOPThreshold,
			})
		}

		payloads, err := db.ListLoaderPayloads(l.ID)
		if err != nil {
			return nil, err
		}
		if len(payloads) > 0 {
			// A shared_window loader has no node of its own (core_node_name is gone), so
			// address its pooled demand at the first window node — a real node. The binding
			// key (station, node, payload) and the signal's address both use it; the Edge
			// resolves the loader by LoaderKey and spreads the empty across every window,
			// so any stable member node serves. A window-less shared loader (admin-created,
			// not yet configured) is not operable and drives no demand.
			addr := ""
			for _, h := range homes {
				if n, nerr := db.GetNode(h.PositionNodeID); nerr == nil && n != nil {
					addr = n.Name
					break
				}
			}
			if addr == "" {
				continue
			}
			for _, p := range payloads {
				out = append(out, demands.RegistryEntry{
					StationID:             stationID,
					CoreNodeName:          addr,
					LoaderID:              l.ID,
					Role:                  role,
					PayloadCode:           p.PayloadCode,
					OutboundDest:          l.OutboundDest,
					ReplenishUOPThreshold: p.UOPThreshold,
				})
			}
		}
	}
	return out, nil
}
