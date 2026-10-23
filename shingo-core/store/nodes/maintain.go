package nodes

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"shingocore/domain"
)

// Maintained-group config: the level a node group holds in EMPTY CARRIERS, and
// which process nodes that level exists to serve.
//
// A maintained group is an NGRP near the equipment it feeds — presses, most of
// the time — that Core keeps stocked with unclaimed empties of declared types:
// "four 45x58x32 and two 45x48x24, at all times". The level is Core's to hold;
// the group's own retrieve/store algorithms decide nothing about it.
//
// NOT "buffer". bin_loaders.home_kind='buffer' is a different, existing thing
// with opposite semantics — a loader slot that counts as a window, is
// budget-bearing, and has no fallback. The two share nothing but a word, so this
// side does not use the word.
//
// SHAPE, following node_bin_types and loaders.Quota: sets live in small
// node-keyed tables with composite primary keys, and the SCALARS that go with
// them (Prop* below) are node_properties rows. That split is not aesthetic. The
// property write path is audited old→new unconditionally at the endpoint
// (www/handlers_nodes.go), so a scalar stored there arrives with a trail; a
// scalar stored in a new table would arrive with none until somebody built one.
//
// READ BY THE LEVEL KEEPER (engine/maintainer.go), which tops each group up to
// its levels, and by the resolver and the capacity plan, which hold a push to
// them. The layer wants (node_maintain_layer_levels, v161) are read by neither:
// they say which heights a group wants, and nothing caps or sources on them.

// Node-property keys for the maintained-group scalars. The sets get tables; the
// four singular answers get properties, for the audit reason above.
const (
	// PropMaintainEnabled is "on" when Core is to hold this group's level.
	// Anything else, including absent, is off — the polarity that matters, since
	// a group nobody has configured must not be one Core starts steering.
	PropMaintainEnabled = "maintain_enabled"

	// PropStrictSourcing is "on" when the group's empties are RESERVED for the
	// processes it supports: an outsider's plant-wide empty scan cannot see them.
	// Separate from PropMaintainEnabled because holding a level and fencing it
	// are two decisions, and a plant may reasonably want the first without the
	// second while it watches what the level does.
	PropStrictSourcing = "strict_sourcing"

	// PropMaintenanceStation names the Edge station that the level keeper's own
	// orders are projected to. REQUIRED when maintenance is enabled: projectOrder
	// no-ops on a blank StationID, so a keeper order without one is invisible on
	// every Edge board in the plant — it would run, and no operator could see it.
	PropMaintenanceStation = "maintenance_station"

	// PropOverflowDestination names a second group to try when a push into this
	// one is refused for being at level. Blank means none, and none is a real
	// answer: the push then parks holding its bin, which is backpressure into
	// whatever was pushing — uncomfortable, not dangerous.
	PropOverflowDestination = "overflow_destination"

	// PropBinTypeMode is NOT a maintained-group setting — it predates all of
	// this and governs how a node's allowed carrier types resolve ("all",
	// "specific", or ""/"inherit" to walk the parent chain). It is named here
	// because a maintained group has to carry an EXPLICIT one: inherit-by-
	// default means an ancestor's list can silently govern a group that has just
	// been told to hold two specific types.
	PropBinTypeMode = "bin_type_mode"

	// BinTypeModeAll is the no-restriction value, and the one a group gets when
	// a maintained level is declared on a group that never had a mode. It is
	// what an unconfigured group already behaves as; narrowing on the operator's
	// behalf would be making a decision for them.
	BinTypeModeAll = "all"
)

// MaintainLevel is one line of a maintained group's declared level: how many
// unclaimed empty carriers of one type the group is to hold.
//
// A CAP, and this is where it differs from loaders.Quota — which is a
// PREFERENCE bounded by the never-2N window count, deciding only which type to
// fetch next. Want here is the number itself: the keeper tops up TO it and the
// resolver will later refuse a push that would exceed it. The two tables stay
// separate for exactly that reason; merging them would put cap semantics in a
// table documented as not-a-cap.
//
// want=0 is a legitimate declared value meaning "none of this type" — a way to
// zero a line without forgetting it existed. RemoveMaintainLevel is how a type
// stops being declared at all.
type MaintainLevel struct {
	GroupNodeID int64  `json:"group_node_id"`
	BinTypeID   int64  `json:"bin_type_id"`
	BinTypeCode string `json:"bin_type_code"`
	Want        int    `json:"want"`
}

// MaintainSupport is one process node a maintained group serves.
//
// NODES, not claims or processes, and the editor's vocabulary is the only place
// "process" survives. Claims are Edge-local and structurally unreachable from
// Core, so a rule that had to resolve one at read time could not be evaluated on
// this side at all; the resolved node set is the thing Core can actually check a
// need against.
type MaintainSupport struct {
	GroupNodeID     int64  `json:"group_node_id"`
	ProcessNodeID   int64  `json:"process_node_id"`
	ProcessNodeName string `json:"process_node_name"`
}

// SetMaintainLevel declares (or re-declares) how many empties of one type a
// group holds. Upsert, so the editor can send a row without knowing whether it
// is new.
func SetMaintainLevel(db *sql.DB, l MaintainLevel) error {
	_, err := db.Exec(`
		INSERT INTO node_maintain_levels (group_node_id, bin_type_id, want)
		VALUES ($1,$2,$3)
		ON CONFLICT (group_node_id, bin_type_id)
		DO UPDATE SET want=EXCLUDED.want, updated_at=NOW()`,
		l.GroupNodeID, l.BinTypeID, l.Want)
	if err != nil {
		return fmt.Errorf("set maintain level group=%d/bin_type=%d: %w", l.GroupNodeID, l.BinTypeID, err)
	}
	return nil
}

// MaintainLayerWant is how many carts of one level's type, standing a given
// number of layers high, a maintained group wants: one row of
// node_maintain_layer_levels.
//
// A HEIGHT OF 1 OR MORE IS THE STACK LEVEL SAID BY HEIGHT: the cap is the
// type's MaintainLevel, those wants are not summed against it, and nothing
// refuses a push or mints an ask on them. They say which heights the group is
// short of, so a person at an unloader can be told what to send a cart back as.
//
// THE ROW AT 0 LAYERS IS TOPS & BOTTOMS, AND IT IS A CAP (v164): its own line
// beside the stack level. The store caps judge a cart at 0 against its want
// (TopsAndBottomsWant, binresolver.AtDeclaredLevel), a group that keeps the
// type with no such row refuses one, and the save counts it against the
// group's positions. The keeper never fetches tops & bottoms: it is made at
// the unloader.
//
// A layer want belongs to its level: it is written with the level's save and
// removed with the level (SetMaintainLevelAndLayerWants, RemoveMaintainLevel).
// TotesPerLayer is the type's bin_types.totes_per_layer, joined on read; nil
// when the type does not state one, and then no cart can be counted at a
// height.
type MaintainLayerWant struct {
	GroupNodeID   int64  `json:"group_node_id"`
	BinTypeID     int64  `json:"bin_type_id"`
	BinTypeCode   string `json:"bin_type_code"`
	Layers        int    `json:"layers"`
	Want          int    `json:"want"`
	TotesPerLayer *int   `json:"totes_per_layer,omitempty"`
}

// SetMaintainLevelAndLayerWants declares a level and replaces that type's
// layer wants with the given set, in one transaction: a save that changes the
// level and its heights changes both or neither. An empty set clears the
// type's layer wants. The rows' GroupNodeID and BinTypeID are the level's;
// only Layers and Want are read.
func SetMaintainLevelAndLayerWants(db *sql.DB, l MaintainLevel, wants []MaintainLayerWant) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin set maintain level group=%d/bin_type=%d: %w", l.GroupNodeID, l.BinTypeID, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if _, err := tx.Exec(`
		INSERT INTO node_maintain_levels (group_node_id, bin_type_id, want)
		VALUES ($1,$2,$3)
		ON CONFLICT (group_node_id, bin_type_id)
		DO UPDATE SET want=EXCLUDED.want, updated_at=NOW()`,
		l.GroupNodeID, l.BinTypeID, l.Want); err != nil {
		return fmt.Errorf("set maintain level group=%d/bin_type=%d: %w", l.GroupNodeID, l.BinTypeID, err)
	}
	if _, err := tx.Exec(
		`DELETE FROM node_maintain_layer_levels WHERE group_node_id=$1 AND bin_type_id=$2`,
		l.GroupNodeID, l.BinTypeID); err != nil {
		return fmt.Errorf("clear layer wants group=%d/bin_type=%d: %w", l.GroupNodeID, l.BinTypeID, err)
	}
	for _, w := range wants {
		if _, err := tx.Exec(`
			INSERT INTO node_maintain_layer_levels (group_node_id, bin_type_id, layers, want)
			VALUES ($1,$2,$3,$4)`,
			l.GroupNodeID, l.BinTypeID, w.Layers, w.Want); err != nil {
			return fmt.Errorf("set layer want group=%d/bin_type=%d/layers=%d: %w",
				l.GroupNodeID, l.BinTypeID, w.Layers, err)
		}
	}
	return tx.Commit()
}

// RemoveMaintainLevel stops declaring a type for a group entirely, and removes
// that type's layer wants in the same transaction: a height of a type the
// group no longer holds says nothing. Distinct from setting want=0, which
// keeps the line and says "none right now".
//
// The layer wants go here and not by a foreign key from them to the level
// row: v161 declared them against nodes and bin_types only.
func RemoveMaintainLevel(db *sql.DB, groupNodeID, binTypeID int64) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin remove maintain level group=%d/bin_type=%d: %w", groupNodeID, binTypeID, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if _, err := tx.Exec(
		`DELETE FROM node_maintain_layer_levels WHERE group_node_id=$1 AND bin_type_id=$2`,
		groupNodeID, binTypeID); err != nil {
		return fmt.Errorf("remove layer wants group=%d/bin_type=%d: %w", groupNodeID, binTypeID, err)
	}
	if _, err := tx.Exec(
		`DELETE FROM node_maintain_levels WHERE group_node_id=$1 AND bin_type_id=$2`,
		groupNodeID, binTypeID); err != nil {
		return fmt.Errorf("remove maintain level group=%d/bin_type=%d: %w", groupNodeID, binTypeID, err)
	}
	return tx.Commit()
}

// ListMaintainLayerWants returns a group's layer wants, every type's, in the
// levels' order (by code, as ListMaintainLevels) and then by height, with the
// code and the type's totes per layer joined on.
func ListMaintainLayerWants(db *sql.DB, groupNodeID int64) ([]MaintainLayerWant, error) {
	rows, err := db.Query(`
		SELECT w.group_node_id, w.bin_type_id, bt.code, w.layers, w.want, bt.totes_per_layer
		FROM node_maintain_layer_levels w
		JOIN bin_types bt ON bt.id = w.bin_type_id
		WHERE w.group_node_id=$1
		ORDER BY bt.code, w.layers`, groupNodeID)
	if err != nil {
		return nil, fmt.Errorf("list layer wants group=%d: %w", groupNodeID, err)
	}
	defer rows.Close()
	var out []MaintainLayerWant
	for rows.Next() {
		var w MaintainLayerWant
		if err := rows.Scan(&w.GroupNodeID, &w.BinTypeID, &w.BinTypeCode, &w.Layers, &w.Want, &w.TotesPerLayer); err != nil {
			return nil, fmt.Errorf("scan layer want: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// TopsAndBottomsWant is the want of the tops & bottoms row (layers 0) of one
// level's type in a group, and false when the group has no such row: the cap
// the tops & bottoms line is judged against (binresolver.AtDeclaredLevel). One
// statement, asked only for a cart at 0.
func TopsAndBottomsWant(db *sql.DB, groupNodeID, binTypeID int64) (int, bool, error) {
	var want int
	err := db.QueryRow(`SELECT want FROM node_maintain_layer_levels
		WHERE group_node_id=$1 AND bin_type_id=$2 AND layers=0`, groupNodeID, binTypeID).Scan(&want)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("tops & bottoms row group=%d/bin_type=%d: %w", groupNodeID, binTypeID, err)
	}
	return want, true, nil
}

// TypeInPlay is a bin type an unloader's group line reads (ListTypesInPlay):
// the type's dimensions and totes per layer, and the group's layer wants of it
// (by height, none when it wants none).
type TypeInPlay struct {
	Type       domain.BinType
	LayerWants []MaintainLayerWant
}

// ListTypesInPlay reads, in one statement, the bin types named by codes (a
// group's level types and the types of the carts at its unloader's windows)
// with their width, length and totes per layer, and group groupNodeID's layer
// wants of each, by code and then height. It is the group line's one types
// read, in place of ListMaintainLayerWants there: the floor sizes ride the
// read the heights already made.
func ListTypesInPlay(db *sql.DB, groupNodeID int64, codes []string) ([]TypeInPlay, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(codes)+1)
	args = append(args, groupNodeID)
	for _, c := range codes {
		args = append(args, c)
	}
	rows, err := db.Query(`
		SELECT bt.id, bt.code, bt.width_in, bt.length_in, bt.totes_per_layer, w.layers, w.want
		  FROM bin_types bt
		  LEFT JOIN node_maintain_layer_levels w ON w.bin_type_id = bt.id AND w.group_node_id = $1
		 WHERE bt.code IN (`+placeholders(2, len(codes))+`)
		 ORDER BY bt.code, w.layers`, args...)
	if err != nil {
		return nil, fmt.Errorf("list types in play group=%d: %w", groupNodeID, err)
	}
	defer rows.Close()
	var out []TypeInPlay
	for rows.Next() {
		var (
			bt           domain.BinType
			layers, want sql.NullInt64
		)
		if err := rows.Scan(&bt.ID, &bt.Code, &bt.WidthIn, &bt.LengthIn, &bt.TotesPerLayer, &layers, &want); err != nil {
			return nil, fmt.Errorf("scan type in play: %w", err)
		}
		if len(out) == 0 || out[len(out)-1].Type.ID != bt.ID {
			out = append(out, TypeInPlay{Type: bt})
		}
		if layers.Valid {
			t := &out[len(out)-1]
			t.LayerWants = append(t.LayerWants, MaintainLayerWant{GroupNodeID: groupNodeID, BinTypeID: bt.ID,
				BinTypeCode: bt.Code, Layers: int(layers.Int64), Want: int(want.Int64), TotesPerLayer: bt.TotesPerLayer})
		}
	}
	return out, rows.Err()
}

// ListMaintainLevels returns a group's declared level with the bin-type CODES
// joined on, because the code is what a person reads and what every other
// carrier-typed surface in the system prints — the id is a local key. Same
// reason loaders.ListQuotas carries one.
func ListMaintainLevels(db *sql.DB, groupNodeID int64) ([]MaintainLevel, error) {
	rows, err := db.Query(`
		SELECT l.group_node_id, l.bin_type_id, bt.code, l.want
		FROM node_maintain_levels l
		JOIN bin_types bt ON bt.id = l.bin_type_id
		WHERE l.group_node_id=$1
		ORDER BY bt.code`, groupNodeID)
	if err != nil {
		return nil, fmt.Errorf("list maintain levels group=%d: %w", groupNodeID, err)
	}
	defer rows.Close()
	var out []MaintainLevel
	for rows.Next() {
		var l MaintainLevel
		if err := rows.Scan(&l.GroupNodeID, &l.BinTypeID, &l.BinTypeCode, &l.Want); err != nil {
			return nil, fmt.Errorf("scan maintain level: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// MaintainedGroupIDs is the set of groups that declare a level — the groups
// for which ListMaintainLevels answers non-empty, read for every group in one
// statement. The node list marks them for the Edge (protocol.NodeInfo's
// Maintained), which is why this is one read per list and not one per node.
// The join is ListMaintainLevels' own, so the two cannot disagree about a row.
func MaintainedGroupIDs(db *sql.DB) (map[int64]bool, error) {
	rows, err := db.Query(`
		SELECT DISTINCT l.group_node_id
		FROM node_maintain_levels l
		JOIN bin_types bt ON bt.id = l.bin_type_id`)
	if err != nil {
		return nil, fmt.Errorf("list maintained groups: %w", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan maintained group: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

// SetMaintainSupports replaces the whole supported-process-node set for a group,
// in one transaction.
//
// REPLACE rather than add/remove, mirroring bins.SetNodeTypes: the editor holds
// the whole set on screen and sends the whole set, so a partial-update API would
// only invent a way for the screen and the table to disagree. An empty set is
// legal and means the group supports nobody, which is what a group in the middle
// of being configured looks like.
func SetMaintainSupports(db *sql.DB, groupNodeID int64, processNodeIDs []int64) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin set maintain supports group=%d: %w", groupNodeID, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if _, err := tx.Exec(`DELETE FROM node_maintain_supports WHERE group_node_id=$1`, groupNodeID); err != nil {
		return fmt.Errorf("clear maintain supports group=%d: %w", groupNodeID, err)
	}
	for _, pnID := range processNodeIDs {
		if _, err := tx.Exec(`
			INSERT INTO node_maintain_supports (group_node_id, process_node_id)
			VALUES ($1,$2) ON CONFLICT DO NOTHING`, groupNodeID, pnID); err != nil {
			return fmt.Errorf("add maintain support group=%d/node=%d: %w", groupNodeID, pnID, err)
		}
	}
	return tx.Commit()
}

// ListMaintainSupports returns the process nodes a group serves, names joined
// on for the same reason the levels carry codes.
func ListMaintainSupports(db *sql.DB, groupNodeID int64) ([]MaintainSupport, error) {
	rows, err := db.Query(`
		SELECT s.group_node_id, s.process_node_id, n.name
		FROM node_maintain_supports s
		JOIN nodes n ON n.id = s.process_node_id
		WHERE s.group_node_id=$1
		ORDER BY n.name`, groupNodeID)
	if err != nil {
		return nil, fmt.Errorf("list maintain supports group=%d: %w", groupNodeID, err)
	}
	defer rows.Close()
	var out []MaintainSupport
	for rows.Next() {
		var s MaintainSupport
		if err := rows.Scan(&s.GroupNodeID, &s.ProcessNodeID, &s.ProcessNodeName); err != nil {
			return nil, fmt.Errorf("scan maintain support: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GroupFencesAsker reports whether a STRICT maintained group excludes an asker
// that has named it explicitly.
//
// THE GO TWIN OF bins.FencedNodesCTE's FIRST RULE, and it exists because the
// two questions are asked at different moments. The CTE answers "which carriers
// can this plant-wide scan see", inside the query, silently. This answers "may
// this need, which NAMED this group, be served from it" — and that one needs a
// DISPOSITION, not a filter: a need turned away here parks with a cause an
// operator can read, rather than quietly finding nothing.
//
// Same predicate, two forms, and they must not drift: strict_sourcing on, and
// the process node absent from the supports list. A group that supports nobody
// fences everybody, which is the same reading the CTE takes.
//
// A blank processNode is an outsider — an ask that names no process cannot be
// supported anywhere, and the safe reading of that is "not for you".
func GroupFencesAsker(db *sql.DB, groupNodeID int64, processNode string) (bool, error) {
	var fenced bool
	err := db.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM node_properties np
			 WHERE np.node_id = $1
			   AND np.key = $2 AND np.value = 'on'
			   AND NOT EXISTS (
				 SELECT 1 FROM node_maintain_supports s
				 JOIN nodes pn ON pn.id = s.process_node_id
				 WHERE s.group_node_id = np.node_id AND pn.name = $3
			   )
		)`, groupNodeID, PropStrictSourcing, processNode).Scan(&fenced)
	if err != nil {
		return false, fmt.Errorf("group %d fences %q: %w", groupNodeID, processNode, err)
	}
	return fenced, nil
}

// MaintainedGroupsSupporting returns the maintained groups that serve a process
// node.
//
// UNFILTERED BY strict_sourcing, unlike the fence. The MG3-5 audit asks "did
// the press that this group exists to serve have to go elsewhere", and that
// question is about the LEVEL, not about the fence — a group holding a level
// for a press it does not fence is still a group that came up short when the
// press sourced from across the plant.
func MaintainedGroupsSupporting(db *sql.DB, processNode string) ([]int64, error) {
	if processNode == "" {
		return nil, nil
	}
	rows, err := db.Query(`
		SELECT s.group_node_id
		  FROM node_maintain_supports s
		  JOIN nodes pn ON pn.id = s.process_node_id
		 WHERE pn.name = $1`, processNode)
	if err != nil {
		return nil, fmt.Errorf("maintained groups supporting %q: %w", processNode, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan maintained group: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// NodeIsUnderAny reports whether nodeID sits inside any of the given subtrees,
// each root INCLUSIVE.
//
// Inclusive because a carrier standing on a group node itself — synthetic and
// therefore holding none today — would otherwise read as outside the group it
// is literally on, the day that stops being true.
func NodeIsUnderAny(db *sql.DB, nodeID int64, roots []int64) (bool, error) {
	if nodeID == 0 || len(roots) == 0 {
		return false, nil
	}
	var under bool
	err := db.QueryRow(`
		WITH RECURSIVE up(id, parent_id) AS (
			SELECT id, parent_id FROM nodes WHERE id = $1
			UNION ALL
			SELECT n.id, n.parent_id FROM nodes n JOIN up ON n.id = up.parent_id
		)
		SELECT EXISTS (SELECT 1 FROM up WHERE up.id IN (`+placeholders(2, len(roots))+`))`,
		append([]any{nodeID}, int64sToAny(roots)...)...).Scan(&under)
	if err != nil {
		return false, fmt.Errorf("node %d under any of %v: %w", nodeID, roots, err)
	}
	return under, nil
}

// placeholders renders $start..$start+n-1, so an IN list does not need a driver
// array type. Two helpers rather than one lib/pq dependency in a package that
// has none.
func placeholders(start, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf("$%d", start+i)
	}
	return strings.Join(parts, ", ")
}

func int64sToAny(xs []int64) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}
