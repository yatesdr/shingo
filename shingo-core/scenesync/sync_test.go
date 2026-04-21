package scenesync

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"shingocore/fleet"
	"shingocore/store"
)

// fakeStore is an in-memory implementation of scenesync.Store. It's
// intentionally minimal — enough to drive the functions under test
// without pulling in a real Postgres connection.
type fakeStore struct {
	// scene_points keyed by area_name + "/" + instance_name.
	points map[string]*store.ScenePoint

	// nodes keyed by ID.
	nodes  map[int64]*store.Node
	nextID int64

	// nodeTypes keyed by code.
	nodeTypes map[string]*store.NodeType

	// Knobs for error injection.
	errDelete     error
	errUpsert     error
	errCreate     error
	errUpdate     error
	errList       error
	errDeleteNode error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		points:    map[string]*store.ScenePoint{},
		nodes:     map[int64]*store.Node{},
		nodeTypes: map[string]*store.NodeType{},
	}
}

func (f *fakeStore) key(areaName, instanceName string) string {
	return areaName + "/" + instanceName
}

func (f *fakeStore) DeleteScenePointsByArea(areaName string) error {
	if f.errDelete != nil {
		return f.errDelete
	}
	for k, sp := range f.points {
		if sp.AreaName == areaName {
			delete(f.points, k)
		}
	}
	return nil
}

func (f *fakeStore) UpsertScenePoint(sp *store.ScenePoint) error {
	if f.errUpsert != nil {
		return f.errUpsert
	}
	// Copy so later mutation by the caller doesn't bleed back into our
	// recorded state.
	cp := *sp
	f.points[f.key(sp.AreaName, sp.InstanceName)] = &cp
	return nil
}

func (f *fakeStore) GetNodeTypeByCode(code string) (*store.NodeType, error) {
	nt, ok := f.nodeTypes[code]
	if !ok {
		return nil, fmt.Errorf("node type %q not found", code)
	}
	return nt, nil
}

func (f *fakeStore) GetNodeByName(name string) (*store.Node, error) {
	for _, n := range f.nodes {
		if n.Name == name {
			return n, nil
		}
	}
	return nil, fmt.Errorf("node %q not found", name)
}

func (f *fakeStore) CreateNode(n *store.Node) error {
	if f.errCreate != nil {
		return f.errCreate
	}
	f.nextID++
	n.ID = f.nextID
	f.nodes[n.ID] = n
	return nil
}

func (f *fakeStore) UpdateNode(n *store.Node) error {
	if f.errUpdate != nil {
		return f.errUpdate
	}
	if _, ok := f.nodes[n.ID]; !ok {
		return fmt.Errorf("node id %d not found", n.ID)
	}
	f.nodes[n.ID] = n
	return nil
}

func (f *fakeStore) ListNodes() ([]*store.Node, error) {
	if f.errList != nil {
		return nil, f.errList
	}
	out := make([]*store.Node, 0, len(f.nodes))
	for _, n := range f.nodes {
		out = append(out, n)
	}
	return out, nil
}

func (f *fakeStore) DeleteNode(id int64) error {
	if f.errDeleteNode != nil {
		return f.errDeleteNode
	}
	delete(f.nodes, id)
	return nil
}

// noopLog drops every formatted log line — scenesync logs via callback
// and we don't want the tests to produce noisy output.
func noopLog(format string, args ...any) {}

// recordingLog captures log calls for assertions.
type recordingLog struct {
	lines []string
}

func (r *recordingLog) log(format string, args ...any) {
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

// recordingChange captures onChange invocations.
type recordingChange struct {
	events []string
}

func (r *recordingChange) on(nodeID int64, nodeName, action string) {
	r.events = append(r.events, fmt.Sprintf("%s:%s:%d", action, nodeName, nodeID))
}

// --- SyncScenePoints -------------------------------------------------

func TestSyncScenePoints_Empty(t *testing.T) {
	db := newFakeStore()
	total, loc := SyncScenePoints(db, noopLog, nil)
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
	if len(loc) != 0 {
		t.Errorf("locationSet has %d entries, want 0", len(loc))
	}
	if len(db.points) != 0 {
		t.Errorf("db.points has %d entries, want 0", len(db.points))
	}
}

func TestSyncScenePoints_AdvancedAndBins(t *testing.T) {
	db := newFakeStore()

	areas := []fleet.SceneArea{
		{
			Name: "AreaA",
			AdvancedPoints: []fleet.ScenePoint{
				{InstanceName: "ap1", ClassName: "AP", Label: "AP1", PosX: 1, PosY: 2, PosZ: 3, Dir: 0.5, PropertiesJSON: `{"k":"v"}`},
				{InstanceName: "ap2", ClassName: "AP", Label: "AP2"},
			},
			BinLocations: []fleet.ScenePoint{
				{InstanceName: "bin1", ClassName: "BIN", PointName: "P1", GroupName: "G1", Label: "Bin1", PosX: 10, PosY: 20, PosZ: 30, PropertiesJSON: `{"g":"1"}`},
			},
		},
		{
			Name: "AreaB",
			BinLocations: []fleet.ScenePoint{
				{InstanceName: "bin2", ClassName: "BIN", Label: "Bin2"},
			},
		},
	}

	total, loc := SyncScenePoints(db, noopLog, areas)

	// 2 advanced + 1 bin in AreaA, 1 bin in AreaB = 4 points upserted.
	if total != 4 {
		t.Errorf("total = %d, want 4", total)
	}

	// locationSet tracks bin locations only.
	if got := loc["bin1"]; got != "AreaA" {
		t.Errorf("loc[bin1] = %q, want %q", got, "AreaA")
	}
	if got := loc["bin2"]; got != "AreaB" {
		t.Errorf("loc[bin2] = %q, want %q", got, "AreaB")
	}
	if _, ok := loc["ap1"]; ok {
		t.Errorf("advanced point ap1 leaked into locationSet")
	}
	if len(loc) != 2 {
		t.Errorf("len(loc) = %d, want 2", len(loc))
	}

	// Check the db captured representative fields on at least one point.
	if sp := db.points["AreaA/ap1"]; sp == nil {
		t.Fatalf("AreaA/ap1 not persisted")
	} else {
		if sp.Label != "AP1" {
			t.Errorf("AP1 label = %q", sp.Label)
		}
		if sp.Dir != 0.5 {
			t.Errorf("AP1 dir = %v, want 0.5", sp.Dir)
		}
	}
	if sp := db.points["AreaA/bin1"]; sp == nil {
		t.Fatal("AreaA/bin1 not persisted")
	} else {
		if sp.PointName != "P1" || sp.GroupName != "G1" {
			t.Errorf("bin1 point/group = %q/%q, want P1/G1", sp.PointName, sp.GroupName)
		}
	}
}

func TestSyncScenePoints_DeletePerAreaCalled(t *testing.T) {
	db := newFakeStore()
	// Pre-seed a stale point in AreaA that sync should nuke.
	_ = db.UpsertScenePoint(&store.ScenePoint{AreaName: "AreaA", InstanceName: "stale"})

	areas := []fleet.SceneArea{
		{Name: "AreaA", AdvancedPoints: []fleet.ScenePoint{{InstanceName: "fresh", ClassName: "AP"}}},
	}
	_, _ = SyncScenePoints(db, noopLog, areas)

	if _, ok := db.points["AreaA/stale"]; ok {
		t.Errorf("stale point in AreaA should have been deleted before re-upsert")
	}
	if _, ok := db.points["AreaA/fresh"]; !ok {
		t.Errorf("fresh point missing after sync")
	}
}

func TestSyncScenePoints_ErrorsLoggedNotFatal(t *testing.T) {
	db := newFakeStore()
	db.errUpsert = errors.New("boom")
	rec := &recordingLog{}

	areas := []fleet.SceneArea{
		{Name: "AreaA", AdvancedPoints: []fleet.ScenePoint{{InstanceName: "ap1", ClassName: "AP"}}},
	}
	total, _ := SyncScenePoints(db, rec.log, areas)
	// The function still increments total even on upsert error — it
	// doesn't short-circuit. That's the current behaviour; documenting
	// it with this assertion keeps a regression visible.
	if total != 1 {
		t.Errorf("total = %d, want 1 (counts attempts, not successes)", total)
	}
	if len(rec.lines) == 0 {
		t.Errorf("expected at least one log line for upsert error")
	}
}

// --- SyncFleetNodes --------------------------------------------------

func TestSyncFleetNodes_CreatesMissingNodes(t *testing.T) {
	db := newFakeStore()
	db.nodeTypes["STAG"] = &store.NodeType{ID: 7, Code: "STAG"}
	rc := &recordingChange{}

	loc := map[string]string{
		"bin1": "AreaA",
		"bin2": "AreaB",
	}

	created, deleted := SyncFleetNodes(db, noopLog, rc.on, loc)
	if created != 2 {
		t.Errorf("created = %d, want 2", created)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}

	// Both new nodes should carry the STAG type id.
	for _, n := range db.nodes {
		if n.NodeTypeID == nil || *n.NodeTypeID != 7 {
			t.Errorf("node %q NodeTypeID = %v, want *7", n.Name, n.NodeTypeID)
		}
		if !n.Enabled {
			t.Errorf("node %q Enabled = false, want true", n.Name)
		}
	}

	// At least one "created" event should have been emitted per new node.
	if len(rc.events) != 2 {
		t.Errorf("onChange events = %v, want 2 created events", rc.events)
	}
}

func TestSyncFleetNodes_UpdatesZoneOnExisting(t *testing.T) {
	db := newFakeStore()
	db.nodeTypes["STAG"] = &store.NodeType{ID: 1, Code: "STAG"}

	// Pre-existing node with wrong zone.
	_ = db.CreateNode(&store.Node{Name: "bin1", Zone: "OLD", Enabled: true})

	loc := map[string]string{"bin1": "AreaA"}
	created, deleted := SyncFleetNodes(db, noopLog, nil, loc)
	if created != 0 {
		t.Errorf("created = %d, want 0 (node pre-existed)", created)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}

	n, err := db.GetNodeByName("bin1")
	if err != nil {
		t.Fatalf("bin1 disappeared: %v", err)
	}
	if n.Zone != "AreaA" {
		t.Errorf("Zone = %q, want %q (zone should have been reconciled)", n.Zone, "AreaA")
	}
}

func TestSyncFleetNodes_DeletesNodesNotInScene(t *testing.T) {
	db := newFakeStore()
	db.nodeTypes["STAG"] = &store.NodeType{ID: 1, Code: "STAG"}

	// A physical node not referenced by locationSet must be deleted.
	_ = db.CreateNode(&store.Node{Name: "bin-ghost", Enabled: true})

	// A synthetic node must NOT be deleted even if missing from the scene.
	_ = db.CreateNode(&store.Node{Name: "group1", IsSynthetic: true, Enabled: true})

	// A child node must NOT be deleted — its ParentID is set.
	parentID := int64(42)
	_ = db.CreateNode(&store.Node{Name: "child-bin", ParentID: &parentID, Enabled: true})

	// A nameless node must NOT be deleted.
	_ = db.CreateNode(&store.Node{Name: "", Enabled: true})

	rc := &recordingChange{}
	loc := map[string]string{} // scene now has no bins at all
	created, deleted := SyncFleetNodes(db, noopLog, rc.on, loc)

	if created != 0 {
		t.Errorf("created = %d, want 0", created)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1 (only bin-ghost qualifies)", deleted)
	}

	// Walk remaining nodes: bin-ghost must be gone, the others must remain.
	names := map[string]bool{}
	for _, n := range db.nodes {
		names[n.Name] = true
	}
	if names["bin-ghost"] {
		t.Errorf("bin-ghost was not deleted")
	}
	if !names["group1"] || !names["child-bin"] || !names[""] {
		t.Errorf("protected nodes were unexpectedly deleted; remaining = %v", names)
	}

	// Check we emitted exactly one "deleted" event.
	deletedEvents := 0
	for _, e := range rc.events {
		if len(e) >= len("deleted:") && e[:len("deleted:")] == "deleted:" {
			deletedEvents++
		}
	}
	if deletedEvents != 1 {
		t.Errorf("deleted events = %d, want 1; events = %v", deletedEvents, rc.events)
	}
}

func TestSyncFleetNodes_NoNodeTypeStillCreates(t *testing.T) {
	// Absence of the STAG node type should not stop node creation —
	// NodeTypeID is just left nil.
	db := newFakeStore()

	loc := map[string]string{"bin1": "AreaA"}
	created, _ := SyncFleetNodes(db, noopLog, nil, loc)
	if created != 1 {
		t.Errorf("created = %d, want 1", created)
	}
	n, err := db.GetNodeByName("bin1")
	if err != nil {
		t.Fatalf("bin1 not created: %v", err)
	}
	if n.NodeTypeID != nil {
		t.Errorf("NodeTypeID = %v, want nil when STAG lookup fails", n.NodeTypeID)
	}
}

// --- UpdateNodeZones -------------------------------------------------

func TestUpdateNodeZones_OverwriteTrue(t *testing.T) {
	db := newFakeStore()
	_ = db.CreateNode(&store.Node{Name: "a", Zone: "OLD"})
	_ = db.CreateNode(&store.Node{Name: "b", Zone: ""})
	_ = db.CreateNode(&store.Node{Name: "c", Zone: "KEEP"}) // not in locationSet
	_ = db.CreateNode(&store.Node{Name: "", Zone: "IGN"})   // nameless → skipped

	loc := map[string]string{
		"a": "AREA1",
		"b": "AREA2",
	}
	rc := &recordingChange{}
	UpdateNodeZones(db, noopLog, rc.on, loc, true)

	na, _ := db.GetNodeByName("a")
	nb, _ := db.GetNodeByName("b")
	nc, _ := db.GetNodeByName("c")
	if na.Zone != "AREA1" {
		t.Errorf("a.Zone = %q, want AREA1", na.Zone)
	}
	if nb.Zone != "AREA2" {
		t.Errorf("b.Zone = %q, want AREA2", nb.Zone)
	}
	if nc.Zone != "KEEP" {
		t.Errorf("c.Zone = %q, want KEEP (not in locationSet)", nc.Zone)
	}
	if len(rc.events) != 2 {
		t.Errorf("update events = %d, want 2", len(rc.events))
	}
}

func TestUpdateNodeZones_OverwriteFalse(t *testing.T) {
	db := newFakeStore()
	_ = db.CreateNode(&store.Node{Name: "a", Zone: "OLD"}) // non-empty → skipped
	_ = db.CreateNode(&store.Node{Name: "b", Zone: ""})    // empty → filled

	loc := map[string]string{
		"a": "AREA1",
		"b": "AREA2",
	}
	UpdateNodeZones(db, noopLog, nil, loc, false)

	na, _ := db.GetNodeByName("a")
	nb, _ := db.GetNodeByName("b")
	if na.Zone != "OLD" {
		t.Errorf("a.Zone = %q, want OLD (overwrite=false must not clobber)", na.Zone)
	}
	if nb.Zone != "AREA2" {
		t.Errorf("b.Zone = %q, want AREA2 (overwrite=false should still fill empty)", nb.Zone)
	}
}

func TestUpdateNodeZones_ListError(t *testing.T) {
	db := newFakeStore()
	db.errList = errors.New("list exploded")
	rec := &recordingLog{}

	// Must not panic; must log the failure.
	UpdateNodeZones(db, rec.log, nil, map[string]string{"a": "X"}, true)
	if len(rec.lines) == 0 {
		t.Errorf("expected a log line on ListNodes error")
	}
}

// --- Sync ------------------------------------------------------------

// fakeSyncer implements fleet.SceneSyncer in-process.
type fakeSyncer struct {
	areas []fleet.SceneArea
	err   error
}

func (f *fakeSyncer) GetSceneAreas() ([]fleet.SceneArea, error) {
	return f.areas, f.err
}

func TestSync_HappyPath(t *testing.T) {
	db := newFakeStore()
	db.nodeTypes["STAG"] = &store.NodeType{ID: 1, Code: "STAG"}

	syncer := &fakeSyncer{
		areas: []fleet.SceneArea{
			{
				Name: "AreaA",
				AdvancedPoints: []fleet.ScenePoint{
					{InstanceName: "ap1", ClassName: "AP"},
				},
				BinLocations: []fleet.ScenePoint{
					{InstanceName: "bin1", ClassName: "BIN"},
				},
			},
		},
	}
	var running atomic.Bool

	total, created, deleted, err := Sync(db, noopLog, nil, syncer, &running)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if created != 1 {
		t.Errorf("created = %d, want 1", created)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}

	// After completion, the running flag should be back to false so a
	// second Sync can proceed.
	if running.Load() {
		t.Errorf("running flag still true after Sync completed")
	}
}

func TestSync_RejectsConcurrent(t *testing.T) {
	db := newFakeStore()
	syncer := &fakeSyncer{}
	var running atomic.Bool
	running.Store(true) // pretend another Sync is already going

	_, _, _, err := Sync(db, noopLog, nil, syncer, &running)
	if err == nil {
		t.Fatal("Sync returned nil error while another run was in progress")
	}
	// The running flag must remain true — the guarded run didn't start,
	// so it must not clear it on exit. This also guards against the
	// classic deferred-Store-false footgun.
	if !running.Load() {
		t.Errorf("running flag = false; guard path wrongly cleared it")
	}
}

func TestSync_SyncerError(t *testing.T) {
	db := newFakeStore()
	wantErr := errors.New("fleet down")
	syncer := &fakeSyncer{err: wantErr}
	var running atomic.Bool

	total, created, deleted, err := Sync(db, noopLog, nil, syncer, &running)
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
	if total != 0 || created != 0 || deleted != 0 {
		t.Errorf("counts on error: total=%d created=%d deleted=%d; want 0/0/0", total, created, deleted)
	}
	// Guard should be released after an error so the next call isn't
	// permanently blocked.
	if running.Load() {
		t.Errorf("running flag still true after Sync errored out")
	}
}
