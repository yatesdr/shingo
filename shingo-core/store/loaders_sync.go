package store

// Loader → protocol projection for the downward config sync (loader refactor
// cutover). Lives in the outer store/ package because it crosses two
// aggregates: loaders (config) and nodes (position_node_id → name resolution,
// since Edge keys on node names). Live: Core authors loaders and the node-list
// builder carries BuildLoaderInfos' output down to the Edge.

import (
	"fmt"
	"log"

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
			ConfigGen:     l.ConfigGen,
			FunnelWindows: l.FunnelWindows,
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
		// What each window can physically take. Absent = takes anything.
		capabilities, err := db.ListLoaderHomeBinTypes(l.ID)
		if err != nil {
			return nil, err
		}
		// Every member's name in ONE query, rather than a lookup per home.
		//
		// This sync runs on a timer — the node list, and this slice with it, is
		// re-sent every other heartbeat tick (~2 min) whether or not anything
		// changed. A per-home read made that a lookup for each position at every
		// plant, forever, to rebuild a projection that is almost always identical
		// to the one before it.
		//
		// The absent-node behaviour is preserved exactly: MemberNodeNames INNER
		// JOINs nodes, so a home pointing at a deleted node is simply not in the
		// map, and the !ok skip below is the old `node == nil` skip.
		//
		// A read FAILURE now fails the whole projection instead of dropping one
		// position, and that is the safer end: the caller already logs and sends
		// the node list without loaders, leaving Edge on its last-known-good
		// cache. A loader shipped one position short would be cached as truth and
		// spread empties across the wrong window count.
		names, err := db.LoaderMemberNodeNames(l.ID)
		if err != nil {
			return nil, err
		}
		for _, h := range homes {
			name, ok := names[h.PositionNodeID]
			if !ok {
				continue // position node vanished — skip rather than fail the sync
			}
			// home_kind, carried rather than left to be guessed at. Core reads
			// it in InSourcePool to keep an unassigned home out of the source
			// pool; the Edge had no way to ask, so it classified by empty
			// payload and disagreed on exactly that case.
			homeKind := h.Kind
			if homeKind == "" {
				homeKind = protocol.LoaderHomeKindHome // the column's own default
			}
			info.Positions = append(info.Positions, protocol.LoaderPosition{
				CoreNodeName: name,
				PayloadCode:  h.PayloadCode,
				Kind:         positionKind,
				HomeKind:     homeKind,
				UOPThreshold: h.UOPThreshold,
				// The operator's arrangement, carried down. It was persisted
				// here and read by the admin screen, and stopped at this
				// function — everything below re-sorted by name.
				Ordinal:  h.SortOrder,
				BinTypes: capabilities[h.PositionNodeID],
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

		quotas, err := db.ListLoaderQuotas(l.ID)
		if err != nil {
			return nil, err
		}
		for _, q := range quotas {
			info.Quota = append(info.Quota, protocol.LoaderQuota{
				BinTypeCode: q.BinTypeCode,
				Want:        q.Want,
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

		// A consume loader drains; nothing on either side acts on a threshold it
		// carries. The service refuses the combination at the door now, so this is
		// for the row that predates that refusal or arrived by a direct database
		// edit: derive the entry, but with NO threshold, so the monitor does not
		// fire signals the Edge is guaranteed to drop.
		//
		// Zeroed rather than skipped. The registry entry does more than carry a
		// threshold — dropping the loader from it entirely would take away the
		// manual_swap binding too, trading an inert threshold for a broken swap.
		consumeThreshold := l.Role == loaders.RoleConsume && l.Replenishment == loaders.ReplenishmentThreshold
		if consumeThreshold {
			log.Printf("demand registry: loader %q (%s) is consume+threshold, which nothing acts on — deriving its entries with no threshold; fix the loader's replenishment mode", l.Name, loaders.Key(l.ID))
		}
		thresholdFor := func(v int) int {
			if consumeThreshold {
				return 0
			}
			return v
		}

		homes, err := db.ListLoaderHomes(l.ID)
		if err != nil {
			return nil, err
		}
		// One query for every member name, same as BuildLoaderInfos above. Both
		// loops below resolved a name per home; the second did it only to find
		// the first resolvable one.
		names, err := db.LoaderMemberNodeNames(l.ID)
		if err != nil {
			return nil, err
		}
		for _, h := range homes {
			// A BUFFER slot holds kept partials and pins no payload — it drives no
			// threshold demand of its own (it is fed by parked returns, not the
			// monitor). Skip it by KIND, not by blank payload, so it is no longer
			// conflated with an unconfigured position.
			if h.Kind == loaders.HomeKindBuffer {
				continue
			}
			// A HOME with no payload yet also drives no demand, but for a different
			// reason: a shared_window loader's homes are physical WINDOWS (the payload
			// set in bin_loader_payloads governs), and a just-dropped dedicated
			// position is unassigned until the operator picks a payload. Emitting an
			// empty-payload registry entry would be junk.
			if h.PayloadCode == "" {
				continue
			}
			name, ok := names[h.PositionNodeID]
			if !ok {
				continue
			}
			out = append(out, demands.RegistryEntry{
				StationID:             stationID,
				CoreNodeName:          name,
				LoaderID:              l.ID,
				Role:                  role,
				PayloadCode:           h.PayloadCode,
				OutboundDest:          l.OutboundDest,
				ReplenishUOPThreshold: thresholdFor(h.UOPThreshold),
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
				if n, ok := names[h.PositionNodeID]; ok {
					addr = n
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
					ReplenishUOPThreshold: thresholdFor(p.UOPThreshold),
				})
			}
		}
	}
	return out, nil
}
