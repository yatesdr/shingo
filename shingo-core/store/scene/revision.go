package scene

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"

	"shingo/protocol"
)

// Revision identifies one state of the scene, for the node-list sync's "send
// geometry only when it changed" short-circuit and the scene feed's digest.
//
// A SHA-256 OVER WHAT THE ROWS SAY: every column a sync writes, of every point
// and every edge, and neither a row's id nor its synced_at. Scene sync
// replaces an area by deleting its rows and inserting them again
// (ReplaceArea), which mints new ids and stamps synced_at = NOW() on every row
// whether anything moved or not; a revision over (id, synced_at) moved on
// every sync, and every Edge holding the scene was sent its whole geometry
// again. Over the content, the same scene written again is the same revision,
// and a row that moved, appeared or vanished is a new one. Pinned by
// TestRevision_IsAFunctionOfTheContentOnly and
// TestNodeFeed_TheSameSceneReSyncedIsNotSentAgain.
//
// EVERY COLUMN, NOT ONLY THE ONES THE NODE LIST CARRIES (names, classes,
// coordinates, handles). A column the projection starts carrying later is
// covered already; the price is one geometry send when a property the Edge
// does not hold changes. The rows are in memory for the name set anyway, so
// hashing them costs no query.
//
// NO SCENE IS ITS OWN REVISION. The Edge caches geometry only when a response
// carries both points and edges (domain.NewSceneGeometry), so a hash over a
// scene with neither, or with one and not the other, names a scene no Edge can
// ever hold: the Edge kept quoting what it last held, the scene feed never
// converged, and Core resent it on every guard window and flagged the station
// — on a plant that never had a map, and on one whose map was deleted. Such a
// scene is protocol.SceneRevisionNone: the Edge clears its geometry and holds
// that value, and the feed converges. Not "", which stays what a partial read
// mints (messaging/scene_slices.go) and changes nothing on the Edge.
//
// A FUNCTION OF THE CONTENT ONLY. Two Cores answering the same scene, one Core
// restarted, or one scene synced twice give the same revision, so an Edge's
// cached revision is comparable across Core lifetimes and syncs. It does not
// depend on read order, row ids, the clock or the process: each row is one
// line, its strings quoted and its numbers exact, the lines are sorted, and the
// hash is over the sorted lines.
func Revision(points []*Point, edges []*Edge) string {
	if len(points) == 0 || len(edges) == 0 {
		return protocol.SceneRevisionNone
	}
	lines := make([]string, 0, len(points)+len(edges))
	for _, p := range points {
		lines = append(lines, fmt.Sprintf("p %q %q %q %q %q %q %s %s %s %s %q",
			p.AreaName, p.InstanceName, p.ClassName, p.PointName, p.GroupName, p.Label,
			revisionFloat(p.PosX), revisionFloat(p.PosY), revisionFloat(p.PosZ), revisionFloat(p.Dir),
			p.PropertiesJSON))
	}
	for _, e := range edges {
		lines = append(lines, fmt.Sprintf("e %q %q %q %q %q %s %s %s %s %s %s %s %s",
			e.AreaName, e.InstanceName, e.ClassName, e.FromName, e.ToName,
			revisionFloat(e.FromX), revisionFloat(e.FromY), revisionFloat(e.ToX), revisionFloat(e.ToY),
			revisionHandle(e.Ctrl1X), revisionHandle(e.Ctrl1Y), revisionHandle(e.Ctrl2X), revisionHandle(e.Ctrl2Y)))
	}
	sort.Strings(lines)
	h := sha256.New()
	for _, l := range lines {
		fmt.Fprintln(h, l)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// revisionFloat is a number exactly: the shortest text that reads back as the
// same float64.
func revisionFloat(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// revisionHandle is a control handle: "-" for none, which no number prints as.
func revisionHandle(v *float64) string {
	if v == nil {
		return "-"
	}
	return revisionFloat(*v)
}
