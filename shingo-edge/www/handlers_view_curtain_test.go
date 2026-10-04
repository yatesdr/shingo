package www

import (
	"net/http"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/config"
	"shingoedge/domain"
	"shingoedge/plc"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// The operator station's /view carries curtain_ok, stamped from the PLC poll
// cache: true when every curtained node the tile's release enters reads its
// release value, false when one does not (or the cache has nothing for it),
// absent when none is curtained. "Enters" is the tile's own node plus the
// paired positions of its configured claim, and a paired position that has
// no station of its own - a press-index back position - still counts.
func TestOperatorStationView_CarriesCurtainState(t *testing.T) {
	h, router := newOperatorStationsRouter(t)
	stub := h.engine.(*stubEngine)

	pid := seedProcess(t, "SYN-CurtainViewLine")
	sid := seedOperatorStation(t, pid, "OS-SYN-CV-1", "SYN Curtain View")
	styleID, err := testDB.CreateStyle("SYN-CV-STYLE", "", pid)
	testutil.MustNoErr(t, err, "create style")
	testutil.MustNoErr(t, testDB.SetActiveStyle(pid, &styleID), "set active style")

	claim := func(in processes.NodeClaimInput) {
		t.Helper()
		in.StyleID, in.Role, in.PayloadCode, in.OutboundDestination = styleID, protocol.ClaimRoleProduce, "PART-X", "SYN-FG-OUT"
		_, err := processes.UpsertClaim(testDB.DB, domain.CoreNodeKinds{}, in)
		testutil.MustNoErr(t, err, "seed claim "+in.CoreNodeName)
	}
	// A press-index front whose back position has its own curtain and no
	// station; an ordinary FG node with its own curtain; a node with neither.
	claim(processes.NodeClaimInput{CoreNodeName: "SYN-CV-PLN-1", SwapMode: protocol.SwapModeTwoRobotPressIndex, PairedCoreNode: "SYN-CV-PLN-2"})
	claim(processes.NodeClaimInput{CoreNodeName: "SYN-CV-PLN-3", SwapMode: protocol.SwapModeTwoRobot, InboundStaging: "SYN-STG-1"})
	front := seedProcessNode(t, pid, sid, "SYN-CV-PLN-1")
	back := seedProcessNode(t, pid, 0, "SYN-CV-PLN-2")
	own := seedProcessNode(t, pid, sid, "SYN-CV-PLN-3")
	plain := seedProcessNode(t, pid, sid, "SYN-CV-PLN-4")
	f := false
	testutil.MustNoErr(t, testDB.SetProcessNodeCurtain(back, true, "SYN-PLC", "CUR_BACK", &f), "curtain the back position")
	testutil.MustNoErr(t, testDB.SetProcessNodeCurtain(own, true, "SYN-PLC", "CUR_OWN", &f), "curtain the FG node")

	curtainOK := func() map[int64]*bool {
		t.Helper()
		resp := doRequest(t, router, "GET", "/api/operator-stations/"+itoa(sid)+"/view", nil, nil)
		assertStatus(t, resp, http.StatusOK)
		var view store.OperatorStationView
		decodeJSON(t, resp, &view)
		out := map[int64]*bool{}
		for _, n := range view.Nodes {
			out[n.Node.ID] = n.CurtainOK
		}
		return out
	}
	expect := func(got map[int64]*bool, id int64, name string, want *bool) {
		t.Helper()
		g, w := "absent", "absent"
		if got[id] != nil {
			g = map[bool]string{true: "true", false: "false"}[*got[id]]
		}
		if want != nil {
			w = map[bool]string{true: "true", false: "false"}[*want]
		}
		if g != w {
			t.Errorf("%s: curtain_ok %s, want %s", name, g, w)
		}
	}
	yes, no := true, false

	// Both curtains bypassed (FALSE is the release value here).
	mgr := plc.NewManager(nil, config.Defaults(), nil, nil)
	mgr.SetTagValueForTest("SYN-PLC", "CUR_BACK", false)
	mgr.SetTagValueForTest("SYN-PLC", "CUR_OWN", false)
	stub.plcMgr = mgr
	got := curtainOK()
	expect(got, front, "front (its back position is curtained, safe)", &yes)
	expect(got, own, "FG node (own curtain, safe)", &yes)
	expect(got, plain, "uncurtained node", nil)

	// The FG node's curtain goes live: its tile greys, the others do not move.
	mgr.SetTagValueForTest("SYN-PLC", "CUR_OWN", true)
	got = curtainOK()
	expect(got, own, "FG node (own curtain, live)", &no)
	expect(got, front, "front (its back position still safe)", &yes)

	// The cache has no value for the back position's tag: held, not absent.
	bare := plc.NewManager(nil, config.Defaults(), nil, nil)
	bare.SetTagValueForTest("SYN-PLC", "CUR_OWN", false)
	stub.plcMgr = bare
	got = curtainOK()
	expect(got, front, "front (back position's tag not in the cache)", &no)
	expect(got, own, "FG node (safe)", &yes)
	expect(got, plain, "uncurtained node", nil)
}
