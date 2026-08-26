//go:build docker

package service

import (
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/scene"
)

// A robot reports the point it is standing at. At Springfield that has NEVER
// been a Core node name — nine distinct live values, all map furniture — so
// branch A's identity lookup missed every time and 69 log lines said the deck
// emptied somewhere Core could not name. These pin the alias that answers it,
// and every refusal it must still make.

// seedScenePoint writes one scene row. A GeneralLocation carries the station in
// instance_name and the point the robot reports in point_name; every other
// class carries only its instance name, exactly as the sync writes them.
func seedScenePoint(t *testing.T, db *store.DB, area, instance, class, point string) {
	t.Helper()
	testutil.MustNoErr(t, db.UpsertScenePoint(&scene.Point{
		AreaName: area, InstanceName: instance, ClassName: class, PointName: point,
		// The column is jsonb and NOT NULL; the sync always writes the point's
		// property array. Empty is not valid json, and an empty ARRAY is what a
		// point with no properties actually has.
		PropertiesJSON: "[]",
	}), "seed scene point "+instance)
}

func mustNode(t *testing.T, db *store.DB, name string, enabled bool) *nodes.Node {
	t.Helper()
	n := &nodes.Node{Name: name, Enabled: enabled}
	testutil.MustNoErr(t, db.CreateNode(n), "create node "+name)
	return n
}

// THE INCIDENT'S RESOLUTION. AP102 is not a node; scene_points says it is the
// action point of SMN_007, and that is the station the bin was set down at.
func TestResolveReportedPoints_ResolvesAnActionPointThroughTheSceneAlias(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewNodeService(db)

	dest := mustNode(t, db, "SMN_007", true)
	seedScenePoint(t, db, "Area-01", "SMN_007", "GeneralLocation", "AP102")

	node, point, ok := ResolveReportedPoints(svc, "AP102", "")
	if !ok || node == nil {
		t.Fatalf("AP102 did not resolve; the alias row says it is SMN_007's action point")
	}
	if node.ID != dest.ID {
		t.Errorf("AP102 resolved to %s, want SMN_007", node.Name)
	}
	if point != "AP102" {
		t.Errorf("resolved point = %q, want the name the robot reported", point)
	}
}

// MEMBERSHIP, NOT THE PREFIX — the pin the whole rule rests on. Eight of
// Springfield's sixty AP points are plain ActionPoints with no station bound to
// them. A prefix rule would place a bin at a station that does not exist.
func TestResolveReportedPoints_BareActionPointIsNotAStation(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewNodeService(db)

	mustNode(t, db, "SMN_007", true)
	seedScenePoint(t, db, "Area-01", "SMN_007", "GeneralLocation", "AP102")
	seedScenePoint(t, db, "Area-01", "AP350", "ActionPoint", "")

	if _, _, ok := ResolveReportedPoints(svc, "AP350", ""); ok {
		t.Fatal("AP350 resolved — it is an ActionPoint with no station bound to it, and " +
			"any rule that reads it as a station is keying on the AP prefix")
	}
	why := DescribeUnresolvedPoints(svc, "AP350", "")
	if !strings.Contains(why, "action point") {
		t.Errorf("the refusal must say what AP350 is: %q", why)
	}
}

// The three classes the deck actually empties at when it is not a station: a
// charge point, a park point, a location mark. Each declines, and each says
// what it is — that sentence IS the operator's next action.
func TestResolveReportedPoints_NonStationClassesDeclineByName(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewNodeService(db)

	mustNode(t, db, "SMN_007", true)
	seedScenePoint(t, db, "Area-01", "SMN_007", "GeneralLocation", "AP102")
	seedScenePoint(t, db, "Area-01", "CP37", "ChargePoint", "")
	seedScenePoint(t, db, "Area-01", "PP95", "ParkPoint", "")
	seedScenePoint(t, db, "Area-01", "LM100", "LocationMark", "")

	for point, want := range map[string]string{
		"CP37":  "charge point",
		"PP95":  "park point",
		"LM100": "location mark",
	} {
		if _, _, ok := ResolveReportedPoints(svc, point, ""); ok {
			t.Errorf("%s resolved to a station", point)
		}
		if why := DescribeUnresolvedPoints(svc, point, ""); !strings.Contains(why, want) {
			t.Errorf("refusal for %s = %q, must name it as a %s", point, why, want)
		}
	}
}

// PER NAME, NOT PER STAGE. CurrentStation is the newer reading, so a
// CurrentStation only the alias can read must beat a LastStation that happens to
// resolve by identity — the two names describe different moments.
func TestResolveReportedPoints_CurrentStationWinsEvenWhenOnlyTheAliasReadsIt(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewNodeService(db)

	current := mustNode(t, db, "SMN_007", true)
	mustNode(t, db, "DROP-EARLIER", true)
	seedScenePoint(t, db, "Area-01", "SMN_007", "GeneralLocation", "AP102")

	node, _, ok := ResolveReportedPoints(svc, "AP102", "DROP-EARLIER")
	if !ok {
		t.Fatal("nothing resolved")
	}
	if node.ID != current.ID {
		t.Errorf("resolved to %s — LastStation beat CurrentStation because identity was "+
			"tried for every name before the alias was tried for any", node.Name)
	}
}

// FAIL CLOSED ON AMBIGUITY. Instance names are unique per AREA, so two mapped
// areas may each bind an AP102, and nothing on the wire says which. Picking one
// would be a coin toss that writes a bin's location.
func TestResolveReportedPoints_AmbiguousAcrossAreasDeclinesNamingBoth(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewNodeService(db)

	mustNode(t, db, "SMN_007", true)
	mustNode(t, db, "SMN_112", true)
	seedScenePoint(t, db, "Area-01", "SMN_007", "GeneralLocation", "AP102")
	seedScenePoint(t, db, "Area-02", "SMN_112", "GeneralLocation", "AP102")

	if _, _, ok := ResolveReportedPoints(svc, "AP102", ""); ok {
		t.Fatal("an AP102 that names two stations resolved to one of them")
	}
	why := DescribeUnresolvedPoints(svc, "AP102", "")
	for _, want := range []string{"SMN_007", "SMN_112", "more than one area"} {
		if !strings.Contains(why, want) {
			t.Errorf("the refusal must name every candidate: %q missing %q", why, want)
		}
	}
}

// A DISABLED NODE IS NOT A PLACE THE FLOOR WILL LOOK. SMN_001 is enabled=false
// at Springfield and branch A would have placed a bin on it today.
func TestResolveReportedPoints_RefusesADisabledNode(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewNodeService(db)

	mustNode(t, db, "SMN_001", false)
	seedScenePoint(t, db, "Area-01", "SMN_001", "GeneralLocation", "AP51")

	if _, _, ok := ResolveReportedPoints(svc, "AP51", ""); ok {
		t.Fatal("AP51 resolved onto a disabled node")
	}
	if why := DescribeUnresolvedPoints(svc, "AP51", ""); !strings.Contains(why, "disabled") {
		t.Errorf("the refusal must say the node is disabled: %q", why)
	}
}

// Synthetic is refused through the ALIAS route too, not only through identity.
// _TRANSIT and the carrier nodes are bookkeeping; a bin placed on one is a bin
// recorded at a location that does not exist on the floor.
func TestResolveReportedPoints_RefusesASyntheticNodeThroughTheAlias(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewNodeService(db)

	group := &nodes.Node{Name: "SNF2 Lineside Market", IsSynthetic: true, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(group), "create synthetic node")
	seedScenePoint(t, db, "Area-01", "SNF2 Lineside Market", "GeneralLocation", "AP900")

	if _, _, ok := ResolveReportedPoints(svc, "AP900", ""); ok {
		t.Fatal("the alias placed a station onto a synthetic node")
	}
	if why := DescribeUnresolvedPoints(svc, "AP900", ""); !strings.Contains(why, "bookkeeping") {
		t.Errorf("the refusal must say why a synthetic node is not a place: %q", why)
	}
}

// A NEVER-SYNCED SCENE IS A FACT ABOUT CORE, NOT ABOUT THE FLOOR, and an
// operator sent to look for a bin deserves to know which one they are being
// told. Degrading to today's behaviour is right; saying nothing about why is
// not.
func TestDescribeUnresolvedPoints_NeverSyncedSaysSo(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewNodeService(db)

	if _, _, ok := ResolveReportedPoints(svc, "AP102", ""); ok {
		t.Fatal("AP102 resolved with no scene at all")
	}
	why := DescribeUnresolvedPoints(svc, "AP102", "")
	if !strings.Contains(why, "never synced") {
		t.Errorf("refusal = %q, want the never-synced reason rather than "+
			"'not a bin location', which would blame the map for Core's gap", why)
	}
}

// NO DISTANCES, ANYWHERE. 25 Springfield station pairs sit within 2.0 m and the
// bin-37 drop was 2.094 m from a station its order had nothing to do with. A
// figure that looks like evidence and is not is worse than no figure, so this
// asserts the absence.
func TestDescribeUnresolvedPoints_CarriesNoDistanceFigure(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewNodeService(db)

	mustNode(t, db, "SMN_007", true)
	seedScenePoint(t, db, "Area-01", "SMN_007", "GeneralLocation", "AP102")
	seedScenePoint(t, db, "Area-01", "CP37", "ChargePoint", "")

	why := DescribeUnresolvedPoints(svc, "CP37", "")
	for _, banned := range []string{" m from", "metres", "meters", "nearest"} {
		if strings.Contains(strings.ToLower(why), strings.ToLower(banned)) {
			t.Errorf("refusal %q contains %q — no nearest-station math, here or anywhere", why, banned)
		}
	}
}
