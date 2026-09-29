package www

import (
	"net/http"
	"testing"

	"shingo/protocol/testutil"
)

// The FG light-curtain interlock's settings endpoint: the write lands on the
// process row, an enabled toggle without pointers is a 400, disabling keeps
// the pointers (a re-enable is a toggle-flip, not a re-typing), and the
// polarity rides through as sent.
func TestApiProcessCurtainSetting(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	pid, err := testDB.CreateProcess("CurtainProc", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")

	// Enable without pointers: refused, nothing stamped.
	resp := doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/curtain-setting",
		map[string]any{"enabled": true, "plc_name": "", "tag_name": "", "safe_value": true}, cookie)
	assertStatus(t, resp, http.StatusBadRequest)

	// Enable with pointers: the row carries all four columns.
	resp = doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/curtain-setting",
		map[string]any{"enabled": true, "plc_name": "PRESS-PLC", "tag_name": "FG_CURTAIN", "safe_value": false}, cookie)
	assertStatus(t, resp, http.StatusOK)
	proc, err := testDB.GetProcess(pid)
	testutil.MustNoErr(t, err, "get process")
	if !proc.CurtainEnabled || proc.CurtainPLCName != "PRESS-PLC" || proc.CurtainTagName != "FG_CURTAIN" || proc.CurtainSafeValue {
		t.Errorf("process = %+v, want the interlock on, pointers set, polarity FALSE", proc)
	}

	// Disable: the toggle clears, the pointers stay.
	resp = doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/curtain-setting",
		map[string]any{"enabled": false, "plc_name": "PRESS-PLC", "tag_name": "FG_CURTAIN", "safe_value": false}, cookie)
	assertStatus(t, resp, http.StatusOK)
	proc, err = testDB.GetProcess(pid)
	testutil.MustNoErr(t, err, "get process after disable")
	if proc.CurtainEnabled || proc.CurtainPLCName != "PRESS-PLC" || proc.CurtainTagName != "FG_CURTAIN" || !proc.CurtainSafeValue == false {
		t.Errorf("process = %+v, want the toggle off with pointers kept", proc)
	}
}
