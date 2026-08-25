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
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// The bins page is the only surface that can offer recover_carried_bin.
// RecoverCarriedBin accepts exactly the bins on a `_ROBOT:*` node, and
// ListAnomalousTransitBins excludes those by name — so the diagnostics
// anomalies table cannot list one, and the action had no handle anywhere. Zero
// rows have ever been recorded at Springfield.

// loginCookie performs a real login and returns the session cookie, so a page
// render sees .Authenticated true the way a browser does.
func loginCookie(t *testing.T, h *Handlers) *http.Cookie {
	t.Helper()
	h.ensureDefaultAdmin() // admin/admin
	form := url.Values{}
	form.Set("username", "admin")
	form.Set("password", "admin")
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.handleLogin(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionName {
			return c
		}
	}
	t.Fatal("login did not set a session cookie")
	return nil
}

func binsPage(t *testing.T, h *Handlers, cookie *http.Cookie) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/bins", nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.handleBins(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bins page status %d", rec.Code)
	}
	return rec.Body.String()
}

// THE BUTTON IS FOR CARRIED BINS AND NOTHING ELSE. A bin at a real node is not
// on anybody's deck, and offering to ask a robot to put it down would be
// offering an action that can only be refused.
func TestBinsPage_SetItDownButtonRendersOnlyForCarriedBins(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	loadTestTemplates(t, h)

	bt := &bins.BinType{Code: "CARRIED-UI", Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")

	carrier := &nodes.Node{Name: "_ROBOT:AMR-09", IsSynthetic: true, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(carrier), "create carrier node")
	riding := &bins.Bin{BinTypeID: bt.ID, Label: "RIDING-1", NodeID: &carrier.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(riding), "create carried bin")

	parked := &nodes.Node{Name: "SMN_007", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(parked), "create station")
	settled := &bins.Bin{BinTypeID: bt.ID, Label: "SETTLED-1", NodeID: &parked.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(settled), "create settled bin")

	body := binsPage(t, h, loginCookie(t, h))

	if !strings.Contains(body, "Ask AMR-09 to set it down") {
		t.Error("the carried bin's row offers no way to ask the robot to put it down — " +
			"which is the state that made recover_carried_bin unreachable")
	}
	if got := strings.Count(body, "askRobotToSetDown"); got != 1 {
		t.Errorf("%d rows carry the button, want exactly 1 — only the bin on a deck can "+
			"be set down by a robot", got)
	}
	if !strings.Contains(body, "askRobotToSetDown:"+strconv.FormatInt(riding.ID, 10)) {
		t.Errorf("the button does not name bin %d", riding.ID)
	}
}

// A READER IS NOT AN OPERATOR. Every other write affordance on this page is
// behind .Authenticated and this one dispatches a robot, so it is too.
func TestBinsPage_SetItDownButtonIsHiddenFromAnAnonymousReader(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	loadTestTemplates(t, h)

	bt := &bins.BinType{Code: "CARRIED-ANON", Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	carrier := &nodes.Node{Name: "_ROBOT:AMR-10", IsSynthetic: true, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(carrier), "create carrier node")
	riding := &bins.Bin{BinTypeID: bt.ID, Label: "RIDING-2", NodeID: &carrier.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(riding), "create carried bin")

	body := binsPage(t, h, nil)

	if strings.Contains(body, "askRobotToSetDown") {
		t.Error("an unauthenticated reader is offered a button that dispatches a robot")
	}
	if !strings.Contains(body, "on AMR-10") {
		t.Fatal("setup: the carried bin's row did not render at all")
	}
}

// THE REFUSAL IS SHOWN VERBATIM. Every refusal from this door is a sentence
// somebody wrote for a person; the handler must not wrap it, and must not
// flatten it to "could not repair" — the reason is the only useful part.
func TestApiRepairAnomaly_CarriedBinRefusalIsTheReasonVerbatim(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)

	bt := &bins.BinType{Code: "REFUSE-UI", Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	node := &nodes.Node{Name: "SMN_008", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(node), "create station")
	settled := &bins.Bin{BinTypeID: bt.ID, Label: "NOT-RIDING", NodeID: &node.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(settled), "create bin")

	rec := postJSON(t, h.apiRepairAnomaly, "/api/recovery/repair",
		map[string]any{"action": "recover_carried_bin", "bin_id": settled.ID})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The Reason itself, in the words carried_bin_recovery.go wrote.
	if !strings.Contains(body, "not on a robot's deck") {
		t.Errorf("body %q does not carry the reason", body)
	}
	// And NOT wrapped in Error()'s prefix, which repeats what the row the
	// operator is looking at already says.
	if strings.Contains(body, "cannot be recovered by order right now") {
		t.Errorf("body %q wraps the reason instead of showing it", body)
	}
}
