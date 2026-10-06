package www

import (
	"net/http"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

// The FG light-curtain interlock's per-node settings endpoint: the write
// lands on the node row; an enabled toggle without pointers, or without a
// chosen polarity, is a 400 that stamps nothing; disabling keeps the pointers
// and the polarity (a re-enable is a toggle-flip, not a re-typing); and the
// polarity rides through as sent.
func TestApiProcessNodeCurtainSetting(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	pid, err := testDB.CreateProcess("SYN-CurtainNodeProc", "", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	nid, err := testDB.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: "SYN-FG-CUR-1", Name: "SYN-FG-CUR-1", Enabled: true})
	testutil.MustNoErr(t, err, "create node")
	url := "/api/process-nodes/" + itoa(nid) + "/curtain-setting"
	untouched := func(why string) {
		t.Helper()
		n, err := testDB.GetProcessNode(nid)
		testutil.MustNoErr(t, err, "get node")
		if n.CurtainEnabled || n.CurtainPLCName != "" || n.CurtainTagName != "" || n.CurtainSafeValue != nil {
			t.Errorf("%s: node = %+v, want nothing stamped", why, n)
		}
	}

	// Enable without pointers: refused, nothing stamped.
	resp := doRequest(t, router, "POST", url,
		map[string]any{"enabled": true, "plc_name": "", "tag_name": "", "safe_value": false}, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	untouched("no pointers")

	// Enable with no polarity chosen - absent, and explicit null: refused.
	resp = doRequest(t, router, "POST", url,
		map[string]any{"enabled": true, "plc_name": "PRESS-PLC", "tag_name": "FG_CURTAIN"}, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	untouched("polarity absent")
	resp = doRequest(t, router, "POST", url,
		map[string]any{"enabled": true, "plc_name": "PRESS-PLC", "tag_name": "FG_CURTAIN", "safe_value": nil}, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	untouched("polarity null")

	// Enable with pointers and FALSE: the row carries all four columns.
	resp = doRequest(t, router, "POST", url,
		map[string]any{"enabled": true, "plc_name": "PRESS-PLC", "tag_name": "FG_CURTAIN", "safe_value": false}, cookie)
	assertStatus(t, resp, http.StatusOK)
	n, err := testDB.GetProcessNode(nid)
	testutil.MustNoErr(t, err, "get node")
	if !n.CurtainEnabled || n.CurtainPLCName != "PRESS-PLC" || n.CurtainTagName != "FG_CURTAIN" ||
		n.CurtainSafeValue == nil || *n.CurtainSafeValue {
		t.Errorf("node = %+v, want the interlock on, pointers set, polarity FALSE", n)
	}

	// Disable: the toggle clears, the pointers and the polarity stay.
	resp = doRequest(t, router, "POST", url,
		map[string]any{"enabled": false, "plc_name": "PRESS-PLC", "tag_name": "FG_CURTAIN", "safe_value": false}, cookie)
	assertStatus(t, resp, http.StatusOK)
	n, err = testDB.GetProcessNode(nid)
	testutil.MustNoErr(t, err, "get node after disable")
	if n.CurtainEnabled || n.CurtainPLCName != "PRESS-PLC" || n.CurtainTagName != "FG_CURTAIN" ||
		n.CurtainSafeValue == nil || *n.CurtainSafeValue {
		t.Errorf("node = %+v, want the toggle off with pointers and polarity kept", n)
	}

	// An unknown node is a 404, not a silent success.
	resp = doRequest(t, router, "POST", "/api/process-nodes/99999999/curtain-setting",
		map[string]any{"enabled": false}, cookie)
	assertStatus(t, resp, http.StatusNotFound)
}
