package main

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/plantspec"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/demands"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// seedCore writes the plant's core (Postgres) topology through store accessors,
// in dependency order: node types → payloads + bin types → storage hierarchy
// (zone → lane → slot) + stations → bins (+ manifests) → node↔payload links →
// demand registry. Every create is existence-checked first, so it's idempotent.
//
// binIDByNode (caller-allocated) is filled with node-name → core bin id for
// every seeded bin, so seedEdge can bind a node's runtime active_bin_id to the
// bin physically at it (the loops only tick when a bin is bound).
func seedCore(db *store.DB, p *plantspec.Plant, binIDByNode map[string]int64) error {
	// --- node types (NGRP/LANE ship from migrations; ensure STOR + STATION) ---
	ngrpType, err := ensureNodeType(db, "NGRP", "Node Group", true)
	if err != nil {
		return err
	}
	laneType, err := ensureNodeType(db, "LANE", "Lane", true)
	if err != nil {
		return err
	}
	storType, err := ensureNodeType(db, "STOR", "Storage Slot", false)
	if err != nil {
		return err
	}
	stationType, err := ensureNodeType(db, "STATION", "Station", false)
	if err != nil {
		return err
	}

	// --- bin types + payloads (+ payload→bin-type links) ---
	binTypeIDs := make(map[string]int64)
	for _, bt := range p.BinTypes {
		id, err := ensureBinType(db, bt)
		if err != nil {
			return err
		}
		binTypeIDs[bt] = id
	}
	payloadIDs := make(map[string]int64)
	for _, pl := range p.Payloads {
		id, err := ensurePayload(db, pl)
		if err != nil {
			return err
		}
		payloadIDs[pl.Code] = id
		if btID, ok := binTypeIDs[pl.BinType]; ok {
			if err := db.SetPayloadBinTypes(id, []int64{btID}); err != nil {
				return fmt.Errorf("link payload %s → bin type %s: %w", pl.Code, pl.BinType, err)
			}
		}
	}

	// --- storage hierarchy: zone (NGRP) → lane (LANE) → slot (STOR, with depth) ---
	nodeIDs := make(map[string]int64) // node name → id (for parent links + bin placement)
	for _, z := range p.Zones {
		zID, err := ensureNode(db, z.Name, ptr(ngrpType), nil, z.Name, nil, true)
		if err != nil {
			return err
		}
		nodeIDs[z.Name] = zID
		for _, ln := range z.Lanes {
			lnID, err := ensureNode(db, ln.Name, ptr(laneType), ptr(zID), z.Name, nil, true)
			if err != nil {
				return err
			}
			nodeIDs[ln.Name] = lnID
			for _, s := range ln.Slots {
				depth := s.Depth
				sID, err := ensureNode(db, s.Name, ptr(storType), ptr(lnID), z.Name, &depth, false)
				if err != nil {
					return err
				}
				nodeIDs[s.Name] = sID
			}
		}
	}
	// --- stations (line/press/weld/loader/unloader/staging/dest) ---
	for _, st := range p.Stations {
		id, err := ensureNode(db, st.Name, ptr(stationType), nil, st.Zone, nil, false)
		if err != nil {
			return err
		}
		nodeIDs[st.Name] = id
	}

	// --- bins (+ manifest for loaded ones) ---
	for _, b := range p.Bins {
		nodeID, ok := nodeIDs[b.Slot]
		if !ok {
			return fmt.Errorf("bin %s: slot %q not found among seeded nodes", b.Name, b.Slot)
		}
		btCode := b.BinType
		if btCode == "" && len(p.BinTypes) > 0 {
			btCode = p.BinTypes[0]
		}
		btID, ok := binTypeIDs[btCode]
		if !ok {
			return fmt.Errorf("bin %s: bin type %q not seeded", b.Name, btCode)
		}
		binID, created, err := ensureBin(db, b.Name, btID, nodeID)
		if err != nil {
			return err
		}
		binIDByNode[b.Slot] = binID
		// Bins must be 'available' for the retrieve resolver to
		// find them. Re-assert on every seed run (idempotent re-seed
		// may find existing bins with stale status from a prior run).
		if err := db.UpdateBinStatus(binID, domain.BinStatusAvailable); err != nil {
			return fmt.Errorf("bin %s set available: %w", b.Name, err)
		}
		if created && b.Payload != "" {
			manifest := buildManifest(b.Payload, b.UOP)
			if err := db.SetBinManifest(binID, manifest, b.Payload, int(b.UOP)); err != nil {
				return fmt.Errorf("bin %s set manifest: %w", b.Name, err)
			}
			// ConfirmBinManifest's 2nd arg is producedAt — it becomes the bin's
			// loaded_at, which FIFO retrieve orders by. Pass RFC3339 now so it
			// doesn't fall back to server time with a warning; AgeS backdates it so
			// a buried slot can be made the globally-oldest bin (reshuffle test).
			producedAt := time.Now().UTC()
			if b.AgeS > 0 {
				producedAt = producedAt.Add(-time.Duration(b.AgeS) * time.Second)
			}
			if err := db.ConfirmBinManifest(binID, producedAt.Format(time.RFC3339)); err != nil {
				return fmt.Errorf("bin %s confirm manifest: %w", b.Name, err)
			}
		}
	}

	// --- node↔payload links: every claim's node accepts its payload ---
	for _, c := range p.Claims {
		nodeID, ok := nodeIDs[c.CoreNode]
		if !ok {
			continue
		}
		if pid, ok := payloadIDs[c.Payload]; ok {
			if err := db.AssignPayloadToNode(nodeID, pid); err != nil {
				return fmt.Errorf("assign payload %s → node %s: %w", c.Payload, c.CoreNode, err)
			}
		}
		for _, ap := range c.AllowedPayloads {
			if pid, ok := payloadIDs[ap]; ok {
				if err := db.AssignPayloadToNode(nodeID, pid); err != nil {
					return fmt.Errorf("assign allowed payload %s → node %s: %w", ap, c.CoreNode, err)
				}
			}
		}
	}

	// --- demand registry (role + threshold from spec, fallback to claim inference) ---
	claimByNodePayload := make(map[string]plantspec.Claim)
	for _, c := range p.Claims {
		claimByNodePayload[c.CoreNode+"|"+c.Payload] = c
	}
	stationID := p.Namespace + "." + p.LineID
	entries := make([]demands.RegistryEntry, 0, len(p.Demands))
	for _, d := range p.Demands {
		role := protocol.ClaimRoleConsume
		var threshold int
		var outboundDest string
		if c, ok := claimByNodePayload[d.Node+"|"+d.Payload]; ok {
			role = protocol.ClaimRole(c.Role)
			outboundDest = c.OutboundDestination
			// Use the explicit threshold from the spec if provided (G5),
			// otherwise fall back to inferring from the claim's reorder_point.
			if d.ReplenishUOPThreshold != nil {
				threshold = *d.ReplenishUOPThreshold
			} else {
				threshold = int(c.ReorderPoint)
			}
		}
		entries = append(entries, demands.RegistryEntry{
			StationID:             stationID,
			CoreNodeName:          d.Node,
			Role:                  role,
			PayloadCode:           d.Payload,
			OutboundDest:          outboundDest,
			ReplenishUOPThreshold: threshold,
		})
	}
	if len(entries) > 0 {
		if _, err := db.SyncDemandRegistry(stationID, entries); err != nil {
			return fmt.Errorf("sync demand registry: %w", err)
		}
	}

	log.Printf("core: %d node types, %d payloads, %d nodes, %d bins, %d demands",
		4, len(payloadIDs), len(nodeIDs), len(p.Bins), len(entries))
	return nil
}

func ensureNodeType(db *store.DB, code, name string, synthetic bool) (int64, error) {
	if t, err := db.GetNodeTypeByCode(code); err == nil && t != nil {
		return t.ID, nil
	}
	nt := &nodes.NodeType{Code: code, Name: name, IsSynthetic: synthetic}
	if err := db.CreateNodeType(nt); err != nil {
		return 0, fmt.Errorf("create node type %s: %w", code, err)
	}
	return nt.ID, nil
}

func ensureNode(db *store.DB, name string, typeID, parentID *int64, zone string, depth *int, synthetic bool) (int64, error) {
	if n, err := db.GetNodeByName(name); err == nil && n != nil {
		return n.ID, nil
	}
	n := &nodes.Node{
		Name:        name,
		NodeTypeID:  typeID,
		ParentID:    parentID,
		Zone:        zone,
		Depth:       depth,
		IsSynthetic: synthetic,
		Enabled:     true,
	}
	if err := db.CreateNode(n); err != nil {
		return 0, fmt.Errorf("create node %s: %w", name, err)
	}
	return n.ID, nil
}

func ensureBinType(db *store.DB, code string) (int64, error) {
	if bt, err := db.GetBinTypeByCode(code); err == nil && bt != nil {
		return bt.ID, nil
	}
	bt := &bins.BinType{Code: code, Description: code + " (dev)"}
	if err := db.CreateBinType(bt); err != nil {
		return 0, fmt.Errorf("create bin type %s: %w", code, err)
	}
	return bt.ID, nil
}

func ensurePayload(db *store.DB, pl plantspec.Payload) (int64, error) {
	if p, err := db.GetPayloadByCode(pl.Code); err == nil && p != nil {
		return p.ID, nil
	}
	p := &payloads.Payload{Code: pl.Code, UOPCapacity: int(pl.UOPCapacity), Description: pl.Code + " (dev)"}
	if err := db.CreatePayload(p); err != nil {
		return 0, fmt.Errorf("create payload %s: %w", pl.Code, err)
	}
	return p.ID, nil
}

// ensureBin returns (id, createdNow, err). createdNow=false means the bin
// already existed (re-run) and its manifest is left untouched.
func ensureBin(db *store.DB, label string, binTypeID, nodeID int64) (int64, bool, error) {
	if b, err := db.GetBinByLabel(label); err == nil && b != nil {
		return b.ID, false, nil
	}
	nid := nodeID
	b := &bins.Bin{BinTypeID: binTypeID, Label: label, NodeID: &nid, Status: "available"}
	if err := db.CreateBin(b); err != nil {
		return 0, false, fmt.Errorf("create bin %s: %w", label, err)
	}
	return b.ID, true, nil
}

// buildManifest renders a one-line manifest JSON for a loaded bin.
func buildManifest(payloadCode string, uop int64) string {
	type item struct {
		PartNumber string `json:"part_number"`
		Quantity   int64  `json:"quantity"`
	}
	m := struct {
		Items []item `json:"items"`
	}{Items: []item{{PartNumber: payloadCode, Quantity: uop}}}
	b, err := json.Marshal(m)
	if err != nil {
		return `{"items":[]}`
	}
	return string(b)
}

func ptr(v int64) *int64 { return &v }
