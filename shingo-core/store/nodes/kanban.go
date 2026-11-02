package nodes

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"shingocore/domain"
	"shingocore/kanban"
	"shingocore/store/internal/helpers"
)

// Kanban-group config: the level a node group holds in EMPTY CARRIERS, and
// which process nodes that level exists to serve.
//
// A kanban is an NGRP near the equipment it feeds — presses, most of
// the time — that Core keeps stocked with unclaimed empties of declared types:
// "four 45x58x32 and two 45x48x24, at all times". The level is Core's to hold;
// the group's own retrieve/store algorithms decide nothing about it.
//
// NOT "buffer". bin_loaders.home_kind='buffer' is a different, existing thing
// with opposite semantics — a loader slot that counts as a window, is
// budget-bearing, and has no fallback. The two share nothing but a word, so this
// side does not use the word.
//
// SHAPE: one kanbans row per kanban holding its settings (Kanban below), with
// the sets (kanban_levels, kanban_supports, kanban_layer_levels, kanban_sources)
// keyed on it. Production writes all of it through store.SaveKanban, which
// appends one audit row per changed field or list entry.
//
// READ BY THE LEVEL KEEPER (engine/kanban_keeper.go), which tops each group up to
// its levels, and by the resolver and the capacity plan, which hold a push to
// them. The layer wants (kanban_layer_levels, v161): a height of 1 or more says
// which heights a group wants, read by an unloader's group line, and nothing caps
// or sources on it; the row at 0 layers is a cap of its own (TopsAndBottomsWant).

const (
	// PropOverflowDestination is a PLAIN group's overflow: a second group to try
	// when a push into this one is refused for room. A kanban has none: a sized loop always
	// has a slot, and a cart that cannot land parks visible. Blank
	// means none, and none is a real answer: the push then parks holding its bin,
	// which is backpressure into whatever was pushing — uncomfortable, not
	// dangerous.
	PropOverflowDestination = "overflow_destination"
)

// KanbanLevel is one line of a kanban's declared level (kanban.TypeLevel).
type KanbanLevel = kanban.TypeLevel

// KanbanSupport is one process node a kanban serves.
//
// NODES, not claims or processes, and the editor's vocabulary is the only place
// "process" survives. Claims are Edge-local and structurally unreachable from
// Core, so a rule that had to resolve one at read time could not be evaluated on
// this side at all; the resolved node set is the thing Core can actually check a
// need against.
type KanbanSupport struct {
	GroupNodeID     int64  `json:"group_node_id"`
	ProcessNodeID   int64  `json:"process_node_id"`
	ProcessNodeName string `json:"process_node_name"`
}

// Execer is what the statements below run on: *sql.DB, or the transaction
// store.SaveKanban holds.
type Execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// EnsureKanban gives a group its kanbans row, every setting at its default. A
// level, a support, a layer want or a source hangs off the row by foreign key.
func EnsureKanban(x Execer, nodeID int64) error {
	if _, err := x.Exec(`INSERT INTO kanbans (node_id) VALUES ($1) ON CONFLICT (node_id) DO NOTHING`, nodeID); err != nil {
		return fmt.Errorf("ensure kanban %d: %w", nodeID, err)
	}
	return nil
}

// UpsertLevel declares (or re-declares) how many empties of one type a kanban
// holds. The kanban's row exists.
func UpsertLevel(x Execer, l KanbanLevel) error {
	_, err := x.Exec(`
		INSERT INTO kanban_levels (kanban_node_id, bin_type_id, want)
		VALUES ($1,$2,$3)
		ON CONFLICT (kanban_node_id, bin_type_id)
		DO UPDATE SET want=EXCLUDED.want, updated_at=NOW()`,
		l.GroupNodeID, l.BinTypeID, l.Want)
	if err != nil {
		return fmt.Errorf("set kanban level group=%d/bin_type=%d: %w", l.GroupNodeID, l.BinTypeID, err)
	}
	return nil
}

// ReplaceLayerWants replaces one level type's layer wants with the given set; an
// empty set clears them. Only Layers and Want of each row are read.
func ReplaceLayerWants(x Execer, kanbanNodeID, binTypeID int64, wants []KanbanLayerWant) error {
	if _, err := x.Exec(`DELETE FROM kanban_layer_levels WHERE kanban_node_id=$1 AND bin_type_id=$2`,
		kanbanNodeID, binTypeID); err != nil {
		return fmt.Errorf("clear layer wants group=%d/bin_type=%d: %w", kanbanNodeID, binTypeID, err)
	}
	for _, w := range wants {
		if _, err := x.Exec(`
			INSERT INTO kanban_layer_levels (kanban_node_id, bin_type_id, layers, want)
			VALUES ($1,$2,$3,$4)`,
			kanbanNodeID, binTypeID, w.Layers, w.Want); err != nil {
			return fmt.Errorf("set layer want group=%d/bin_type=%d/layers=%d: %w", kanbanNodeID, binTypeID, w.Layers, err)
		}
	}
	return nil
}

// DeleteLevel stops declaring a type, and removes its layer wants: a height of a
// type the kanban no longer holds says nothing. Distinct from want=0, which keeps
// the line and says "none right now". The layer wants go here and not by a
// foreign key from them to the level row: v161 declared them against the group
// and the type only.
func DeleteLevel(x Execer, kanbanNodeID, binTypeID int64) error {
	if _, err := x.Exec(`DELETE FROM kanban_layer_levels WHERE kanban_node_id=$1 AND bin_type_id=$2`,
		kanbanNodeID, binTypeID); err != nil {
		return fmt.Errorf("remove layer wants group=%d/bin_type=%d: %w", kanbanNodeID, binTypeID, err)
	}
	if _, err := x.Exec(`DELETE FROM kanban_levels WHERE kanban_node_id=$1 AND bin_type_id=$2`,
		kanbanNodeID, binTypeID); err != nil {
		return fmt.Errorf("remove kanban level group=%d/bin_type=%d: %w", kanbanNodeID, binTypeID, err)
	}
	return nil
}

// ReplaceSupports replaces the whole supported-process-node set of a kanban.
func ReplaceSupports(x Execer, kanbanNodeID int64, processNodeIDs []int64) error {
	if _, err := x.Exec(`DELETE FROM kanban_supports WHERE kanban_node_id=$1`, kanbanNodeID); err != nil {
		return fmt.Errorf("clear kanban supports group=%d: %w", kanbanNodeID, err)
	}
	for _, pnID := range processNodeIDs {
		if _, err := x.Exec(`
			INSERT INTO kanban_supports (kanban_node_id, process_node_id)
			VALUES ($1,$2) ON CONFLICT DO NOTHING`, kanbanNodeID, pnID); err != nil {
			return fmt.Errorf("add kanban support group=%d/node=%d: %w", kanbanNodeID, pnID, err)
		}
	}
	return nil
}

// ReplaceSources replaces a kanban's sources: the list order is the rank, dense
// from 1.
func ReplaceSources(x Execer, kanbanNodeID int64, sourceNodeIDs []int64) error {
	if _, err := x.Exec(`DELETE FROM kanban_sources WHERE kanban_node_id=$1`, kanbanNodeID); err != nil {
		return fmt.Errorf("clear sources kanban=%d: %w", kanbanNodeID, err)
	}
	for i, id := range sourceNodeIDs {
		if _, err := x.Exec(`INSERT INTO kanban_sources (kanban_node_id, rank, source_node_id) VALUES ($1,$2,$3)`,
			kanbanNodeID, i+1, id); err != nil {
			return fmt.Errorf("add source kanban=%d/node=%d: %w", kanbanNodeID, id, err)
		}
	}
	return nil
}

// SetKanbanScalars writes the kanbans row's two settings. A blank station is
// NULL.
func SetKanbanScalars(x Execer, k Kanban) error {
	if _, err := x.Exec(`UPDATE kanbans
		SET station_id=NULLIF($2::text,''), strict=$3, updated_at=NOW()
		WHERE node_id=$1`, k.NodeID, k.StationID, k.Strict); err != nil {
		return fmt.Errorf("set kanban %d: %w", k.NodeID, err)
	}
	return nil
}

// Kanban is one kanbans row: a kanban's settings. HasSupports is read beside
// them because the keeper's skip of a kanban with no station needs it.
type Kanban struct {
	NodeID      int64
	StationID   string
	Strict      bool
	HasSupports bool
}

const kanbanSelect = `SELECT k.node_id, COALESCE(k.station_id, ''), k.strict,
		EXISTS (SELECT 1 FROM kanban_supports s WHERE s.kanban_node_id = k.node_id)
	  FROM kanbans k`

func scanKanban(row interface{ Scan(dest ...any) error }) (Kanban, error) {
	var k Kanban
	err := row.Scan(&k.NodeID, &k.StationID, &k.Strict, &k.HasSupports)
	return k, err
}

// GetKanban reads one kanbans row; nil when the group has none.
func GetKanban(q helpers.QueryRower, nodeID int64) (*Kanban, error) {
	k, err := scanKanban(q.QueryRow(kanbanSelect+` WHERE k.node_id=$1`, nodeID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read kanban %d: %w", nodeID, err)
	}
	return &k, nil
}

// ListKanbans reads every kanbans row in one statement.
func ListKanbans(q helpers.RowsQuerier) ([]Kanban, error) {
	rows, err := q.Query(kanbanSelect)
	if err != nil {
		return nil, fmt.Errorf("list kanbans: %w", err)
	}
	defer rows.Close()
	var out []Kanban
	for rows.Next() {
		k, err := scanKanban(rows)
		if err != nil {
			return nil, fmt.Errorf("scan kanban: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// KanbanEpisode is what an open kanban episode tells the finder: the group it
// fills, the carrier type it is short of, and the groups it may pull from in rank
// order (none: the plant). The zero value is no such episode.
type KanbanEpisode struct {
	Group   string
	BinType string
	Sources []int64
}

// KanbanSource is one entry of a kanban's sources, in rank order.
type KanbanSource struct {
	KanbanNodeID   int64  `json:"kanban_node_id"`
	Rank           int    `json:"rank"`
	SourceNodeID   int64  `json:"source_node_id"`
	SourceNodeName string `json:"source_node_name"`
}

// ListKanbanSources returns a kanban's sources in rank order, names joined on.
func ListKanbanSources(q helpers.RowsQuerier, kanbanNodeID int64) ([]KanbanSource, error) {
	rows, err := q.Query(`
		SELECT s.kanban_node_id, s.rank, s.source_node_id, n.name
		  FROM kanban_sources s
		  JOIN nodes n ON n.id = s.source_node_id
		 WHERE s.kanban_node_id=$1
		 ORDER BY s.rank`, kanbanNodeID)
	if err != nil {
		return nil, fmt.Errorf("list sources kanban=%d: %w", kanbanNodeID, err)
	}
	defer rows.Close()
	var out []KanbanSource
	for rows.Next() {
		var s KanbanSource
		if err := rows.Scan(&s.KanbanNodeID, &s.Rank, &s.SourceNodeID, &s.SourceNodeName); err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// OverflowTarget is where a plain group's overflow points: its overflow_destination property
// as written, which may not resolve. A kanban has none.
type OverflowTarget struct {
	Name string
}

const overflowTargetSQL = `SELECT CASE WHEN k.node_id IS NULL THEN COALESCE(p.value, '') ELSE '' END
	  FROM nodes g
	  LEFT JOIN kanbans k ON k.node_id = g.id
	  LEFT JOIN node_properties p ON p.node_id = g.id AND p.key = '` + PropOverflowDestination + `'`

// OverflowTargetOf reads a group's overflow in one statement. Unknown group:
// the zero value.
func OverflowTargetOf(q helpers.QueryRower, groupNodeID int64) (OverflowTarget, error) {
	return scanOverflowTarget(q.QueryRow(overflowTargetSQL+` WHERE g.id=$1`, groupNodeID), groupNodeID)
}

// OverflowTargetByName is OverflowTargetOf for a caller holding the group's name.
func OverflowTargetByName(q helpers.QueryRower, groupName string) (OverflowTarget, error) {
	return scanOverflowTarget(q.QueryRow(overflowTargetSQL+` WHERE g.name=$1`, groupName), groupName)
}

func scanOverflowTarget(row *sql.Row, group any) (OverflowTarget, error) {
	var t OverflowTarget
	err := row.Scan(&t.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return OverflowTarget{}, nil
	}
	if err != nil {
		return OverflowTarget{}, fmt.Errorf("read overflow of %v: %w", group, err)
	}
	return t, nil
}

// OverflowReader is what OverflowGroupOf reads: where the group's overflow points
// and the node there.
type OverflowReader interface {
	OverflowTargetOf(groupNodeID int64) (OverflowTarget, error)
	GetNode(id int64) (*Node, error)
	GetNodeByDotName(name string) (*Node, error)
}

// OverflowGroupOf is a group's configured overflow destination: the node it
// names, or nil. With nil, why is "" when none is configured (a real answer, not
// a missing one) and a sentence when one is configured and unusable.
//
// no type stated, its own row's. A plain group's is the name in its
// overflow_destination property, which is why this can still fail to resolve
// (OverflowTargetOf; the pin of the plain arm is
// TestOverflow_APlainGroupKeepsItsProperty).
//
// THE LOOKUP HALF OF plan.OverflowDestination. The cancel-return policy
// (engine/cancel_return.go) follows the same overflow for a bin it is returning
// to a group at its level, and two lookups would be two answers the first time
// either grows a rule.
func OverflowGroupOf(db OverflowReader, group *Node) (*Node, string) {
	target, err := db.OverflowTargetOf(group.ID)
	if err != nil {
		return nil, fmt.Sprintf("overflow of %s could not be read (%v)", group.Name, err)
	}
	if target.Name == "" {
		return nil, ""
	}
	dest, err := db.GetNodeByDotName(target.Name)
	if err != nil || dest == nil {
		return nil, fmt.Sprintf("overflow %q of %s does not resolve (%v)", target.Name, group.Name, err)
	}
	if dest.ID == group.ID {
		// A group naming itself is the one-hop rule's degenerate case, and it is
		// worth refusing explicitly: without this it would re-resolve the same
		// full group and return the same capacity error, which reads as a
		// mysterious no-op rather than a configuration mistake.
		return nil, fmt.Sprintf("overflow of %s names itself", group.Name)
	}
	return dest, ""
}

// ResolveDotNameID is GetByDotName's resolution of a name to a node id, on any
// querier, for the writers that hold a transaction: false when nothing answers.
// Pinned to GetByDotName by TestGetByDotNames_AgreesWithGetByDotName.
func ResolveDotNameID(q helpers.QueryRower, name string) (int64, bool, error) {
	var row *sql.Row
	if parts := strings.SplitN(name, ".", 2); len(parts) == 2 {
		row = q.QueryRow(`SELECT n.id FROM nodes n
			WHERE n.name=$1 AND n.parent_id IN (SELECT id FROM nodes WHERE name=$2)`, parts[1], parts[0])
	} else {
		row = q.QueryRow(`SELECT id FROM nodes WHERE name=$1`, name)
	}
	var id int64
	err := row.Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("resolve node %q: %w", name, err)
	}
	return id, true, nil
}

// SetKanbanLevel seeds one level, its row made if absent, with no audit: a
// fixture's write. Production saves go through store.SaveKanban.
func SetKanbanLevel(db *sql.DB, l KanbanLevel) error {
	return inTx(db, func(tx *sql.Tx) error {
		if err := EnsureKanban(tx, l.GroupNodeID); err != nil {
			return err
		}
		return UpsertLevel(tx, l)
	})
}

// inTx runs fn in one transaction.
func inTx(db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// KanbanLayerWant is how many carts of one level's type, standing a given
// number of layers high, a kanban wants: one row of
// kanban_layer_levels.
//
// A HEIGHT OF 1 OR MORE IS THE STACK LEVEL SAID BY HEIGHT: the cap is the
// type's KanbanLevel, those wants are not summed against it, and nothing
// refuses a push or mints an ask on them. They say which heights the group is
// short of, so a person at an unloader can be told what to send a cart back as.
//
// THE ROW AT 0 LAYERS IS TOPS & BOTTOMS, AND IT IS A CAP (v164): its own line
// beside the stack level. The store caps judge a cart at 0 against its want
// (TopsAndBottomsWant, kanban.AtLevel), a group that keeps the
// type with no such row refuses one, and the save counts it against the
// group's positions. The keeper never fetches tops & bottoms: it is made at
// the unloader.
//
// A layer want belongs to its level: it is written with the level's save and
// removed with the level (SetKanbanLevelAndLayerWants, RemoveKanbanLevel).
// TotesPerLayer is the type's bin_types.totes_per_layer, joined on read; nil
// when the type does not state one, and then no cart can be counted at a
// height.
type KanbanLayerWant struct {
	GroupNodeID   int64  `json:"group_node_id"`
	BinTypeID     int64  `json:"bin_type_id"`
	BinTypeCode   string `json:"bin_type_code"`
	Layers        int    `json:"layers"`
	Want          int    `json:"want"`
	TotesPerLayer *int   `json:"totes_per_layer,omitempty"`
}

// SetKanbanLevelAndLayerWants seeds a level and that type's layer wants in one
// transaction, its row made if absent, with no audit: a fixture's write.
// Production saves go through store.SaveKanban. The rows' GroupNodeID and
// BinTypeID are the level's; only Layers and Want are read.
func SetKanbanLevelAndLayerWants(db *sql.DB, l KanbanLevel, wants []KanbanLayerWant) error {
	return inTx(db, func(tx *sql.Tx) error {
		if err := EnsureKanban(tx, l.GroupNodeID); err != nil {
			return err
		}
		if err := UpsertLevel(tx, l); err != nil {
			return err
		}
		return ReplaceLayerWants(tx, l.GroupNodeID, l.BinTypeID, wants)
	})
}

// RemoveKanbanLevel removes a seeded level and its layer wants with no audit:
// a fixture's write (DeleteLevel). Production saves go through store.SaveKanban.
func RemoveKanbanLevel(db *sql.DB, groupNodeID, binTypeID int64) error {
	return inTx(db, func(tx *sql.Tx) error { return DeleteLevel(tx, groupNodeID, binTypeID) })
}

// ListKanbanLayerWants returns a group's layer wants, every type's, in the
// levels' order (by code, as ListKanbanLevels) and then by height, with the
// code and the type's totes per layer joined on.
func ListKanbanLayerWants(db helpers.RowsQuerier, groupNodeID int64) ([]KanbanLayerWant, error) {
	rows, err := db.Query(`
		SELECT w.kanban_node_id, w.bin_type_id, bt.code, w.layers, w.want, bt.totes_per_layer
		FROM kanban_layer_levels w
		JOIN bin_types bt ON bt.id = w.bin_type_id
		WHERE w.kanban_node_id=$1
		ORDER BY bt.code, w.layers`, groupNodeID)
	if err != nil {
		return nil, fmt.Errorf("list layer wants group=%d: %w", groupNodeID, err)
	}
	defer rows.Close()
	var out []KanbanLayerWant
	for rows.Next() {
		var w KanbanLayerWant
		if err := rows.Scan(&w.GroupNodeID, &w.BinTypeID, &w.BinTypeCode, &w.Layers, &w.Want, &w.TotesPerLayer); err != nil {
			return nil, fmt.Errorf("scan layer want: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// TopsAndBottomsWant is the want of the tops & bottoms row (layers 0) of one
// level's type in a group, and false when the group has no such row: the cap
// the tops & bottoms line is judged against (kanban.AtLevel). One
// statement, asked only for a cart at 0.
func TopsAndBottomsWant(db *sql.DB, groupNodeID, binTypeID int64) (int, bool, error) {
	var want int
	err := db.QueryRow(`SELECT want FROM kanban_layer_levels
		WHERE kanban_node_id=$1 AND bin_type_id=$2 AND layers=0`, groupNodeID, binTypeID).Scan(&want)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("tops & bottoms row group=%d/bin_type=%d: %w", groupNodeID, binTypeID, err)
	}
	return want, true, nil
}

// TypeInPlay is a bin type an unloader's group line reads (ListTypesInPlay):
// the type's dimensions and totes per layer, and the groups' layer wants of it
// (by group and height, none when they want none).
type TypeInPlay struct {
	Type       domain.BinType
	LayerWants []KanbanLayerWant
}

// ListTypesInPlay reads, in one statement, the bin types named by codes (the
// groups' level types and the types of the carts at their unloaders' windows)
// with their family and totes per layer, and the named groups' layer wants of each, by code,
// then group, then height. It is a snapshot build's one types read, in place of
// ListKanbanLayerWants there: the families ride the read the heights already made.
func ListTypesInPlay(db *sql.DB, groupNodeIDs []int64, codes []string) ([]TypeInPlay, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	rows, err := db.Query(`
		SELECT bt.id, bt.code, bt.family_id, bt.totes_per_layer, w.kanban_node_id, w.layers, w.want
		  FROM bin_types bt
		  LEFT JOIN kanban_layer_levels w ON w.bin_type_id = bt.id AND w.kanban_node_id = ANY($1::bigint[])
		 WHERE bt.code = ANY($2::text[])
		 ORDER BY bt.code, w.kanban_node_id, w.layers`, groupNodeIDs, codes)
	if err != nil {
		return nil, fmt.Errorf("list types in play groups=%v: %w", groupNodeIDs, err)
	}
	defer rows.Close()
	var out []TypeInPlay
	for rows.Next() {
		var (
			bt                  domain.BinType
			group, layers, want sql.NullInt64
		)
		if err := rows.Scan(&bt.ID, &bt.Code, &bt.FamilyID, &bt.TotesPerLayer, &group, &layers, &want); err != nil {
			return nil, fmt.Errorf("scan type in play: %w", err)
		}
		if len(out) == 0 || out[len(out)-1].Type.ID != bt.ID {
			out = append(out, TypeInPlay{Type: bt})
		}
		if layers.Valid {
			t := &out[len(out)-1]
			t.LayerWants = append(t.LayerWants, KanbanLayerWant{GroupNodeID: group.Int64, BinTypeID: bt.ID,
				BinTypeCode: bt.Code, Layers: int(layers.Int64), Want: int(want.Int64), TotesPerLayer: bt.TotesPerLayer})
		}
	}
	return out, rows.Err()
}

// ListKanbanLevels returns a group's DECLARED level with the bin-type CODES
// joined on, because the code is what a person reads and what every other
// carrier-typed surface in the system prints — the id is a local key. Same
// reason loaders.ListQuotas carries one. A reader holding the plant to a level
// asks store.DB.ListKanbanLevels, which answers the effective want; this one
// is the configuration's.
func ListKanbanLevels(db helpers.RowsQuerier, groupNodeID int64) ([]KanbanLevel, error) {
	rows, err := db.Query(`
		SELECT l.kanban_node_id, l.bin_type_id, bt.code, l.want
		FROM kanban_levels l
		JOIN bin_types bt ON bt.id = l.bin_type_id
		WHERE l.kanban_node_id=$1
		ORDER BY bt.code`, groupNodeID)
	if err != nil {
		return nil, fmt.Errorf("list kanban levels group=%d: %w", groupNodeID, err)
	}
	defer rows.Close()
	var out []KanbanLevel
	for rows.Next() {
		var l KanbanLevel
		if err := rows.Scan(&l.GroupNodeID, &l.BinTypeID, &l.BinTypeCode, &l.Want); err != nil {
			return nil, fmt.Errorf("scan kanban level: %w", err)
		}
		l.Base = l.Want
		out = append(out, l)
	}
	return out, rows.Err()
}

// DeclaresLevel reports whether a group declares a level, in one statement
// (helpers.DeclaresLevelSQL): whether ListKanbanLevels would answer non-empty.
// It is the one test of a kanban inbound source: the loader save refuses
// an unloader whose source is one (service ErrFullsFromAKanban).
func DeclaresLevel(db *sql.DB, groupNodeID int64) (bool, error) {
	var declares bool
	if err := db.QueryRow(`SELECT `+helpers.DeclaresLevelSQL("$1"), groupNodeID).Scan(&declares); err != nil {
		return false, fmt.Errorf("read whether group %d declares a level: %w", groupNodeID, err)
	}
	return declares, nil
}

// KanbanIDs is the set of groups that declare a level — the groups
// for which ListKanbanLevels answers non-empty, read for every group in one
// statement. The node list marks them for the Edge (protocol.NodeInfo's
// Kanban), which is why this is one read per list and not one per node.
// The join is ListKanbanLevels' own, so the two cannot disagree about a row.
func KanbanIDs(db *sql.DB) (map[int64]bool, error) {
	rows, err := db.Query(`
		SELECT DISTINCT l.kanban_node_id
		FROM kanban_levels l
		JOIN bin_types bt ON bt.id = l.bin_type_id`)
	if err != nil {
		return nil, fmt.Errorf("list kanbans: %w", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan kanban: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

// SetKanbanSupports seeds the whole supported-process-node set in one
// transaction, the row made if absent, with no audit: a fixture's write.
// Production saves go through store.SaveKanban.
//
// REPLACE rather than add/remove, mirroring bins.SetNodeTypes: the editor holds
// the whole set on screen and sends the whole set, so a partial-update API would
// only invent a way for the screen and the table to disagree. An empty set is
// legal and means the group supports nobody, which is what a group in the middle
// of being configured looks like.
func SetKanbanSupports(db *sql.DB, groupNodeID int64, processNodeIDs []int64) error {
	return inTx(db, func(tx *sql.Tx) error {
		if err := EnsureKanban(tx, groupNodeID); err != nil {
			return err
		}
		return ReplaceSupports(tx, groupNodeID, processNodeIDs)
	})
}

// ListKanbanSupports returns the process nodes a group serves, names joined
// on for the same reason the levels carry codes.
func ListKanbanSupports(db helpers.RowsQuerier, groupNodeID int64) ([]KanbanSupport, error) {
	rows, err := db.Query(`
		SELECT s.kanban_node_id, s.process_node_id, n.name
		FROM kanban_supports s
		JOIN nodes n ON n.id = s.process_node_id
		WHERE s.kanban_node_id=$1
		ORDER BY n.name`, groupNodeID)
	if err != nil {
		return nil, fmt.Errorf("list kanban supports group=%d: %w", groupNodeID, err)
	}
	defer rows.Close()
	var out []KanbanSupport
	for rows.Next() {
		var s KanbanSupport
		if err := rows.Scan(&s.GroupNodeID, &s.ProcessNodeID, &s.ProcessNodeName); err != nil {
			return nil, fmt.Errorf("scan kanban support: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GroupFencesAsker reports whether a STRICT kanban excludes an asker
// that has named it explicitly.
//
// bins.FencedNodesCTE's FIRST RULE as an answer, and it exists because the two
// questions are asked at different moments. The CTE answers "which carriers
// can this plant-wide scan see", inside the query, silently. This answers "may
// this need, which NAMED this group, be served from it" — and that one needs a
// DISPOSITION, not a filter: a need turned away here parks with a cause an
// operator can read, rather than quietly finding nothing.
//
// One predicate, helpers.StrictFencesSQL, in the two places it is asked. A blank
// processNode is an outsider — an ask that names no process cannot be supported
// anywhere, and the safe reading of that is "not for you".
func GroupFencesAsker(db *sql.DB, groupNodeID int64, processNode string) (bool, error) {
	var fenced bool
	err := db.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM kanbans k
			 WHERE k.node_id = $1 AND `+helpers.StrictFencesSQL("$2")+`
		)`, groupNodeID, processNode).Scan(&fenced)
	if err != nil {
		return false, fmt.Errorf("group %d fences %q: %w", groupNodeID, processNode, err)
	}
	return fenced, nil
}

// KanbansSupporting returns the kanbans that serve a process
// node, whether or not they are strict.
func KanbansSupporting(db *sql.DB, processNode string) ([]int64, error) {
	if processNode == "" {
		return nil, nil
	}
	rows, err := db.Query(`
		SELECT s.kanban_node_id
		  FROM kanban_supports s
		  JOIN nodes pn ON pn.id = s.process_node_id
		 WHERE pn.name = $1`, processNode)
	if err != nil {
		return nil, fmt.Errorf("kanbans supporting %q: %w", processNode, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan kanban: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// KanbansSupportingProcess returns the kanbans with a supported position a process's
// claims name (core, paired or second paired), by the same match as the picture's
// connected set. Each once, by id.
func KanbansSupportingProcess(db *sql.DB, processID string) ([]int64, error) {
	rows, err := db.Query(`
		SELECT DISTINCT s.kanban_node_id
		  FROM kanban_supports s
		  JOIN nodes pn ON pn.id = s.process_node_id
		  JOIN style_claims sc ON sc.process_id = $1
		   AND pn.name IN (sc.core_node_name, sc.paired_core_node, sc.second_paired_core_node)
		 ORDER BY s.kanban_node_id`, processID)
	if err != nil {
		return nil, fmt.Errorf("kanbans supporting process %q: %w", processID, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan kanban: %w", err)
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
