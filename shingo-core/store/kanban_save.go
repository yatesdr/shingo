package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"shingocore/store/audit"
	"shingocore/store/nodes"
)

// kanban_save.go — the one write of a kanban's configuration.

// KanbanChange is what one save changes. A nil field leaves what is stored.
type KanbanChange struct {
	NodeID int64
	// EnsureRow makes the group's kanbans row if it has none. With no row and no
	// EnsureRow the group stays plain, and only PlainOverflow is read.
	EnsureRow bool
	Station   *string
	Strict    *bool
	// PlainOverflow is a plain group's overflow_destination property.
	PlainOverflow *string
	Levels        []KanbanLevelChange
	RemoveLevels  []int64
	// Pools sets a family's pool, by family id.
	Pools    map[int64]int
	Supports *[]int64
	Sources  *[]int64
}

// KanbanLevelChange is what one save changes of one level. A level that does not
// exist yet needs a Want.
type KanbanLevelChange struct {
	BinTypeID  int64
	Want       *int
	LayerWants *[]KanbanLayerWant
}

// SaveKanban applies one change in one transaction and appends one audit row
// per changed field or list entry, naming actor. The group's row locks against a
// second save, so each audit row reads what it replaced.
//
// A group's first row ends its plain overflow_destination property: a kanban has no overflow,
// and the audit row says it went.
//
// It enforces no save rules: those read the plant around the kanban and live in
// service.ValidateKanban, which runs before this.
func (db *DB) SaveKanban(c KanbanChange, actor string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("save kanban %d: begin: %w", c.NodeID, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	s := kanbanSave{tx: tx, id: c.NodeID, actor: actor}
	if err := s.apply(c); err != nil {
		return fmt.Errorf("save kanban %d: %w", c.NodeID, err)
	}
	return tx.Commit()
}

// kanbanSave is one SaveKanban's transaction and the rows' audit identity.
type kanbanSave struct {
	tx    *sql.Tx
	id    int64
	actor string
}

func (s kanbanSave) audit(action, before, after string) error {
	if before == after {
		return nil
	}
	return audit.Append(s.tx, "node", s.id, action, before, after, s.actor)
}

func (s kanbanSave) apply(c KanbanChange) error {
	var locked int64
	if err := s.tx.QueryRow(`SELECT id FROM nodes WHERE id = $1 FOR NO KEY UPDATE`, s.id).Scan(&locked); err != nil {
		return fmt.Errorf("lock node: %w", err)
	}
	k, err := nodes.GetKanban(s.tx, s.id)
	if err != nil {
		return err
	}
	if k == nil {
		if !c.EnsureRow {
			return s.applyPlainOverflow(c.PlainOverflow)
		}
		if k, err = s.becomeKanban(); err != nil {
			return err
		}
	}
	if err := s.applyScalars(*k, c); err != nil {
		return err
	}
	if err := s.applyLevels(c); err != nil {
		return err
	}
	if c.Supports != nil {
		if err := s.applySupports(*c.Supports); err != nil {
			return err
		}
	}
	if err := s.applyPools(c.Pools); err != nil {
		return err
	}
	if c.Sources != nil {
		return s.applySources(*c.Sources)
	}
	return nil
}

func (s kanbanSave) readPlainOverflow() (string, error) {
	var v string
	err := s.tx.QueryRow(`SELECT value FROM node_properties WHERE node_id=$1 AND key=$2`,
		s.id, nodes.PropOverflowDestination).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// applyPlainOverflow writes a plain group's overflow property, with the audit row
// every property write leaves (action property:<key>).
func (s kanbanSave) applyPlainOverflow(next *string) error {
	if next == nil {
		return nil
	}
	before, err := s.readPlainOverflow()
	if err != nil {
		return fmt.Errorf("read overflow property: %w", err)
	}
	if before == *next {
		return nil
	}
	if _, err := s.tx.Exec(`INSERT INTO node_properties (node_id, key, value) VALUES ($1, $2, $3)
		ON CONFLICT (node_id, key) DO UPDATE SET value=EXCLUDED.value, updated_at=NOW()`,
		s.id, nodes.PropOverflowDestination, *next); err != nil {
		return fmt.Errorf("write overflow property: %w", err)
	}
	return s.audit("property:"+nodes.PropOverflowDestination, before, *next)
}

// becomeKanban makes the row and ends the group's overflow property: a kanban has none.
func (s kanbanSave) becomeKanban() (*nodes.Kanban, error) {
	if err := nodes.EnsureKanban(s.tx, s.id); err != nil {
		return nil, err
	}
	name, err := s.readPlainOverflow()
	if err != nil {
		return nil, fmt.Errorf("read overflow property: %w", err)
	}
	if name != "" {
		if _, err := s.tx.Exec(`DELETE FROM node_properties WHERE node_id=$1 AND key=$2`,
			s.id, nodes.PropOverflowDestination); err != nil {
			return nil, fmt.Errorf("delete overflow property: %w", err)
		}
		if err := s.audit("property:"+nodes.PropOverflowDestination, name, ""); err != nil {
			return nil, err
		}
	}
	return nodes.GetKanban(s.tx, s.id)
}

// applyPools writes each family's pool, and audits the ones that changed.
func (s kanbanSave) applyPools(pools map[int64]int) error {
	for familyID, pool := range pools {
		var code string
		var before sql.NullInt64
		if err := s.tx.QueryRow(`SELECT f.code, kf.pool FROM carrier_families f
			LEFT JOIN kanban_families kf ON kf.family_id = f.id AND kf.kanban_node_id = $1
			WHERE f.id = $2`, s.id, familyID).Scan(&code, &before); err != nil {
			return fmt.Errorf("read pool of family %d: %w", familyID, err)
		}
		if _, err := s.tx.Exec(`INSERT INTO kanban_families (kanban_node_id, family_id, pool) VALUES ($1,$2,$3)
			ON CONFLICT (kanban_node_id, family_id) DO UPDATE SET pool = EXCLUDED.pool, updated_at = NOW()`, s.id, familyID, pool); err != nil {
			return fmt.Errorf("write pool of family %d: %w", familyID, err)
		}
		was := ""
		if before.Valid {
			was = strconv.FormatInt(before.Int64, 10)
		}
		if err := s.audit("kanban:pool:"+code, was, strconv.Itoa(pool)); err != nil {
			return err
		}
	}
	return nil
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// nodeName reads a node's name for an audit row; blank for 0.
func (s kanbanSave) nodeName(id int64) (string, error) {
	if id == 0 {
		return "", nil
	}
	var name string
	if err := s.tx.QueryRow(`SELECT name FROM nodes WHERE id=$1`, id).Scan(&name); err != nil {
		return "", fmt.Errorf("read node %d: %w", id, err)
	}
	return name, nil
}

// applyScalars writes the row's settings and audits each one that changed. A
// save that changes none of them writes nothing.
func (s kanbanSave) applyScalars(k nodes.Kanban, c KanbanChange) error {
	next := k
	if c.Station != nil {
		next.StationID = strings.TrimSpace(*c.Station)
	}
	if c.Strict != nil {
		next.Strict = *c.Strict
	}
	if next.StationID == k.StationID && next.Strict == k.Strict {
		return nil
	}
	if err := nodes.SetKanbanScalars(s.tx, next); err != nil {
		return err
	}
	for _, r := range []struct{ action, before, after string }{
		{"kanban:station", k.StationID, next.StationID},
		{"kanban:strict", onOff(k.Strict), onOff(next.Strict)},
	} {
		if err := s.audit(r.action, r.before, r.after); err != nil {
			return err
		}
	}
	return nil
}

// applyLevels applies the level changes against what the picture says stands.
func (s kanbanSave) applyLevels(c KanbanChange) error {
	if len(c.Levels) == 0 && len(c.RemoveLevels) == 0 {
		return nil
	}
	picture, err := listKanbanPicture(s.tx, []int64{s.id})
	if err != nil {
		return err
	}
	before := make(map[int64]KanbanPictureRow, len(picture))
	for _, r := range picture {
		before[r.BinTypeID] = r
	}
	wants, err := nodes.ListKanbanLayerWants(s.tx, s.id)
	if err != nil {
		return err
	}
	for _, id := range c.RemoveLevels {
		row, ok := before[id]
		if !ok {
			continue
		}
		if err := nodes.DeleteLevel(s.tx, s.id, id); err != nil {
			return err
		}
		if err := s.audit("kanban:level:"+row.BinTypeCode, strconv.Itoa(row.Base), ""); err != nil {
			return err
		}
		if err := s.auditLayerWants(row.BinTypeCode, layerWantsOf(wants, id), nil); err != nil {
			return err
		}
	}
	for _, lc := range c.Levels {
		if err := s.applyLevel(lc, before, wants); err != nil {
			return err
		}
	}
	return nil
}

func (s kanbanSave) applyLevel(lc KanbanLevelChange, before map[int64]KanbanPictureRow, wants []KanbanLayerWant) error {
	row, had := before[lc.BinTypeID]
	code := row.BinTypeCode
	if !had {
		if lc.Want == nil {
			return fmt.Errorf("carrier type %d has no level to change", lc.BinTypeID)
		}
		if err := s.tx.QueryRow(`SELECT code FROM bin_types WHERE id=$1`, lc.BinTypeID).Scan(&code); err != nil {
			return fmt.Errorf("read carrier type %d: %w", lc.BinTypeID, err)
		}
	}
	if lc.Want != nil && (!had || *lc.Want != row.Base) {
		if err := nodes.UpsertLevel(s.tx, KanbanLevel{GroupNodeID: s.id, BinTypeID: lc.BinTypeID, Want: *lc.Want}); err != nil {
			return err
		}
		was := ""
		if had {
			was = strconv.Itoa(row.Base)
		}
		if err := s.audit("kanban:level:"+code, was, strconv.Itoa(*lc.Want)); err != nil {
			return err
		}
	}
	if lc.LayerWants != nil {
		old := layerWantsOf(wants, lc.BinTypeID)
		if err := nodes.ReplaceLayerWants(s.tx, s.id, lc.BinTypeID, *lc.LayerWants); err != nil {
			return err
		}
		if err := s.auditLayerWants(code, old, *lc.LayerWants); err != nil {
			return err
		}
	}
	return nil
}

func layerWantsOf(all []KanbanLayerWant, binTypeID int64) []KanbanLayerWant {
	var out []KanbanLayerWant
	for _, w := range all {
		if w.BinTypeID == binTypeID {
			out = append(out, w)
		}
	}
	return out
}

// auditLayerWants appends one row per height added, changed or removed.
func (s kanbanSave) auditLayerWants(code string, before, after []KanbanLayerWant) error {
	was := map[int]int{}
	for _, w := range before {
		was[w.Layers] = w.Want
	}
	now := map[int]int{}
	for _, w := range after {
		now[w.Layers] = w.Want
	}
	heights := make([]int, 0, len(was)+len(now))
	for h := range was {
		heights = append(heights, h)
	}
	for h := range now {
		if _, ok := was[h]; !ok {
			heights = append(heights, h)
		}
	}
	sort.Ints(heights)
	for _, h := range heights {
		oldV, newV := "", ""
		if v, ok := was[h]; ok {
			oldV = strconv.Itoa(v)
		}
		if v, ok := now[h]; ok {
			newV = strconv.Itoa(v)
		}
		if err := s.audit(fmt.Sprintf("kanban:layer_want:%s:%d", code, h), oldV, newV); err != nil {
			return err
		}
	}
	return nil
}

// applySupports replaces the set, one audit row per node added or removed.
func (s kanbanSave) applySupports(next []int64) error {
	before, err := nodes.ListKanbanSupports(s.tx, s.id)
	if err != nil {
		return err
	}
	had := make(map[int64]bool, len(before))
	for _, b := range before {
		had[b.ProcessNodeID] = true
	}
	keep := make(map[int64]bool, len(next))
	for _, id := range next {
		keep[id] = true
	}
	if err := nodes.ReplaceSupports(s.tx, s.id, next); err != nil {
		return err
	}
	for _, b := range before {
		if !keep[b.ProcessNodeID] {
			if err := s.audit("kanban:support", b.ProcessNodeName, ""); err != nil {
				return err
			}
		}
	}
	seen := make(map[int64]bool, len(next))
	for _, id := range next {
		if had[id] || seen[id] {
			continue
		}
		seen[id] = true
		name, err := s.nodeName(id)
		if err != nil {
			return err
		}
		if err := s.audit("kanban:support", "", name); err != nil {
			return err
		}
	}
	return nil
}

// applySources replaces the list, one audit row per source added, removed or
// moved; old and new are its rank.
func (s kanbanSave) applySources(next []int64) error {
	before, err := nodes.ListKanbanSources(s.tx, s.id)
	if err != nil {
		return err
	}
	if err := nodes.ReplaceSources(s.tx, s.id, next); err != nil {
		return err
	}
	rank := make(map[int64]int, len(next))
	for i, id := range next {
		rank[id] = i + 1
	}
	had := make(map[int64]bool, len(before))
	for _, b := range before {
		had[b.SourceNodeID] = true
		now := ""
		if r, ok := rank[b.SourceNodeID]; ok {
			now = strconv.Itoa(r)
		}
		if err := s.audit("kanban:source:"+b.SourceNodeName, strconv.Itoa(b.Rank), now); err != nil {
			return err
		}
	}
	for _, id := range next {
		if had[id] {
			continue
		}
		name, err := s.nodeName(id)
		if err != nil {
			return err
		}
		if err := s.audit("kanban:source:"+name, "", strconv.Itoa(rank[id])); err != nil {
			return err
		}
	}
	return nil
}
