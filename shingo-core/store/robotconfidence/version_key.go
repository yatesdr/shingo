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
// ABSENCE IS PART OF THE KEY, NOT A HOLE IN IT. A reading taken before the
// lane had a version is a real row and a distinct one from readings taken
// after; folding the two together would average across an edit, which is
// exactly the blend this split exists to prevent.
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
