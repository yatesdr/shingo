package robotconfidence

import (
	"database/sql"
	"strconv"
	"strings"
	"time"
)

// Geometry version on the aggregation key.
//
// WHY A DAY IS NO LONGER A FINE ENOUGH GRAIN. Maps at this plant are edited
// close to daily. An edit at 14:00 Tuesday leaves that lane with six hours of
// one geometry and ten of another, and a single Tuesday row averages across
// both — a blend presented as a measurement, undetectable by any reader,
// on the day the reader most wants to look. Carrying the version in the key
// splits it into one row per geometry, so the day a lane changed becomes the
// day it is most readable rather than least.

// VersionResolver supplies, per sample, the id of the lane-geometry version
// in force when the reading was taken.
//
// An interface rather than a direct dependency, so the two packages do not
// have to import each other. It is REQUIRED: version_id is NOT NULL, so a
// roll-up without a resolver would quarantine every sample and silently write
// nothing, and RollUp rejects that rather than degrading into it.
type VersionResolver interface {
	Load(db *sql.DB, from, to time.Time) (VersionLookup, error)
}

// VersionLookup answers the per-sample question.
type VersionLookup interface {
	At(area, lane string, at time.Time) *int64
}

// versionSep separates the lane key from its version. \x01 rather than \x00
// because Segment.Key already uses \x00 between area and lane, and reusing it
// would make the key ambiguous to split.
const versionSep = '\x01'

// versionedKey extends a lane key with the geometry version.
//
// THE NIL BRANCH IS A GUARD, NOT A LIVE CASE, and the distinction is worth
// stating because this comment used to claim the opposite. It said "absence is
// part of the key" and described a reading with no version as a real row — the
// model that existed while version_id was nullable. It is not that any more:
// RollUp counts an unversioned reading as UnversionedSamples and returns
// before it can reach a key, so no row written by this package carries a nil
// version. The branch stays because it is what makes the NOT NULL column safe
// if that ever stops being true; it is not a state the writer produces.
func versionedKey(laneKey string, versionID *int64) string {
	if versionID == nil {
		return laneKey + string(versionSep)
	}
	return laneKey + string(versionSep) + strconv.FormatInt(*versionID, 10)
}

// splitVersionedKey reverses versionedKey.
func splitVersionedKey(key string) (laneKey string, versionID *int64) {
	i := strings.LastIndexByte(key, versionSep)
	if i < 0 {
		return key, nil
	}
	laneKey = key[:i]
	rest := key[i+1:]
	if rest == "" {
		return laneKey, nil
	}
	v, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		// An unparseable version is treated as absent rather than guessed
		// at. It cannot happen from versionedKey's own output; if it ever
		// does, "we do not know which geometry" is the true answer.
		return laneKey, nil
	}
	return laneKey, &v
}

// AreaClassResolver supplies each declared zone's class as of an instant.
//
// An interface for the same reason VersionResolver is one: robotconfidence and
// sceneversion must not import each other, and the adapter that joins them
// lives in the store delegate that already knows both.
//
// OPTIONAL, UNLIKE THE VERSION RESOLVER, and the asymmetry is deliberate. A
// missing version resolver would quarantine every sample and write nothing —
// a total silent loss — so it is an error. A missing class resolver costs one
// descriptive column: the zone rows are still correct, they just cannot say
// which kind of zone they describe. Refusing to write a day's zone statistics
// because the map sync has not run yet would throw away the measurement to
// protect a label.
type AreaClassResolver interface {
	// ClassesAt returns area id -> class name for the zones in force at an
	// instant. Ids are NORMALISED to the map's zero-padded spelling.
	ClassesAt(db *sql.DB, at time.Time) (map[string]string, error)
}

// parsePGTextArray decodes Postgres's TEXT[] output form into a Go slice.
//
// WHY THIS EXISTS AT ALL. shingo-core reaches Postgres through pgx's
// database/sql shim, which binds a []string INTO a TEXT[] on write and cannot
// scan one back OUT: the value arrives as the literal string "{8,12}" and
// Scan refuses it. That asymmetry is a driver property, not something the type
// checker can answer, and it is exactly what the v78 round-trip test was
// written to pin — it pinned the write direction, which worked, and nothing
// read the column until the zone roll-up did.
//
// The alternatives were worse. array_to_string() in the query would work until
// an id contained the separator, which is "any separator is a convention with
// a collision waiting in it" — the argument that chose TEXT[] over a delimited
// string in the first place, re-introduced one layer down. Aggregating the
// zones in SQL with unnest() would mean the map-mismatch and reloc_status
// filters existed twice, in two languages, drifting apart.
//
// So the literal is parsed, properly, including the quoting Postgres applies
// when an element contains a comma, a quote, a backslash, a brace or
// whitespace. Area ids never need it today; a parser that only handles today
// is how this class of bug arrives.
func parsePGTextArray(s string) []string {
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return nil
	}
	body := s[1 : len(s)-1]
	if body == "" {
		return []string{}
	}
	out := []string{}
	var cur []byte
	inQuotes := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case inQuotes && c == '\\' && i+1 < len(body):
			i++
			cur = append(cur, body[i])
		case c == '"':
			inQuotes = !inQuotes
		case c == ',' && !inQuotes:
			out = append(out, string(cur))
			cur = cur[:0]
		default:
			cur = append(cur, c)
		}
	}
	return append(out, string(cur))
}
