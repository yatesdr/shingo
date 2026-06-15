package store

// Edge persistent cache of Core's bin_loaders aggregate. Written full-state on
// each node-list sync from NodeListResponse.Loaders; read by the aggregate
// LoaderStore. Persistent so an Edge reboot during a Core partition keeps loaders
// configured. Keyed by loader_key — the loader's surrogate identity token
// ("loader:<id>"). The loader has no node of its own (a multi-window loader spans
// many nodes), so only its members (positions/windows) carry real node names.

import (
	"database/sql"
	"fmt"

	"shingo/protocol"
)

// CoreLoader is the cached read shape for one Core-owned loader, assembled with
// its positions (dedicated_positions) and/or payloads (shared_window).
type CoreLoader struct {
	LoaderKey     string // the loader IDENTITY token ("loader:<id>") — the cache key
	Role          string
	Name          string
	Layout        string
	Replenishment string
	OutboundDest  string
	InboundSource string
	BufferDest    string
	ConfigGen     int64
	Positions     []CoreLoaderPosition
	Payloads      []CoreLoaderPayload
}

// CoreLoaderPosition is one home of a cached loader (position node NAME). For a
// dedicated loader it carries one payload; for a shared_window loader it is a
// window (no payload — the shared set lives in Payloads). Kind makes that
// explicit (protocol.LoaderPositionKind*), synced from Core; empty on rows
// written by a pre-Kind Core, in which case the parent loader's Layout is
// authoritative.
type CoreLoaderPosition struct {
	PositionNode string
	PayloadCode  string
	Kind         string
	MinStock     int
	UOPThreshold int
}

// CoreLoaderPayload is one entry in a shared_window allowed set.
type CoreLoaderPayload struct {
	PayloadCode  string
	MinStock     int
	UOPThreshold int
}

// ReplaceCoreLoaders fully replaces the cached Core loader config (full-state
// sync: delete all, re-insert) atomically. On any error the tx rolls back, so
// the previous last-known-good cache is preserved rather than half-written.
func (db *DB) ReplaceCoreLoaders(loaders []protocol.LoaderInfo) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, t := range []string{"core_loader_positions", "core_loader_payloads", "core_loaders"} {
		if _, err := tx.Exec("DELETE FROM " + t); err != nil {
			return fmt.Errorf("clear %s: %w", t, err)
		}
	}

	for _, l := range loaders {
		if _, err := tx.Exec(
			`INSERT INTO core_loaders (loader_key, role, name, layout, replenishment, outbound_dest, inbound_source, buffer_dest, config_gen, synced_at)
			 VALUES (?,?,?,?,?,?,?,?,?,datetime('now'))`,
			l.LoaderKey, l.Role, l.Name, l.Layout, l.Replenishment, l.OutboundDest, l.InboundSource, l.BufferDest, l.ConfigGen,
		); err != nil {
			return fmt.Errorf("insert core_loader %s/%s: %w", l.LoaderKey, l.Role, err)
		}
		for _, p := range l.Positions {
			if _, err := tx.Exec(
				`INSERT INTO core_loader_positions (loader_key, position_node, payload_code, kind, min_stock, uop_threshold) VALUES (?,?,?,?,?,?)`,
				l.LoaderKey, p.CoreNodeName, p.PayloadCode, p.Kind, p.MinStock, p.UOPThreshold,
			); err != nil {
				return fmt.Errorf("insert position %s: %w", p.CoreNodeName, err)
			}
		}
		for _, p := range l.Payloads {
			if _, err := tx.Exec(
				`INSERT INTO core_loader_payloads (loader_key, payload_code, min_stock, uop_threshold) VALUES (?,?,?,?)`,
				l.LoaderKey, p.PayloadCode, p.MinStock, p.UOPThreshold,
			); err != nil {
				return fmt.Errorf("insert payload %s: %w", p.PayloadCode, err)
			}
		}
	}
	return tx.Commit()
}

// ListCoreLoaders returns every cached loader assembled with positions+payloads.
func (db *DB) ListCoreLoaders() ([]CoreLoader, error) {
	rows, err := db.Query(`SELECT loader_key, role, name, layout, replenishment, outbound_dest, inbound_source, buffer_dest, config_gen FROM core_loaders ORDER BY loader_key`)
	if err != nil {
		return nil, fmt.Errorf("list core_loaders: %w", err)
	}
	out, err := scanCoreLoaders(rows)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if err := db.attachCoreLoaderChildren(&out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// GetCoreLoader returns the cached loader with loaderKey, or nil.
func (db *DB) GetCoreLoader(loaderKey string) (*CoreLoader, error) {
	rows, err := db.Query(`SELECT loader_key, role, name, layout, replenishment, outbound_dest, inbound_source, buffer_dest, config_gen FROM core_loaders WHERE loader_key=?`, loaderKey)
	if err != nil {
		return nil, fmt.Errorf("get core_loader %s: %w", loaderKey, err)
	}
	out, err := scanCoreLoaders(rows)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	if err := db.attachCoreLoaderChildren(&out[0]); err != nil {
		return nil, err
	}
	return &out[0], nil
}

func scanCoreLoaders(rows *sql.Rows) ([]CoreLoader, error) {
	defer rows.Close()
	var out []CoreLoader
	for rows.Next() {
		var l CoreLoader
		if err := rows.Scan(&l.LoaderKey, &l.Role, &l.Name, &l.Layout, &l.Replenishment,
			&l.OutboundDest, &l.InboundSource, &l.BufferDest, &l.ConfigGen); err != nil {
			return nil, fmt.Errorf("scan core_loader: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (db *DB) attachCoreLoaderChildren(l *CoreLoader) error {
	prows, err := db.Query(`SELECT position_node, payload_code, kind, min_stock, uop_threshold FROM core_loader_positions WHERE loader_key=? ORDER BY position_node`, l.LoaderKey)
	if err != nil {
		return fmt.Errorf("list positions %s: %w", l.LoaderKey, err)
	}
	for prows.Next() {
		var p CoreLoaderPosition
		if err := prows.Scan(&p.PositionNode, &p.PayloadCode, &p.Kind, &p.MinStock, &p.UOPThreshold); err != nil {
			prows.Close()
			return err
		}
		l.Positions = append(l.Positions, p)
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return err
	}

	yrows, err := db.Query(`SELECT payload_code, min_stock, uop_threshold FROM core_loader_payloads WHERE loader_key=? ORDER BY payload_code`, l.LoaderKey)
	if err != nil {
		return fmt.Errorf("list payloads %s: %w", l.LoaderKey, err)
	}
	defer yrows.Close()
	for yrows.Next() {
		var p CoreLoaderPayload
		if err := yrows.Scan(&p.PayloadCode, &p.MinStock, &p.UOPThreshold); err != nil {
			return err
		}
		l.Payloads = append(l.Payloads, p)
	}
	return yrows.Err()
}
