package domain

import (
	"sort"
)

// cell_picture.go — the data behind the station's read-only picture of its
// cell: the press positions in their true relative arrangement, the running
// style's choreography on them, and the dock the bins come from and go to.
//
// Built here, in Go, from three things the Edge already holds — the process's
// nodes, the active style's claims, and the scene geometry cache — so the
// screen draws what it is handed and the join rule lives in one testable
// place. The renderer (operator-flow.js) adds nothing to it.

// CellPicture is one station's cell. Positions is every press position in
// process_nodes.sequence order; Geometry says whether every one of them was
// placed on the map, and when it is false the renderer draws them evenly
// spaced under a "positions not to scale" caption. It is never nil for a
// station: a cell with no nodes is an empty picture, not a missing one.
type CellPicture struct {
	Positions []CellPosition `json:"positions"`
	Geometry  bool           `json:"geometry"`
	// NO SceneRevision. It shipped on every picture — the station's, on every
	// poll, and the desktop's — so a screen could say "is this the map Core
	// has now", and no screen ever did. The desktop's map caption asks that
	// question and answers it from ComposerMap.Revision, which is the read
	// that actually draws the geometry.
	//
	// Groups is NGRP name → member node names, for the dock strip's member
	// line under each group. Empty when the node list has not arrived.
	Groups map[string][]string `json:"groups,omitempty"`
}

// CellPosition is one press position. X/Y are the bin location's scene
// coordinates — the GeneralLocation row — and nil when the map has no such
// row, which the picture renders as the schematic.
//
// Two different questions are answered by Role and Kind. Role is about the
// RUNNING style: "front" for a position it claims, "back" for one a claim
// uses as its on-deck (PartnerKind "deck") or staging (PartnerKind
// "staging") position, "" when the running style does not use it. Kind is
// about the PRESS: "back" for a position any of the process's styles ever
// uses as a partner slot — Hopkinsville's PLN_02 and PLN_05 under every
// style — and "front" otherwise. An unused card is captioned by its Kind
// ("front position" / "back position"), because the press does not change
// shape when the style does.
type CellPosition struct {
	CoreNodeName string     `json:"core_node_name"`
	Sequence     int        `json:"sequence"`
	X            *float64   `json:"x,omitempty"`
	Y            *float64   `json:"y,omitempty"`
	Role         string     `json:"role,omitempty"`
	Kind         string     `json:"kind"`
	Claim        *CellClaim `json:"claim,omitempty"`
	PartnerOf    string     `json:"partner_of,omitempty"`
	PartnerKind  string     `json:"partner_kind,omitempty"`
}

// CellClaim is the slice of a NodeClaim the picture reads: the choreography,
// the part, the partner positions and the dock.
//
// NO BIN TYPE. It carried one, and the picture's only use for it was to pick
// between the words "totes" and "bins" — which owner ruling R2 (2026-09-12)
// removed from every surface. A field whose one reader is gone is not a field.
type CellClaim struct {
	SwapMode             string `json:"swap_mode"`
	PayloadCode          string `json:"payload_code"`
	PairedCoreNode       string `json:"paired_core_node,omitempty"`
	SecondPairedCoreNode string `json:"second_paired_core_node,omitempty"`
	InboundStaging       string `json:"inbound_staging,omitempty"`
	OutboundStaging      string `json:"outbound_staging,omitempty"`
	InboundSource        string `json:"inbound_source,omitempty"`
	OutboundDestination  string `json:"outbound_destination,omitempty"`
}

// CellPictureInput is everything BuildCellPicture reads.
type CellPictureInput struct {
	// StationID is the station the picture is for; a node is a position of
	// this cell when it is bound to this station, or when the running style
	// names it as a partner (see cellPositionNames).
	StationID int64
	// Nodes is EVERY process node of the process, deleted ones included; the
	// builder filters. Partner positions are usually stationless rows.
	Nodes []Node
	// Claims is the running style's claims by core node name. Nil when no
	// style is running.
	Claims map[string]NodeClaim
	// BackPositions is every position any of the process's styles names as
	// a partner slot (paired, second paired, inbound or outbound staging) —
	// the press's back positions, whichever style is running. Nil reads as
	// "none known", and the running claims' partners still count.
	BackPositions map[string]bool
	// Geometry is the scene cache, nil before the first full sync.
	Geometry *SceneGeometry
	// Groups is NGRP name → members, as the engine retained them.
	Groups map[string][]string
}

// LocateCellPosition is THE JOIN RULE: a core node name is placed at the
// scene point with the same instance_name whose class is GeneralLocation —
// the bin location. Not the ActionPoint that row's point_name names (the
// robot's approach pose, half a metre off), and never the label, which
// Hopkinsville happens to populate and Springfield does not. ok is false when
// the map has no such row, and false is "unplaced", never (0,0).
func LocateCellPosition(g *SceneGeometry, coreNodeName string) (x, y float64, ok bool) {
	if g == nil || coreNodeName == "" {
		return 0, 0, false
	}
	p, found := g.Points[coreNodeName]
	if !found || p.ClassName != "GeneralLocation" {
		return 0, 0, false
	}
	return p.X, p.Y, true
}

// cellPositionNames is the position set of one station's cell: its own
// nodes, plus every position the process's styles lean on — the on-deck
// position of a press-index claim, the staging positions of a swap — which
// are ordinarily stationless rows and would otherwise be invisible exactly
// when the flow needs them drawn. The press-wide set (BackPositions) counts
// as well as the running claims' partners, so the back slots are drawn
// between styles too: the press does not lose two positions because nothing
// is running on them. A supermarket node that is a process node of the same
// process but bound to no station and named by no claim is not a press
// position and is not drawn.
// stationID 0 means EVERY STATION OF THIS PROCESS, and it is the desktop's scope.
//
// Not "every process node": a process's node list also carries the routing nodes
// it draws from — Hopkinsville P400 owns SMN_01, SMN_03 and SMN_04 — and those
// are supermarket slots, not press positions. Taking the node list whole put
// them in the picture as cards and offered them under "+ Add a position", which
// is an invitation to claim a supermarket slot as part of a press.
//
// A press position is one an operator screen works. Bound to ANY of the
// process's screens is the union of the station-scoped pictures, which is what
// "the whole press" means.
//
// A station's picture is the positions bound to that station plus whatever the
// running style pairs with — right for an HMI, which is one screen of one cell.
// The engineer editing the same press is not standing at any of its screens:
// their table has a row per position and offers the free ones, so their picture
// is every live position of the process. One builder answers both rather than
// the desktop growing a second drawing of the same press.
func cellPositionNames(stationID int64, nodes []Node, claims map[string]NodeClaim, backPositions map[string]bool) map[string]bool {
	partners := map[string]bool{}
	for name := range backPositions {
		partners[name] = true
	}
	for _, c := range claims {
		// THE INDEX POSITIONS BY NAME. This hand-wrote the two fields into a
		// literal, which is the duplication ExtensionPositions exists to end —
		// a third position added to the layout would have had to find this
		// site. The two STAGING nodes are a different question (where a bin is
		// parked, not which nodes the cell occupies) and stay their own list.
		for _, name := range c.ExtensionPositions() {
			partners[name] = true
		}
		for _, name := range []string{c.InboundStaging, c.OutboundStaging} {
			if name != "" {
				partners[name] = true
			}
		}
	}
	out := map[string]bool{}
	for _, n := range nodes {
		if n.DeletedAt != nil || !n.Enabled {
			continue
		}
		bound := n.OperatorStationID != nil &&
			(stationID == 0 || *n.OperatorStationID == stationID)
		if bound || partners[n.CoreNodeName] {
			out[n.CoreNodeName] = true
		}
	}
	return out
}

// BuildCellPicture assembles the picture. Pure: it reads its input and
// returns a value, so the join and the roles are tested without a database.
func BuildCellPicture(in CellPictureInput) *CellPicture {
	pic := &CellPicture{Groups: in.Groups}
	names := cellPositionNames(in.StationID, in.Nodes, in.Claims, in.BackPositions)

	// Partner roles first, so a back position knows whose it is.
	type partner struct{ of, kind string }
	partners := map[string]partner{}
	for front, c := range in.Claims {
		// Same question, same name: the positions behind the front node.
		// ExtensionPositions drops the blanks, so the guard the literal needed
		// goes with it.
		for _, deck := range c.ExtensionPositions() {
			if _, taken := partners[deck]; !taken {
				partners[deck] = partner{of: front, kind: "deck"}
			}
		}
		for _, stg := range []string{c.InboundStaging, c.OutboundStaging} {
			if stg != "" {
				if _, taken := partners[stg]; !taken {
					partners[stg] = partner{of: front, kind: "staging"}
				}
			}
		}
	}

	placed := 0
	seen := map[string]bool{}
	for _, n := range in.Nodes {
		if !names[n.CoreNodeName] || seen[n.CoreNodeName] {
			continue
		}
		seen[n.CoreNodeName] = true
		pos := CellPosition{CoreNodeName: n.CoreNodeName, Sequence: n.Sequence}
		if x, y, ok := LocateCellPosition(in.Geometry, n.CoreNodeName); ok {
			pos.X, pos.Y = &x, &y
			placed++
		}
		pos.Kind = "front"
		if _, isPartner := partners[n.CoreNodeName]; isPartner || in.BackPositions[n.CoreNodeName] {
			pos.Kind = "back"
		}
		if c, ok := in.Claims[n.CoreNodeName]; ok {
			pos.Role = "front"
			cc := &CellClaim{
				SwapMode: string(c.SwapMode), PayloadCode: c.PayloadCode,
				PairedCoreNode: c.PairedCoreNode, SecondPairedCoreNode: c.SecondPairedCoreNode,
				InboundStaging: c.InboundStaging, OutboundStaging: c.OutboundStaging,
				InboundSource: c.InboundSource, OutboundDestination: c.OutboundDestination,
			}
			pos.Claim = cc
		} else if p, ok := partners[n.CoreNodeName]; ok {
			pos.Role, pos.PartnerOf, pos.PartnerKind = "back", p.of, p.kind
		}
		pic.Positions = append(pic.Positions, pos)
	}
	sort.SliceStable(pic.Positions, func(i, j int) bool {
		a, b := pic.Positions[i], pic.Positions[j]
		if a.Sequence != b.Sequence {
			return a.Sequence < b.Sequence
		}
		return a.CoreNodeName < b.CoreNodeName
	})
	pic.Geometry = len(pic.Positions) > 0 && placed == len(pic.Positions)
	return pic
}
