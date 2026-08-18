package nodes

import (
	"database/sql"
	"fmt"
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
// INERT AS OF v90. Nothing reads any of this. The level keeper that will is a
// later phase, and until it exists these tables describe an intent the system
// does not yet act on.

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

// RemoveMaintainLevel stops declaring a type for a group entirely. Distinct from
// setting want=0, which keeps the line and says "none right now".
func RemoveMaintainLevel(db *sql.DB, groupNodeID, binTypeID int64) error {
	if _, err := db.Exec(
		`DELETE FROM node_maintain_levels WHERE group_node_id=$1 AND bin_type_id=$2`,
		groupNodeID, binTypeID); err != nil {
		return fmt.Errorf("remove maintain level group=%d/bin_type=%d: %w", groupNodeID, binTypeID, err)
	}
	return nil
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
