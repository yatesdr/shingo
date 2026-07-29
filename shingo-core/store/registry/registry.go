// Package registry holds edge-station registry persistence for
// shingo-core.
//
// Phase 5 of the architecture plan moved the edge_registry CRUD +
// heartbeat upsert + stale-edge sweep out of the flat store/ package
// and into this sub-package. The outer store/ keeps a type alias
// (`store.EdgeRegistration = registry.Edge`) and one-line delegate
// methods on *store.DB so external callers see no API change.
package registry

import (
	"database/sql"
	"encoding/json"
	"log"
	"strconv"
	"time"

	"shingocore/domain"
)

// Edge represents one registered edge station. The struct lives in
// shingocore/domain (Stage 2A.2); this alias keeps the registry.Edge
// name used by every read helper, scan function, and Register /
// MarkStale call site in this package, plus the outer store/
// re-export and the page-data builder that surfaces registry status
// to the admin UI.
type Edge = domain.RegistryEdge

// Conflict reports that a register arrived from a machine other than the one
// this station id is bound to.
//
// It is NOT an error type on purpose. Register still succeeds and still writes
// the row; see Register's comment for why refusing is the wrong shape today.
type Conflict struct {
	StationID string
	// Bound is the hostname that first registered this station id, and the
	// one Register keeps. Never reassigned except by Rebind.
	Bound string
	// Claimant is the hostname that just registered and does not match.
	Claimant string
	// Count is how many mismatching registers this station has now seen,
	// including this one. A count that keeps climbing means two machines are
	// alive and taking turns; a count that stopped at a small number and an
	// old ConflictAt means the station moved boxes once.
	Count      int64
	ConflictAt time.Time
}

func (c *Conflict) String() string {
	return "station " + c.StationID + " is bound to hostname " + c.Bound +
		" but " + c.Claimant + " just registered as it (" +
		strconv.FormatInt(c.Count, 10) + " mismatching register(s) so far)"
}

// Register upserts an edge registration. If the station_id already
// exists, it updates the record and resets status to active.
//
// IT ALSO DETECTS TWO MACHINES CLAIMING ONE STATION ID, which is the thing this
// statement previously made unobservable. `hostname = excluded.hostname`
// overwrites the only evidence that a second machine exists, and station_id is
// UNIQUE, so a second Pi does not collide — it takes turns owning the row. Every
// downstream consequence follows from that one row being shared: HandleEdgeRegister
// re-derives demand_registry and re-stales edge_cells per station, so each
// register wipes the other machine's routing and catalog; production_tick_dedup
// is keyed on the station string; and because Kafka topics are created with one
// partition and the consumer group is derived from the station id, ONE OF THE TWO
// EDGES RECEIVES NO DISPATCH TRAFFIC AT ALL while still heartbeating,
// registering and publishing. There is no error anywhere in that picture. The
// conflict returned here is the only thing in the system that names the cause.
//
// A DETECTOR AND NOT A GATE, and the reason is the signal rather than caution.
// "Different hostname" is equally the signature of a legitimate hardware
// replacement — a reimaged Pi, a DHCP-derived name change, a rename — and Core
// cannot tell the two apart, because it has no enrollment step at which a human
// says which one this is. Refusing would make a replaced Pi into a plant with no
// Edge, recoverable only by hand-editing Core's Postgres. And in the other
// direction the signal has a false NEGATIVE that matters more: two Pis flashed
// from one SD image share a hostname, which is exactly how additional edges get
// stood up, and this detector is blind to that case. A signal that
// false-positives on the benign case and false-negatives on the expected one is
// not a gate. It is evidence, and it is recorded as evidence.
//
// SO THE REGISTER PROCEEDS EXACTLY AS BEFORE — same row written, same catalog and
// demand-registry work downstream. Behaviour is unchanged; only the evidence is
// added. Refusing the register, or even just skipping the downstream re-derive,
// would deny routing to a machine that might be the plant's only Edge.
//
// ONE STATEMENT, NOT READ-THEN-WRITE. Two edges racing a read-compare-write
// would each read the other's row and neither would record a conflict. The
// CASE expressions all read the PRE-UPDATE row (that is what the table name
// means inside DO UPDATE, as against `excluded`), so the comparison and the
// overwrite are the same atomic statement.
//
// EVERY BRANCH ALSO REQUIRES A NON-EMPTY INCOMING HOSTNAME, because
// os.Hostname() failing on the Edge yields an empty string (heartbeat.go
// discards the error), and an unknown hostname must read as "cannot judge"
// rather than as a different machine — in either direction. It must not raise a
// false alarm against the bound host, and it must not become the binding, which
// would then make the REAL hostname look like the intruder.
func Register(db *sql.DB, stationID, hostname, version string, lineIDs []string) (*Conflict, error) {
	lineJSON, _ := json.Marshal(lineIDs)

	var bound string
	var count int64
	var at sql.NullTime
	err := db.QueryRow(`
		INSERT INTO edge_registry (station_id, hostname, version, line_ids, registered_at, status, bound_hostname)
		VALUES ($1, $2, $3, $4, NOW(), 'active', $2)
		ON CONFLICT(station_id) DO UPDATE SET
			hostname = excluded.hostname,
			version = excluded.version,
			line_ids = excluded.line_ids,
			registered_at = excluded.registered_at,
			status = 'active',
			bound_hostname = CASE
				WHEN edge_registry.bound_hostname = '' THEN excluded.hostname
				ELSE edge_registry.bound_hostname END,
			conflict_hostname = CASE
				WHEN edge_registry.bound_hostname NOT IN ('', excluded.hostname)
				     AND excluded.hostname <> '' THEN excluded.hostname
				ELSE edge_registry.conflict_hostname END,
			conflict_count = edge_registry.conflict_count + CASE
				WHEN edge_registry.bound_hostname NOT IN ('', excluded.hostname)
				     AND excluded.hostname <> '' THEN 1
				ELSE 0 END,
			conflict_at = CASE
				WHEN edge_registry.bound_hostname NOT IN ('', excluded.hostname)
				     AND excluded.hostname <> '' THEN NOW()
				ELSE edge_registry.conflict_at END
		RETURNING bound_hostname, conflict_count, conflict_at
	`, stationID, hostname, version, string(lineJSON)).Scan(&bound, &count, &at)
	if err != nil {
		return nil, err
	}
	if hostname == "" || bound == "" || bound == hostname {
		return nil, nil
	}
	c := &Conflict{StationID: stationID, Bound: bound, Claimant: hostname, Count: count}
	if at.Valid {
		c.ConflictAt = at.Time
	}
	// LOGGED HERE, NOT LEFT TO THE CALLER. The alarm is the entire value of the
	// detection, and an alarm a caller has to remember to raise is one a second
	// caller will not. The returned Conflict is for callers that can do MORE
	// than log — CoreDataService names the incumbent in the registration ack so
	// the line appears in the claimant's own journal too.
	log.Printf("registry: DUPLICATE EDGE IDENTITY — %s. "+
		"Two machines configured with one station id share one registry row, one of them "+
		"receives no dispatch traffic (single-partition consumer group), and each register "+
		"wipes the other's demand routing and cell catalog. Give each edge a unique "+
		"namespace/line_id in shingoedge.yaml, or if this station moved to a new box, "+
		"rebind it deliberately.", c)
	return c, nil
}

// Rebind moves a station's hostname binding to a new machine and clears the
// conflict record. Reports whether a row matched.
//
// THIS EXISTS SO THE ALARM CAN BE TURNED OFF, and that is not a convenience.
// bound_hostname is never reassigned by a register, so after a legitimate box
// replacement every subsequent register mismatches and conflict_at stays
// permanently fresh. A signal that cannot be cleared is a signal people learn to
// ignore — the same latching defect this repository already fixed once in the
// Core Health db-waits gauge. Rebind is the sanctioned "yes, this station lives
// here now", and it is deliberately a separate, explicit act rather than
// something a register can do to itself.
//
// It is also the seam the eventual enrollment step lands on: enrollment is this
// operation plus an identity Core issues, with a human on the other end of it.
func Rebind(db *sql.DB, stationID, hostname string) (bool, error) {
	res, err := db.Exec(`
		UPDATE edge_registry
		SET bound_hostname = $2, conflict_hostname = '', conflict_count = 0, conflict_at = NULL
		WHERE station_id = $1
	`, stationID, hostname)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 0 {
		log.Printf("registry: station %s rebound to hostname %s (conflict record cleared)", stationID, hostname)
	}
	return n > 0, nil
}

// UpdateHeartbeat upserts last_heartbeat and sets status to active. If
// the edge hasn't registered yet, creates a minimal registry entry.
// Returns true if a new row was inserted (unregistered edge detected).
func UpdateHeartbeat(db *sql.DB, stationID string) (isNew bool, err error) {
	var exists bool
	db.QueryRow(`SELECT 1 FROM edge_registry WHERE station_id = $1`, stationID).Scan(&exists)

	_, err = db.Exec(`
		INSERT INTO edge_registry (station_id, last_heartbeat, status)
		VALUES ($1, NOW(), 'active')
		ON CONFLICT(station_id) DO UPDATE SET
			last_heartbeat = NOW(),
			status = 'active'
	`, stationID)
	return !exists, err
}

// List returns all registered edges.
//
// The binding columns are selected because this is the read every existing
// surface already goes through — the nodes page data builder, the loader
// service, the JSON API. Carrying them on the row means a conflict is available
// wherever a station is displayed, without a second query anybody has to know
// to add.
func List(db *sql.DB) ([]Edge, error) {
	rows, err := db.Query(`
		SELECT id, station_id, hostname, version, line_ids, registered_at, last_heartbeat, status,
		       bound_hostname, conflict_hostname, conflict_count, conflict_at
		FROM edge_registry ORDER BY station_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var edges []Edge
	for rows.Next() {
		var e Edge
		var lineJSON string
		if err := rows.Scan(&e.ID, &e.StationID, &e.Hostname, &e.Version, &lineJSON,
			&e.RegisteredAt, &e.LastHeartbeat, &e.Status,
			&e.BoundHostname, &e.ConflictHostname, &e.ConflictCount, &e.ConflictAt); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(lineJSON), &e.LineIDs)
		edges = append(edges, e)
	}
	return edges, rows.Err()
}

// MarkStale sets status='stale' for active edges whose last_heartbeat is
// older than the threshold. Returns the marked station IDs.
//
// THE CUTOFF IS COMPUTED BY POSTGRES. That is what `NOW() - $1::interval`
// is for, and it is the whole content of this change.
//
// UpdateHeartbeat writes `last_heartbeat = NOW()`, so the write side has
// always been on the database clock. Deriving the cutoff from time.Now()
// on the caller's host — which is what this did — compared two
// independent clocks and silently added the difference to the caller's
// threshold: an edge went stale after `threshold + (db_clock - host_clock)`
// rather than after `threshold`.
//
// That term is neither small nor bounded. Core's Postgres runs on a
// separate server at both plants, nothing disciplines the two clocks
// against each other, and the error has a direction on each side:
//
//   - database AHEAD of Core's host: edges go stale LATE, by the skew.
//     Detection is delayed, and the delay is invisible.
//   - database BEHIND Core's host: edges go stale EARLY. As the skew
//     approaches the configured threshold the effective threshold reaches
//     zero, and every edge — including one heartbeating normally — is
//     marked stale on every 60-second tick, which also reaps that
//     station's demand_registry rows in CoreHandler.staleEdgeLoop.
//
// The same comparison is what made TestCoverage_MarkStaleEdges (I6)
// flake, where the two clocks are the test container's and the host's and
// the entire margin was a 5 ms sleep. Verified by injecting a 50 ms offset
// into the old host-side cutoff: the test failed deterministically with
// `stale ids len = 0, want 2`. NEITHER the threshold's value NOR
// t.Parallel() was implicated — any threshold smaller than the skew
// behaves identically, and injecting a fake clock into Go would have left
// the skew exactly where it was.
func MarkStale(db *sql.DB, threshold time.Duration) ([]string, error) {
	rows, err := db.Query(`
		UPDATE edge_registry
		SET status = 'stale'
		WHERE status = 'active'
		  AND last_heartbeat IS NOT NULL
		  AND last_heartbeat < NOW() - $1::interval
		RETURNING station_id
	`, pgInterval(threshold))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var staleIDs []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, err
		}
		staleIDs = append(staleIDs, sid)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(staleIDs) == 0 {
		return nil, nil
	}
	return staleIDs, nil
}

// pgInterval renders a Go duration as a Postgres interval literal.
//
// Microseconds because that is Postgres' interval resolution. A
// sub-microsecond threshold therefore renders as `0 microseconds`, which
// still marks every heartbeat written by an earlier transaction: NOW() is
// the current transaction's start time, so an earlier transaction's NOW()
// is strictly less than it. That is a database ordering guarantee rather
// than a race, which is the difference this whole change is about.
//
// A negative duration renders as written rather than being clamped. A
// caller passing one means something by it, and quietly turning it into
// zero would hide the mistake instead of showing it.
func pgInterval(d time.Duration) string {
	return strconv.FormatInt(d.Microseconds(), 10) + " microseconds"
}
