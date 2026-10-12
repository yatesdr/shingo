// Package plantclaims holds the Core mirror of Edge's plant-spec claim set
// (the plant.claims feed, Edge → Core). process_styles + style_claims are a
// pure mirror: the handler replaces a process's rows on every message, so
// the tables reflect Edge's plant spec at a point in time. The dirty index
// the sourceability recompute consumes (payload_code → set of (process,
// style)) is derived here from style_claims.
//
// Loaders/unloaders (manual_swap claims) never reach the claims mirror — they
// are excluded at the publisher and enter the computation as pool supply/demand
// via the loader aggregate, not as style claims. Their seats arrive as the
// report's Positions, mirrored in process_positions for addressing only
// (ClaimingStationsSQL); the sourceability recompute never reads them.
package plantclaims

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"shingo/protocol"
	"shingocore/plantspec"
)

// StyleRow is one (process, style) Edge reports in a plant-claims message.
type StyleRow struct {
	ProcessID string
	StyleID   string
	ConfigGen int64
	// IsActive marks the style the process is currently running, mirrored from
	// Edge's processes.active_style_id. At most one style per process carries
	// it; false for every style when Edge has no active style set, or when the
	// report came from an Edge too old to publish the flag.
	IsActive bool
}

// ClaimRow is one node claim under a (process, style), as mirrored.
// AllowedPayloadCodes is the effective payload set (mirrors
// PlantClaim.AllowedPayloadCodes on the wire).
//
// ContainmentDestination mirrors PlantClaim.ContainmentDestination: where a
// contained payload's bins divert instead of the claim's ordinary FG outbound
// destination; blank = unconfigured.
//
// The last four are the claim's LEGS — the nodes it draws from, ships to, and
// is paired with. They are mirrored for the demand loop compiler, which needs
// the edges between nodes and not just the nodes; the sourceability recompute
// reads none of them (store/sourceability). Blank
// means the Edge did not report a leg, which is the same value an Edge too old
// to publish them leaves behind — and Core must read both as "unknown", never
// as a node whose name is empty.
type ClaimRow struct {
	ProcessID              string
	StyleID                string
	CoreNodeName           string
	Role                   protocol.ClaimRole
	SwapMode               protocol.SwapMode
	PayloadCode            string
	ContainmentDestination string
	AllowedPayloadCodes    []string
	UOPCapacity            int
	ReorderPoint           int
	Seq                    int
	InboundSource          string
	OutboundDestination    string
	PairedCoreNode         string
	SecondPairedCoreNode   string
}

// IsLoader is the loader question for a mirrored claim, asked through the one
// Core derivation (plantspec.Claim.IsLoader) rather than by mode name: the mode
// field is the storage for the fact, not the question.
func (c ClaimRow) IsLoader() bool {
	return plantspec.Claim{SwapMode: string(c.SwapMode)}.IsLoader()
}

// DemandLevel is the claim's level, the Edge's rule (protocol.ClaimDemandLevel):
// a consume claim's reorder point, a produce claim's capacity unless its reorder
// point is earlier than full. 0 is no level.
func (c ClaimRow) DemandLevel() int {
	return protocol.ClaimDemandLevel(c.Role, c.ReorderPoint, c.UOPCapacity)
}

// ReplaceProcess replaces the mirror for one process in a single transaction.
// Every message is authoritative for its process, so this DELETEs the process's
// existing rows (process_styles + style_claims) then re-inserts the reported
// set. staleGuardConfigGen, when > 0, aborts the replace if the mirror already
// holds a NEWER config_gen for this process — so an out-of-order (older)
// snapshot landing after a newer one is dropped, not applied.
//
// styles with no sourceability-relevant claims still get a process_styles row
// (so an all-styles recompute knows the style exists) and an empty claim set.
func ReplaceProcess(db *sql.DB, processID string, styles []StyleRow, claims []ClaimRow, staleGuardConfigGen int64) error {
	_, err := ReplaceProcessFromStation(db, processID, "", styles, claims, nil, staleGuardConfigGen)
	return err
}

// ReplaceProcessFromStation is ReplaceProcess recording WHICH station published
// the process: process_styles.station_id, the address the sourceability feed
// sends that process's verdicts to. stationID "" records NULL — the station is
// unknown, and the feed then sends the process to every station, as before.
//
// It returns the station the mirror named for this process BEFORE the replace
// ("" when none). A different, non-empty previous station means two stations
// are publishing one process: the caller logs it, and this write — the last
// writer — is what the mirror keeps.
//
// positions are the report's Positions (the seats the process binds at that no
// claim names: loader windows and homes, FG drains), mirrored in
// process_positions in the same transaction; nil clears them. Their station is
// the process's process_styles.station_id, like the claims'.
func ReplaceProcessFromStation(db *sql.DB, processID, stationID string, styles []StyleRow, claims []ClaimRow, positions []string, staleGuardConfigGen int64) (previousStation string, err error) {
	tx, err := db.Begin()
	if err != nil {
		return "", fmt.Errorf("plantclaims replace %s: begin: %w", processID, err)
	}
	defer tx.Rollback()

	var prev sql.NullString
	if err := tx.QueryRow(
		`SELECT MAX(station_id) FROM process_styles WHERE process_id = $1`, processID,
	).Scan(&prev); err != nil {
		return "", fmt.Errorf("plantclaims replace %s: read station: %w", processID, err)
	}
	previousStation = prev.String

	if staleGuardConfigGen > 0 {
		var have sql.NullInt64
		if err := tx.QueryRow(
			`SELECT MAX(config_gen) FROM process_styles WHERE process_id = $1`, processID,
		).Scan(&have); err != nil {
			return previousStation, fmt.Errorf("plantclaims replace %s: stale-guard read: %w", processID, err)
		}
		if have.Valid && have.Int64 > staleGuardConfigGen {
			// A newer snapshot already landed for this process. Drop this one.
			return previousStation, nil
		}
	}

	if _, err := tx.Exec(`DELETE FROM process_styles WHERE process_id = $1`, processID); err != nil {
		return previousStation, fmt.Errorf("plantclaims replace %s: delete styles: %w", processID, err)
	}
	if _, err := tx.Exec(`DELETE FROM style_claims WHERE process_id = $1`, processID); err != nil {
		return previousStation, fmt.Errorf("plantclaims replace %s: delete claims: %w", processID, err)
	}
	if err := replacePositions(tx, processID, positions); err != nil {
		return previousStation, err
	}

	for _, s := range styles {
		if _, err := tx.Exec(
			`INSERT INTO process_styles (process_id, style_id, config_gen, is_active, station_id)
			 VALUES ($1, $2, $3, $4, NULLIF($5, ''))`,
			s.ProcessID, s.StyleID, s.ConfigGen, s.IsActive, stationID,
		); err != nil {
			return previousStation, fmt.Errorf("plantclaims replace %s: insert style %s: %w", processID, s.StyleID, err)
		}
	}

	// swap_mode HAS NO CORE READER, AND THE WRITE STILL CANNOT STOP. No SELECT
	// anywhere in shingo-core reads style_claims.swap_mode back — the mirror's own
	// readers (sourceability, lane_queries, bins, inventory)
	// all project other columns — so this is a write-only column.
	//
	// It cannot simply be dropped from the INSERT: the column is
	// `swap_mode TEXT NOT NULL` with NO DEFAULT (store/migrations.go v49, and the
	// frozen schema.snapshot.sql), so omitting it fails every plant-claims replace
	// with a not-null violation and the mirror stops ingesting at both plants.
	// Retiring it therefore needs a migration to add a default or drop the column,
	// which is an owner decision, not a cleanup. Left as-is deliberately.
	//
	// Note the wire already excludes loaders: the Edge publisher skips
	// IsLoaderNode() claims ("pool, not claims"), so manual_swap never reaches
	// this column in the first place.
	//
	// THE FOUR LEG COLUMNS BELOW ARE THE OPPOSITE CASE, and it is worth saying
	// so next to a write-only column. They have readers: the cancel-return
	// policy (store/return_sources.go) reads inbound_source and
	// outbound_destination to know where a line's bins come from and its
	// empties go, and the containment divert (store/containment.go) reads
	// outbound_destination to scope itself to the claim's own flow. The
	// sourceability path still must not, and legs_not_read_test.go names the
	// sanctioned readers so the next one lands flagged. They are written
	// because the mirror is REPLACED WHOLESALE on every message:
	// there is no later pass that could fill them in, so a claim mirrored
	// without its legs is a claim whose legs are lost until Edge happens to
	// republish that process. v118 gives them an empty-string default, so unlike
	// swap_mode they could be dropped from this INSERT later without breaking
	// ingestion.
	styleInsert, err := tx.Prepare(
		`INSERT INTO style_claims
		 (process_id, style_id, core_node_name, role, swap_mode, payload_code,
		  containment_destination, allowed_payload_codes, uop_capacity, reorder_point, seq,
		  inbound_source, outbound_destination, paired_core_node, second_paired_core_node)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`)
	if err != nil {
		return previousStation, fmt.Errorf("plantclaims replace %s: prepare claim insert: %w", processID, err)
	}
	defer styleInsert.Close()

	for _, c := range claims {
		allowed, err := json.Marshal(c.AllowedPayloadCodes)
		if err != nil {
			return previousStation, fmt.Errorf("plantclaims replace %s: marshal allowed for %s: %w", processID, c.CoreNodeName, err)
		}
		if c.AllowedPayloadCodes == nil {
			allowed = []byte("[]")
		}
		if _, err := styleInsert.Exec(
			c.ProcessID, c.StyleID, c.CoreNodeName, string(c.Role), string(c.SwapMode),
			c.PayloadCode, c.ContainmentDestination, string(allowed), c.UOPCapacity, c.ReorderPoint, c.Seq,
			c.InboundSource, c.OutboundDestination, c.PairedCoreNode, c.SecondPairedCoreNode,
		); err != nil {
			return previousStation, fmt.Errorf("plantclaims replace %s: insert claim %s: %w", processID, c.CoreNodeName, err)
		}
	}

	return previousStation, tx.Commit()
}

// replacePositions replaces one process's process_positions rows on tx: one
// DELETE, and one multi-row INSERT only when there are positions. The
// replace runs only when a report arrives, which the claims digest makes a
// change (Lane 0 item 6: "+1 mirror table and its write on change").
func replacePositions(tx *sql.Tx, processID string, positions []string) error {
	if _, err := tx.Exec(`DELETE FROM process_positions WHERE process_id = $1`, processID); err != nil {
		return fmt.Errorf("plantclaims replace %s: delete positions: %w", processID, err)
	}
	if len(positions) == 0 {
		return nil
	}
	values := make([]string, 0, len(positions))
	args := make([]any, 0, len(positions)+1)
	args = append(args, processID)
	for _, p := range positions {
		args = append(args, p)
		values = append(values, fmt.Sprintf("($1, $%d)", len(args)))
	}
	if _, err := tx.Exec(`INSERT INTO process_positions (process_id, core_node_name) VALUES `+
		strings.Join(values, ", ")+` ON CONFLICT DO NOTHING`, args...); err != nil {
		return fmt.Errorf("plantclaims replace %s: insert positions: %w", processID, err)
	}
	return nil
}

// WipeAll deletes every mirror row. Used by the full-snapshot rebuild path
// (a late-joining Core applies a full snapshot by wiping then re-inserting
// every process) and by the mirror-rebuild test. Safe on an empty mirror.
func WipeAll(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("plantclaims wipe: begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM style_claims`); err != nil {
		return fmt.Errorf("plantclaims wipe: claims: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM process_styles`); err != nil {
		return fmt.Errorf("plantclaims wipe: styles: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM process_positions`); err != nil {
		return fmt.Errorf("plantclaims wipe: positions: %w", err)
	}
	return tx.Commit()
}

// ProcessKey is the (process, style) pair — the sourceability computation key.
type ProcessKey struct {
	ProcessID string
	StyleID   string
}

// ProcessNodeRef is one Core node a process's claims name.
type ProcessNodeRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// ProcessNodeOption is one process and the Core nodes its claims resolve to —
// the shape a config editor needs to let a person pick a PROCESS while what gets
// stored is NODES.
type ProcessNodeOption struct {
	ProcessID string           `json:"process_id"`
	Nodes     []ProcessNodeRef `json:"nodes"`
}

// ListProcessNodeOptions returns every process in the mirror with the Core nodes
// its claims name, resolved to ids.
//
// It exists so a config screen can offer processes and PERSIST NODES. That
// asymmetry is the round-1 ruling and it is not cosmetic: a process is what a
// person points at, but a claim lives on the Edge and Core cannot read one at
// decision time — so a rule stored as "process P" would have nothing to evaluate
// against. Resolving here, at config time, is the only point where both halves
// are in hand.
//
// The consequence is stated rather than hidden: a claim set that changes AFTER a
// save leaves the stored node set describing the old topology. That is the same
// bargain every resolved-at-save reference in this system makes, and the answer
// is the editor showing what is stored, not a re-resolve nobody asked for.
//
// A process whose claims name nodes Core has never heard of contributes an entry
// with fewer nodes than it has claims; a process with no resolvable node at all
// is omitted, because an option that can select nothing is not an option.
func ListProcessNodeOptions(db *sql.DB) ([]ProcessNodeOption, error) {
	rows, err := db.Query(`
		SELECT DISTINCT sc.process_id, n.id, n.name
		FROM style_claims sc
		JOIN nodes n ON n.name = sc.core_node_name
		ORDER BY sc.process_id, n.name`)
	if err != nil {
		return nil, fmt.Errorf("plantclaims process node options: query: %w", err)
	}
	defer rows.Close()

	var out []ProcessNodeOption
	for rows.Next() {
		var processID, nodeName string
		var nodeID int64
		if err := rows.Scan(&processID, &nodeID, &nodeName); err != nil {
			return nil, fmt.Errorf("plantclaims process node options: scan: %w", err)
		}
		if n := len(out); n > 0 && out[n-1].ProcessID == processID {
			out[n-1].Nodes = append(out[n-1].Nodes, ProcessNodeRef{ID: nodeID, Name: nodeName})
			continue
		}
		out = append(out, ProcessNodeOption{
			ProcessID: processID,
			Nodes:     []ProcessNodeRef{{ID: nodeID, Name: nodeName}},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("plantclaims process node options: rows: %w", err)
	}
	return out, nil
}

// ProcessStations maps each process to the station that published it
// (process_styles.station_id). A process with no recorded station is absent:
// the sourceability feed sends those to every station.
func ProcessStations(db *sql.DB) (map[string]string, error) {
	rows, err := db.Query(`SELECT process_id, MAX(station_id) FROM process_styles
		WHERE station_id IS NOT NULL GROUP BY process_id`)
	if err != nil {
		return nil, fmt.Errorf("plantclaims process stations: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var process, station string
		if err := rows.Scan(&process, &station); err != nil {
			return nil, fmt.Errorf("plantclaims process stations: scan: %w", err)
		}
		out[process] = station
	}
	return out, rows.Err()
}

// ClaimingStationsSQL is the ONE spelling of "the stations whose processes
// claim the node nameExpr names", as a SELECT of station_id rows, de-duplicated
// (UNION). nameExpr is an SQL expression for the node's name: "$1" for a read
// by name, or a column of an outer row (the lifecycle announcements' RETURNING,
// bins.LifecycleAddressSQL). Two sources, both publishing station from
// process_styles.station_id:
//   - a style_claims row naming the node as its own position or as one of a
//     press-index cell's paired positions (a station binds at those too, after a
//     changeover creates the row: Lane H, H.3; DESIGN-H §4; DECISIONS D9; the
//     same predicate as the empty scan's, bins.emptyCarrierWhere);
//   - a process_positions row: a seat the process binds at that no claim names,
//     a loader window or home or an FG drain (the report's Positions; Lane 0
//     item 6, closing DESIGN-H §4's S1).
//
// The landing (ResolveLandingAddress), StationsClaimingNode and the lifecycle
// announcements all read it, so they cannot disagree about who owns a seat.
func ClaimingStationsSQL(nameExpr string) string {
	return `SELECT ps.station_id FROM style_claims sc
		  JOIN process_styles ps ON ps.process_id = sc.process_id AND ps.style_id = sc.style_id
		 WHERE ` + nameExpr + ` IN (sc.core_node_name, sc.paired_core_node, sc.second_paired_core_node)
		   AND ps.station_id IS NOT NULL
		UNION
		SELECT ps.station_id FROM process_positions pp
		  JOIN process_styles ps ON ps.process_id = pp.process_id
		 WHERE pp.core_node_name = ` + nameExpr + ` AND ps.station_id IS NOT NULL`
}

// LastArmEdgesSQL is the FROM/WHERE of the last arm's address: every registered
// Edge (RULINGS-C2c-run-through §4). Shared by the landing and the lifecycle
// announcements; the alias is er.
const LastArmEdgesSQL = `FROM edge_registry er WHERE er.station_id <> ''`

// StationsClaimingNode lists the stations whose processes claim a node, from
// the mirror (ClaimingStationsSQL), sorted. One statement. A node no process
// claims, or one claimed only by processes with no recorded station, lists
// none.
func StationsClaimingNode(db *sql.DB, nodeName string) ([]string, error) {
	rows, err := db.Query(`SELECT station_id FROM (`+ClaimingStationsSQL("$1")+`) c ORDER BY station_id`, nodeName)
	if err != nil {
		return nil, fmt.Errorf("plantclaims stations claiming %s: %w", nodeName, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var station string
		if err := rows.Scan(&station); err != nil {
			return nil, fmt.Errorf("plantclaims stations claiming %s: scan: %w", nodeName, err)
		}
		out = append(out, station)
	}
	return out, rows.Err()
}

// Querier is a *sql.DB or a *sql.Tx.
type Querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// AddressedStation is a station a landing could go to, with the schema
// version that station last registered with (edge_registry.schema_version).
// SchemaVersion is 0 for a station that has not registered or reported none.
type AddressedStation struct {
	StationID     string
	SchemaVersion int
}

// LandingAddress is what one read tells the landing about where it goes
// (Lane H, H.3): the stations whose processes claim the node, and the order's
// own material station (its station, or the one a station-unaware order
// projects to; empty when it has none), each with its schema version (H.7).
//
// Plant is the last arm's address (RULINGS-C2c-run-through §4, D19's reach
// gap): every registered Edge (edge_registry), filled ONLY when no station
// claims the node and the order has no station, and nil otherwise, so a
// landing that has an address pays nothing for it.
type LandingAddress struct {
	Claiming  []AddressedStation
	Order     AddressedStation
	OrderUUID string
	Plant     []AddressedStation
}

// ResolveLandingAddress reads LandingAddress for a carrier placed at nodeName
// by orderID (0 for a placement with no order). ONE statement, the "+1 mirror
// read per arrival at a non-storage node" the brief names: the claims mirror,
// the order's station and both stations' schema versions together, so the
// version check (H.7) and the fallback cost nothing more. The last arm's
// Edges (Plant) come in the same statement, so it costs nothing either.
//
// The order's station is spelled as engine.materialStation spells it: the
// order's own station unless it is station-unaware, then the station it
// projects to.
func ResolveLandingAddress(q Querier, nodeName string, orderID int64) (LandingAddress, error) {
	rows, err := q.Query(`
		WITH c AS (`+ClaimingStationsSQL("$1")+`),
		     o AS (SELECT CASE WHEN station_unaware THEN projects_to ELSE station_id END AS st, edge_uuid
		             FROM orders WHERE id = $2)
		SELECT 'claim', c.station_id, COALESCE(er.schema_version, 0), ''
		  FROM c LEFT JOIN edge_registry er ON er.station_id = c.station_id
		UNION ALL
		SELECT 'order', o.st, COALESCE(er.schema_version, 0), o.edge_uuid
		  FROM o LEFT JOIN edge_registry er ON er.station_id = o.st AND o.st <> ''
		UNION ALL
		SELECT 'plant', er.station_id, COALESCE(er.schema_version, 0), ''
		  `+LastArmEdgesSQL+`
		   AND NOT EXISTS (SELECT 1 FROM c)
		   AND NOT EXISTS (SELECT 1 FROM o WHERE COALESCE(o.st, '') <> '')
		ORDER BY 1, 2`, nodeName, orderID)
	if err != nil {
		return LandingAddress{}, fmt.Errorf("plantclaims landing address %s: %w", nodeName, err)
	}
	defer rows.Close()
	var out LandingAddress
	for rows.Next() {
		var kind, uuid string
		var st AddressedStation
		if err := rows.Scan(&kind, &st.StationID, &st.SchemaVersion, &uuid); err != nil {
			return LandingAddress{}, fmt.Errorf("plantclaims landing address %s: scan: %w", nodeName, err)
		}
		switch kind {
		case "order":
			out.Order, out.OrderUUID = st, uuid
		case "plant":
			out.Plant = append(out.Plant, st)
		default:
			out.Claiming = append(out.Claiming, st)
		}
	}
	return out, rows.Err()
}

// ClaimInboundSource is where the active claim at a station's core node draws
// from (style_claims.inbound_source): for a produce claim, its empties. Blank
// when the station's active style has no claim at the node, or the claim names
// no source. One read.
func ClaimInboundSource(db *sql.DB, stationID, coreNodeName string) (string, error) {
	var src string
	err := db.QueryRow(`SELECT sc.inbound_source
		FROM style_claims sc
		JOIN process_styles ps ON ps.process_id = sc.process_id AND ps.style_id = sc.style_id
		WHERE ps.is_active AND ps.station_id = $1 AND sc.core_node_name = $2 AND sc.inbound_source <> ''
		ORDER BY sc.process_id, sc.seq
		LIMIT 1`, stationID, coreNodeName).Scan(&src)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return src, err
}
