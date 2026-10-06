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
// on anybody's deck, and offering to return it would be
// offering an action that can only be refused.
func TestBinsPage_ReturnButtonRendersOnlyForCarriedBins(t *testing.T) {
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

	if got := strings.Count(body, "askRobotToSetDown"); got != 1 {
		t.Errorf("%d rows carry the button, want exactly 1 — only the bin on a deck can "+
			"be returned by a robot", got)
	}
	at := strings.Index(body, "askRobotToSetDown:"+strconv.FormatInt(riding.ID, 10)+":AMR-09")
	if at < 0 {
		t.Fatalf("the button does not name bin %d and AMR-09", riding.ID)
	}
	// IN THE ACTIONS CELL, NOT THE LOCATION CELL: the nearest cell opened
	// before the button is the row's flags cell.
	if cell := body[strings.LastIndex(body[:at], "<td"):at]; !strings.HasPrefix(cell, `<td class="bin-flags">`) {
		t.Errorf("the Return button is not in the actions cell; it sits in %.60q", cell)
	}
	if !strings.Contains(body[at:], ">Return</button>") {
		t.Error(`the button is not labelled "Return"`)
	}
}

// A READER IS NOT AN OPERATOR. Every other write affordance on this page is
// behind .Authenticated and this one dispatches a robot, so it is too.
func TestBinsPage_ReturnButtonIsHiddenFromAnAnonymousReader(t *testing.T) {
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

// THE BIN-TYPE ALLOW-LIST IS EVERY PAYLOAD'S, SORTED BY CODE. The page's
// bin editor reads PAGE_PAYLOAD_BIN_TYPES to offer only the types a payload
// allows; a payload with none is unrestricted and must read [], not null.
func TestBinsPage_PayloadBinTypesAreOneRead(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	loadTestTemplates(t, h)

	// Created out of code order, so id order and code order differ.
	z := &bins.BinType{Code: "PIN-BT-Z", Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(z), "create bin type Z")
	a := &bins.BinType{Code: "PIN-BT-A", Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(a), "create bin type A")
	_, err := db.Exec(`INSERT INTO payloads (code) VALUES ('PIN-BINS-A'), ('PIN-BINS-B')`)
	testutil.MustNoErr(t, err, "payloads")
	_, err = db.Exec(`INSERT INTO payload_bin_types (payload_id, bin_type_id)
		SELECT p.id, b.id FROM payloads p, bin_types b WHERE p.code = 'PIN-BINS-A' AND b.code LIKE 'PIN-BT-%'`)
	testutil.MustNoErr(t, err, "payload bin types")

	body := binsPage(t, h, loginCookie(t, h))

	at := strings.Index(body, "var PAGE_PAYLOAD_BIN_TYPES = ")
	if at < 0 {
		t.Fatal("the page does not carry PAGE_PAYLOAD_BIN_TYPES")
	}
	line := body[at : at+strings.Index(body[at:], "\n")]
	for _, want := range []string{
		`"PIN-BINS-A":[` + strconv.FormatInt(a.ID, 10) + `,` + strconv.FormatInt(z.ID, 10) + `]`,
		`"PIN-BINS-B":[]`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("allow-list missing %s:\n%s", want, line)
		}
	}
}

// binRowHTML is one bin's <tr> out of the rendered bins page.
func binRowHTML(t *testing.T, body string, id int64) string {
	t.Helper()
	at := strings.Index(body, `<tr data-id="`+strconv.FormatInt(id, 10)+`"`)
	if at < 0 {
		t.Fatalf("bin %d has no row on the page", id)
	}
	end := strings.Index(body[at:], "</tr>")
	if end < 0 {
		t.Fatalf("bin %d row is not closed", id)
	}
	return body[at : at+end]
}

// uopCell is the text of a bin row's UoP cell, the one sorted by the count.
func uopCell(t *testing.T, row string, uop int) string {
	t.Helper()
	open := `<td data-sort-value="` + strconv.Itoa(uop) + `">`
	at := strings.Index(row, open)
	if at < 0 {
		t.Fatalf("no UoP cell sorted by %d in row:\n%s", uop, row)
	}
	rest := row[at+len(open):]
	return strings.TrimSpace(rest[:strings.Index(rest, "</td>")])
}

// binsAt makes a bin of the given type on its own fresh node.
func binsAt(t *testing.T, db interface {
	CreateNode(*nodes.Node) error
	CreateBin(*bins.Bin) error
}, bt *bins.BinType, label string) *bins.Bin {
	t.Helper()
	n := &nodes.Node{Name: "NODE-" + label, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(n), "create node "+label)
	b := &bins.Bin{BinTypeID: bt.ID, Label: label, NodeID: &n.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(b), "create bin "+label)
	return b
}

// THE NOTES FLAG MEANS A PERSON WROTE A NOTE. A note is an audit row whose
// action is "note:<type>" (the add_note door, store.AddBinNote); every other bin
// audit row is the system's own record of a move, a count, a status change.
func TestBinsPage_NotesFlagIsAPersonsNote(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	loadTestTemplates(t, h)

	bt := &bins.BinType{Code: "NOTES-UI", Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	moved := binsAt(t, db, bt, "MOVED-ONLY")
	testutil.MustNoErr(t, db.AppendAudit("bin", moved.ID, "moved", "A", "B", "system"), "system audit row")
	noted := binsAt(t, db, bt, "NOTED")
	testutil.MustNoErr(t, db.AddBinNote(noted.ID, "damage", "cracked corner", "op-1"), "note")

	body := binsPage(t, h, loginCookie(t, h))

	// Was: the system-only bin rendered the flag too (the query read every
	// audit row), which put it on every bin at Springfield.
	if strings.Contains(binRowHTML(t, body, moved.ID), `title="Has notes"`) {
		t.Error("a bin with only a system audit row renders the notes flag")
	}
	if !strings.Contains(binRowHTML(t, body, noted.ID), `title="Has notes">notes</span>`) {
		t.Error("a bin a person wrote a note on lost its labelled notes flag")
	}
}

// THE COUNT IS SHOWN ON EVERY BIN THAT HAS ONE. Inventory's "Count below zero"
// list reads bins.uop_remaining < 0 whatever the payload; the bins table only
// printed the count when the bin carried a payload, so a cleared bin left
// negative showed a dash on one page and -3 on the other.
func TestBinsPage_CountShownOnEveryBinThatHasOne(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	loadTestTemplates(t, h)
	testdb.SetupStandardData(t, db) // payload PART-A

	bt := &bins.BinType{Code: "COUNT-UI", Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	negative := binsAt(t, db, bt, "NEG-NO-PAYLOAD")
	loaded := binsAt(t, db, bt, "LOADED")
	empty := binsAt(t, db, bt, "EMPTY-ZERO")
	_, err := db.Exec(`UPDATE bins SET uop_remaining = -3 WHERE id = $1`, negative.ID)
	testutil.MustNoErr(t, err, "negative count, no payload")
	_, err = db.Exec(`UPDATE bins SET payload_code = 'PART-A', uop_remaining = 7, manifest_confirmed = true WHERE id = $1`, loaded.ID)
	testutil.MustNoErr(t, err, "loaded bin")

	body := binsPage(t, h, loginCookie(t, h))

	// Was: the dash — the count printed only beside a payload.
	if got := uopCell(t, binRowHTML(t, body, negative.ID), -3); got != "-3" {
		t.Errorf("a payload-less bin at -3 shows %q, want -3", got)
	}
	if got := uopCell(t, binRowHTML(t, body, loaded.ID), 7); got != "7" {
		t.Errorf("a loaded bin's count shows %q, want 7", got)
	}
	if got := uopCell(t, binRowHTML(t, body, empty.ID), 0); got != `<span class="text-muted">-</span>` {
		t.Errorf("an empty bin with no count shows %q, want the not-applicable dash", got)
	}
}

// THE FLAGS SAY WHAT THEY ARE. Each flag was an emoji with its meaning only in a
// hover title.
func TestBinsPage_FlagsAreLabelled(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	loadTestTemplates(t, h)
	testdb.SetupStandardData(t, db)

	bt := &bins.BinType{Code: "FLAGS-UI", Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	b := binsAt(t, db, bt, "FLAGGED")
	_, err := db.Exec(`UPDATE bins SET locked = true, locked_by = 'op-2', payload_code = 'PART-A',
		manifest_confirmed = false, anomaly_at = NOW() WHERE id = $1`, b.ID)
	testutil.MustNoErr(t, err, "flag the bin")

	row := binRowHTML(t, binsPage(t, h, loginCookie(t, h)), b.ID)
	// Was: &#128274; &#9888; &#9940;, meaning in the title only.
	for _, want := range []string{
		`title="Locked by op-2">locked</span>`,
		`title="Manifest unconfirmed">unconfirmed</span>`,
		`>counts refused</span>`,
	} {
		if !strings.Contains(row, want) {
			t.Errorf("labelled flag %s missing from the row:\n%s", want, row)
		}
	}
	if strings.Contains(row, "&#") {
		t.Errorf("a flag is still a glyph entity:\n%s", row)
	}
}
