package service

import (
	"fmt"
	"strings"

	"shingocore/fleet"
	"shingocore/store/nodes"
)

// jackStateLoadedInPlace / jackStateUnloadedInPlace are the two AT-REST values
// of the deck's state machine (Robokit API Protocol §11027). The moving values
// (0 loading, 2 unloading) and 4/0xFF are deliberately not named: nothing here
// should act on a deck mid-travel.
const (
	jackStateLoadedInPlace   = 1
	jackStateUnloadedInPlace = 3
)

// liftLoadedThresholdM is the fallback proxy for a fleet that reports no
// jack_state. Measured at Springfield: a loaded deck reads 0.0601 m and an
// empty one reads ≈ -0.0001 m, so anything above half a centimetre is a bin.
//
// A PROXY, NOT THE SIGNAL. JackState is read first wherever it is present,
// because a height is a measurement of a mast and a state is the vendor's own
// answer to the question being asked.
const liftLoadedThresholdM = 0.005

// RobotCarryingBin reports whether a robot's deck has something on it, and
// whether that reading is trustworthy enough to act on.
//
// ok is false while the deck is MOVING (jack_state 0 or 2) or in its error
// states, because a bin halfway down is neither on the robot nor at the
// station, and either answer would be a guess. The stranded-bin inference falls
// to its ambiguous branch rather than picking one.
func RobotCarryingBin(r fleet.RobotStatus) (carrying, ok bool) {
	switch r.JackState {
	case jackStateLoadedInPlace:
		return true, true
	case jackStateUnloadedInPlace:
		return false, true
	case 0:
		// No jack_state on the wire at all — an older RDS, or a fleet whose
		// robots do not report one. Fall back to the height proxy rather than
		// reading the zero value as "loading".
		if !r.JackIsFull && !r.IsLoaded && r.LiftHeight == 0 {
			return false, false
		}
		return r.LiftHeight > liftLoadedThresholdM || r.JackIsFull || r.IsLoaded, true
	default:
		// Moving, stopped mid-travel, or failed.
		return false, false
	}
}

// ── Resolving a reported point to a place a bin can be put ─────────────────
//
// THE IDENTITY MAPPING IS THE SIMULATOR'S CASE, NOT THE PLANT'S. This used to
// say the mapping was identity because Core sends the node's own dot-name as
// the RDS block Location, so the point names RDS reports back are Core node
// names. That is true of fleet/simulator, which publishes node names straight
// back (fleet/simulator/parity.go), and it is why every branch-A test passes.
// It has never been true at Springfield: `rbk_report.current_station` there
// reports MAP FURNITURE — AP102, CP37, PP95, LM9 — and not one of the nine
// distinct live values has ever been a Core node name. Identity is tried
// first because it costs one lookup and it is right for the simulator and for
// any fleet that names its points after its stations; the SCENE ALIAS is what
// answers at the plant.
//
// The alias is `scene_points`: a GeneralLocation row's `point_name` is what
// the robot reports and its `instance_name` is the station. Keyed on class
// membership, never on the `AP` prefix — see scene.StationsForPointName.
//
// LastStation is tried after CurrentStation because CurrentStation is empty
// while a robot is between points, which is most of the time it is moving. A
// robot that is parked reports both. The order is PER NAME, not per stage: a
// CurrentStation only the alias can read must beat a LastStation that happens
// to resolve by identity, because the two names describe different moments and
// the newer one is the one being asked about.

// refusalKind is why a reported point did not name a place a bin can be put.
//
// A CODE, NOT A SENTENCE. The resolve path runs on every two-second poll for
// every stranded bin, and the English costs a query (the point's class, or
// whether a scene has ever been synced). It is rendered only where it is read
// — the operator's anomaly note — by DescribeUnresolvedPoints.
type refusalKind int

const (
	refusalNone refusalKind = iota
	// refusalUnknown: nothing in Core names this point. Either it is not a
	// bin location on the synced map, or no scene has ever been synced.
	refusalUnknown
	// refusalAmbiguous: the point names a station in more than one area, and
	// nothing on the wire says which. Fails closed — see the note below.
	refusalAmbiguous
	// refusalSynthetic: it resolves to bookkeeping (_TRANSIT, a carrier node,
	// a node group), which is not a place on the floor.
	refusalSynthetic
	// refusalDisabled: it resolves to a real node the plant has switched off.
	// SMN_001 is live proof this is needed: enabled=false at Springfield, and
	// branch A would have placed a bin on it.
	refusalDisabled
	// refusalLookup: the scene or node table could not be read.
	refusalLookup
)

// pointRefusal carries what the sentence needs without rendering it.
type pointRefusal struct {
	Kind refusalKind
	// Point is the name the robot reported.
	Point string
	// Station is the node the point resolved to, when it resolved to one that
	// was then refused.
	Station string
	// Candidates is every station an ambiguous point names.
	Candidates []string
	// Detail carries a read error's own words.
	Detail string
}

// ResolveRobotStation maps the RDS point a robot is sitting at to a Core node.
//
// The signature is unchanged so its other caller — the carried-bin recovery's
// tier 3 (engine/carried_bin_recovery.go) — inherits the alias untouched. That
// tier has never been reachable at the plant, because the point a parked robot
// reports never resolved; it is reachable now.
func ResolveRobotStation(s *NodeService, r fleet.RobotStatus) (*nodes.Node, bool) {
	node, _, ok := ResolveReportedPoints(s, r.CurrentStation, r.LastStation)
	return node, ok
}

// ResolveReportedPoints resolves point names the caller supplies, in the order
// supplied, and reports which one answered.
//
// Taking the names rather than the whole status is what lets a FROZEN
// observation resolve by exactly the rules a live one does — the drop sample is
// kept raw and re-resolved on every tick, so a scene sync that lands after the
// bin was set down can still rescue it (engine's dropObservation).
func ResolveReportedPoints(s *NodeService, names ...string) (*nodes.Node, string, bool) {
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if node, _ := resolvePoint(s, name); node != nil {
			return node, name, true
		}
	}
	return nil, "", false
}

// resolvePoint is the whole rule for ONE reported name: identity, then the
// scene alias, with every refusal named rather than collapsed into "no".
func resolvePoint(s *NodeService, name string) (*nodes.Node, pointRefusal) {
	node, identityWhy := placeableNode(s, name)
	if node != nil {
		return node, pointRefusal{}
	}

	stations, err := s.StationsForPointName(name)
	switch {
	case err != nil:
		return nil, pointRefusal{Kind: refusalLookup, Point: name, Detail: err.Error()}
	case len(stations) > 1:
		// FAILS CLOSED, and it has to. Instance names are unique per AREA, so
		// two mapped areas may each bind an AP102, and nothing on the wire
		// disambiguates them: RDS's basic_info.current_area is not mapped onto
		// fleet.RobotStatus, and rbk_report.area_ids is a different namespace.
		// Picking one would be a coin toss that writes a bin's location.
		return nil, pointRefusal{Kind: refusalAmbiguous, Point: name, Candidates: stations}
	case len(stations) == 1:
		aliased, why := placeableNode(s, stations[0])
		if aliased != nil {
			return aliased, pointRefusal{}
		}
		why.Point, why.Station = name, stations[0]
		return nil, why
	}

	// No alias row at all. If identity found a node and refused it, that
	// refusal is the specific answer and is worth more than "unknown".
	if identityWhy.Kind != refusalUnknown {
		identityWhy.Point, identityWhy.Station = name, name
		return nil, identityWhy
	}
	return nil, pointRefusal{Kind: refusalUnknown, Point: name}
}

// placeableNode loads a node by name and owns EVERY refusal a resolved node
// can earn, so identity and the alias cannot disagree about what is placeable.
//
// Synthetic is refused because _TRANSIT and the per-robot carrier nodes are
// bookkeeping, not places, and resolving a station onto one would move a bin to
// a location that does not exist on the floor. Disabled is refused because a
// node the plant has switched off is not somewhere the floor will look —
// usableDropPoint has always checked it and branch A never did.
func placeableNode(s *NodeService, name string) (*nodes.Node, pointRefusal) {
	node, err := s.GetByName(name)
	if err != nil || node == nil {
		return nil, pointRefusal{Kind: refusalUnknown}
	}
	switch {
	case node.IsSynthetic:
		return nil, pointRefusal{Kind: refusalSynthetic, Station: node.Name}
	case !node.Enabled:
		return nil, pointRefusal{Kind: refusalDisabled, Station: node.Name}
	}
	return node, pointRefusal{}
}

// DescribeUnresolvedPoints renders, for a person, why the points a robot
// reported did not name a place — the sentence branch C puts on the bin.
//
// ON THE REFUSAL PATH ONLY. It asks the scene what a point IS (a charge point,
// a park point) and, when nothing is known at all, whether a scene has ever
// been synced — two questions worth a query when someone is about to walk the
// floor, and worth none on the tick that places a bin.
//
// NO DISTANCES, AND NO NEAREST-STATION MATH, HERE OR ANYWHERE. Twenty-five of
// Springfield's station pairs sit within 2.0 m of each other, and the bin-37
// drop was 2.094 m from SMN_007 and 24.9 m from where its order was going — a
// confident-looking figure for a completely unrelated station. A number that
// looks like evidence and is not is worse than no number.
func DescribeUnresolvedPoints(s *NodeService, names ...string) string {
	// ONE CLAUSE PER DISTINCT POINT. A PARKED robot reports the same value in
	// CurrentStation and LastStation — which is most of the population this
	// sentence is written for — and describing both produced "PP41 is a park
	// point, not a station; PP41 is a park point, not a station". strandedNote
	// has always deduped the same pair; this did not, and since the note now
	// renders on the bins page it was a stutter on the row an operator reads.
	var refusals []pointRefusal
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if _, why := resolvePoint(s, name); why.Kind != refusalNone {
			refusals = append(refusals, why)
		}
	}
	if len(refusals) == 0 {
		return "the robot reported no station at all"
	}
	if neverSynced(s, refusals) {
		return "Core has never synced a scene from the fleet, so no point a robot " +
			"reports can name a station"
	}
	parts := make([]string, 0, len(refusals))
	for _, why := range refusals {
		parts = append(parts, describeRefusal(s, why))
	}
	return strings.Join(parts, "; ")
}

// neverSynced distinguishes "that point is not a station" from "Core has no
// map to check against". Only asked when every refusal was a plain unknown —
// an ambiguous or disabled point proves the scene is there.
func neverSynced(s *NodeService, refusals []pointRefusal) bool {
	for _, why := range refusals {
		if why.Kind != refusalUnknown {
			return false
		}
	}
	n, err := s.CountStationPoints()
	return err == nil && n == 0
}

func describeRefusal(s *NodeService, why pointRefusal) string {
	switch why.Kind {
	case refusalAmbiguous:
		return fmt.Sprintf("%s names a station in more than one area (%s), and nothing "+
			"the robot reports says which", why.Point, strings.Join(why.Candidates, ", "))
	case refusalSynthetic:
		return fmt.Sprintf("%s resolves to %s, which is bookkeeping rather than a place "+
			"on the floor", why.Point, why.Station)
	case refusalDisabled:
		return fmt.Sprintf("%s resolves to %s, which is disabled", why.Point, why.Station)
	case refusalLookup:
		return fmt.Sprintf("%s could not be looked up (%s)", why.Point, why.Detail)
	}
	return describeUnknownPoint(s, why.Point)
}

// describeUnknownPoint says what the point IS, which is the operator's next
// action. "The deck emptied at CP37" is a search; "CP37 is a charge point"
// says the robot took the bin to charge.
func describeUnknownPoint(s *NodeService, point string) string {
	class, err := s.ClassOfPoint(point)
	if err != nil {
		return fmt.Sprintf("%s could not be looked up (%v)", point, err)
	}
	switch class {
	case "ChargePoint":
		return point + " is a charge point, not a station"
	case "ParkPoint":
		return point + " is a park point, not a station"
	case "LocationMark":
		return point + " is a location mark, not a station"
	case "ActionPoint":
		return point + " is an action point with no station bound to it"
	case "GeneralLocation":
		return point + " is a station on the map but no node in Core carries that name"
	}
	return point + " is not a bin location on the synced map"
}
