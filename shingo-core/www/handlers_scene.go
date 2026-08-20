package www

// SEER scene-map read API, and the two handlers that trigger a scene sync.
//
// Split out of handlers_nodes.go (2026-08-19), which had welded three concerns
// into ~1,021 lines: node CRUD, this, and the test-data generator. Pure code
// movement along the existing function boundaries — no body changed, every
// receiver is still *Handlers in package www, and no route moved. Same shape as
// the 2026-07-23 handlers_bins.go split (dd84d33c), which has not regrown.
//
// What belongs here: reads of the vendor scene — points, marks, edges, areas,
// reflectors, diffs — the robot-group list derived from it, and the sync
// triggers. Node CRUD stays in handlers_nodes.go even where it reads a node the
// scene created; the question this file answers is what the FLOOR looks like,
// not what Shingo has recorded about it.

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"shingocore/domain"
	"shingocore/fleet"
)

func (h *Handlers) apiScenePoints(w http.ResponseWriter, r *http.Request) {
	class := r.URL.Query().Get("class")
	area := r.URL.Query().Get("area")

	var (
		points []*domain.ScenePoint
		err    error
	)
	switch {
	case class != "":
		points, err = h.engine.NodeService().ListScenePointsByClass(class)
	case area != "":
		points, err = h.engine.NodeService().ListScenePointsByArea(area)
	default:
		points, err = h.engine.NodeService().ListScenePoints()
	}
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, points)
}

// apiSceneMarks is the picker's view of the scene: the labelled points a robot
// can be told to drive to, slimmed and searchable.
//
// WHY NOT /map/points, which already lists them. That endpoint answers the MAP's
// question — it returns every column including properties_json, the raw vendor
// property blob, because the map draws from it. A plant's scene carries a lot of
// location marks, and a picker that has to download every one of them with its
// property blob attached to show a dropdown is paying for a payload it discards.
// This returns the four fields a human picks by and nothing else.
//
// SEARCH IS SERVER-SIDE and matches the name, the label and the class, because a
// human looking for a waiting point knows it by whichever of those they were told.
// The cap is a guard, not paging: a picker showing 200 candidates has already
// failed at helping, and the honest response to "too many" is to say so and let
// them type more, not to silently truncate.
//
// AN EMPTY SCENE IS NOT AN ERROR. A sim backend with no scene sync, or a plant
// before its first sync, returns an empty list — and the picker falls back to
// typed entry, which is also the emergency path when the marks are stale.
func (h *Handlers) apiSceneMarks(w http.ResponseWriter, r *http.Request) {
	points, err := h.engine.NodeService().ListScenePoints()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type mark struct {
		Name  string `json:"name"`  // what the fleet is told; the value written to the property
		Label string `json:"label"` // the human name from the vendor scene, often blank
		Class string `json:"class"` // LocationMark, ParkPoint, … — the vendor's own class
		Area  string `json:"area"`
	}

	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	const cap = 200
	out := make([]mark, 0, 32)
	matched := 0
	for _, p := range points {
		if p == nil || p.InstanceName == "" {
			continue // a point with no name cannot be sent to the fleet
		}
		if q != "" && !strings.Contains(strings.ToLower(p.InstanceName), q) &&
			!strings.Contains(strings.ToLower(p.Label), q) &&
			!strings.Contains(strings.ToLower(p.ClassName), q) {
			continue
		}
		matched++
		if len(out) < cap {
			out = append(out, mark{
				Name:  p.InstanceName,
				Label: p.Label,
				Class: p.ClassName,
				Area:  p.AreaName,
			})
		}
	}

	h.jsonOK(w, map[string]any{
		"marks":     out,
		"matched":   matched,
		"truncated": matched > len(out),
	})
}

// apiSceneEdges returns the drivable path segments (advanced curves)
// synced from the fleet scene. Consumed by the robot-map dashboard to
// draw the travel network and route robots along real aisles instead of
// proximity-derived links.
func (h *Handlers) apiSceneEdges(w http.ResponseWriter, r *http.Request) {
	edges, err := h.engine.NodeService().ListSceneEdges()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, edges)
}

// ── The scene's own structure: areas and reflectors ────────────────────────
//
// Deliberately BESIDE /api/map/points and /api/map/edges rather than under a
// page-specific prefix. Structure is not owned by whichever page first needed
// it: the moment areas live under a confidence URL, the second consumer either
// re-implements them or imports a confidence endpoint to draw a wall.
//
// Both take an optional ?at=<RFC3339> and default to now. The parameter is the
// point — the scene is versioned, and a reader asking what the map looked like
// last Tuesday must not be silently answered with today's.

// atParam resolves the instant a scene query is asked about.
//
// A malformed timestamp is an ERROR, not a silent fallback to now. Quietly
// answering a different question than the one asked is how a reader ends up
// comparing this week's geometry against last week's numbers and concluding
// something moved.
func atParam(r *http.Request) (time.Time, error) {
	raw := r.URL.Query().Get("at")
	if raw == "" {
		return time.Now(), nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("at: %q is not an RFC3339 timestamp", raw)
	}
	return t, nil
}

// apiSceneAreas returns the declared map areas in force at an instant.
//
// The class is the field to render. Measured, the count of reflectors inside a
// zone has no predictive power over its no-estimate rate and the sign runs
// backwards; what predicts is whether it is a ReflectorArea. The count travels
// as provenance — "this declared reflector zone contains zero reflectors" is
// the most actionable sentence this work produced — and must not drive a mark.
func (h *Handlers) apiSceneAreas(w http.ResponseWriter, r *http.Request) {
	at, err := atParam(r)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	areas, err := h.engine.NodeService().SceneAreasAt(at)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, areas)
}

// apiSceneReflectors returns reflector positions in force at an instant.
func (h *Handlers) apiSceneReflectors(w http.ResponseWriter, r *http.Request) {
	at, err := atParam(r)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	reflectors, err := h.engine.NodeService().SceneReflectorsAt(at)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, reflectors)
}

// apiSceneDiffs returns the map change log, newest first, each row carrying
// the lanes it touched.
//
// This is what replaces a curated findings list on the diagnostic page: a
// standing narrative goes stale within a week, while "what changed, and to
// what" is the question an engineer actually arrives with.
func (h *Handlers) apiSceneDiffs(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	diffs, err := h.engine.NodeService().RecentSceneDiffsWithLanes(limit)
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.jsonOK(w, diffs)
}
func (h *Handlers) handleNodeSyncFleet(w http.ResponseWriter, r *http.Request) {
	total, created, deleted, err := h.orchestration.SceneSync()
	if err != nil {
		log.Printf("node sync: %v", err)
	} else {
		log.Printf("node sync: %d scene points, created %d, deleted %d nodes", total, created, deleted)
	}
	http.Redirect(w, r, "/nodes", http.StatusSeeOther)
}
func (h *Handlers) handleSceneSync(w http.ResponseWriter, r *http.Request) {
	syncer, ok := h.engine.Fleet().(fleet.SceneSyncer)
	if !ok {
		log.Printf("scene sync: fleet backend does not support scene sync")
		http.Redirect(w, r, "/nodes", http.StatusSeeOther)
		return
	}
	areas, err := syncer.GetSceneAreas()
	if err != nil {
		log.Printf("scene sync: fleet error: %v", err)
		http.Redirect(w, r, "/nodes", http.StatusSeeOther)
		return
	}
	total, locationSet := h.orchestration.SyncScenePoints(areas)
	h.orchestration.UpdateNodeZones(locationSet, false)
	log.Printf("scene sync: %d points synced", total)
	http.Redirect(w, r, "/nodes", http.StatusSeeOther)
}

// apiRobotGroups lists the fleet's robot-dispatch groups for the payload-editor
// picker. Degrades gracefully on purpose: an RDS outage or a backend with no
// scene (e.g. the simulator) returns available=false + an empty list at 200, so
// the payload form falls back to free-text — the saved robot_group lives in
// Postgres and must never be lost just because the picker couldn't load.
func (h *Handlers) apiRobotGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := h.engine.RobotGroups()
	if err != nil {
		h.jsonOK(w, map[string]any{"available": false, "groups": []fleet.RobotGroup{}})
		return
	}
	h.jsonOK(w, map[string]any{"available": true, "groups": groups})
}
