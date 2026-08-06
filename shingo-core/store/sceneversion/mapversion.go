package sceneversion

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"shingocore/scenemap"
)

// The .smap archive and the area/reflector version streams.
//
// Everything here is on the ROBOT's transport, gated by current_map_md5, and
// therefore on its own timeline from the lane versions in sceneversion.go,
// which come from RDS's /scene gated by scene_md5. The diff row is what
// relates an edit seen on one to an edit seen on the other — an engineer who
// moved a lane and redrew a zone in one sitting did ONE edit, and without a
// shared diff id nothing could ever say so.

// MapSnapshot is one observed .smap.
type MapSnapshot struct {
	MapName     string
	MapMD5      string
	SourceRobot string
	// Raw is the bytes the robot sent, unaltered.
	Raw        []byte
	Parsed     *scenemap.Map
	ObservedAt time.Time
}

// MapSyncResult reports what one map observation did.
type MapSyncResult struct {
	DiffResult
	MapVersionID int64
	// Unchanged is true when the content hash matched the newest stored
	// version, so nothing was written at all.
	Unchanged bool
	// StoredBytes and CloudBytes are what the archive actually cost, after
	// compression and after the scan cloud was split out.
	StoredBytes int
	CloudBytes  int
	// EmptyReflectorAreas is the finding, counted on every sync: declared
	// reflector zones holding no reflectors. Nine at Springfield.
	EmptyReflectorAreas int
}

func (r MapSyncResult) String() string {
	if r.Unchanged {
		return "map unchanged"
	}
	return fmt.Sprintf("map_version=%d %s body=%dB cloud=%dB empty_reflector_areas=%d",
		r.MapVersionID, r.DiffResult.String(), r.StoredBytes, r.CloudBytes,
		r.EmptyReflectorAreas)
}

// ApplyMapSnapshot archives a .smap and versions its areas and reflectors.
//
// IDEMPOTENT ON CONTENT, NOT ON TIME. The same map observed twice writes one
// version row and no diff. That is what makes the diff log a record of edits
// rather than of polls, and it is why the content hash is taken over the RAW
// BYTES the robot sent rather than any re-marshalled form — a canonical
// re-encoding would change with Go's map iteration order and fire the change
// trigger on every single sync.
func ApplyMapSnapshot(db *sql.DB, snap MapSnapshot, previousSync *time.Time) (MapSyncResult, error) {
	var res MapSyncResult
	if snap.Parsed == nil {
		return res, fmt.Errorf("sceneversion: map snapshot has no parsed content")
	}
	sum := sha256.Sum256(snap.Raw)
	contentSHA := hex.EncodeToString(sum[:])

	// Already have this exact map?
	var existingID int64
	err := db.QueryRow(
		`SELECT id FROM scene_map_versions WHERE map_name=$1 AND content_sha=$2`,
		snap.MapName, contentSHA).Scan(&existingID)
	if err == nil {
		res.Unchanged = true
		res.MapVersionID = existingID
		return res, nil
	}
	if err != sql.ErrNoRows {
		return res, fmt.Errorf("sceneversion: look up map version: %w", err)
	}

	body, cloud, err := splitAndCompress(snap.Raw)
	if err != nil {
		return res, err
	}
	res.StoredBytes, res.CloudBytes = len(body), len(cloud)

	tx, err := db.Begin()
	if err != nil {
		return res, fmt.Errorf("sceneversion: begin: %w", err)
	}
	defer tx.Rollback()

	if err := tx.QueryRow(
		`INSERT INTO scene_diffs (source, gate_hash, observed_at, previous_sync)
		 VALUES ($1,$2,$3,$4) RETURNING id`,
		SourceRobotSmap, snap.MapMD5, snap.ObservedAt, previousSync).Scan(&res.DiffID); err != nil {
		return res, fmt.Errorf("sceneversion: open map diff: %w", err)
	}

	// Close the previous map version. Closed, never deleted — §F's whole
	// argument is that the map's history IS the finding, and the question
	// "when did these nine polygons appear" is only answerable if every
	// version before them survives.
	if _, err := tx.Exec(
		`UPDATE scene_map_versions SET superseded_at=$1
		  WHERE map_name=$2 AND superseded_at IS NULL`,
		snap.ObservedAt, snap.MapName); err != nil {
		return res, fmt.Errorf("sceneversion: supersede map version: %w", err)
	}

	if err := tx.QueryRow(
		`INSERT INTO scene_map_versions
		   (map_name, content_sha, map_md5, source_robot, body_gz, scan_cloud_gz,
		    raw_bytes, synced_at, diff_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
		snap.MapName, contentSHA, snap.MapMD5, snap.SourceRobot,
		body, cloud, len(snap.Raw), snap.ObservedAt, res.DiffID).Scan(&res.MapVersionID); err != nil {
		return res, fmt.Errorf("sceneversion: insert map version: %w", err)
	}

	added, changed, removed, deltas, empty, err := applyAreas(tx, snap, res.DiffID, res.MapVersionID)
	if err != nil {
		return res, err
	}
	res.Added, res.Changed, res.Removed = added, changed, removed
	res.EmptyReflectorAreas = empty

	ra, rc, rr, err := applyReflectors(tx, snap, res.DiffID, res.MapVersionID)
	if err != nil {
		return res, err
	}
	res.Added += ra
	res.Changed += rc
	res.Removed += rr

	res.MedianDelta, res.MaxDelta = medianAndMax(deltas)
	if _, err := tx.Exec(
		`UPDATE scene_diffs
		    SET objects_added=$1, objects_changed=$2, objects_removed=$3,
		        median_delta_m=$4, max_delta_m=$5
		  WHERE id=$6`,
		res.Added, res.Changed, res.Removed,
		nullable(res.MedianDelta, len(deltas)), nullable(res.MaxDelta, len(deltas)),
		res.DiffID); err != nil {
		return res, fmt.Errorf("sceneversion: complete map diff: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("sceneversion: commit: %w", err)
	}
	return res, nil
}

// applyAreas versions the declared areas.
func applyAreas(tx *sql.Tx, snap MapSnapshot, diffID, mapVersionID int64) (
	added, changed, removed int, deltas []float64, emptyReflectorAreas int, err error) {

	type openArea struct {
		id      int64
		defHash string
		polygon []scenemap.Point
	}
	open := map[string]openArea{}
	rows, err := tx.Query(
		`SELECT id, area_name, def_hash, polygon FROM scene_areas WHERE valid_to IS NULL`)
	if err != nil {
		return 0, 0, 0, nil, 0, fmt.Errorf("sceneversion: load open areas: %w", err)
	}
	for rows.Next() {
		var a openArea
		var name string
		var poly []byte
		if err := rows.Scan(&a.id, &name, &a.defHash, &poly); err != nil {
			rows.Close()
			return 0, 0, 0, nil, 0, err
		}
		if err := json.Unmarshal(poly, &a.polygon); err != nil {
			rows.Close()
			return 0, 0, 0, nil, 0, fmt.Errorf("sceneversion: decode polygon %s: %w", name, err)
		}
		open[name] = a
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, 0, nil, 0, err
	}

	seen := map[string]bool{}
	for _, a := range snap.Parsed.Areas {
		seen[a.Name] = true
		fp := scenemap.AreaFingerprint(a)
		reflectors := snap.Parsed.ReflectorsInside(a)
		if a.Class == scenemap.ClassReflectorArea && reflectors == 0 {
			emptyReflectorAreas++
		}
		prev, exists := open[a.Name]
		if exists && prev.defHash == fp.DefHash {
			continue
		}
		var supersedes *int64
		var delta *float64
		if exists {
			supersedes = &prev.id
			if d := scenemap.MaxVertexDelta(prev.polygon, a.Polygon); !isInf(d) {
				delta = &d
				deltas = append(deltas, d)
			}
			if _, err := tx.Exec(`UPDATE scene_areas SET valid_to=$1 WHERE id=$2`,
				snap.ObservedAt, prev.id); err != nil {
				return 0, 0, 0, nil, 0, fmt.Errorf("sceneversion: close area %s: %w", a.Name, err)
			}
			changed++
		} else {
			added++
		}
		polyJSON, _ := json.Marshal(a.Polygon)
		propsJSON, _ := json.Marshal(a.Properties)
		if _, err := tx.Exec(
			`INSERT INTO scene_areas
			   (area_name, class_name, polygon, reflector_count, color_pen, color_brush,
			    properties, shape_hash, def_hash, max_vertex_delta_m, supersedes_id,
			    diff_id, map_version_id, valid_from)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			a.Name, a.Class, polyJSON, reflectors, a.ColorPen, a.ColorBrush,
			propsJSON, fp.ShapeHash, fp.DefHash, delta, supersedes,
			diffID, mapVersionID, snap.ObservedAt); err != nil {
			return 0, 0, 0, nil, 0, fmt.Errorf("sceneversion: insert area %s: %w", a.Name, err)
		}
	}
	for name, prev := range open {
		if seen[name] {
			continue
		}
		if _, err := tx.Exec(`UPDATE scene_areas SET valid_to=$1 WHERE id=$2`,
			snap.ObservedAt, prev.id); err != nil {
			return 0, 0, 0, nil, 0, fmt.Errorf("sceneversion: close removed area %s: %w", name, err)
		}
		removed++
	}
	return added, changed, removed, deltas, emptyReflectorAreas, nil
}

// applyReflectors versions the reflector positions.
//
// IDENTITY IS THE POSITION, because the vendor gives a reflector no id and its
// index in the list is not stable across edits. So a reflector that moves is
// recorded as one removed and one added rather than as one changed — which is
// the honest description: nothing links the two, and claiming a link would be
// inventing one.
func applyReflectors(tx *sql.Tx, snap MapSnapshot, diffID, mapVersionID int64) (
	added, changed, removed int, err error) {

	open := map[string]int64{} // shape hash -> row id
	rows, err := tx.Query(
		`SELECT id, shape_hash FROM scene_reflectors WHERE valid_to IS NULL`)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("sceneversion: load open reflectors: %w", err)
	}
	for rows.Next() {
		var id int64
		var h string
		if err := rows.Scan(&id, &h); err != nil {
			rows.Close()
			return 0, 0, 0, err
		}
		open[h] = id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, 0, err
	}

	seen := map[string]bool{}
	for _, r := range snap.Parsed.Reflectors {
		fp := scenemap.ReflectorFingerprint(r)
		seen[fp.ShapeHash] = true
		if _, exists := open[fp.ShapeHash]; exists {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO scene_reflectors
			   (kind, x, y, width, shape_hash, diff_id, map_version_id, valid_from)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			r.Kind, r.X, r.Y, r.Width, fp.ShapeHash, diffID, mapVersionID,
			snap.ObservedAt); err != nil {
			return 0, 0, 0, fmt.Errorf("sceneversion: insert reflector: %w", err)
		}
		added++
	}
	for h, id := range open {
		if seen[h] {
			continue
		}
		if _, err := tx.Exec(`UPDATE scene_reflectors SET valid_to=$1 WHERE id=$2`,
			snap.ObservedAt, id); err != nil {
			return 0, 0, 0, fmt.Errorf("sceneversion: close removed reflector: %w", err)
		}
		removed++
	}
	return added, changed, removed, nil
}

// splitAndCompress separates the laser scan cloud from everything else and
// gzips both.
//
// THE CLOUD IS 85-87% OF THE BYTES AND THE LEAST LIKELY THING ANYONE ASKS FOR.
// Measured on Springfield's map: 7.31 MB total, of which normalPosList alone
// is essentially all of it, and the whole file gzips to 1.11 MB. Everything
// except the cloud — areas, reflectors, points, curves, annotation lines — is
// about 1 MB gzipped at the 5x map, which is a COMPLETE history for roughly
// 365 MB a year. Splitting them is what lets a byte cap age out the residue
// while the record itself is kept, instead of a cap that quietly governs both.
//
// BYTEA of gzipped bytes rather than JSONB: this column is never queried into
// — the parsed tables are what queries read — and JSONB stores per-object keys
// undeduplicated across a quarter of a million scan points, so it would be
// larger than the text it replaced and cost a parse on every insert.
func splitAndCompress(raw []byte) (body, cloud []byte, err error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		// Unparseable: archive it whole rather than lose it. A body we cannot
		// split is still evidence, and refusing to store it would discard the
		// one artifact that could explain why it was unparseable.
		gz, gerr := gzipBytes(raw)
		return gz, nil, gerr
	}
	cloudDoc := map[string]json.RawMessage{}
	for _, k := range []string{"normalPosList", "rssiPosList"} {
		if v, ok := doc[k]; ok {
			cloudDoc[k] = v
			delete(doc, k)
		}
	}
	bodyJSON, err := json.Marshal(doc)
	if err != nil {
		return nil, nil, fmt.Errorf("sceneversion: re-encode map body: %w", err)
	}
	if body, err = gzipBytes(bodyJSON); err != nil {
		return nil, nil, err
	}
	if len(cloudDoc) == 0 {
		return body, nil, nil
	}
	cloudJSON, err := json.Marshal(cloudDoc)
	if err != nil {
		return nil, nil, fmt.Errorf("sceneversion: re-encode scan cloud: %w", err)
	}
	cloud, err = gzipBytes(cloudJSON)
	return body, cloud, err
}

func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		return nil, fmt.Errorf("sceneversion: gzip: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("sceneversion: gzip close: %w", err)
	}
	return buf.Bytes(), nil
}
