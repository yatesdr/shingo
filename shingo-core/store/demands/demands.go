// Package demands holds demand-registry persistence for shingo-core: the
// derived map from a payload code to the manual_swap nodes that accept it,
// and the per-(loader, payload) replenishment threshold hung off it.
//
// THE `demands` TABLE IT WAS NAMED FOR IS GONE (v106), along with the two
// siblings that moved here with it — production_log at v92, a write-only
// shadow of bin_uop_ledger, and the quota table itself. The package keeps the
// name because the registry is the demand model that survived. The outer
// store/ keeps one-line delegate methods on *store.DB.
package demands

import (
	"database/sql"
	"log"
	"strings"
	"time"

	"shingo/protocol"
)

// RegistryEntry maps a payload code to a manual_swap node that accepts
// it. Derived by Core from the bin_loaders aggregate (BuildDemandRegistryFromAggregate).
//
// ReplenishUOPThreshold (UOP-threshold replenishment) carries the
// per-(loader, payload) trigger Core uses to decide when to send
// LoopBelowThresholdSignal. Zero means "Core does not monitor this
// pair" — Edge falls back to legacy bin-count behavior.
type RegistryEntry struct {
	ID           int64  `json:"id"`
	StationID    string `json:"station_id"`
	CoreNodeName string `json:"core_node_name"`
	// LoaderID is the owning loader's surrogate (the identity cutover). Set from the
	// aggregate at re-derive time; 0/NULL for legacy ClaimSync rows. The threshold
	// monitor mints LoaderKey="loader:<id>" from it onto the signal.
	LoaderID              int64              `json:"loader_id,omitempty"`
	Role                  protocol.ClaimRole `json:"role"` // "produce" or "consume"
	PayloadCode           string             `json:"payload_code"`
	OutboundDest          string             `json:"outbound_dest"`
	ReplenishUOPThreshold int                `json:"replenish_uop_threshold"`
	UpdatedAt             time.Time          `json:"updated_at"`
}

// registrySelectCols is the canonical column list for demand_registry
// reads. Centralized so a future column addition only requires touching
// the SELECT list in one place — and the scan order in the helpers
// below. Adding a new column without updating both will break the
// positional Scan().
const registrySelectCols = `id, station_id, core_node_name, role, payload_code, outbound_dest, replenish_uop_threshold, loader_id, updated_at`

func scanRegistryEntry(row interface{ Scan(...any) error }) (RegistryEntry, error) {
	var e RegistryEntry
	var loaderID sql.NullInt64
	err := row.Scan(&e.ID, &e.StationID, &e.CoreNodeName, &e.Role, &e.PayloadCode, &e.OutboundDest, &e.ReplenishUOPThreshold, &loaderID, &e.UpdatedAt)
	if loaderID.Valid {
		e.LoaderID = loaderID.Int64
	}
	return e, err
}

// RegistryChange describes a single (loader, payload) row whose
// replenish_uop_threshold value moved as a result of a SyncRegistry
// call. Callers (specifically threshold_monitor) use this to reset
// per-binding debounce state so a new threshold takes immediate effect.
type RegistryChange struct {
	StationID    string
	CoreNodeName string
	PayloadCode  string
	OldThreshold int
	NewThreshold int
}

// SyncRegistry replaces all demand_registry entries for a station atomically.
//
// Returns the list of (loader, payload) rows whose threshold value
// changed (including newly-created rows where old=0, and deleted rows
// where new=0 because the entry vanished). Empty slice when no thresholds
// shifted. Threshold-monitor consumes this to reset its in-memory
// debounce timers so the new threshold engages without waiting out the
// debounce window.
func SyncRegistry(db *sql.DB, stationID string, entries []RegistryEntry) ([]RegistryChange, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Snapshot existing thresholds for change detection. Keyed by
	// (core_node_name, payload_code) — the natural composite that a
	// loader binding is identified by.
	prior := make(map[string]int)
	priorKey := func(node, payload string) string { return node + "\x00" + payload }
	rows, err := tx.Query(`SELECT core_node_name, payload_code, replenish_uop_threshold FROM demand_registry WHERE station_id = $1`, stationID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n, p string
		var thr int
		if err := rows.Scan(&n, &p, &thr); err != nil {
			rows.Close()
			return nil, err
		}
		prior[priorKey(n, p)] = thr
	}
	rows.Close()

	if _, err := tx.Exec(`DELETE FROM demand_registry WHERE station_id = $1`, stationID); err != nil {
		return nil, err
	}

	var changes []RegistryChange
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		// Wire boundary: CoreNodeName comes from the loader aggregate. Trim
		// defensively and warn if the upstream sent whitespace —
		// forensic signal that an Edge admin or sync path leaked
		// non-canonical data.
		coreNodeName := strings.TrimSpace(e.CoreNodeName)
		if coreNodeName != e.CoreNodeName {
			log.Printf("WARNING demand sync: Edge sent CoreNodeName with whitespace: %q (station=%q)", e.CoreNodeName, stationID)
			e.CoreNodeName = coreNodeName
		}
		var lid any
		if e.LoaderID > 0 {
			lid = e.LoaderID // NULL for legacy ClaimSync rows; the loader token rides this
		}
		if _, err := tx.Exec(`INSERT INTO demand_registry (station_id, core_node_name, role, payload_code, outbound_dest, replenish_uop_threshold, loader_id) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			stationID, e.CoreNodeName, e.Role, e.PayloadCode, e.OutboundDest, e.ReplenishUOPThreshold, lid); err != nil {
			return nil, err
		}
		key := priorKey(e.CoreNodeName, e.PayloadCode)
		seen[key] = true
		old := prior[key]
		if old != e.ReplenishUOPThreshold {
			changes = append(changes, RegistryChange{
				StationID:    stationID,
				CoreNodeName: e.CoreNodeName,
				PayloadCode:  e.PayloadCode,
				OldThreshold: old,
				NewThreshold: e.ReplenishUOPThreshold,
			})
		}
	}
	// Deleted rows whose old threshold was non-zero: clear the
	// debounce timer so a future re-create at a different threshold
	// (or any value) engages cleanly.
	for k, old := range prior {
		if seen[k] || old == 0 {
			continue
		}
		sep := -1
		for i := 0; i < len(k); i++ {
			if k[i] == 0 {
				sep = i
				break
			}
		}
		if sep < 0 {
			continue
		}
		changes = append(changes, RegistryChange{
			StationID:    stationID,
			CoreNodeName: k[:sep],
			PayloadCode:  k[sep+1:],
			OldThreshold: old,
			NewThreshold: 0,
		})
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return changes, nil
}

// LookupRegistry returns all demand_registry entries for a given payload code.
func LookupRegistry(db *sql.DB, payloadCode string) ([]RegistryEntry, error) {
	rows, err := db.Query(`SELECT `+registrySelectCols+`
		FROM demand_registry WHERE payload_code = $1`, payloadCode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []RegistryEntry
	for rows.Next() {
		e, err := scanRegistryEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// LookupThresholdsByPayload returns every demand_registry binding for
// the given payload that has a non-zero replenish_uop_threshold —
// the set of (loader, payload) pairs Core's threshold monitor is
// responsible for. Pairs with threshold = 0 are opt-out (legacy
// bin-count owned by Edge) and excluded.
func LookupThresholdsByPayload(db *sql.DB, payloadCode string) ([]RegistryEntry, error) {
	rows, err := db.Query(`SELECT `+registrySelectCols+`
		FROM demand_registry
		WHERE payload_code = $1 AND replenish_uop_threshold > 0`, payloadCode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []RegistryEntry
	for rows.Next() {
		e, err := scanRegistryEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// ListThresholds returns every demand_registry binding with a non-zero
// replenish_uop_threshold across all payloads. Used by the threshold-
// monitor startup sweep, which iterates every active binding once on
// startup bypassing debounce.
func ListThresholds(db *sql.DB) ([]RegistryEntry, error) {
	rows, err := db.Query(`SELECT ` + registrySelectCols + `
		FROM demand_registry
		WHERE replenish_uop_threshold > 0
		ORDER BY station_id, core_node_name, payload_code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []RegistryEntry
	for rows.Next() {
		e, err := scanRegistryEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// ListRegistry returns every demand_registry entry.
func ListRegistry(db *sql.DB) ([]RegistryEntry, error) {
	rows, err := db.Query(`SELECT ` + registrySelectCols + `
		FROM demand_registry ORDER BY station_id, core_node_name, payload_code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []RegistryEntry
	for rows.Next() {
		e, err := scanRegistryEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
