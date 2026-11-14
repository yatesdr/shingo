package scenesync

import (
	"fmt"
	"sort"
	"strings"

	"shingocore/store/scene"
)

// PointIndex turns the fleet's route point ids into the synced scene's point
// names (shared-map lead decision D5, 2026-11-14).
//
// RDS names a point on a planned route by its bare number ("12"), while the
// scene, and so every row the Core map draws, names it with a class prefix
// ("LM12", "AP12"): the number is the name's numeric suffix. The shared-map
// study matched the ids of three live routes to the 394 vertex names of one
// plant's scene this way with no two names sharing a number.
//
// Built from what scene sync STORED (not from what the fleet sent), so a name
// it hands out is a name the map has a row for. It is built once per sync and
// read on every robot poll, where a resolve is one map lookup per id.
//
// Vertex names are the endpoints of the stored edges plus the stored points
// that are not bin locations. A bin location (a row with a point_name) is a
// station placed on a point, not a point a route runs through: no route ever
// names one, so indexing one could only invent a clash with the point it
// sits on.
type PointIndex struct {
	// byID holds each number exactly one name ends in.
	byID map[string]string
	// clash holds each number two or more names end in, with those names
	// sorted. An id here resolves to nothing: guessing between them would
	// draw a route through the wrong point.
	clash map[string][]string
}

// NewPointIndex indexes the vertex names of a stored scene by numeric suffix.
// A name that is not letters followed by digits ("Press-In", "LM_12") is not
// indexed, so no id can resolve to it.
func NewPointIndex(points []*scene.Point, edges []*scene.Edge) *PointIndex {
	ix := &PointIndex{byID: map[string]string{}, clash: map[string][]string{}}
	for _, e := range edges {
		ix.add(e.FromName)
		ix.add(e.ToName)
	}
	for _, p := range points {
		if p.PointName == "" {
			ix.add(p.InstanceName)
		}
	}
	for _, names := range ix.clash {
		sort.Strings(names)
	}
	return ix
}

func (ix *PointIndex) add(name string) {
	id, ok := numericSuffix(name)
	if !ok {
		return
	}
	if names, clashed := ix.clash[id]; clashed {
		for _, n := range names {
			if n == name {
				return
			}
		}
		ix.clash[id] = append(names, name)
		return
	}
	prev, seen := ix.byID[id]
	switch {
	case !seen:
		ix.byID[id] = name
	case prev != name:
		delete(ix.byID, id)
		ix.clash[id] = []string{prev, name}
	}
}

// numericSuffix splits a name of ASCII letters then ASCII digits ("LM12") and
// returns the digits. The digits are compared as written: "LM012" is point
// "012", not point "12".
func numericSuffix(name string) (string, bool) {
	i := len(name)
	for i > 0 && name[i-1] >= '0' && name[i-1] <= '9' {
		i--
	}
	if i == 0 || i == len(name) {
		return "", false
	}
	for j := 0; j < i; j++ {
		c := name[j]
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
			return "", false
		}
	}
	return name[i:], true
}

// Resolve names a route's ids in the order given. Any id the index cannot
// name, because no name ends in it or because two do, resolves the WHOLE
// route to nil: a route with a hole drawn across it would show a robot
// driving somewhere it is not, so the map draws no route instead. bad is the
// first id that failed; Explain says why. A nil index has nothing to name
// from, and an empty route resolves to nil with ok true.
func (ix *PointIndex) Resolve(ids []string) (names []string, bad string, ok bool) {
	if len(ids) == 0 {
		return nil, "", true
	}
	if ix == nil {
		return nil, ids[0], false
	}
	names = make([]string, len(ids))
	for i, id := range ids {
		name, found := ix.byID[id]
		if !found {
			return nil, id, false
		}
		names[i] = name
	}
	return names, "", true
}

// Explain says why id does not resolve, for the one log line a robot gets
// when its route stops being drawable.
func (ix *PointIndex) Explain(id string) string {
	if ix == nil || len(ix.byID)+len(ix.clash) == 0 {
		return fmt.Sprintf("no synced scene to name point %q from", id)
	}
	if names, clashed := ix.clash[id]; clashed {
		return fmt.Sprintf("point %q could be any of %s", id, strings.Join(names, ", "))
	}
	return fmt.Sprintf("no scene point is numbered %q", id)
}
