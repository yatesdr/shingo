package inventory

import (
	"database/sql"
	"fmt"
	"time"

	"shingo/protocol"
)

// The /inventory page's reads: the loop as the threshold monitor watches it,
// and the running styles' claims. Each statement reads every row at once; none
// is per row.

// Binding is one (loader position, payload) the threshold monitor watches —
// a demand_registry row with a UoP threshold — with what the page shows beside
// it: the loader, the part's pack, the carriers coming, and the last recorded
// change of its threshold.
type Binding struct {
	CoreNodeName   string
	PayloadCode    string
	Threshold      int
	LoaderID       int64
	LoaderName     string
	Layout         string
	PositionNodeID int64 // the binding node's id; a dedicated home's position
	Description    string
	Pack           int    // payloads.uop_capacity; 0 = the part states none
	Windows        int    // the loader's windows (a shared-window loader's bin count)
	Members        string // the loader's member node names, comma-separated
	Coming         int    // orders to the position (any window of a shared loader) queued or in transit, not yet delivered
	// CompletedL1 counts the threshold L1 retrieves (retrieve_empty, demand
	// episode kind threshold) delivered to the position (any window of a shared
	// loader) and confirmed inside the window: the calculator's L1 cycle,
	// counted where it happened. No suggestion is shown until it is 1.
	CompletedL1 int
	// The latest uop_threshold audit row for the position and part, if any.
	ProvOld   sql.NullString
	ProvNew   sql.NullString
	ProvActor sql.NullString
	ProvAt    sql.NullTime
}

// ThresholdAuditEntity is the audit_log entity a binding's threshold change is
// recorded under: a dedicated home by its position node, a shared-window
// loader's part by the loader. The values are "<payload> <n>" (old) and
// "<payload> <n> <source>" (new).
const (
	ThresholdAuditHome    = "loader_home"
	ThresholdAuditPayload = "loader_payload"
	ThresholdAuditAction  = "uop_threshold"
)

// loopBindingsSQL reads every monitored binding once, deduplicated across
// stations (a position no station owns yet is derived for every station; the
// page names no station, so the copies are one row). Coming, the completed L1
// cycles and the provenance are per-row subqueries inside the one statement:
// orders through idx_orders_delivery_node (an IN over the position, or a
// shared loader's windows; an OR there turned it into a scan of orders per
// row), demand_origins by its key and order_history through
// idx_order_history_order for the completed cycles, audit_log through
// idx_audit_entity. $1 and $2 bracket the calculator's window.
var loopBindingsSQL = fmt.Sprintf(`
SELECT DISTINCT ON (r.core_node_name, r.payload_code)
       r.core_node_name, r.payload_code, r.replenish_uop_threshold,
       COALESCE(r.loader_id, 0), COALESCE(l.name, ''), COALESCE(l.layout, ''),
       COALESCE(n.id, 0), COALESCE(p.description, ''), COALESCE(p.uop_capacity, 0),
       (SELECT count(*) FROM bin_loader_homes w WHERE w.loader_id = r.loader_id AND w.home_kind = 'home'),
       COALESCE((SELECT string_agg(wn.name, ',') FROM bin_loader_homes w JOIN nodes wn ON wn.id = w.position_node_id
                  WHERE w.loader_id = r.loader_id), ''),
       (SELECT count(*) FROM orders o
         WHERE o.payload_code = r.payload_code AND o.status NOT IN (%s, '%s')
           AND o.delivery_node IN (
               SELECT r.core_node_name
               UNION
               SELECT wn.name FROM bin_loader_homes w JOIN nodes wn ON wn.id = w.position_node_id
                WHERE w.loader_id = r.loader_id AND l.layout = '%s')),
       (SELECT count(*) FROM orders o
          JOIN demand_origins d ON d.origin_id = o.origin_id AND d.kind = '%s'
         WHERE o.payload_code = r.payload_code AND o.order_type = '%s'
           AND o.delivery_node IN (
               SELECT r.core_node_name
               UNION
               SELECT wn.name FROM bin_loader_homes w JOIN nodes wn ON wn.id = w.position_node_id
                WHERE w.loader_id = r.loader_id AND l.layout = '%s')
           AND EXISTS (SELECT 1 FROM order_history h WHERE h.order_id = o.id AND h.status = '%s'
                        AND h.created_at >= $1 AND h.created_at <= $2)),
       pv.old_value, pv.new_value, pv.actor, pv.created_at
FROM demand_registry r
LEFT JOIN bin_loaders l ON l.id = r.loader_id
LEFT JOIN nodes n ON n.name = r.core_node_name
LEFT JOIN payloads p ON p.code = r.payload_code
LEFT JOIN LATERAL (
    SELECT a.old_value, a.new_value, a.actor, a.created_at FROM audit_log a
     WHERE a.entity_type = CASE WHEN l.layout = '%s' THEN '%s' ELSE '%s' END
       AND a.entity_id = CASE WHEN l.layout = '%s' THEN r.loader_id ELSE n.id END
       AND a.action = '%s'
       AND a.new_value LIKE r.payload_code || ' %%'
     ORDER BY a.id DESC LIMIT 1) pv ON true
WHERE r.replenish_uop_threshold > 0
ORDER BY r.core_node_name, r.payload_code, r.replenish_uop_threshold DESC`,
	protocol.TerminalStatusSQLList(), protocol.StatusDelivered, protocol.LoaderLayoutSharedWindow,
	protocol.EpisodeKindThreshold, protocol.OrderTypeRetrieveEmpty, protocol.LoaderLayoutSharedWindow,
	protocol.StatusConfirmed,
	protocol.LoaderLayoutSharedWindow, ThresholdAuditPayload, ThresholdAuditHome,
	protocol.LoaderLayoutSharedWindow, ThresholdAuditAction)

// LoopBindings returns every monitored binding, one row per (position, part),
// with its completed L1 cycles between start and end.
func LoopBindings(db *sql.DB, start, end time.Time) ([]Binding, error) {
	rows, err := db.Query(loopBindingsSQL, start.UTC(), end.UTC())
	if err != nil {
		return nil, fmt.Errorf("loop bindings: %w", err)
	}
	defer rows.Close()
	var out []Binding
	for rows.Next() {
		var b Binding
		if err := rows.Scan(&b.CoreNodeName, &b.PayloadCode, &b.Threshold, &b.LoaderID, &b.LoaderName, &b.Layout,
			&b.PositionNodeID, &b.Description, &b.Pack, &b.Windows, &b.Members, &b.Coming, &b.CompletedL1,
			&b.ProvOld, &b.ProvNew, &b.ProvActor, &b.ProvAt); err != nil {
			return nil, fmt.Errorf("loop bindings: scan: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// LoopBin is one bin holding a part in the loop: the statuses and the carrier
// stamp SystemUOPForPayload counts, so the page can name the bins behind a sum
// (off their home, below zero, unconfirmed) without a second spelling of it.
type LoopBin struct {
	ID          int64
	Label       string
	Node        string
	PayloadCode string
	UOP         int
	Confirmed   bool
}

// LoopBins returns every bin SystemUOPForPayload would count, for any part.
// outOfLoop is the sum's own lifecycle predicate, passed in so the two reads
// cannot disagree about which bins are in the loop.
func LoopBins(db *sql.DB, outOfLoop string) ([]LoopBin, error) {
	rows, err := db.Query(`SELECT b.id, b.label, COALESCE(n.name, ''), b.payload_code, b.uop_remaining, b.manifest_confirmed
		FROM bins b LEFT JOIN nodes n ON n.id = b.node_id
		WHERE COALESCE(b.payload_code, '') <> '' AND b.undeclared_carrier_at IS NULL AND b.` + outOfLoop + `
		ORDER BY b.payload_code, n.name, b.label`)
	if err != nil {
		return nil, fmt.Errorf("loop bins: %w", err)
	}
	defer rows.Close()
	var out []LoopBin
	for rows.Next() {
		var b LoopBin
		if err := rows.Scan(&b.ID, &b.Label, &b.Node, &b.PayloadCode, &b.UOP, &b.Confirmed); err != nil {
			return nil, fmt.Errorf("loop bins: scan: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// LoopClaim is one cell of a running style, from the plant-claims mirror: a
// row of the Lines table, consume or produce.
type LoopClaim struct {
	ProcessID    string
	CoreNodeName string
	PayloadCode  string
	Role         string
	ReorderPoint int
	Pack         int  // the claim's uop_capacity
	LoaderMember bool // the node is a loader's home or window
	// Station names the Edge that runs the claim's process: the edge's display
	// name in edge_registry when it has one, else the station id
	// process_styles carries. "" when the mirror names no station.
	Station string
}

// loopClaimsSQL is the running styles' claims, in one statement. The line's
// rate is not read here: sourceability's node rates are (store/sourceability
// ReadNodeRates), the one spelling of how fast a line draws or fills.
const loopClaimsSQL = `
WITH claims AS (
    SELECT DISTINCT c.process_id, c.core_node_name, c.payload_code, c.role, c.reorder_point, c.uop_capacity,
           COALESCE(s.station_id, '') AS station_id
      FROM style_claims c
      JOIN process_styles s ON s.process_id = c.process_id AND s.style_id = c.style_id AND s.is_active
     WHERE c.payload_code <> '')
SELECT c.process_id, c.core_node_name, c.payload_code, c.role, c.reorder_point, c.uop_capacity,
       EXISTS (SELECT 1 FROM bin_loader_homes h JOIN nodes hn ON hn.id = h.position_node_id WHERE hn.name = c.core_node_name),
       COALESCE(NULLIF((SELECT e.display_name FROM edge_registry e WHERE e.station_id = c.station_id
                         ORDER BY e.id LIMIT 1), ''), c.station_id)
  FROM claims c
 ORDER BY c.process_id, c.core_node_name, c.payload_code, c.role`

// LoopClaims returns the running styles' claims.
func LoopClaims(db *sql.DB) ([]LoopClaim, error) {
	rows, err := db.Query(loopClaimsSQL)
	if err != nil {
		return nil, fmt.Errorf("loop claims: %w", err)
	}
	defer rows.Close()
	var out []LoopClaim
	for rows.Next() {
		var c LoopClaim
		if err := rows.Scan(&c.ProcessID, &c.CoreNodeName, &c.PayloadCode, &c.Role, &c.ReorderPoint, &c.Pack,
			&c.LoaderMember, &c.Station); err != nil {
			return nil, fmt.Errorf("loop claims: scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// LiveOrder is a live order not yet delivered, as the Lines table's On the way
// reads it: enough for protocol.OrderPlacesBinAt to say where it sets a bin
// down, and the part it carries.
type LiveOrder struct {
	OrderType    protocol.OrderType
	StepsJSON    string
	DeliveryNode string
	PayloadCode  string
}

// LoopLiveOrders returns every live order not yet delivered (a delivered bin is
// already at its node). The status term is NonTerminalStatusSQLList's positive
// form, so idx_orders_status serves it; the live set is small.
func LoopLiveOrders(db *sql.DB) ([]LiveOrder, error) {
	rows, err := db.Query(`SELECT order_type, steps_json, delivery_node, payload_code FROM orders
		WHERE status IN (`+protocol.NonTerminalStatusSQLList()+`) AND status <> $1`, protocol.StatusDelivered)
	if err != nil {
		return nil, fmt.Errorf("loop live orders: %w", err)
	}
	defer rows.Close()
	var out []LiveOrder
	for rows.Next() {
		var o LiveOrder
		if err := rows.Scan(&o.OrderType, &o.StepsJSON, &o.DeliveryNode, &o.PayloadCode); err != nil {
			return nil, fmt.Errorf("loop live orders: scan: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
