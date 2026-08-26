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
	"sort"

	"shingo/protocol"
	"shingo/shared/windoworder"
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
	ConfigGen     int64
	// FunnelWindows: take one window at a time rather than spreading empties
	// across all of them. Synced from Core, which owns the setting; false (spread)
	// on a row written by a Core that predates the field, which is the behaviour
	// every loader had when it was a plant-wide config key.
	FunnelWindows bool
	// ChangeoverLoadDirective: a changeover commandeers this station's card,
	// naming the carrier the incoming style needs rather than offering the
	// whole board. Core owns it; this is the mirror.
	ChangeoverLoadDirective bool
	Positions               []CoreLoaderPosition
	Payloads                []CoreLoaderPayload
	// Quota is the declared carrier mix. Empty means none declared, which is
	// today's behaviour: the loader takes whatever compatible carrier it finds.
	Quota []CoreLoaderQuota
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
	// HomeKind is 'home' | 'buffer', synced from Core. EMPTY MEANS UNKNOWN, not
	// home: a Core predating the field sends blank for buffer slots too, so a
	// reader must fall back to the old empty-payload inference rather than
	// treating blank as a positive answer.
	HomeKind     string
	UOPThreshold int
	// Ordinal is where the operator dragged this window, synced from Core.
	// Zero on every row means nothing was arranged (or the Core that sent it
	// predates the field), which falls through to a number-aware name sort.
	Ordinal int
	// BinTypes is what this window can PHYSICALLY take, synced from Core.
	// EMPTY MEANS ANYTHING — the state every window is in until configured.
	BinTypes []string
}

// CoreLoaderQuota is one line of a loader's declared carrier mix.
type CoreLoaderQuota struct {
	BinTypeCode string
	Want        int
}

// CoreLoaderPayload is one entry in a shared_window allowed set.
type CoreLoaderPayload struct {
	PayloadCode  string
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

	for _, t := range []string{"core_loader_window_bin_types", "core_loader_quotas", "core_loader_positions", "core_loader_payloads", "core_loaders"} {
		if _, err := tx.Exec("DELETE FROM " + t); err != nil {
			return fmt.Errorf("clear %s: %w", t, err)
		}
	}

	for _, l := range loaders {
		if _, err := tx.Exec(
			`INSERT INTO core_loaders (loader_key, role, name, layout, replenishment, outbound_dest, inbound_source, config_gen, funnel_windows, changeover_load_directive, synced_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,datetime('now'))`,
			l.LoaderKey, l.Role, l.Name, l.Layout, l.Replenishment, l.OutboundDest, l.InboundSource, l.ConfigGen, l.FunnelWindows, l.ChangeoverLoadDirective,
		); err != nil {
			return fmt.Errorf("insert core_loader %s/%s: %w", l.LoaderKey, l.Role, err)
		}
		for _, p := range l.Positions {
			// min_stock column left dormant (bin-count floor retired) — defaults to 0.
			if _, err := tx.Exec(
				`INSERT INTO core_loader_positions (loader_key, position_node, payload_code, kind, home_kind, uop_threshold, ordinal) VALUES (?,?,?,?,?,?,?)`,
				l.LoaderKey, p.CoreNodeName, p.PayloadCode, p.Kind, p.HomeKind, p.UOPThreshold, p.Ordinal,
			); err != nil {
				return fmt.Errorf("insert position %s: %w", p.CoreNodeName, err)
			}
			for _, bt := range p.BinTypes {
				if _, err := tx.Exec(
					`INSERT INTO core_loader_window_bin_types (loader_key, position_node, bin_type_code) VALUES (?,?,?)`,
					l.LoaderKey, p.CoreNodeName, bt,
				); err != nil {
					return fmt.Errorf("insert window bin type %s/%s: %w", p.CoreNodeName, bt, err)
				}
			}
		}
		for _, q := range l.Quota {
			if _, err := tx.Exec(
				`INSERT INTO core_loader_quotas (loader_key, bin_type_code, want) VALUES (?,?,?)`,
				l.LoaderKey, q.BinTypeCode, q.Want,
			); err != nil {
				return fmt.Errorf("insert quota %s/%s: %w", l.LoaderKey, q.BinTypeCode, err)
			}
		}
		for _, p := range l.Payloads {
			if _, err := tx.Exec(
				`INSERT INTO core_loader_payloads (loader_key, payload_code, uop_threshold) VALUES (?,?,?)`,
				l.LoaderKey, p.PayloadCode, p.UOPThreshold,
			); err != nil {
				return fmt.Errorf("insert payload %s: %w", p.PayloadCode, err)
			}
		}
	}
	return tx.Commit()
}

// ListCoreLoaders returns every cached loader assembled with positions+payloads.
func (db *DB) ListCoreLoaders() ([]CoreLoader, error) {
	rows, err := db.Query(`SELECT loader_key, role, name, layout, replenishment, outbound_dest, inbound_source, config_gen, funnel_windows, changeover_load_directive FROM core_loaders ORDER BY loader_key`)
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
	rows, err := db.Query(`SELECT loader_key, role, name, layout, replenishment, outbound_dest, inbound_source, config_gen, funnel_windows, changeover_load_directive FROM core_loaders WHERE loader_key=?`, loaderKey)
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
			&l.OutboundDest, &l.InboundSource, &l.ConfigGen, &l.FunnelWindows,
			&l.ChangeoverLoadDirective); err != nil {
			return nil, fmt.Errorf("scan core_loader: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// attachCoreLoaderChildren loads a cached loader's positions and payloads.
//
// THE POSITION ORDER IS THE DELIVERY DECISION, not presentation. The funnel
// case delivers to "the first window" and spreading fills free windows in
// order, so the sequence this returns decides which window a carrier physically
// goes to — and it has to match the order Core computes for the same loader, or
// the two sides send carriers to different places.
//
// It is sorted in Go rather than in the query because the rule is
// shared/windoworder, which both sides import: the operator's arrangement
// first, then a number-aware name sort (so W2 comes before W10). SQL can order
// by the ordinal but cannot do the number-aware half, and having the tiebreak
// in one place matters more than doing the whole thing in one statement.
//
// This read used to be `ORDER BY position_node` — plain name order, with the
// operator's arrangement discarded because there was no column to put it in.
func (db *DB) attachCoreLoaderChildren(l *CoreLoader) error {
	prows, err := db.Query(`SELECT position_node, payload_code, kind, home_kind, uop_threshold, ordinal FROM core_loader_positions WHERE loader_key=?`, l.LoaderKey)
	if err != nil {
		return fmt.Errorf("list positions %s: %w", l.LoaderKey, err)
	}
	for prows.Next() {
		var p CoreLoaderPosition
		if err := prows.Scan(&p.PositionNode, &p.PayloadCode, &p.Kind, &p.HomeKind, &p.UOPThreshold, &p.Ordinal); err != nil {
			prows.Close()
			return err
		}
		l.Positions = append(l.Positions, p)
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return err
	}
	sort.SliceStable(l.Positions, func(i, j int) bool {
		return windoworder.Less(
			windoworder.Window{Ordinal: l.Positions[i].Ordinal, Name: l.Positions[i].PositionNode},
			windoworder.Window{Ordinal: l.Positions[j].Ordinal, Name: l.Positions[j].PositionNode},
		)
	})

	btrows, err := db.Query(`SELECT position_node, bin_type_code FROM core_loader_window_bin_types WHERE loader_key=? ORDER BY position_node, bin_type_code`, l.LoaderKey)
	if err != nil {
		return fmt.Errorf("list window bin types %s: %w", l.LoaderKey, err)
	}
	caps := map[string][]string{}
	for btrows.Next() {
		var node, code string
		if err := btrows.Scan(&node, &code); err != nil {
			btrows.Close()
			return err
		}
		caps[node] = append(caps[node], code)
	}
	btrows.Close()
	if err := btrows.Err(); err != nil {
		return err
	}
	for i := range l.Positions {
		l.Positions[i].BinTypes = caps[l.Positions[i].PositionNode]
	}

	qrows, err := db.Query(`SELECT bin_type_code, want FROM core_loader_quotas WHERE loader_key=? ORDER BY bin_type_code`, l.LoaderKey)
	if err != nil {
		return fmt.Errorf("list quotas %s: %w", l.LoaderKey, err)
	}
	for qrows.Next() {
		var q CoreLoaderQuota
		if err := qrows.Scan(&q.BinTypeCode, &q.Want); err != nil {
			qrows.Close()
			return err
		}
		l.Quota = append(l.Quota, q)
	}
	qrows.Close()
	if err := qrows.Err(); err != nil {
		return err
	}

	yrows, err := db.Query(`SELECT payload_code, uop_threshold FROM core_loader_payloads WHERE loader_key=? ORDER BY payload_code`, l.LoaderKey)
	if err != nil {
		return fmt.Errorf("list payloads %s: %w", l.LoaderKey, err)
	}
	defer yrows.Close()
	for yrows.Next() {
		var p CoreLoaderPayload
		if err := yrows.Scan(&p.PayloadCode, &p.UOPThreshold); err != nil {
			return err
		}
		l.Payloads = append(l.Payloads, p)
	}
	return yrows.Err()
}
