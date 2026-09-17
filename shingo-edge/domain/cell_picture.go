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
	// Version is the token the station POLL carries in place of this whole
	// structure, stamped on the picture so the page can compare the two
	// directly: the picture it holds is current exactly while its Version
	// equals the one on the last view. See cell_picture_version.go.
	//
	// STAMPED ON THE PICTURE AND NOT ONLY ON THE VIEW, because the fetch and
	// the poll are separate reads and the picture may be built from facts
	// NEWER than the version that sent the page to fetch it. Storing the
	// picture's own version rather than the one asked for makes that
	// self-correcting: the next poll already matches, and nothing refetches.
	Version string `json:"version,omitempty"`
	// NO SceneRevision. It shipped on every picture — the station's, on every
	// poll, and the desktop's — so a screen could say "is this the map Core
	// has now", and no screen ever did. The desktop's map caption asks that
	// question and answers it from ComposerMap.Revision, which is the read
	// that actually draws the geometry.
	//
	// Groups is NGRP name → member node names, for the dock strip's member
	// line under each group. Empty when the node list has not arrived.
	//
	// FILTERED TO THE GROUPS THIS PICTURE NAMES, and that is the whole of P3.
	// The input is the plant's entire NGRP map, deep-copied out of the engine
	// under the core-node lock; the one reader of this field is the dock
	// strip's member line (composer-model.js's membersOf), which indexes at
	// most TWO entries — the inbound source and the outbound destination the
	// picture's claims carry. The rest was the whole plant's group membership
	// serialised into every poll of every board for nobody.
	Groups map[string][]string `json:"groups,omitempty"`
	// Staging is where a bin RESTS during this cell's swap: the lanes the
	// running flow parks at, and — on the composer's picture — the ones the
	// process's routing set offers it.
	//
	// THEIR OWN LIST, NOT A Kind IN Positions (SYNTH §3 B1). A member of
	// Positions is a position an operator may claim, and nine sites act on
	// that: the model mints a tappable cell per position, "+ Add a position"
	// offers the free ones, the desktop's table gives each a row, partner
	// pickers list them, placeEvenly puts them in the front row, rowKinds
	// captions them. Each would need a guard, and one miss saves a supermarket
	// lane as a cell position. A separate list makes "a staging node is not a
	// position" true by construction.
	//
	// A STAGING NODE THAT IS A POSITION IS NOT IN HERE. Hopkinsville's P400
	// parks on its own back slots, and PLN_02/PLN_05 stay exactly the
	// positions they have always been, captioned by the PartnerKind they
	// already carried.
	Staging []CellStaging `json:"staging,omitempty"`
	// NO LM COORDINATES (owner, 2026-09-17: "the point of the LMs isn't to
	// represent them to scale, it's to direct flow"). This carried the vendor
	// map's waypoints for the region around the cell so the HMI could draw each
	// one at its true place. It drew nothing on a real cell — the picture frames
	// the cell at 120 px/m and the aisle an engineer routes through is metres
	// outside that frame — and the route strip that replaced it needs no
	// coordinates at all: it says ORDER and DIRECTION, from the names
	// CellClaim.KeyRoute already carries.
	//
	// Geography lives on the desktop's ComposerMap, which has the whole plant
	// and draws the aisles. The station is not given one.
}

// CellStaging is one staging slot beside the cell.
//
// THE CLAIM'S OWN FIELD NAMES (owner ruling F3). Field is `inbound_staging` or
// `outbound_staging` — the column that named this lane — so the caption on the
// card, the row in the position panel and a server refusal about it are one
// word. A card nothing is using yet carries neither Field nor PartnerOf, which
// is what "offered" means and needs no flag of its own.
type CellStaging struct {
	CoreNodeName string   `json:"core_node_name"`
	X            *float64 `json:"x,omitempty"`
	Y            *float64 `json:"y,omitempty"`
	// PartnerOf is the position whose swap parks here, empty on an offered
	// lane. PartnerKind is "staging" — the value CellPosition already uses for
	// exactly this relationship, not a second word for it.
	PartnerOf   string `json:"partner_of,omitempty"`
	PartnerKind string `json:"partner_kind,omitempty"`
	Field       string `json:"field,omitempty"`
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
	// KeyRoute is the ordered waypoints SEER is told to drive for this claim's
	// supply trip, and the picture marks them on the leg (CellPicture.LMs).
	//
	// IT ARRIVED WITH THE DRAWING. The picture carried every other field it
	// draws and not this one, so the board could place a robot's waypoints and
	// had no way to know which of them anybody had CHOSEN — the half of the LM
	// drawing that carries a name. It costs the poll nothing, because the poll
	// no longer carries the picture.
	KeyRoute []string `json:"key_route,omitempty"`
}

// CellPictureInput is everything BuildCellPicture reads.
type CellPictureInput struct {
	// StationID is the station the picture is for; a node is a position of
	// this cell when it is bound to this station, or when the running style
	// names it as a partner (see cellPositionNames).
	StationID int64
	// Nodes is EVERY process node of the process, not only this station's:
	// partner and staging positions are usually stationless rows, so a list
	// scoped to the station would leave the picture with the front cards and
	// none of the cards they trade bins with.
	//
	// THE LIVE ROWS, as both callers supply it. This said "deleted ones
	// included; the builder filters", which is false about the input: the
	// station read and the composer read both fill it from
	// ListProcessNodesByProcess, whose SQL is `WHERE n.process_id=? AND
	// n.deleted_at IS NULL` (store/processes.ListNodesByProcess), so no deleted
	// row has ever reached this field in production.
	//
	// cellPositionNames drops deleted and disabled rows anyway, and that guard
	// stays: this is a pure function with exported input, its unit tests hand
	// it rows the query would not (cell_picture_test.go marks one deleted by
	// hand and pins that it draws no card), and a caller that one day reads the
	// nodes some other way should not silently gain a deleted position. What
	// the guard is NOT is the reason the field can be filled carelessly.
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
	// Groups is NGRP name → members, as the engine retained them. The whole
	// plant's; the builder keeps the entries this picture's claims name and
	// drops the rest. See CellPicture.Groups.
	Groups map[string][]string
	// Version is the token the poll compares against, stamped onto the
	// picture. Built by CellPictureVersion from the same facts.
	Version string
	// StagingOffers is the process's ENABLED staging-role routing nodes — what
	// this cell may park at, whether or not the running flow does (R4). Set on
	// the composer's read and empty on the board's, which draws the staging the
	// running flow names and nothing it merely could name.
	StagingOffers []string
}

// groupsNamedBy keeps the NGRP entries the picture's own claims reach for: the
// dock strip draws a member line under the inbound source and the outbound
// destination, and under nothing else.
//
// Nil rather than an empty map when nothing is named, so `omitempty` takes the
// field off the wire entirely: a picture with no claims has no dock groups to
// draw, and `"groups":{}` is two bytes saying so on a payload that exists to be
// small.
func groupsNamedBy(all map[string][]string, positions []CellPosition) map[string][]string {
	if len(all) == 0 {
		return nil
	}
	var out map[string][]string
	for i := range positions {
		c := positions[i].Claim
		if c == nil {
			continue
		}
		for _, name := range []string{c.InboundSource, c.OutboundDestination} {
			if name == "" {
				continue
			}
			members, known := all[name]
			if !known {
				continue
			}
			if out == nil {
				out = make(map[string][]string, 2)
			}
			out[name] = members
		}
	}
	return out
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
	pic := &CellPicture{Version: in.Version}
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
				KeyRoute: append([]string(nil), c.KeyRoute...),
			}
			pos.Claim = cc
		} else if p, ok := partners[n.CoreNodeName]; ok {
			pos.Role, pos.PartnerOf, pos.PartnerKind = "back", p.of, p.kind
		}
		pic.Positions = append(pic.Positions, pos)
	}
	pic.Groups = groupsNamedBy(in.Groups, pic.Positions)
	sort.SliceStable(pic.Positions, func(i, j int) bool {
		a, b := pic.Positions[i], pic.Positions[j]
		if a.Sequence != b.Sequence {
			return a.Sequence < b.Sequence
		}
		return a.CoreNodeName < b.CoreNodeName
	})
	// GEOMETRY IS ABOUT THE POSITIONS, and the staging cards are built after
	// it is decided for exactly that reason. A staging lane the map does not
	// carry must not flip a cell whose every position IS placed into the
	// schematic — the cards are laid in their own band, so an unplaced one
	// costs its own card a coordinate and nothing else.
	pic.Geometry = len(pic.Positions) > 0 && placed == len(pic.Positions)
	pic.Staging = stagingCards(in, pic.Positions)
	return pic
}

// stagingCards is the staging list: the lanes the running claims park at, then
// the ones the routing set offers, minus anything that is a position of this
// cell.
//
// SORTED BY NAME, not by the order the claims happened to iterate in: the
// claims arrive from a map, and a band of cards that reshuffled itself between
// two draws of the same flow would be a picture that moves while nothing does.
func stagingCards(in CellPictureInput, positions []CellPosition) []CellStaging {
	isPosition := make(map[string]bool, len(positions))
	for _, p := range positions {
		isPosition[p.CoreNodeName] = true
	}
	byName := map[string]CellStaging{}
	add := func(name, partnerOf, field string) {
		if name == "" || isPosition[name] {
			return
		}
		// FIRST CLAIM WINS, like the partner map above: two flows parking at one
		// lane is one card, captioned for whichever position the picture met
		// first, and a second card for the same lane would be the same place
		// drawn twice.
		if existing, taken := byName[name]; taken && existing.PartnerOf != "" {
			return
		}
		card := CellStaging{CoreNodeName: name, PartnerOf: partnerOf, Field: field}
		if partnerOf != "" {
			card.PartnerKind = "staging"
		}
		if x, y, ok := LocateCellPosition(in.Geometry, name); ok {
			card.X, card.Y = &x, &y
		}
		byName[name] = card
	}
	for front, c := range in.Claims {
		add(c.InboundStaging, front, "inbound_staging")
		add(c.OutboundStaging, front, "outbound_staging")
	}
	for _, name := range in.StagingOffers {
		add(name, "", "")
	}
	out := make([]CellStaging, 0, len(byName))
	for _, card := range byName {
		out = append(out, card)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CoreNodeName < out[j].CoreNodeName })
	return out
}
