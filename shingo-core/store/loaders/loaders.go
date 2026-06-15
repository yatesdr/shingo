// Package loaders holds the Core-owned bin-loader aggregate (loader refactor
// cutover): bin_loaders + bin_loader_homes + bin_loader_payloads. The loader's
// identity and per-position/per-payload replenishment config live here, on
// Core, replacing the Edge style_node_claims encoding.
//
// Free functions over *sql.DB mirror the store/demands sub-package; the outer
// store/ keeps one-line delegate methods on *store.DB (store/loaders.go).
//
// These reads/writes back the Core-owned loader read path: the aggregate the
// Edge syncs into its core_loaders cache and resolves loaders from.
package loaders

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Key mints the opaque wire/identity token for a loader from its surrogate id:
// "loader:<id>". The constant prefix keeps it obviously-a-loader and greppable and
// guarantees it never collides with a node name (the identity-as-node trap). There is
// NO plant/factory prefix — cross-plant federation is not a goal, so no per-plant
// config value is needed (decision §0-10). Core is the sole minter; Edge propagates
// the token off the wire (LoaderInfo.LoaderKey) and never re-derives it.
func Key(id int64) string { return "loader:" + strconv.FormatInt(id, 10) }

// Enum values mirror the v34 CHECK constraints. New names per the refactor
// (D4): layout = shared_window | dedicated_positions; replenishment = auto |
// operator. role stays produce | consume.
const (
	RoleProduce = "produce"
	RoleConsume = "consume"

	LayoutSharedWindow       = "shared_window"
	LayoutDedicatedPositions = "dedicated_positions"

	ReplenishmentAuto     = "auto"
	ReplenishmentOperator = "operator"
)

// Loader is the aggregate root: a bin loader (produce) or unloader (consume)
// anchored at a core node. config_gen is bumped on every config-affecting write
// and rides the downward sync so Edge can detect stale config.
type Loader struct {
	ID            int64      `json:"id"`
	Name          string     `json:"name"`
	Role          string     `json:"role"`
	Layout        string     `json:"layout"`
	Replenishment string     `json:"replenishment"`
	OutboundDest  string     `json:"outbound_dest"`
	InboundSource string     `json:"inbound_source"`
	BufferDest    string     `json:"buffer_dest"`
	ConfigGen     int64      `json:"config_gen"`
	ArchivedAt    *time.Time `json:"archived_at,omitempty"` // soft-delete marker; nil = active (step 7)
}

// Home is one dedicated position: exactly one payload. The global
// UNIQUE(position_node_id) makes "one payload per position, one loader per node"
// unrepresentable-otherwise — the structural fix for the SLN_002 incident.
type Home struct {
	LoaderID       int64  `json:"loader_id"`
	PositionNodeID int64  `json:"position_node_id"`
	PayloadCode    string `json:"payload_code"`
	MinStock       int    `json:"min_stock"`
	UOPThreshold   int    `json:"uop_threshold"`
	SortOrder      int    `json:"sort_order"`
}

// Payload is one entry in a shared_window loader's allowed set.
type Payload struct {
	LoaderID     int64  `json:"loader_id"`
	PayloadCode  string `json:"payload_code"`
	MinStock     int    `json:"min_stock"`
	UOPThreshold int    `json:"uop_threshold"`
}

// Config bundles a loader with its positions (dedicated_positions) and/or
// payload set (shared_window) — the assembled read the downward sync and the
// future runtime LoaderStore consume.
type Config struct {
	Loader   Loader    `json:"loader"`
	Homes    []Home    `json:"homes"`
	Payloads []Payload `json:"payloads"`
}

const loaderCols = `id, name, role, layout, replenishment, outbound_dest, inbound_source, buffer_dest, config_gen, archived_at`

type scanner interface{ Scan(...any) error }

func scanLoader(s scanner) (Loader, error) {
	var l Loader
	var archivedAt sql.NullTime
	err := s.Scan(&l.ID, &l.Name, &l.Role, &l.Layout, &l.Replenishment,
		&l.OutboundDest, &l.InboundSource, &l.BufferDest, &l.ConfigGen, &archivedAt)
	if archivedAt.Valid {
		l.ArchivedAt = &archivedAt.Time
	}
	return l, err
}

// CreateLoader inserts a loader and returns its id. The surrogate id IS the loader's
// identity (minted onto the wire as the loader_key token); role + layout are fixed
// after creation.
func CreateLoader(db *sql.DB, l Loader) (int64, error) {
	var id int64
	err := db.QueryRow(`
		INSERT INTO bin_loaders (name, role, layout, replenishment, outbound_dest, inbound_source, buffer_dest)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		l.Name, l.Role, l.Layout, l.Replenishment, l.OutboundDest, l.InboundSource, l.BufferDest,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create loader %q: %w", l.Name, err)
	}
	return id, nil
}

// GetLoader returns the loader by id, or (nil, nil) if absent.
func GetLoader(db *sql.DB, id int64) (*Loader, error) {
	l, err := scanLoader(db.QueryRow(`SELECT `+loaderCols+` FROM bin_loaders WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get loader %d: %w", id, err)
	}
	return &l, nil
}

// GetLoaderByName returns the loader named (name, role), or (nil, nil) if absent.
// (name, role) is the seed / migrateloaders idempotency key now that identity is the
// surrogate id: a re-run finds the existing loader by its stable operator-facing name
// rather than the dropped core_node_name.
func GetLoaderByName(db *sql.DB, name, role string) (*Loader, error) {
	l, err := scanLoader(db.QueryRow(`SELECT `+loaderCols+` FROM bin_loaders WHERE name=$1 AND role=$2`, name, role))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get loader by name %s/%s: %w", name, role, err)
	}
	return &l, nil
}

// ListLoaders returns every ACTIVE loader (archived_at IS NULL), ordered by name. This
// is the config enumeration the downward sync (BuildLoaderInfos) and demand derivation
// (BuildDemandRegistryFromAggregate) consume, so a soft-deleted loader stops driving the
// plant. Analytics that must include retired loaders read bin_uop_audit (the stamped
// loader_id survives), not this.
func ListLoaders(db *sql.DB) ([]Loader, error) {
	rows, err := db.Query(`SELECT ` + loaderCols + ` FROM bin_loaders WHERE archived_at IS NULL ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list loaders: %w", err)
	}
	defer rows.Close()
	var out []Loader
	for rows.Next() {
		l, err := scanLoader(rows)
		if err != nil {
			return nil, fmt.Errorf("scan loader: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// UpdateLoader updates the editable fields and bumps config_gen. The surrogate id and
// role are the fixed identity and are not updated here.
func UpdateLoader(db *sql.DB, l Loader) error {
	res, err := db.Exec(`
		UPDATE bin_loaders SET name=$1, layout=$2, replenishment=$3,
			outbound_dest=$4, inbound_source=$5, buffer_dest=$6,
			config_gen=config_gen+1, updated_at=NOW()
		WHERE id=$7`,
		l.Name, l.Layout, l.Replenishment, l.OutboundDest, l.InboundSource, l.BufferDest, l.ID)
	if err != nil {
		return fmt.Errorf("update loader %d: %w", l.ID, err)
	}
	return requireOne(res, "update loader", l.ID)
}

// DeleteLoader SOFT-deletes a loader: it sets archived_at instead of removing the row,
// so the stamped bin_uop_audit history (loader_id is non-cascading) survives a retired
// loader — the whole reason the cascade was removed (6 reviewers flagged it). The
// homes/payloads rows are left intact (a hard DELETE would have cascaded them away).
// Active reads (ListLoaders) filter on archived_at IS NULL, so an archived loader stops
// syncing to Edge and driving demand; analytics read bin_uop_audit, which is preserved.
// Idempotent: re-archiving just re-stamps archived_at. config_gen bumps so the next
// downward sync drops the loader from the Edge cache.
//
// A soft-retire + recreate now lands cleanly: identity is the surrogate id, so a
// replacement loader gets a fresh id and never collides with the archived one (the
// old UNIQUE(core_node_name, role) that used to block this is gone).
func DeleteLoader(db *sql.DB, id int64) error {
	res, err := db.Exec(`UPDATE bin_loaders SET archived_at=NOW(), config_gen=config_gen+1, updated_at=NOW() WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("archive loader %d: %w", id, err)
	}
	return requireOne(res, "archive loader", id)
}

// UpsertHome assigns (or replaces) the payload at a dedicated position. The
// global UNIQUE(position_node_id) means a position belongs to exactly one loader
// and carries one payload; ON CONFLICT moves/relabels it. Bumps config_gen.
//
// Same payload on a second position is allowed (D1) — there is deliberately no
// UNIQUE(loader_id, payload_code) — two homes for a high-runner is legitimate.
func UpsertHome(db *sql.DB, h Home) error {
	// sort_order is set on INSERT (append position) but deliberately NOT in the
	// ON CONFLICT SET — re-assigning a position's payload must preserve its place
	// in the order; only SetHomeOrder rewrites it.
	_, err := db.Exec(`
		INSERT INTO bin_loader_homes (loader_id, position_node_id, payload_code, min_stock, uop_threshold, sort_order)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (position_node_id) DO UPDATE SET
			loader_id=EXCLUDED.loader_id, payload_code=EXCLUDED.payload_code,
			min_stock=EXCLUDED.min_stock, uop_threshold=EXCLUDED.uop_threshold`,
		h.LoaderID, h.PositionNodeID, h.PayloadCode, h.MinStock, h.UOPThreshold, h.SortOrder)
	if err != nil {
		return fmt.Errorf("upsert home pos=%d: %w", h.PositionNodeID, err)
	}
	return bumpGen(db, h.LoaderID)
}

// SetHomeOrder rewrites sort_order for a loader's positions to match the given
// node-id sequence (index = order). Positions not in the list are left
// untouched. Bumps config_gen. Used by the grid-drag reorder.
func SetHomeOrder(db *sql.DB, loaderID int64, orderedNodeIDs []int64) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("reorder homes loader=%d: begin: %w", loaderID, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	for i, nodeID := range orderedNodeIDs {
		if _, err := tx.Exec(`UPDATE bin_loader_homes SET sort_order=$1 WHERE loader_id=$2 AND position_node_id=$3`,
			i, loaderID, nodeID); err != nil {
			return fmt.Errorf("reorder home pos=%d: %w", nodeID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reorder homes loader=%d: commit: %w", loaderID, err)
	}
	return bumpGen(db, loaderID)
}

// RemoveHome clears a position assignment and bumps config_gen.
func RemoveHome(db *sql.DB, loaderID, positionNodeID int64) error {
	if _, err := db.Exec(`DELETE FROM bin_loader_homes WHERE loader_id=$1 AND position_node_id=$2`, loaderID, positionNodeID); err != nil {
		return fmt.Errorf("remove home pos=%d: %w", positionNodeID, err)
	}
	return bumpGen(db, loaderID)
}

// ListHomes returns a loader's positions in operator-defined order (sort_order,
// then position node id as a stable tiebreak).
func ListHomes(db *sql.DB, loaderID int64) ([]Home, error) {
	rows, err := db.Query(`SELECT loader_id, position_node_id, payload_code, min_stock, uop_threshold, sort_order
		FROM bin_loader_homes WHERE loader_id=$1 ORDER BY sort_order, position_node_id`, loaderID)
	if err != nil {
		return nil, fmt.Errorf("list homes loader=%d: %w", loaderID, err)
	}
	defer rows.Close()
	var out []Home
	for rows.Next() {
		var h Home
		if err := rows.Scan(&h.LoaderID, &h.PositionNodeID, &h.PayloadCode, &h.MinStock, &h.UOPThreshold, &h.SortOrder); err != nil {
			return nil, fmt.Errorf("scan home: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// UpsertPayload adds or updates an allowed payload on a shared_window loader.
// Bumps config_gen.
func UpsertPayload(db *sql.DB, p Payload) error {
	_, err := db.Exec(`
		INSERT INTO bin_loader_payloads (loader_id, payload_code, min_stock, uop_threshold)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (loader_id, payload_code) DO UPDATE SET
			min_stock=EXCLUDED.min_stock, uop_threshold=EXCLUDED.uop_threshold`,
		p.LoaderID, p.PayloadCode, p.MinStock, p.UOPThreshold)
	if err != nil {
		return fmt.Errorf("upsert payload %s/loader=%d: %w", p.PayloadCode, p.LoaderID, err)
	}
	return bumpGen(db, p.LoaderID)
}

// RemovePayload drops an allowed payload and bumps config_gen.
func RemovePayload(db *sql.DB, loaderID int64, payloadCode string) error {
	if _, err := db.Exec(`DELETE FROM bin_loader_payloads WHERE loader_id=$1 AND payload_code=$2`, loaderID, payloadCode); err != nil {
		return fmt.Errorf("remove payload %s/loader=%d: %w", payloadCode, loaderID, err)
	}
	return bumpGen(db, loaderID)
}

// ListPayloads returns a loader's allowed payload set, ordered by code.
func ListPayloads(db *sql.DB, loaderID int64) ([]Payload, error) {
	rows, err := db.Query(`SELECT loader_id, payload_code, min_stock, uop_threshold
		FROM bin_loader_payloads WHERE loader_id=$1 ORDER BY payload_code`, loaderID)
	if err != nil {
		return nil, fmt.Errorf("list payloads loader=%d: %w", loaderID, err)
	}
	defer rows.Close()
	var out []Payload
	for rows.Next() {
		var p Payload
		if err := rows.Scan(&p.LoaderID, &p.PayloadCode, &p.MinStock, &p.UOPThreshold); err != nil {
			return nil, fmt.Errorf("scan payload: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetConfig assembles a loader with its homes and payloads, or (nil, nil) if the
// loader is absent.
func GetConfig(db *sql.DB, id int64) (*Config, error) {
	l, err := GetLoader(db, id)
	if err != nil || l == nil {
		return nil, err
	}
	homes, err := ListHomes(db, id)
	if err != nil {
		return nil, err
	}
	payloads, err := ListPayloads(db, id)
	if err != nil {
		return nil, err
	}
	return &Config{Loader: *l, Homes: homes, Payloads: payloads}, nil
}

func bumpGen(db *sql.DB, loaderID int64) error {
	if _, err := db.Exec(`UPDATE bin_loaders SET config_gen=config_gen+1, updated_at=NOW() WHERE id=$1`, loaderID); err != nil {
		return fmt.Errorf("bump config_gen loader %d: %w", loaderID, err)
	}
	return nil
}

func requireOne(res sql.Result, op string, id int64) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s %d: rows affected: %w", op, id, err)
	}
	if n == 0 {
		return fmt.Errorf("%s %d: no such row", op, id)
	}
	return nil
}
