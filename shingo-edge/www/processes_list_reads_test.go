package www

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"shingoedge/domain"
	"shingoedge/internal/testdb"
	"shingoedge/store/processes"
)

// TestApiConfig_ListProcesses_CurtainAndCoreBoardsFromOneNodeRead pins the
// two columns the list draws from process_nodes, and what it reads for them.
//
// curtain_nodes: each process's live positions whose curtain interlock is
// switched on, by name, in the store's order (sequence, then name); a
// curtain set up and switched off is not one, a deleted position is not
// one, and a process with none carries no key. core_boards: true only for a
// process whose every position is a Core loader's window
// (TestApiConfig_ListProcesses_CoreBoardsIsEveryPositionALoaderWindow holds
// the rest of that rule).
//
// THE READ: one statement over process_nodes answers both columns, whatever
// the process count. Two used to (one per column), each reading every live
// position.
func TestApiConfig_ListProcesses_CurtainAndCoreBoardsFromOneNodeRead(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	db, counter := testdb.OpenCounting(t)
	h.engine.(*stubEngine).db = db
	cookie := authCookie(t, h)
	loader, err := domain.NewSharedWindowLoader(domain.LoaderID("LD-READS"), domain.RoleConsume,
		domain.ReplenishmentOperator, []domain.Window{{Node: "LRW_001"}}, nil)
	if err != nil {
		t.Fatalf("NewSharedWindowLoader: %v", err)
	}
	h.engine.(*stubEngine).loaders = oneLoader{l: loader}

	on := true
	curtain := func(id int64, enabled bool) {
		t.Helper()
		if err := processes.SetNodeCurtain(db.DB, id, enabled, "PLC_R", "Curtain_R", &on); err != nil {
			t.Fatalf("curtain %d: %v", id, err)
		}
	}
	press := seedProcess(t, db, "Reads Press")
	curtain(seedProcessNode(t, db, press, 0, "RP-C"), true)
	seedProcessNode(t, db, press, 0, "RP-B")
	curtain(seedProcessNode(t, db, press, 0, "RP-A"), true)
	curtain(seedProcessNode(t, db, press, 0, "RP-OFF"), false)
	gone := seedProcessNode(t, db, press, 0, "RP-GONE")
	curtain(gone, true)
	if _, err := db.Exec(`UPDATE process_nodes SET deleted_at = datetime('now') WHERE id = ?`, gone); err != nil {
		t.Fatalf("delete RP-GONE: %v", err)
	}
	other := seedProcess(t, db, "Reads Other")
	curtain(seedProcessNode(t, db, other, 0, "RO-A"), true)
	ldr := seedProcess(t, db, "Reads Loader")
	seedProcessNode(t, db, ldr, 0, "LRW_001")
	bare := seedProcess(t, db, "Reads Bare")

	counter.Reset()
	resp := doRequest(t, router, "GET", "/api/processes", nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	nodeReads := 0
	for _, s := range counter.Statements() {
		if strings.Contains(s, "FROM process_nodes") {
			nodeReads++
		}
	}
	var rows []map[string]any
	decodeJSON(t, resp, &rows)

	curtains, boards := map[int64]any{}, map[int64]any{}
	for _, r := range rows {
		id, _ := r["id"].(float64)
		curtains[int64(id)] = r["curtain_nodes"]
		boards[int64(id)] = r["core_boards"]
	}
	for pid, want := range map[int64]any{
		press: []any{"RP-C", "RP-A"}, other: []any{"RO-A"}, ldr: nil, bare: nil,
	} {
		if !reflect.DeepEqual(curtains[pid], want) {
			t.Errorf("process %d curtain_nodes = %v, want %v", pid, curtains[pid], want)
		}
	}
	for pid, want := range map[int64]bool{press: false, other: false, ldr: true, bare: false} {
		if boards[pid] != want {
			t.Errorf("process %d core_boards = %v, want %v", pid, boards[pid], want)
		}
	}
	if nodeReads != 1 {
		t.Errorf("the list read process_nodes %d times, pinned at 1:\n  %s", nodeReads, strings.Join(counter.Statements(), "\n  "))
	}
}
