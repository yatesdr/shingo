//go:build docker

package engine

import (
	"strings"
	"testing"
	"time"

	"shingo/protocol/clock"
	"shingo/protocol/testutil"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/scene"
)

// stranded_placement_docker_test.go — the 2026-08-24 incident, pinned.
//
// Order 5427 was cancelled while AMR-09 carried bin 5. The deck emptied at
// AP102 — Core's own SMN_007 — and the station field held that value for 19
// two-second ticks before decaying through LM100, LM7, LM8, LM9 to PP95, a park
// point 12.3 m away, where it sat for another 50 ticks. Every tick rewrote the
// bin's anomaly note, so the pin an operator eventually read was at the wrong
// end of the aisle, and the bin had to be moved by hand.
//
// Two things had to be true for that: the point never resolved (identity only,
// and no live Springfield robot_station value has ever been a node name), and
// the answer was re-sampled rather than frozen.

// seedScenePoint writes one scene row — a GeneralLocation carries the station in
// instance_name and the point the robot reports in point_name.
func seedScenePoint(t *testing.T, db *store.DB, area, instance, class, point string) {
	t.Helper()
	testutil.MustNoErr(t, db.UpsertScenePoint(&scene.Point{
		AreaName: area, InstanceName: instance, ClassName: class, PointName: point,
		PropertiesJSON: "[]",
	}), "seed scene point "+instance)
}

// atPoint is a parked robot with an empty deck, reporting one point.
//
// Connected, because a robot in a test that does not say so is claiming to be a
// robot Core has lost — and the witness now refuses a reading from one of those.
// A fixture that leaves it false is under-specified, not a disconnected robot.
func atPoint(vehicle, point string, x, y float64) fleet.RobotStatus {
	return fleet.RobotStatus{
		VehicleID: vehicle, Connected: true, JackState: 3, LiftHeight: -0.0002,
		CurrentStation: point, LastStation: point, X: x, Y: y,
	}
}

// loadedDeck is the same robot with a bin on it.
func loadedDeck(vehicle string) fleet.RobotStatus {
	return fleet.RobotStatus{
		VehicleID: vehicle, Connected: true, JackState: 1, JackIsFull: true,
		IsLoaded: true, LiftHeight: 0.0601,
	}
}

func binNote(t *testing.T, db *store.DB, binID int64) string {
	t.Helper()
	b, err := db.GetBin(binID)
	testutil.MustNoErr(t, err, "get bin")
	return b.AnomalyNote
}

// ── The incident replay ────────────────────────────────────────────────────

// BIN 5. The deck empties at AP102, the robot then drives away and the station
// field decays through five more points. The placement and the note must both
// name SMN_007, from the FIRST reading — which is the whole of fix B.
func TestCarriedBin_PlacementSurvivesTheStationFieldDecaying(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "SMN_007", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create SMN_007")
	seedScenePoint(t, db, "Area-01", "SMN_007", "GeneralLocation", "AP102")
	// Something occupies the destination for the first few ticks, so the
	// placement CANNOT happen on the tick the good reading is live. That is what
	// makes this a test of the freeze rather than of tick ordering.
	blocker := &bins.Bin{BinTypeID: mustBinType(t, db, "DECAY"), Label: "resident-decay",
		NodeID: &dest.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(blocker), "create blocking bin")

	bin, ord := seedStranded(t, db, "AMR-09")
	cacheRobot(eng, loadedDeck("AMR-09"))
	eng.inferStrandedTransitBin(ord.ID)
	if got := binNodeName(t, db, bin.ID); got != "_ROBOT:AMR-09" {
		t.Fatalf("setup: bin is at %q, want the carrier node", got)
	}

	// Tick 1: the deck reports empty at AP102. The node is still occupied, so
	// the placement is refused — but the reading is taken.
	cacheRobot(eng, atPoint("AMR-09", "AP102", 0.88, 11.82))
	eng.sweepCarriedBins()
	if got := binNodeName(t, db, bin.ID); got != "_ROBOT:AMR-09" {
		t.Fatalf("bin was placed onto an occupied node (%q)", got)
	}

	// The reading decays exactly as the journal recorded it, while the node is
	// still blocked.
	for _, point := range []string{"LM100", "LM7", "LM8", "LM9"} {
		cacheRobot(eng, atPoint("AMR-09", point, 0.9, 20.0))
		eng.sweepCarriedBins()
	}
	// And the robot parks 12.3 m away, where it stayed for 50 ticks.
	cacheRobot(eng, atPoint("AMR-09", "PP95", 0.91, 24.84))
	eng.sweepCarriedBins()

	if note := binNote(t, db, bin.ID); !strings.Contains(note, "AP102") ||
		!strings.Contains(note, "x=0.88") {
		t.Errorf("the note drifted with the robot: %q — it must describe the moment the deck "+
			"emptied, not where the robot went next", note)
	}
	if note := binNote(t, db, bin.ID); strings.Contains(note, "PP95") {
		t.Errorf("the note names the park point the robot drove to: %q", note)
	}

	// The slot frees. The bin is placed at SMN_007 — from the frozen reading,
	// while the robot is still standing at PP95.
	testutil.MustNoErr(t, db.DeleteBin(blocker.ID), "remove the blocking bin")
	eng.sweepCarriedBins()

	if got := binNodeName(t, db, bin.ID); got != "SMN_007" {
		t.Errorf("bin is at %q, want SMN_007 — the frozen reading, not the live one, "+
			"is what says where the bin was set down", got)
	}
}

// A DECK THAT RELOADS DOES NOT ERASE THE ANSWER — the freeze is per bin, and
// the bin left the deck when it emptied. (The robot picking up something else
// afterwards is ordinary.)
func TestCarriedBin_FrozenReadingSurvivesTheDeckReloading(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "SMN_014", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest")
	seedScenePoint(t, db, "Area-01", "SMN_014", "GeneralLocation", "AP204")
	blocker := &bins.Bin{BinTypeID: mustBinType(t, db, "RELOAD"), Label: "resident-reload",
		NodeID: &dest.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(blocker), "create blocking bin")

	bin, ord := seedStranded(t, db, "AMR-RL")
	cacheRobot(eng, loadedDeck("AMR-RL"))
	eng.inferStrandedTransitBin(ord.ID)

	cacheRobot(eng, atPoint("AMR-RL", "AP204", 1.0, 2.0))
	eng.sweepCarriedBins() // frozen; refused, the node is occupied

	// The robot picks up a DIFFERENT bin and drives off.
	cacheRobot(eng, loadedDeck("AMR-RL"))
	eng.sweepCarriedBins()

	// It sets that one down somewhere Core cannot name, and the slot frees.
	testutil.MustNoErr(t, db.DeleteBin(blocker.ID), "remove the blocking bin")
	cacheRobot(eng, atPoint("AMR-RL", "LM77", 40.0, 40.0))
	eng.sweepCarriedBins()

	if got := binNodeName(t, db, bin.ID); got != "SMN_014" {
		t.Errorf("bin is at %q, want SMN_014 — a later load/unload cycle is a different bin's "+
			"story and must not overwrite this one's", got)
	}
}

// A SCENE SYNC THAT LANDS LATE STILL RESCUES THE BIN. This is why the RAW
// sample is frozen and not the resolution: the reading cannot be re-taken, but
// resolving it is free to re-run.
func TestCarriedBin_LateSceneSyncResolvesAFrozenReading(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "SMN_020", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest")

	bin, ord := seedStranded(t, db, "AMR-LATE")
	cacheRobot(eng, loadedDeck("AMR-LATE"))
	eng.inferStrandedTransitBin(ord.ID)

	// The deck empties at AP233 with no scene synced at all.
	cacheRobot(eng, atPoint("AMR-LATE", "AP233", -7.7, -15.6))
	eng.sweepCarriedBins()
	if got := binNodeName(t, db, bin.ID); got != "_ROBOT:AMR-LATE" {
		t.Fatalf("bin moved to %q with no scene to resolve against", got)
	}
	if note := binNote(t, db, bin.ID); !strings.Contains(note, "never synced") {
		t.Errorf("note = %q, want the never-synced reason — that is a fact about Core, "+
			"not about the floor", note)
	}

	// The scene arrives. The robot has long since driven away, and it does not
	// matter.
	seedScenePoint(t, db, "Area-01", "SMN_020", "GeneralLocation", "AP233")
	cacheRobot(eng, atPoint("AMR-LATE", "PP224", 60.0, 60.0))
	eng.sweepCarriedBins()

	if got := binNodeName(t, db, bin.ID); got != "SMN_020" {
		t.Errorf("bin is at %q, want SMN_020 — freezing the RESOLUTION rather than the "+
			"reading would have cemented a failure the sync could fix", got)
	}
}

// ── The restart rule ───────────────────────────────────────────────────────

// STEPHEN'S CALL, AND THE TEST FOR IT. A bin already on a carrier node with an
// already-empty deck at the first sweep tick this process ever runs was
// unloaded while Core was down. During that window an operator may have taken
// the bin off the deck by hand, so the robot's position describes the robot and
// not the bin. It declines, with that reason, and never places — even though
// every other condition is satisfied, which is exactly what makes it dangerous.
func TestCarriedBin_UnwitnessedUnloadAfterARestartIsNeverPlaced(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "SMN_021", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest")
	seedScenePoint(t, db, "Area-01", "SMN_021", "GeneralLocation", "AP198")

	// The state a restart leaves: the bin is on the carrier node already, and
	// this Engine has never seen the deck loaded.
	carrier := &nodes.Node{Name: "_ROBOT:AMR-RESTART", IsSynthetic: true, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(carrier), "create carrier node")
	bin := &bins.Bin{BinTypeID: mustBinType(t, db, "RESTART"), Label: "restart-rider",
		NodeID: &carrier.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create bin")

	cacheRobot(eng, atPoint("AMR-RESTART", "AP198", -6.4, -15.7))
	eng.sweepCarriedBins()

	if got := binNodeName(t, db, bin.ID); got != "_ROBOT:AMR-RESTART" {
		t.Fatalf("bin was placed at %q from a drop nobody watched — while Core was down an "+
			"operator may have taken it off the deck, so this reading says nothing about "+
			"where the bin is", got)
	}
	if note := binNote(t, db, bin.ID); !strings.Contains(note, "restarted") {
		t.Errorf("note = %q, want the honest restart reason", note)
	}
	// And it stays declined, however many ticks pass — with a note that does
	// NOT move as the robot drives on. This decline repeats every two seconds
	// for as long as the bin sits there, so a note carrying live coordinates
	// would be a log line every two seconds forever AND a pin an operator is
	// invited to walk to while the sentence tells them it means nothing.
	before, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin")
	for _, point := range []string{"LM7", "LM8", "PP95"} {
		cacheRobot(eng, atPoint("AMR-RESTART", point, 40, 40))
		eng.sweepCarriedBins()
	}
	after, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin again")
	if got := binNodeName(t, db, bin.ID); got != "_ROBOT:AMR-RESTART" {
		t.Errorf("a later tick placed it at %q — the reason it declined does not expire", got)
	}
	if after.AnomalyNote != before.AnomalyNote {
		t.Errorf("the note moved with the robot: %q then %q",
			before.AnomalyNote, after.AnomalyNote)
	}
	if strings.Contains(after.AnomalyNote, "x=") {
		t.Errorf("note %q offers coordinates the same sentence says are meaningless",
			after.AnomalyNote)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Error("an unchanged decline still bumped bins.updated_at")
	}
}

// The half of the restart promise that IS still kept: a deck still LOADED after
// a restart is re-read, witnessed, and placed when it later empties.
func TestCarriedBin_ARestartWithTheDeckStillLoadedStillPlacesTheBin(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "SMN_022", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest")
	seedScenePoint(t, db, "Area-01", "SMN_022", "GeneralLocation", "AP199")

	carrier := &nodes.Node{Name: "_ROBOT:AMR-LOADED", IsSynthetic: true, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(carrier), "create carrier node")
	bin := &bins.Bin{BinTypeID: mustBinType(t, db, "LOADED"), Label: "loaded-rider",
		NodeID: &carrier.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create bin")

	// First sight after the restart: the deck is still loaded. That IS the
	// witness.
	cacheRobot(eng, loadedDeck("AMR-LOADED"))
	eng.sweepCarriedBins()

	cacheRobot(eng, atPoint("AMR-LOADED", "AP199", -5.0, -15.7))
	eng.sweepCarriedBins()

	if got := binNodeName(t, db, bin.ID); got != "SMN_022" {
		t.Errorf("bin is at %q, want SMN_022 — a deck read loaded and then empty by THIS "+
			"process is a transition it watched, restart or not", got)
	}
}

// ── The gate ───────────────────────────────────────────────────────────────

// BIN 5, PLACED. The whole path, with the audit and the log naming the intent
// that was never consulted as a gate.
func TestCarriedBin_PlacedRecordsTheIntentItDidNotConsult(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "SMN_007", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest")
	seedScenePoint(t, db, "Area-01", "SMN_007", "GeneralLocation", "AP102")

	bin, ord := seedStranded(t, db, "AMR-INTENT")
	// The cancelled order was taking it to SMN_007 — the same answer, which is
	// what makes bin 5 the easy case.
	_, err := db.DB.Exec(`UPDATE orders SET delivery_node='SMN_007' WHERE id=$1`, ord.ID)
	testutil.MustNoErr(t, err, "set delivery node")

	cacheRobot(eng, loadedDeck("AMR-INTENT"))
	eng.inferStrandedTransitBin(ord.ID)
	cacheRobot(eng, atPoint("AMR-INTENT", "AP102", 0.88, 11.82))
	eng.sweepCarriedBins()

	if got := binNodeName(t, db, bin.ID); got != "SMN_007" {
		t.Fatalf("bin is at %q, want SMN_007", got)
	}
	var actor, action, detail string
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT actor, action, detail FROM recovery_actions WHERE target_type='bin' AND target_id=$1
		 ORDER BY id DESC LIMIT 1`, bin.ID).Scan(&actor, &action, &detail),
		"read recovery action")
	if actor != "system:inferred" {
		t.Errorf("actor = %q, want system:inferred — an inferred placement must read as one", actor)
	}
	for _, want := range []string{"AP102", "SMN_007"} {
		if !strings.Contains(detail, want) {
			t.Errorf("audit detail %q is missing %q — the point it resolved and where the "+
				"order was going are both part of why the bin went there", detail, want)
		}
	}
}

// OBSERVATION WINS OVER A DISAGREEING INTENT. An operator usually cancels
// BECAUSE the plan was wrong, so the two sources disagree precisely when the
// observation is the only true answer. Intent is recorded and never votes.
func TestCarriedBin_ObservationBeatsADisagreeingIntent(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	observed := &nodes.Node{Name: "SMN_023", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(observed), "create observed node")
	intended := &nodes.Node{Name: "SMN_033", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(intended), "create intended node")
	seedScenePoint(t, db, "Area-01", "SMN_023", "GeneralLocation", "AP200")

	bin, ord := seedStranded(t, db, "AMR-DISAGREE")
	_, err := db.DB.Exec(`UPDATE orders SET delivery_node='SMN_033' WHERE id=$1`, ord.ID)
	testutil.MustNoErr(t, err, "set delivery node")

	cacheRobot(eng, loadedDeck("AMR-DISAGREE"))
	eng.inferStrandedTransitBin(ord.ID)
	cacheRobot(eng, atPoint("AMR-DISAGREE", "AP200", -3.8, -15.6))
	eng.sweepCarriedBins()

	if got := binNodeName(t, db, bin.ID); got != "SMN_023" {
		t.Errorf("bin is at %q, want SMN_023 — the robot's deck emptied there, and the "+
			"cancelled order's destination is a wish, not an observation", got)
	}
}

// BIN 37. The deck emptied at CP37, a charge point. It declines, the note says
// what CP37 is and where the order had been going — and carries NO distance.
// The drop was 2.094 m from an unrelated station and 24.9 m from the intended
// one; a nearest-station figure would have named the wrong place confidently.
func TestCarriedBin_ChargePointDeclinesWithItsClassAndNoGeometry(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	near := &nodes.Node{Name: "SMN_007", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(near), "create nearby station")
	intended := &nodes.Node{Name: "SMN_033", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(intended), "create intended station")
	seedScenePoint(t, db, "Area-01", "SMN_007", "GeneralLocation", "AP102")
	seedScenePoint(t, db, "Area-01", "SMN_033", "GeneralLocation", "AP234")
	seedScenePoint(t, db, "Area-01", "CP37", "ChargePoint", "")

	bin, ord := seedStranded(t, db, "AMR-11")
	_, err := db.DB.Exec(`UPDATE orders SET delivery_node='SMN_033' WHERE id=$1`, ord.ID)
	testutil.MustNoErr(t, err, "set delivery node")

	cacheRobot(eng, loadedDeck("AMR-11"))
	eng.inferStrandedTransitBin(ord.ID)
	cacheRobot(eng, atPoint("AMR-11", "CP37", 0.65, 14.59))
	eng.sweepCarriedBins()

	if got := binNodeName(t, db, bin.ID); got != "_ROBOT:AMR-11" {
		t.Fatalf("bin was placed at %q from a charge point", got)
	}
	note := binNote(t, db, bin.ID)
	for _, want := range []string{"charge point", "SMN_033", "x=0.65"} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q is missing %q", note, want)
		}
	}
	// ONCE. A parked robot reports the same point as CurrentStation and
	// LastStation, so describing every reported name unconditionally said it
	// twice — and this note renders on the bins page now.
	if n := strings.Count(note, "is a charge point"); n != 1 {
		t.Errorf("note says the point is a charge point %d times, want 1: %q", n, note)
	}
	for _, banned := range []string{" m from", "metres", "meters", "nearest"} {
		if strings.Contains(strings.ToLower(note), banned) {
			t.Errorf("note %q carries %q — no nearest-station math, anywhere: the drop was "+
				"2.094 m from SMN_007, which had nothing to do with it", note, banned)
		}
	}
}

// ── A2: the bin-type gate ──────────────────────────────────────────────────

// A mismatch declines, naming the bin's type AND what the node accepts, because
// one without the other does not tell anybody what to do.
func TestCarriedBin_BinTypeMismatchDeclinesNamingBoth(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "SMN_024", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest")
	seedScenePoint(t, db, "Area-01", "SMN_024", "GeneralLocation", "AP201")

	other := &bins.BinType{Code: "48x45x34", Description: "knockdown"}
	testutil.MustNoErr(t, db.CreateBinType(other), "create other bin type")
	testutil.MustNoErr(t, db.SetNodeProperty(dest.ID, "bin_type_mode", "specific"), "set mode")
	testutil.MustNoErr(t, db.SetNodeBinTypes(dest.ID, []int64{other.ID}), "assign bin type")

	bin, ord := seedStranded(t, db, "AMR-TYPE")
	cacheRobot(eng, loadedDeck("AMR-TYPE"))
	eng.inferStrandedTransitBin(ord.ID)
	cacheRobot(eng, atPoint("AMR-TYPE", "AP201", -2.3, -15.6))
	eng.sweepCarriedBins()

	if got := binNodeName(t, db, bin.ID); got != "_ROBOT:AMR-TYPE" {
		t.Fatalf("bin was placed at %q despite the node not accepting its type", got)
	}
	note := binNote(t, db, bin.ID)
	for _, want := range []string{"SMN_024", "48x45x34"} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q is missing %q — the refusal must name both sides", note, want)
		}
	}
}

// THE DON'T-BREAK-SPRINGFIELD PIN. node_bin_types is empty plant-wide there and
// 41 of 52 physical nodes carry no mode row at all. `all` and an empty
// `inherit` are UNRESTRICTED; a gate that read either as "accepts nothing"
// would refuse every placement at the plant this fix is for.
func TestCarriedBin_AllAndEmptyInheritAreUnrestricted(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	for i, mode := range []string{"all", "inherit", ""} {
		node := &nodes.Node{Name: "SMN_UNR" + string(rune('A'+i)), Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(node), "create dest")
		seedScenePoint(t, db, "Area-01", node.Name, "GeneralLocation", "APU"+string(rune('A'+i)))
		if mode != "" {
			testutil.MustNoErr(t, db.SetNodeProperty(node.ID, "bin_type_mode", mode), "set mode")
		}

		vehicle := "AMR-UNR" + string(rune('A'+i))
		bin, ord := seedStranded(t, db, vehicle)
		cacheRobot(eng, loadedDeck(vehicle))
		eng.inferStrandedTransitBin(ord.ID)
		cacheRobot(eng, atPoint(vehicle, "APU"+string(rune('A'+i)), 1, 1))
		eng.sweepCarriedBins()

		if got := binNodeName(t, db, bin.ID); got != node.Name {
			t.Errorf("mode %q: bin is at %q, want %s — an empty bin-type list under this "+
				"mode means unrestricted, and reading it as a refusal would decline every "+
				"placement at Springfield", mode, got, node.Name)
		}
	}
}

// `specific` WITH NOTHING ASSIGNED is the one empty list that means "accepts
// nothing". It declines naming the CONFIG, because that is what has to change —
// nothing is wrong with the bin.
func TestCarriedBin_SpecificWithNoTypesDeclinesNamingTheConfig(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "SMN_025", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest")
	seedScenePoint(t, db, "Area-01", "SMN_025", "GeneralLocation", "AP206")
	testutil.MustNoErr(t, db.SetNodeProperty(dest.ID, "bin_type_mode", "specific"), "set mode")

	bin, ord := seedStranded(t, db, "AMR-SNF2")
	cacheRobot(eng, loadedDeck("AMR-SNF2"))
	eng.inferStrandedTransitBin(ord.ID)
	cacheRobot(eng, atPoint("AMR-SNF2", "AP206", 0.4, -15.8))
	eng.sweepCarriedBins()

	if got := binNodeName(t, db, bin.ID); got != "_ROBOT:AMR-SNF2" {
		t.Fatalf("bin was placed at %q on a node whose config accepts nothing", got)
	}
	if note := binNote(t, db, bin.ID); !strings.Contains(note, "configuration gap") {
		t.Errorf("note = %q, want the config named as the problem", note)
	}
}

// ── E-prime: pickup age ────────────────────────────────────────────────────

// BIN 37'S FIRST MOMENT. Order 5363 was cancelled 20 h 54 m after the pickup;
// the robot had done other work since and its position described a charging
// bay. Branch A declines — AND the test asserts terminalWithin alone would have
// let it through, which is what makes the no-op un-reintroducible.
func TestBranchA_DeclinesAStalePickupThatTheTerminalWindowWouldHaveAllowed(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "SMN_026", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest")
	seedScenePoint(t, db, "Area-01", "SMN_026", "GeneralLocation", "AP225")

	bin, ord := seedStranded(t, db, "AMR-STALE-PICKUP")
	// The order ended just now — so the terminal window passes, exactly as it
	// does on the event path where the terminal row is milliseconds old.
	strandOrderAt(t, db, ord, clock.Now().UTC())
	pickupOrderAt(t, db, ord, clock.Now().UTC().Add(-21*time.Hour))

	if _, fresh := eng.terminalWithin(ord, eng.strandedSweepWindow()); !fresh {
		t.Fatal("terminalWithin already declines this — the E-prime gate would then be " +
			"untested, and the no-op it replaced could come back unnoticed")
	}

	cacheRobot(eng, atPoint("AMR-STALE-PICKUP", "AP225", 0.63, -18.87))
	eng.inferStrandedTransitBin(ord.ID)

	if got := binNodeName(t, db, bin.ID); got != "_TRANSIT" {
		t.Errorf("bin was placed at %q from a pickup 21 h old — the robot has run other "+
			"jobs since and where it stands now is unrelated to where that bin went", got)
	}
	note := binNote(t, db, bin.ID)
	if !strings.Contains(note, "picked up longer ago") {
		t.Errorf("note = %q, want the pickup age named", note)
	}
	if strings.Contains(note, "x=") {
		t.Errorf("note %q carries the robot's coordinates — the sentence beside them says "+
			"that position no longer describes this bin, and a pin nobody should walk to "+
			"is worse than no pin", note)
	}
}

// A FRESH CANCEL IS UNAFFECTED — the ordinary case must still place.
func TestBranchA_FreshPickupStillPlaces(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "SMN_027", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest")
	seedScenePoint(t, db, "Area-01", "SMN_027", "GeneralLocation", "AP227")

	bin, ord := seedStranded(t, db, "AMR-FRESH")
	cacheRobot(eng, atPoint("AMR-FRESH", "AP227", -2.2, -18.7))
	eng.inferStrandedTransitBin(ord.ID)

	if got := binNodeName(t, db, bin.ID); got != "SMN_027" {
		t.Errorf("bin is at %q, want SMN_027 — a cancel seconds after the pickup is the "+
			"case this whole path exists for", got)
	}
}

// NO in_transit ROW FAILS CLOSED. An order that never reached in_transit cannot
// have had its bin picked up by that robot, and a missing row must never read
// as "recent".
func TestBranchA_MissingPickupRowFailsClosed(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "SMN_028", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest")
	seedScenePoint(t, db, "Area-01", "SMN_028", "GeneralLocation", "AP226")

	bin, ord := seedStranded(t, db, "AMR-NOPICKUP")
	_, err := db.DB.Exec(`DELETE FROM order_history WHERE order_id=$1 AND status='in_transit'`, ord.ID)
	testutil.MustNoErr(t, err, "remove the pickup row")

	cacheRobot(eng, atPoint("AMR-NOPICKUP", "AP226", -3.5, -18.6))
	eng.inferStrandedTransitBin(ord.ID)

	if got := binNodeName(t, db, bin.ID); got != "_TRANSIT" {
		t.Errorf("bin was placed at %q with no record of ever being picked up", got)
	}
}

// BRANCH B IGNORES PICKUP AGE. The jack is the jack: a deck that reports empty
// has set its bin down, however long the bin has been riding.
func TestCarriedBin_PickupAgeDoesNotGateTheJackWatch(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "SMN_029", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest")
	seedScenePoint(t, db, "Area-01", "SMN_029", "GeneralLocation", "AP228")

	bin, ord := seedStranded(t, db, "AMR-LONGRIDE")
	cacheRobot(eng, loadedDeck("AMR-LONGRIDE"))
	eng.inferStrandedTransitBin(ord.ID)
	if got := binNodeName(t, db, bin.ID); got != "_ROBOT:AMR-LONGRIDE" {
		t.Fatalf("setup: bin is at %q", got)
	}

	// Age both windows well past their limits, then unload.
	strandOrderAt(t, db, ord, clock.Now().UTC().Add(-9*time.Hour))
	pickupOrderAt(t, db, ord, clock.Now().UTC().Add(-9*time.Hour))
	cacheRobot(eng, atPoint("AMR-LONGRIDE", "AP228", -5.0, -18.6))
	eng.sweepCarriedBins()

	if got := binNodeName(t, db, bin.ID); got != "SMN_029" {
		t.Errorf("bin is at %q, want SMN_029 — the watch saw this deck empty, which is a "+
			"fact about NOW whatever the order's age", got)
	}
}

// ── The write, and the log ─────────────────────────────────────────────────

// AN UNCHANGED NOTE WRITES NOTHING. The sweep re-marks every stranded bin every
// two seconds; identical bytes must not bump bins.updated_at, which is
// otherwise the obvious "when did this bin last do something" proxy.
func TestDeclinedBin_UnchangedNoteDoesNotTouchUpdatedAt(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	seedScenePoint(t, db, "Area-01", "SMN_030", "GeneralLocation", "AP229")
	seedScenePoint(t, db, "Area-01", "CP41", "ChargePoint", "")

	bin, ord := seedStranded(t, db, "AMR-CHURN")
	cacheRobot(eng, loadedDeck("AMR-CHURN"))
	eng.inferStrandedTransitBin(ord.ID)
	cacheRobot(eng, atPoint("AMR-CHURN", "CP41", 3.0, 4.0))
	eng.sweepCarriedBins()

	b, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin")
	first := b.UpdatedAt

	for i := 0; i < 5; i++ {
		eng.sweepCarriedBins()
	}

	b, err = db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "get bin again")
	if !b.UpdatedAt.Equal(first) {
		t.Errorf("updated_at moved from %s to %s across five identical re-marks — the note "+
			"is frozen now, so the write has nothing to say", first, b.UpdatedAt)
	}
}

// The carrier node goes when the bin leaves it, and the frozen sample goes with
// it — neither is allowed to accumulate one row per vehicle that ever carried
// something.
func TestCarriedBin_PlacementDiscardsTheFrozenSampleAndTheCarrierNode(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	dest := &nodes.Node{Name: "SMN_031", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest")
	seedScenePoint(t, db, "Area-01", "SMN_031", "GeneralLocation", "AP230")

	bin, ord := seedStranded(t, db, "AMR-PRUNE")
	cacheRobot(eng, loadedDeck("AMR-PRUNE"))
	eng.inferStrandedTransitBin(ord.ID)
	cacheRobot(eng, atPoint("AMR-PRUNE", "AP230", -7.9, -18.6))
	eng.sweepCarriedBins()

	if got := binNodeName(t, db, bin.ID); got != "SMN_031" {
		t.Fatalf("bin is at %q", got)
	}
	// The frozen sample is discarded by the placement itself; the seen-loaded
	// mark is pruned against the CARRIED LIST, and on the placing tick the bin
	// was still on that list. One more tick is when it goes — which is the
	// point of pruning against the real population rather than by hand.
	eng.sweepCarriedBins()
	eng.dropObsMu.Lock()
	_, frozen := eng.dropObs[bin.ID]
	_, seen := eng.deckSeenLoaded[bin.ID]
	eng.dropObsMu.Unlock()
	if frozen {
		t.Error("the frozen sample outlived the placement it produced")
	}
	if seen {
		t.Error("the seen-loaded mark outlived the bin's time on the deck")
	}
	var n int
	testutil.MustNoErr(t, db.DB.QueryRow(
		`SELECT count(*) FROM nodes WHERE name='_ROBOT:AMR-PRUNE'`).Scan(&n), "count carrier node")
	if n != 0 {
		t.Error("the carrier node survived with no bins on it")
	}
}

// mustBinType creates a bin type and returns its id.
func mustBinType(t *testing.T, db *store.DB, code string) int64 {
	t.Helper()
	bt := &bins.BinType{Code: "PL-" + code, Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	return bt.ID
}
