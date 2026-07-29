// Package registry holds edge-station persistence for shingo-core.
//
// Phase 5 of the architecture plan moved the edge_registry CRUD +
// heartbeat upsert + stale-edge sweep out of the flat store/ package
// and into this sub-package. The outer store/ keeps a type alias
// (`store.EdgeRegistration = registry.Edge`) and one-line delegate
// methods on *store.DB so external callers see no API change.
//
// # THE STATEMENT SPLIT, which is the reason this file changed
//
// Until v66 one statement — INSERT ... ON CONFLICT(station_id) DO UPDATE —
// both CREATED a station and MUTATED it, driven entirely by a string the Edge
// supplied. Everything that went wrong followed from that single property:
// two Pis with the same configured name did not collide, they took turns
// owning one row, and the `hostname = excluded.hostname` clause deleted the
// only evidence there had been two.
//
// The fix is not a check. It is that CREATE and MUTATE are now different
// functions with different statements:
//
//	Enroll   — INSERT only. Mints identity. Never updates.
//	Register — UPDATE ... WHERE station_uid = $1. Never inserts.
//	           ZERO ROWS IS THE ANSWER: an unknown station is refused, and
//	           there is no path by which a register invents one.
//
// Once no statement can do both, "an unregistered machine silently became
// this station" is not something the code is capable of expressing. That is
// the difference between a guard and an assertion.
package registry

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
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

// ErrUnknownStation is returned by Register when no enrolled station carries
// the presented uid.
//
// It is a REFUSAL, not a failure. The register wrote nothing, and that is the
// whole point: an Edge that Core has never enrolled cannot bring a station row
// into existence by asserting a name at it.
var ErrUnknownStation = errors.New("registry: no enrolled station with that uid")

// ErrAlreadyEnrolled is returned by Enroll when the uid is taken. Enrolling is
// minting; minting the same identity twice is a bug in the caller, not an
// idempotent no-op to swallow.
var ErrAlreadyEnrolled = errors.New("registry: station uid already enrolled")

// Conflict reports that a register arrived from a machine other than the one
// this station is bound to.
//
// It is NOT an error type on purpose. Register still succeeds and still writes
// the row; see Register's comment for why refusing is the wrong shape.
type Conflict struct {
	StationUID string
	// Kind is "hostname" (v64: a different box registered) or "instance"
	// (v66: a process that had already been displaced came BACK).
	Kind string
	// Bound is the hostname that first registered this station, and the one
	// Register keeps. Never reassigned except by Rebind.
	Bound string
	// Claimant is the hostname that just registered and does not match.
	Claimant string
	// Count is how many conflicting registers this station has now seen,
	// including this one.
	Count      int64
	ConflictAt time.Time
}

func (c *Conflict) String() string {
	switch c.Kind {
	case "instance":
		return "station " + c.StationUID + " saw a previously-displaced edge process " +
			"register again (hostname " + c.Claimant + "). Two machines are alive on one " +
			"station uid — most likely two Pis flashed from one SD image, which share a " +
			"hostname and so are invisible to the hostname check (" +
			strconv.FormatInt(c.Count, 10) + " conflicting register(s) so far)"
	default:
		return "station " + c.StationUID + " is bound to hostname " + c.Bound +
			" but " + c.Claimant + " just registered as it (" +
			strconv.FormatInt(c.Count, 10) + " conflicting register(s) so far)"
	}
}

// NewStationUID mints an identity.
//
// OPAQUE ON PURPOSE, and the reason is the whole history of this change. The
// identifier that broke was legible — 'plant-a.line-1' looked like a name, so
// it acquired an editor, and editing it rewrote a key that six tables and a
// backup manifest were built on. Something a human cannot read is something a
// human does not reach for when they want to relabel a station; that is what
// display_name is for, and display_name is safe precisely because nothing is
// keyed on it.
//
// The prefix exists so the value is recognisable in a log line, a Kafka
// consumer group name and a yaml file without being interpretable.
//
// 16 hex characters = 64 bits. Not collision-resistant in the birthday sense
// the way a UUID is, and it does not need to be: Enroll INSERTs against a
// unique index, so a collision is a failed insert and a retry, not a silent
// merge. The uniqueness is enforced by the database, not assumed from the
// entropy — which is the distinction the BIGSERIAL this replaces got wrong in
// the other direction.
func NewStationUID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("mint station uid: %w", err)
	}
	return "stn-" + hex.EncodeToString(b), nil
}

// Enroll mints a station: a Core-owned uid, an operator-owned display name,
// and nothing else. INSERT ONLY — it cannot touch an existing row.
//
// ENROLLMENT IS WHERE THE OWNER'S CONSTRAINT IS ANSWERED, and it is answered
// by what Enroll deliberately does NOT do. There are two hardware events and
// only one of them is an enrollment:
//
//   - A NEW station arrives → Enroll → a fresh uid.
//   - The hardware for an EXISTING station is replaced → NOT an enrollment.
//     The operator reads the existing uid off Core and puts it on the new Pi.
//     History does not move because identity did not move, and there is no
//     migration to run, because nothing was ever keyed on the box.
//
// A first-boot UUID cannot serve the second case at all — not because it is a
// worse identifier but because it is NOT RE-ISSUABLE. Nobody but the dead SD
// card ever knew it. A Core-issued token is re-issuable by definition: Core is
// still holding it. That single property, and not opacity or length or
// collision resistance, is why the uid is minted here.
//
// stationID (the routing address) is set to the uid. They are the same value
// by design — Address.Station is a transport selector whose value IS the
// identity — and they are separate columns because every station-keyed row in
// this database already carries the string, so the day they diverge is a
// migration rather than a rename.
// ENROLLING IS ITSELF THE ACKNOWLEDGEMENT, so claimed_at is set here. An
// operator calling this has already answered "what is this station?" — that is
// what the display name they passed IS. Leaving it NULL would list every
// deliberately-created station as unclaimed, which would make the unclaimed
// state mean nothing within a day of anyone using it.
func Enroll(db *sql.DB, uid, displayName, stationID string) (*Edge, error) {
	if uid == "" {
		return nil, errors.New("registry: enroll requires a station uid")
	}
	if stationID == "" {
		stationID = uid
	}
	if displayName == "" {
		displayName = stationID
	}
	var e Edge
	err := db.QueryRow(`
		INSERT INTO edge_registry (station_uid, display_name, station_id, registered_at, status, claimed_at)
		VALUES ($1, $2, $3, NOW(), 'enrolled', NOW())
		ON CONFLICT DO NOTHING
		RETURNING id, station_uid, display_name, station_id, registered_at, status
	`, uid, displayName, stationID).Scan(
		&e.ID, &e.StationUID, &e.DisplayName, &e.StationID, &e.RegisteredAt, &e.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAlreadyEnrolled
	}
	if err != nil {
		return nil, err
	}
	log.Printf("registry: ENROLLED station uid=%s display=%q — put `station_uid: %s` in that Pi's "+
		"/etc/shingo/shingoedge.yaml and restart shingoedge", e.StationUID, e.DisplayName, e.StationUID)
	return &e, nil
}

// Introduce records an edge that arrived carrying a uid Core has never seen.
//
// THIS IS THE BRANCH THE ENROLLMENT DEPLOY DELETED, REBUILT SO IT IS SAFE.
// The deleted one called Enroll and produced a row indistinguishable from a
// station an operator had deliberately created — "an unregistered machine
// silently became this station", which is precisely what guard 2 exists to
// prevent. This one cannot do that, for two reasons that are worth separating:
//
//  1. THE ROW IS MARKED. claimed_at stays NULL, so the station is visibly
//     unacknowledged on every surface that lists it until a human says what it
//     is. Nothing is silent.
//  2. THE UID IS MINTED, NOT DERIVED. The old defect needed a string that was
//     THE SAME on every unconfigured edge — 'plant-a.line-1', composed from two
//     struct defaults. Two such Pis shared one row and took turns owning it.
//     An edge that draws 64 random bits collides with nothing; it can only ever
//     create its own row. So "a machine can create a station" was never the
//     hazard. "A machine can create THE SAME station as another machine" was,
//     and randomness closes it in a way no guard has to be remembered for.
//
// What an edge still cannot do is claim to BE an existing station. That needs
// the operator to put the existing uid on the box (or restore its backup), and
// no amount of self-introduction reaches it — which is the whole reason the uid
// is Core-issued and re-issuable in the first place.
//
// display_name is left empty deliberately. Enroll defaults it to the uid so an
// operator-created station always reads as something; here, empty is the signal
// that nobody has named this yet, and the Stations page says so.
func Introduce(db *sql.DB, uid, hostname, version string) (*Edge, error) {
	if uid == "" {
		return nil, errors.New("registry: introduce requires a station uid")
	}
	var e Edge
	err := db.QueryRow(`
		INSERT INTO edge_registry (station_uid, display_name, station_id, hostname, version,
		                           registered_at, status, bound_hostname, claimed_at)
		VALUES ($1, '', $1, $2, $3, NOW(), 'active', $2, NULL)
		ON CONFLICT DO NOTHING
		RETURNING id, station_uid, display_name, station_id, registered_at, status
	`, uid, hostname, version).Scan(
		&e.ID, &e.StationUID, &e.DisplayName, &e.StationID, &e.RegisteredAt, &e.Status)
	if errors.Is(err, sql.ErrNoRows) {
		// Raced with another register for the same uid. Not an error: the row
		// exists, which is all the caller needed.
		return nil, ErrAlreadyEnrolled
	}
	if err != nil {
		return nil, err
	}
	log.Printf("registry: UNCLAIMED STATION %s introduced itself from host %q. It is running and "+
		"its work is attributed to this uid. Say what it is on Core's Stations page — name it, or "+
		"if it REPLACES an existing station, put that station's uid on the box instead.",
		e.StationUID, hostname)
	return &e, nil
}

// Claim records a human's answer to "what is this station?".
//
// One column, one direction. It never un-claims: a station that has been
// acknowledged stays acknowledged, because the question it answers is about
// whether anybody ever looked, not about the station's current state.
func Claim(db *sql.DB, uid, displayName string) (bool, error) {
	res, err := db.Exec(`
		UPDATE edge_registry
		SET claimed_at = COALESCE(claimed_at, NOW()),
		    display_name = CASE WHEN $2 <> '' THEN $2 ELSE display_name END
		WHERE station_uid = $1
	`, uid, displayName)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if n > 0 {
		log.Printf("registry: station %s claimed as %q", uid, displayName)
	}
	return n > 0, err
}

// Register records that an already-enrolled station is up, and reports a
// binding conflict if the machine behind it is not the one holding the lease.
//
// UPDATE ONLY, AND ZERO ROWS IS A REFUSAL. This is guard 2 and it is
// structural rather than defensive: there is now no statement anywhere that
// both creates and mutates a registry row, so "a second machine silently
// became this station" is not expressible. The old upsert's whole failure —
// two Pis taking turns owning one row while `hostname = excluded.hostname`
// erased the evidence — required exactly the capability this statement no
// longer has.
//
// # THE LEASE, and why it is not the hostname check again
//
// v64's detector compares hostnames. It has a false NEGATIVE that matters more
// than its false positives, and it is the expansion path this plant actually
// plans to use: TWO PIS FLASHED FROM ONE SD IMAGE SHARE A HOSTNAME. They also,
// after v66, share the station_uid baked into that image. The hostname check
// sees one machine.
//
// instance is a random id the Edge generates ONCE PER PROCESS at boot, so:
//
//	one Pi restarting   → X, then Y, then Z — every value brand new
//	one Pi re-registering→ X, X, X — reconnects and admin re-syncs reuse it
//	two clones alive    → A, B, A, B — a displaced instance COMES BACK
//
// Only the third produces RECURRENCE, and a single machine cannot produce it
// at all: a fresh boot draws a value it has never used, and a live process
// reuses the one it already holds. So the lease moves silently on a new
// instance (that is a restart, or a deliberately re-issued uid on replacement
// hardware — both benign and both common) and alarms only on an instance
// returning after being displaced. One column of memory, prev_instance, is
// what buys the discrimination v64 could not make.
//
// # STILL A DETECTOR, NOT A GATE — and the reason survives the sharper signal
//
// Recurrence identifies that two machines are alive. It does not identify
// WHICH ONE IS WRONG, and it cannot: the two alternate, so refusing "the one
// that recurred" refuses each of them in turn and the plant ends up with no
// Edge at all. Refusal would also not be containment — the Edge gates nothing
// on registration succeeding, so a refusal changes what Core RECORDS, not what
// the second Pi DOES, while throwing away the demand-registry and cell-catalog
// re-derive that a legitimately-replaced Pi needs to function.
//
// ONE STATEMENT, NOT READ-THEN-WRITE, AND THE `prev` CTE IS WHY IT CAN BE.
// Two edges racing a read-compare-write would each read the other's row and
// neither would record anything. The obvious single-statement form does not
// work either: RETURNING evaluates against the POST-update row, so a naive
// `RETURNING (prev_instance = $3)` reads the value this very statement just
// wrote and is false exactly when the alarm should fire. The CTE takes the
// pre-update row under FOR UPDATE, the UPDATE joins to it, and both the CASE
// bodies and the RETURNING predicates read `prev.*` — one snapshot, one lock,
// one statement, and the comparison provably precedes the overwrite.
//
// EVERY BRANCH REQUIRES A NON-EMPTY INCOMING VALUE, because os.Hostname()
// failing on the Edge yields "" (heartbeat.go discards the error). An unknown
// hostname must read as "cannot judge" in BOTH directions: it must not raise a
// false alarm against the bound host, and it must not become the binding,
// which would then make the real hostname look like the intruder.
func Register(db *sql.DB, uid, hostname, instance, version string) (*Conflict, error) {
	var boundHost string
	var count int64
	var at sql.NullTime
	var hostConflict, instConflict bool
	err := db.QueryRow(`
		WITH prev AS (
			SELECT station_uid, bound_hostname, bound_instance, prev_instance, conflict_count
			  FROM edge_registry WHERE station_uid = $1 FOR UPDATE
		)
		UPDATE edge_registry e SET
			hostname = $2,
			version = $4,
			registered_at = NOW(),
			status = 'active',
			bound_hostname = CASE
				WHEN prev.bound_hostname = '' THEN $2 ELSE prev.bound_hostname END,
			prev_instance = CASE
				WHEN $3 <> '' AND prev.bound_instance NOT IN ('', $3)
				THEN prev.bound_instance ELSE prev.prev_instance END,
			bound_instance = CASE WHEN $3 <> '' THEN $3 ELSE prev.bound_instance END,
			bound_at = CASE
				WHEN $3 <> '' AND prev.bound_instance <> $3 THEN NOW() ELSE e.bound_at END,
			conflict_hostname = CASE
				WHEN prev.bound_hostname NOT IN ('', $2) AND $2 <> '' THEN $2
				ELSE e.conflict_hostname END,
			conflict_count = prev.conflict_count + CASE
				WHEN (prev.bound_hostname NOT IN ('', $2) AND $2 <> '')
				  OR ($3 <> '' AND prev.prev_instance = $3) THEN 1 ELSE 0 END,
			conflict_at = CASE
				WHEN (prev.bound_hostname NOT IN ('', $2) AND $2 <> '')
				  OR ($3 <> '' AND prev.prev_instance = $3) THEN NOW()
				ELSE e.conflict_at END
		FROM prev
		WHERE e.station_uid = prev.station_uid
		RETURNING prev.bound_hostname, e.conflict_count, e.conflict_at,
		          (prev.bound_hostname NOT IN ('', $2) AND $2 <> ''),
		          ($3 <> '' AND prev.prev_instance = $3)
	`, uid, hostname, instance, version).Scan(
		&boundHost, &count, &at, &hostConflict, &instConflict)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnknownStation
	}
	if err != nil {
		return nil, err
	}
	if !hostConflict && !instConflict {
		return nil, nil
	}
	c := &Conflict{StationUID: uid, Kind: "hostname", Bound: boundHost, Claimant: hostname, Count: count}
	if instConflict {
		c.Kind = "instance"
	}
	if at.Valid {
		c.ConflictAt = at.Time
	}
	// LOGGED HERE, NOT LEFT TO THE CALLER. The alarm is the entire value of the
	// detection, and an alarm a caller has to remember to raise is one a second
	// caller will not. The returned Conflict is for callers that can do MORE
	// than log — CoreDataService names the incumbent in the registration ack so
	// the line appears in the claimant's own journal too.
	log.Printf("registry: DUPLICATE EDGE IDENTITY — %s. "+
		"Two machines sharing one station uid share one registry row, one of them receives no "+
		"dispatch traffic (single-partition consumer group), and each register wipes the other's "+
		"demand routing and cell catalog. Enroll the second edge as its own station and put ITS "+
		"uid in that Pi's shingoedge.yaml; or if this station moved to a new box, rebind it "+
		"deliberately.", c)
	return c, nil
}

// Rebind moves a station's binding to a new machine and clears the conflict
// record. Reports whether a row matched.
//
// THIS EXISTS SO THE ALARM CAN BE TURNED OFF, and that is not a convenience.
// bound_hostname is never reassigned by a register, so after a legitimate box
// replacement every subsequent register mismatches and conflict_at stays
// permanently fresh. A signal that cannot be cleared is a signal people learn
// to ignore — the same latching defect this repository already fixed once in
// the Core Health db-waits gauge. Rebind is the sanctioned "yes, this station
// lives here now", and it is deliberately a separate, explicit act rather than
// something a register can do to itself.
//
// It clears prev_instance too: leaving the displaced instance behind would arm
// the recurrence detector against a machine that is now the only one there.
func Rebind(db *sql.DB, uid, hostname string) (bool, error) {
	res, err := db.Exec(`
		UPDATE edge_registry
		SET bound_hostname = $2, bound_instance = '', prev_instance = '', bound_at = NOW(),
		    conflict_hostname = '', conflict_count = 0, conflict_at = NULL
		WHERE station_uid = $1
	`, uid, hostname)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 0 {
		log.Printf("registry: station %s rebound to hostname %s (conflict record cleared)", uid, hostname)
	}
	return n > 0, nil
}

// SetDisplayName renames a station for humans. Reports whether a row matched.
//
// THE ENTIRE POINT OF THE MODEL IS THAT THIS IS SAFE. Before v66 the operator
// string and the identity were one column, so renaming a station rewrote the
// key under `orders`, `mission_telemetry`, `outbox`, `node_stations`,
// `cell_targets` and the Edge's own backup manifest — a plant stop caused by
// typing in a text box. display_name is read by nothing except a human, and it
// never crosses the wire as an identifier.
func SetDisplayName(db *sql.DB, uid, displayName string) (bool, error) {
	res, err := db.Exec(`UPDATE edge_registry SET display_name = $2 WHERE station_uid = $1`,
		uid, displayName)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// UpdateHeartbeat marks an enrolled station alive. Returns found=false when no
// enrolled station carries the uid.
//
// UPDATE ONLY — and this one matters as much as Register's. The old heartbeat
// ALSO upserted, and it set status='active', so refusing at Register alone
// would have been theatre: the unknown machine's row would appear anyway, one
// heartbeat later, with no hostname and no version to show for it. Guard 2 is
// "no statement both creates and mutates a registry row", and there were two
// such statements.
//
// found=false keeps the behaviour the old isNew flag drove: Core answers with
// edge.register_request. The difference is that the request is now the ONLY
// outcome, rather than a notification about a row heartbeating itself into
// existence.
func UpdateHeartbeat(db *sql.DB, uid string) (found bool, err error) {
	res, err := db.Exec(`
		UPDATE edge_registry SET last_heartbeat = NOW(), status = 'active'
		WHERE station_uid = $1
	`, uid)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

const edgeColumns = `id, station_uid, display_name, station_id, hostname, version,
	       registered_at, last_heartbeat, status,
	       bound_hostname, bound_instance, prev_instance, bound_at, claimed_at,
	       conflict_hostname, conflict_count, conflict_at`

func scanEdge(sc interface{ Scan(...any) error }) (Edge, error) {
	var e Edge
	err := sc.Scan(&e.ID, &e.StationUID, &e.DisplayName, &e.StationID, &e.Hostname, &e.Version,
		&e.RegisteredAt, &e.LastHeartbeat, &e.Status,
		&e.BoundHostname, &e.BoundInstance, &e.PrevInstance, &e.BoundAt, &e.ClaimedAt,
		&e.ConflictHostname, &e.ConflictCount, &e.ConflictAt)
	return e, err
}

// List returns all registered edges.
//
// The binding columns are selected because this is the read every existing
// surface already goes through — the nodes page data builder, the loader
// service, the JSON API. Carrying them on the row means a conflict is available
// wherever a station is displayed, without a second query anybody has to know
// to add.
func List(db *sql.DB) ([]Edge, error) {
	rows, err := db.Query(`SELECT ` + edgeColumns + ` FROM edge_registry ORDER BY display_name, station_uid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var edges []Edge
	for rows.Next() {
		e, err := scanEdge(rows)
		if err != nil {
			return nil, err
		}
		edges = append(edges, e)
	}
	return edges, rows.Err()
}

// GetByUID returns one enrolled station, or sql.ErrNoRows.
//
// This is the read the hardware-replacement procedure runs: an operator with a
// new Pi needs the EXISTING uid, and Core is the only thing that still has it.
func GetByUID(db *sql.DB, uid string) (*Edge, error) {
	e, err := scanEdge(db.QueryRow(`SELECT `+edgeColumns+` FROM edge_registry WHERE station_uid = $1`, uid))
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// MarkStale sets status='stale' for active edges whose last_heartbeat is
// older than the threshold. Returns the marked station uids.
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
//
// An enrolled-but-never-started station is not swept: status begins as
// 'enrolled', and only a register promotes it to 'active'. Sweeping it would
// mean marking stale a station that has correctly never claimed to be up.
func MarkStale(db *sql.DB, threshold time.Duration) ([]string, error) {
	rows, err := db.Query(`
		UPDATE edge_registry
		SET status = 'stale'
		WHERE status = 'active'
		  AND last_heartbeat IS NOT NULL
		  AND last_heartbeat < NOW() - $1::interval
		RETURNING station_uid
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
