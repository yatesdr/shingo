package store

// Edge persistent cache of the vendor map's geometry — the coordinates of
// every scene point and the shape of every drivable segment, delivered by
// Core on the node-list sync when the Edge's revision is stale. Persistent for
// the same reason core_loaders is: an Edge that reboots during a Core
// partition keeps the map it last held, so the station's picture does not go
// blank until Core is reachable again. Replaced wholesale, in one transaction,
// and only by a response domain.NewSceneGeometry accepted as complete — that
// check runs in front of this write, so a name-only or half-read response
// never reaches it.

import (
	"database/sql"
	"fmt"

	"shingoedge/domain"
)

// ReplaceSceneGeometry replaces the cached scene atomically: every row of the
// three tables goes, the new scene is written, the revision is recorded. On
// any error the transaction rolls back and the previous scene stands.
func (db *DB) ReplaceSceneGeometry(g *domain.SceneGeometry) error {
	if g == nil || g.Revision == "" {
		return fmt.Errorf("replace scene geometry: no revision")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, t := range []string{"scene_geometry_edges", "scene_geometry_points", "scene_geometry_meta"} {
		if _, err := tx.Exec("DELETE FROM " + t); err != nil {
			return fmt.Errorf("clear %s: %w", t, err)
		}
	}
	for _, p := range g.Points {
		if _, err := tx.Exec(
			`INSERT INTO scene_geometry_points (instance_name, class_name, pos_x, pos_y, dir) VALUES (?,?,?,?,?)`,
			p.InstanceName, p.ClassName, p.X, p.Y, p.Dir,
		); err != nil {
			return fmt.Errorf("insert scene point %s: %w", p.InstanceName, err)
		}
	}
	for _, e := range g.Edges {
		var c1x, c1y, c2x, c2y any
		if e.Handles != nil {
			c1x, c1y, c2x, c2y = e.Handles[0], e.Handles[1], e.Handles[2], e.Handles[3]
		}
		if _, err := tx.Exec(
			`INSERT INTO scene_geometry_edges (from_name, to_name, from_x, from_y, to_x, to_y, ctrl1_x, ctrl1_y, ctrl2_x, ctrl2_y)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			e.From, e.To, e.FromX, e.FromY, e.ToX, e.ToY, c1x, c1y, c2x, c2y,
		); err != nil {
			return fmt.Errorf("insert scene edge %s->%s: %w", e.From, e.To, err)
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO scene_geometry_meta (id, revision, synced_at) VALUES (1, ?, datetime('now'))`, g.Revision,
	); err != nil {
		return fmt.Errorf("record scene revision: %w", err)
	}
	return tx.Commit()
}

// LoadSceneGeometry reads the cached scene, or nil when no complete response
// has ever been cached. nil, not an empty scene: an empty scene has a blank
// revision the heartbeater would quote and Core would honour with names only,
// and the picture would never get its geometry.
func (db *DB) LoadSceneGeometry() (*domain.SceneGeometry, error) {
	var revision string
	err := db.QueryRow(`SELECT revision FROM scene_geometry_meta WHERE id = 1`).Scan(&revision)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load scene revision: %w", err)
	}
	if revision == "" {
		return nil, nil
	}
	g := &domain.SceneGeometry{Revision: revision, Points: map[string]domain.ScenePointGeom{}}

	prows, err := db.Query(`SELECT instance_name, class_name, pos_x, pos_y, dir FROM scene_geometry_points`)
	if err != nil {
		return nil, fmt.Errorf("load scene points: %w", err)
	}
	for prows.Next() {
		var p domain.ScenePointGeom
		if err := prows.Scan(&p.InstanceName, &p.ClassName, &p.X, &p.Y, &p.Dir); err != nil {
			prows.Close()
			return nil, fmt.Errorf("scan scene point: %w", err)
		}
		g.Points[p.InstanceName] = p
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return nil, err
	}

	erows, err := db.Query(`SELECT from_name, to_name, from_x, from_y, to_x, to_y, ctrl1_x, ctrl1_y, ctrl2_x, ctrl2_y
		FROM scene_geometry_edges ORDER BY from_name, to_name`)
	if err != nil {
		return nil, fmt.Errorf("load scene edges: %w", err)
	}
	defer erows.Close()
	for erows.Next() {
		var e domain.SceneEdgeGeom
		var c1x, c1y, c2x, c2y sql.NullFloat64
		if err := erows.Scan(&e.From, &e.To, &e.FromX, &e.FromY, &e.ToX, &e.ToY, &c1x, &c1y, &c2x, &c2y); err != nil {
			return nil, fmt.Errorf("scan scene edge: %w", err)
		}
		// All four or none — the rule the write path keeps, restated on the
		// read so a hand-edited row cannot come back as a curve with an
		// invented coordinate.
		if c1x.Valid && c1y.Valid && c2x.Valid && c2y.Valid {
			e.Handles = &[4]float64{c1x.Float64, c1y.Float64, c2x.Float64, c2y.Float64}
		}
		g.Edges = append(g.Edges, e)
	}
	return g, erows.Err()
}
