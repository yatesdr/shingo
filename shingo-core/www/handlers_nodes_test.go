//go:build docker

package www

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
)

// Characterization tests for handlers_nodes.go form handlers — pinned before
// the Stage 1 refactor that replaces h.engine.DB() with named query methods.
//
// The two handlers under characterization (handleNodeCreate, handleNodeUpdate)
// each chain five DB writes followed by an EventBus emit:
//
//   handleNodeCreate (handlers_nodes.go:99):
//     1. CreateNode
//     2. SetNodeProperty("station_mode")     [if non-empty]
//     3. SetNodeStations / clear             [based on mode]
//     4. SetNodeProperty("bin_type_mode")    [if non-empty]
//     5. SetNodeBinTypes / clear             [based on mode]
//     6. EventBus.Emit(NodeUpdated{Action:"created"})
//     7. http.Redirect(303 SeeOther) → /nodes
//
// handleNodeUpdate is identical except for the lookup + UpdateNode at step 1
// and the "updated" action label. The refactor must preserve both the
// ordering and the SeeOther redirect.

// postForm drives a form-based handler with url-encoded values and returns
// the recorder.
func postForm(t *testing.T, handler http.HandlerFunc, path string, values url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// --- handleNodeCreate -------------------------------------------------------

// TestHandleNodeCreate_HappyPathSpecificModes exercises the full 5-write chain
// with both station_mode=specific and bin_type_mode=specific. Each side-table
// must be populated and the EventNodeUpdated (created) event emitted.
func TestHandleNodeCreate_HappyPathSpecificModes(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	snap := captureNodeUpdated(t, h.engine.EventBus())

	form := url.Values{}
	form.Set("name", "STORAGE-NEW-1")
	form.Set("zone", "A")
	form.Set("enabled", "on")
	form.Set("station_mode", "specific")
	form.Add("stations", "line-1")
	form.Add("stations", "line-2")
	form.Set("bin_type_mode", "specific")
	form.Add("bin_type_ids", strconv.FormatInt(sd.BinType.ID, 10))

	rec := postForm(t, h.handleNodeCreate, "/nodes/create", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303 SeeOther; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/nodes" {
		t.Errorf("Location: got %q, want /nodes", loc)
	}

	// CreateNode happened.
	nodeList, err := db.ListNodes()
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	var created *nodes.Node
	for _, n := range nodeList {
		if n.Name == "STORAGE-NEW-1" {
			created = n
			break
		}
	}
	if created == nil {
		t.Fatalf("node STORAGE-NEW-1 not found in db")
	}
	if created.Zone != "A" || !created.Enabled {
		t.Errorf("node fields: got zone=%q enabled=%v, want A/true", created.Zone, created.Enabled)
	}

	// SetNodeProperty wrote both mode keys.
	if got := db.GetNodeProperty(created.ID, "station_mode"); got != "specific" {
		t.Errorf("station_mode property: got %q, want specific", got)
	}
	if got := db.GetNodeProperty(created.ID, "bin_type_mode"); got != "specific" {
		t.Errorf("bin_type_mode property: got %q, want specific", got)
	}

	// SetNodeStations populated 2 entries.
	stations, err := db.ListStationsForNode(created.ID)
	if err != nil {
		t.Fatalf("list stations: %v", err)
	}
	if len(stations) != 2 {
		t.Errorf("stations: got %v (len %d), want 2", stations, len(stations))
	}

	// SetNodeBinTypes populated the supplied id.
	binTypes, err := db.ListBinTypesForNode(created.ID)
	if err != nil {
		t.Fatalf("list bin types: %v", err)
	}
	if len(binTypes) != 1 || binTypes[0].ID != sd.BinType.ID {
		t.Errorf("bin types: got %+v, want [%d]", binTypes, sd.BinType.ID)
	}

	// EventBus(created) emitted with the right node id+name.
	var matched bool
	for _, e := range snap() {
		if e.NodeID == created.ID && e.Action == "created" && e.NodeName == "STORAGE-NEW-1" {
			matched = true
			break
		}
	}
	if !matched {
		t.Errorf("expected NodeUpdated(created) for node %d, got %+v", created.ID, snap())
	}
}

// TestHandleNodeCreate_NonSpecificModesClearAssignments pins the
// "always-clear" branch: when station_mode != "specific" the handler still
// calls SetNodeStations(nil) (and likewise for bin types). A refactor that
// elides the clear when the mode changes would leave stale assignments.
func TestHandleNodeCreate_NonSpecificModesClearAssignments(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)

	form := url.Values{}
	form.Set("name", "STORAGE-NEW-2")
	form.Set("zone", "A")
	form.Set("enabled", "on")
	form.Set("station_mode", "all")
	form.Set("bin_type_mode", "all")

	rec := postForm(t, h.handleNodeCreate, "/nodes/create", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303; body=%s", rec.Code, rec.Body.String())
	}

	nodeList, err := db.ListNodes()
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	var created *nodes.Node
	for _, n := range nodeList {
		if n.Name == "STORAGE-NEW-2" {
			created = n
			break
		}
	}
	if created == nil {
		t.Fatalf("node STORAGE-NEW-2 not found in db")
	}

	// Modes recorded.
	if got := db.GetNodeProperty(created.ID, "station_mode"); got != "all" {
		t.Errorf("station_mode property: got %q, want all", got)
	}
	if got := db.GetNodeProperty(created.ID, "bin_type_mode"); got != "all" {
		t.Errorf("bin_type_mode property: got %q, want all", got)
	}

	// Both side-tables are empty (the clear-branch ran).
	stations, err := db.ListStationsForNode(created.ID)
	if err != nil {
		t.Fatalf("list stations: %v", err)
	}
	if len(stations) != 0 {
		t.Errorf("stations: got %v, want empty", stations)
	}
	binTypes, err := db.ListBinTypesForNode(created.ID)
	if err != nil {
		t.Fatalf("list bin types: %v", err)
	}
	if len(binTypes) != 0 {
		t.Errorf("bin types: got %d, want empty", len(binTypes))
	}
}

// --- handleNodeUpdate -------------------------------------------------------

// TestHandleNodeUpdate_HappyPathRewritesAssignments pins the lookup +
// UpdateNode + 4-write chain for the update path. Existing station/bin-type
// assignments must be replaced (not appended) by the new values.
func TestHandleNodeUpdate_HappyPathRewritesAssignments(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)

	// Seed an existing node with one station and one bin type assigned, plus
	// station_mode=specific so the assignments are visible through
	// effective-stations queries too.
	node := &nodes.Node{Name: "STORAGE-UPD-1", Zone: "A", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(node), "create node")
	testutil.MustNoErr(t, db.SetNodeProperty(node.ID, "station_mode", "specific"), "set station_mode")
	testutil.MustNoErr(t, db.SetNodeStations(node.ID, []string{"line-old"}), "seed stations")
	testutil.MustNoErr(t, db.SetNodeProperty(node.ID, "bin_type_mode", "specific"), "set bin_type_mode")
	testutil.MustNoErr(t, db.SetNodeBinTypes(node.ID, []int64{sd.BinType.ID}), "seed bin types")

	snap := captureNodeUpdated(t, h.engine.EventBus())

	form := url.Values{}
	form.Set("id", strconv.FormatInt(node.ID, 10))
	form.Set("name", "STORAGE-UPD-1-RENAMED")
	form.Set("zone", "B")
	form.Set("enabled", "on")
	form.Set("station_mode", "specific")
	form.Add("stations", "line-new-a")
	form.Add("stations", "line-new-b")
	form.Set("bin_type_mode", "specific")
	form.Add("bin_type_ids", strconv.FormatInt(sd.BinType.ID, 10))

	rec := postForm(t, h.handleNodeUpdate, "/nodes/update", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303; body=%s", rec.Code, rec.Body.String())
	}

	got, err := db.GetNode(node.ID)
	if err != nil {
		t.Fatalf("get node after update: %v", err)
	}
	if got.Name != "STORAGE-UPD-1-RENAMED" || got.Zone != "B" {
		t.Errorf("node fields: got name=%q zone=%q, want renamed/B", got.Name, got.Zone)
	}

	// Stations REPLACED, not appended (delete+insert in SetNodeStations).
	stations, err := db.ListStationsForNode(node.ID)
	if err != nil {
		t.Fatalf("list stations: %v", err)
	}
	if len(stations) != 2 {
		t.Errorf("stations after update: got %v (len %d), want 2 (line-new-*)", stations, len(stations))
	}
	for _, s := range stations {
		if s == "line-old" {
			t.Errorf("stale station %q persisted after update", s)
		}
	}

	// EventNodeUpdated (updated) emitted.
	var matched bool
	for _, e := range snap() {
		if e.NodeID == node.ID && e.Action == "updated" {
			matched = true
			break
		}
	}
	if !matched {
		t.Errorf("expected NodeUpdated(updated) for node %d, got %+v", node.ID, snap())
	}
}

// TestHandleNodeUpdate_MissingIDReturns400 pins the validation branch.
func TestHandleNodeUpdate_MissingIDReturns400(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	form := url.Values{}
	form.Set("name", "no-id")

	rec := postForm(t, h.handleNodeUpdate, "/nodes/update", form)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleNodeUpdate_NotFoundReturns404 pins the GetNode-miss branch.
func TestHandleNodeUpdate_NotFoundReturns404(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	form := url.Values{}
	form.Set("id", "9999999")
	form.Set("name", "ghost")

	rec := postForm(t, h.handleNodeUpdate, "/nodes/update", form)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// --- handleNodeDelete -------------------------------------------------------

// TestHandleNodeDelete_HappyPathEmitsEvent pins the delete contract: the
// node disappears, NodeUpdated(deleted) is emitted with the original name
// (captured BEFORE DeleteNode runs), and the response is 303 to /nodes.
func TestHandleNodeDelete_HappyPathEmitsEvent(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)

	node := &nodes.Node{Name: "STORAGE-DEL-1", Zone: "A", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(node), "create node")
	snap := captureNodeUpdated(t, h.engine.EventBus())

	form := url.Values{}
	form.Set("id", strconv.FormatInt(node.ID, 10))

	rec := postForm(t, h.handleNodeDelete, "/nodes/delete", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/nodes" {
		t.Errorf("Location: got %q, want /nodes", loc)
	}

	if _, err := db.GetNode(node.ID); err == nil {
		t.Errorf("node %d still exists after delete", node.ID)
	}

	var matched bool
	for _, e := range snap() {
		if e.NodeID == node.ID && e.Action == "deleted" && e.NodeName == "STORAGE-DEL-1" {
			matched = true
			break
		}
	}
	if !matched {
		t.Errorf("expected NodeUpdated(deleted) for node %d %q, got %+v",
			node.ID, "STORAGE-DEL-1", snap())
	}
}

// TestHandleNodeDelete_MissingIDReturns400 pins the validation branch.
func TestHandleNodeDelete_MissingIDReturns400(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	form := url.Values{}
	rec := postForm(t, h.handleNodeDelete, "/nodes/delete", form)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleNodeDelete_NotFoundReturns404 pins the GetNode-miss branch.
func TestHandleNodeDelete_NotFoundReturns404(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	form := url.Values{}
	form.Set("id", "9999999")

	rec := postForm(t, h.handleNodeDelete, "/nodes/delete", form)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// --- a node's name, type and parent through the two form doors (docket B2, B15) ---

// pageSaveForm is what the Nodes page's node form posts for n
// (www/templates/nodes.html, #node-form, filled by nodes-detail.js when the box
// opens and submitted whole by handleNodeSave): the id, type and parent in
// hidden inputs, the node's own name in the hidden name input, the display
// name, the enabled box (sent only when ticked) and the two assignment modes.
func pageSaveForm(n *nodes.Node) url.Values {
	form := url.Values{}
	form.Set("id", strconv.FormatInt(n.ID, 10))
	form.Set("node_type_id", "")
	if n.NodeTypeID != nil {
		form.Set("node_type_id", strconv.FormatInt(*n.NodeTypeID, 10))
	}
	form.Set("parent_id", "")
	if n.ParentID != nil {
		form.Set("parent_id", strconv.FormatInt(*n.ParentID, 10))
	}
	form.Set("name", n.Name)
	form.Set("display_name", n.DisplayName)
	if n.Enabled {
		form.Set("enabled", "on")
	}
	form.Set("station_mode", "all")
	form.Set("bin_type_mode", "all")
	return form
}

// idOf prints a nullable id.
func idOf(p *int64) string {
	if p == nil {
		return "none"
	}
	return strconv.FormatInt(*p, 10)
}

// typedLaneSlot is the back slot of a fresh two-slot lane, given a node type:
// a save that drops its parent takes it out of its lane, and one that drops
// its type clears it.
func typedLaneSlot(t *testing.T, db *store.DB, prefix string) *nodes.Node {
	t.Helper()
	fx := setupNodeGroupFixture(t, db, prefix, 2)
	nt := &nodes.NodeType{Code: prefix + "-T", Name: prefix + " type"}
	testutil.MustNoErr(t, db.CreateNodeType(nt), "create a node type")
	slot, err := db.GetNode(fx.Slots[1].ID)
	testutil.MustNoErr(t, err, "read the slot")
	slot.NodeTypeID = &nt.ID
	testutil.MustNoErr(t, db.UpdateNode(slot), "give the slot a type")
	slot, err = db.GetNode(slot.ID)
	testutil.MustNoErr(t, err, "re-read the slot")
	return slot
}

// TestHandleNodeUpdate_ThePagesOwnSaveKeepsNameTypeAndParent: the Nodes page
// posts a lane slot's own name, type and parent back beside its display name;
// the save stores the display name and leaves the rest as they were.
func TestHandleNodeUpdate_ThePagesOwnSaveKeepsNameTypeAndParent(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	slot := typedLaneSlot(t, db, "PAGESAVE")

	form := pageSaveForm(slot)
	form.Set("display_name", "Back slot")
	rec := postForm(t, h.handleNodeUpdate, "/nodes/update", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("the page's save: %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	got, err := db.GetNode(slot.ID)
	testutil.MustNoErr(t, err, "read the slot after the save")
	if got.Name != slot.Name || idOf(got.NodeTypeID) != idOf(slot.NodeTypeID) ||
		idOf(got.ParentID) != idOf(slot.ParentID) || got.Enabled != slot.Enabled {
		t.Errorf("the page's save stored name %q, type %s, parent %s, enabled %v; want %q, %s, %s, %v as they were",
			got.Name, idOf(got.NodeTypeID), idOf(got.ParentID), got.Enabled,
			slot.Name, idOf(slot.NodeTypeID), idOf(slot.ParentID), slot.Enabled)
	}
	if got.DisplayName != "Back slot" {
		t.Errorf("display name %q, want %q", got.DisplayName, "Back slot")
	}
}

// TestNodeForms_ARequestWithNoName (docket B2, B15): the two form doors sent a
// request made by hand, with a name of spaces or none. TODAY each saves a node
// with no name, and an update that leaves the type and the parent out clears
// them too, which takes a lane slot out of its lane.
func TestNodeForms_ARequestWithNoName(t *testing.T) {
	t.Parallel()

	t.Run("a create whose name is spaces", func(t *testing.T) {
		t.Parallel()
		h, db := testHandlers(t)
		form := url.Values{}
		form.Set("name", "   ")
		form.Set("enabled", "on")
		rec := postForm(t, h.handleNodeCreate, "/nodes/create", form)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("create: %d; today it saves (303); body=%s", rec.Code, rec.Body.String())
		}
		if _, err := db.GetNodeByName(""); err != nil {
			t.Errorf("no node with no name after the create (%v); today one is saved", err)
		}
	})

	t.Run("an update whose name is spaces", func(t *testing.T) {
		t.Parallel()
		h, db := testHandlers(t)
		slot := typedLaneSlot(t, db, "SPACESAVE")
		form := pageSaveForm(slot)
		form.Set("name", "   ")
		rec := postForm(t, h.handleNodeUpdate, "/nodes/update", form)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("update: %d; today it saves (303); body=%s", rec.Code, rec.Body.String())
		}
		got, err := db.GetNode(slot.ID)
		testutil.MustNoErr(t, err, "read the slot after the save")
		if got.Name != "" {
			t.Errorf("name %q; today the save stores none", got.Name)
		}
	})

	t.Run("an update that sends only the id and the enabled box", func(t *testing.T) {
		t.Parallel()
		h, db := testHandlers(t)
		slot := typedLaneSlot(t, db, "BARESAVE")
		form := url.Values{}
		form.Set("id", strconv.FormatInt(slot.ID, 10))
		form.Set("enabled", "on")
		rec := postForm(t, h.handleNodeUpdate, "/nodes/update", form)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("update: %d; today it saves (303); body=%s", rec.Code, rec.Body.String())
		}
		got, err := db.GetNode(slot.ID)
		testutil.MustNoErr(t, err, "read the slot after the save")
		if got.Name != "" || got.NodeTypeID != nil || got.ParentID != nil {
			t.Errorf("stored name %q, type %s, parent %s; today the save clears all three",
				got.Name, idOf(got.NodeTypeID), idOf(got.ParentID))
		}
	})
}
