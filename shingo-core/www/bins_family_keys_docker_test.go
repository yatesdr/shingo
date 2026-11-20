//go:build docker

package www

import (
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
)

// fkType makes a bin type in the carrier family of this code ("" leaves it in
// a family of its own) and reads it back with its family.
func fkType(t *testing.T, h *Handlers, db *store.DB, code, family string) *bins.BinType {
	t.Helper()
	bt := &bins.BinType{Code: code}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type "+code)
	testutil.MustNoErr(t, h.engine.BinService().SetBinTypeFamilyByCode(bt.ID, family), "family of "+code)
	got, err := db.GetBinType(bt.ID)
	testutil.MustNoErr(t, err, "read bin type "+code)
	return got
}

// fkFieldRe is a key field's host as the page renders it: the field the RDS
// keys adapter (bins-rdskeys.js) draws the shared picker from.
var fkFieldRe = regexp.MustCompile(`<div class="bt-key-pick" id="(bt-[a-z]+-\d+)" data-label="[^"]*" data-field="([^"]*)"(?: hidden)?></div>`)

// fkPickers reads the page's key fields by id (bt-lift-<type id>,
// bt-wait-<type id>), each as optionLines writes a picker.
func fkPickers(t *testing.T, page string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, s := range fkFieldRe.FindAllStringSubmatch(page, -1) {
		var f keyField
		testutil.MustNoErr(t, json.Unmarshal([]byte(html.UnescapeString(s[2])), &f), "decode the field "+s[1])
		out[s[1]] = optionLines(f.Options)
	}
	return out
}

// fkTabPanel is a family tab's panel: from its id to the next panel or the end
// of the section.
func fkTabPanel(t *testing.T, page, key string) string {
	t.Helper()
	i := strings.Index(page, `id="tab-`+key+`"`)
	if i < 0 {
		t.Fatalf("no panel tab-%s", key)
	}
	rest := page[i+1:]
	end := len(rest)
	for _, stop := range []string{`class="tab-panel`, `id="bin-type-filter"`} {
		if j := strings.Index(rest, stop); j >= 0 && j < end {
			end = j
		}
	}
	return rest[:end]
}

func fkStatementsNaming(stmts []string, word string) int {
	n := 0
	for _, s := range stmts {
		if strings.Contains(s, word) {
			n++
		}
	}
	return n
}

// TestBinsPage_FamilyKeys: the Bin Types table as an engineer gets it. Each
// type's row shows its family's keys, selected in pickers that offer the
// scene copy's keys with their operation, default first; a saved key the copy
// lacks stays, marked "not in RDS"; the table says when the copy was synced,
// and says plainly when there is none. The page reads scene_points once and
// the families' keys once (the statement that reads lift_bintask): the count
// is logged.
func TestBinsPage_FamilyKeys(t *testing.T) {
	t.Parallel()
	h, db, counter := countingHandlers(t)
	loadTestTemplates(t, h)
	cookie := loginCookie(t, h)

	a := fkType(t, h, db, "FK-4548-A", "FK-4548")
	b := fkType(t, h, db, "FK-4548-B", "FK-4548")
	c := fkType(t, h, db, "FK-3230", "")
	testutil.MustNoErr(t, db.SetCarrierFamilyKeys(a.FamilyID, "jackload_4548", "wait_4548"), "save the family's keys")
	if b.FamilyID != a.FamilyID || c.FamilyID == a.FamilyID {
		t.Fatalf("families: A %d, B %d, C %d; the fixture needs A and B in one, C in another", a.FamilyID, b.FamilyID, c.FamilyID)
	}

	// No scene copy yet: said plainly, and the saved keys are not in it.
	page := binsPage(t, h, cookie)
	if !strings.Contains(fkStrip(t, page, a.FamilyID), "No copy of the RDS scene yet") {
		t.Error("with no scene copy the key strip does not say so")
	}
	if got, want := fkPickers(t, page)["bt-lift-"+strconv.FormatInt(a.FamilyID, 10)],
		[]string{"|JackLoad (default)", "jackload_4548|jackload_4548 · not in RDS*"}; !reflect.DeepEqual(got, want) {
		t.Errorf("with no scene copy, A's lift picker = %q, want %q", got, want)
	}

	testdb.SeedSceneBinTasks(t, db, "FK-LOC-1", testdb.DefaultSceneBinTasks()...)
	testdb.SeedSceneBinTasks(t, db, "FK-LOC-2", append(testdb.DefaultSceneBinTasks(),
		testdb.SceneBinTask{Name: "jackload_4548", Operation: "JackLoad"})...)
	testdb.SeedSceneBinTasks(t, db, "FK-LOC-3") // the property with no value

	counter.Reset()
	page = binsPage(t, h, cookie)
	stmts := counter.Statements()
	sceneReads, keyReads := fkStatementsNaming(stmts, "scene_points"), fkStatementsNaming(stmts, "lift_bintask")
	t.Logf("Bins page: %d statements; %d read scene_points, %d read the families' keys", len(stmts), sceneReads, keyReads)
	if sceneReads != 1 || keyReads != 1 {
		t.Errorf("the page read scene_points %d times and the families' keys %d times, want 1 and 1", sceneReads, keyReads)
	}

	listed := []string{"JackLoad|JackLoad · JackLoad", "jackload_4548|jackload_4548 · JackLoad",
		"JackUnload|JackUnload · JackUnload", "Wait|Wait · Wait"}
	keyed := map[string][]string{
		"lift": {"|JackLoad (default)", listed[0], listed[1] + "*", listed[2], listed[3]},
		"wait": append([]string{"|Wait (default)", "wait_4548|wait_4548 · not in RDS*"}, listed...),
	}
	blank := map[string][]string{
		"lift": append([]string{"|JackLoad (default)*"}, listed...),
		"wait": append([]string{"|Wait (default)*"}, listed...),
	}
	// One pair of pickers per family, on its tab, not one per type.
	pickers := fkPickers(t, page)
	if len(pickers) != 4 {
		t.Errorf("the page draws %d key fields, want 4: a lift and a wait for each of two families", len(pickers))
	}
	for _, fam := range []struct {
		bt   *bins.BinType
		want map[string][]string
	}{{a, keyed}, {c, blank}} {
		for _, key := range []string{"lift", "wait"} {
			id := "bt-" + key + "-" + strconv.FormatInt(fam.bt.FamilyID, 10)
			if got := pickers[id]; !reflect.DeepEqual(got, fam.want[key]) {
				t.Errorf("family %s's %s picker:\n got  %q\n want %q", fam.bt.FamilyCode, key, got, fam.want[key])
			}
		}
	}
	// A tab per family, counting its types, its types under it.
	for _, fam := range []struct {
		bt        *bins.BinType
		count     string
		has, lack []string
	}{
		{a, "2", []string{"FK-4548-A", "FK-4548-B"}, []string{"FK-3230"}},
		{c, "1", []string{"FK-3230"}, []string{"FK-4548-A", "FK-4548-B"}},
	} {
		key := "bt-family-" + strconv.FormatInt(fam.bt.FamilyID, 10)
		tab := `data-tab="` + key + `" data-family="` + fam.bt.FamilyCode + `"`
		if !strings.Contains(page, tab) || !strings.Contains(page, fam.bt.FamilyCode+` <span class="text-muted">`+fam.count+`</span>`) {
			t.Errorf("no tab for family %s counting %s type(s)", fam.bt.FamilyCode, fam.count)
		}
		panel := fkTabPanel(t, page, key)
		for _, code := range fam.has {
			if !strings.Contains(panel, "<code>"+code+"</code>") {
				t.Errorf("family %s's tab does not list %s", fam.bt.FamilyCode, code)
			}
		}
		for _, code := range fam.lack {
			if strings.Contains(panel, "<code>"+code+"</code>") {
				t.Errorf("family %s's tab lists %s, another family's type", fam.bt.FamilyCode, code)
			}
		}
	}

	var synced time.Time
	testutil.MustNoErr(t, db.QueryRow(`SELECT MAX(synced_at) FROM scene_points WHERE class_name = 'GeneralLocation'`).
		Scan(&synced), "read the newest sync time")
	if want := `synced <time data-utc="` + synced.UTC().Format(time.RFC3339) + `"`; !strings.Contains(fkStrip(t, page, a.FamilyID), want) {
		t.Errorf("the key strip does not show the last scene sync (%s)", want)
	}

	// At rest the strip is one line: each key as saved, with a small Change and
	// the hidden input the form posts; Save keys and Cancel are not shown, and
	// no picker is drawn until Change opens one. One hint, over the tabs: none
	// under the strip.
	strip := fkStrip(t, page, a.FamilyID)
	for _, want := range []string{
		`<code>jackload_4548</code>`, `<code>wait_4548</code> <span class="bt-key-note">not in RDS</span>`,
		`data-action="changeFamilyKey:lift">Change</button>`, `data-action="changeFamilyKey:wait">Change</button>`,
		`<input type="hidden" name="lift_bintask" value="jackload_4548">`,
		`<input type="hidden" name="wait_bintask" value="wait_4548">`,
		`class="btn btn-primary btn-sm bt-key-save" hidden>Save keys</button>`,
		`class="btn btn-sm bt-key-cancel" data-action="cancelFamilyKeys" hidden>Cancel</button>`,
	} {
		if !strings.Contains(strip, want) {
			t.Errorf("family %s's strip at rest lacks %q", a.FamilyCode, want)
		}
	}
	if strings.Count(strip, `class="bt-key-pick"`) != 2 || strings.Count(strip, "hidden></div>") != 2 {
		t.Errorf("family %s's strip should hold its two picker hosts, both hidden at rest", a.FamilyCode)
	}
	if strings.Contains(page, "One lift key and one wait key") {
		t.Error("a second hint is drawn under the strip; the sentence over the tabs is the one hint")
	}
	if blank := fkStrip(t, page, c.FamilyID); !strings.Contains(blank, `<code>JackLoad (default)</code>`) ||
		!strings.Contains(blank, `<code>Wait (default)</code>`) {
		t.Errorf("family %s's strip does not read its blank keys as the defaults", c.FamilyCode)
	}
}

// fkStrip is a family tab's key strip: its form, from the tag to its end.
func fkStrip(t *testing.T, page string, familyID int64) string {
	t.Helper()
	panel := fkTabPanel(t, page, "bt-family-"+strconv.FormatInt(familyID, 10))
	i := strings.Index(panel, `<form method="POST" action="/bin-types/family-keys"`)
	if i < 0 {
		t.Fatalf("family %d's tab has no key strip", familyID)
	}
	end := strings.Index(panel[i:], "</form>")
	if end < 0 {
		t.Fatalf("family %d's key strip has no end", familyID)
	}
	return panel[i : i+end]
}

// fkSave posts the Bin Types table's Save of a family's keys.
func fkSave(t *testing.T, h *Handlers, familyID int64, lift, wait string) (int, string, string) {
	t.Helper()
	rec := postForm(t, h.handleBinTypeFamilyKeys, "/bin-types/family-keys", url.Values{
		"family_id": {strconv.FormatInt(familyID, 10)}, "lift_bintask": {lift}, "wait_bintask": {wait}})
	return rec.Code, rec.Header().Get("Location"), rec.Body.String()
}

// fkFamilyKeys is a family's keys as ListCarrierFamilies reads them; ok false
// when the family is not there.
func fkFamilyKeys(t *testing.T, db *store.DB, familyID int64) (lift, wait string, ok bool) {
	t.Helper()
	families, err := db.ListCarrierFamilies()
	testutil.MustNoErr(t, err, "list carrier families")
	for _, f := range families {
		if f.ID == familyID {
			return f.LiftBinTask, f.WaitBinTask, true
		}
	}
	return "", "", false
}

// TestFamilyKeysSave_SetsEveryTypeOfTheFamily: a Save on one row sets the
// family's keys, so every type of the family shows them, through the
// dispatcher's door (its held set already matches the store: a reload finds
// nothing changed), and lands back on the open Bin Types table. Another
// family is untouched. A blank Save stores blank.
func TestFamilyKeysSave_SetsEveryTypeOfTheFamily(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	loadTestTemplates(t, h)
	cookie := loginCookie(t, h)
	a := fkType(t, h, db, "FS-4548-A", "FS-4548")
	b := fkType(t, h, db, "FS-4548-B", "FS-4548")
	other := fkType(t, h, db, "FS-3230", "")

	code, location, body := fkSave(t, h, a.FamilyID, "jackload_4548", "wait_4548")
	if code != http.StatusSeeOther || location != "/bins#bt-accordion" {
		t.Fatalf("save answered %d to %q (%s), want 303 to /bins#bt-accordion", code, location, body)
	}
	if lift, wait, _ := fkFamilyKeys(t, db, a.FamilyID); lift != "jackload_4548" || wait != "wait_4548" {
		t.Errorf("the family's keys are %q / %q, want jackload_4548 / wait_4548", lift, wait)
	}
	if lift, wait, _ := fkFamilyKeys(t, db, other.FamilyID); lift != "" || wait != "" {
		t.Errorf("another family's keys moved to %q / %q", lift, wait)
	}
	if changed, err := h.engine.Dispatcher().ReloadCarrierKeys(); err != nil || changed {
		t.Errorf("a reload after the save changed the dispatcher's keys (%v, %v): the save went around its door", changed, err)
	}
	pickers := fkPickers(t, binsPage(t, h, cookie))
	if b.FamilyID != a.FamilyID {
		t.Fatalf("A and B are in families %d and %d; the fixture needs them in one", a.FamilyID, b.FamilyID)
	}
	for key, want := range map[string]string{"lift": "jackload_4548", "wait": "wait_4548"} {
		id := "bt-" + key + "-" + strconv.FormatInt(a.FamilyID, 10)
		if got := pickers[id]; len(got) < 2 || got[1] != want+"|"+want+" · not in RDS*" {
			t.Errorf("family %s's %s picker = %q, want %s selected", a.FamilyCode, key, got, want)
		}
	}

	if code, _, body := fkSave(t, h, a.FamilyID, "", ""); code != http.StatusSeeOther {
		t.Fatalf("blank save answered %d (%s)", code, body)
	}
	if lift, wait, _ := fkFamilyKeys(t, db, a.FamilyID); lift != "" || wait != "" {
		t.Errorf("a blank save left %q / %q, want both blank", lift, wait)
	}
}

// TestFamilyKeysSave_ARefusalReachesTheEngineer: when the save is refused,
// the engineer reads why, and nothing is written. The refusal here is forced
// with a trigger on the families' write; the table-turn door's is the same
// path (TestFamilyKeysRefusal_ATableTurnRefusalIsShownAsSaid gives its status).
func TestFamilyKeysSave_ARefusalReachesTheEngineer(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	a := fkType(t, h, db, "FR-4548", "FR-4548")
	_, err := db.DB.Exec(`CREATE FUNCTION refuse_family_keys() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN RAISE EXCEPTION 'the keys are refused for the test'; END $$;
		CREATE TRIGGER refuse_family_keys BEFORE UPDATE ON carrier_families FOR EACH ROW
		EXECUTE FUNCTION refuse_family_keys()`)
	testutil.MustNoErr(t, err, "refuse the families' write")

	code, _, body := fkSave(t, h, a.FamilyID, "jackload_4548", "wait_4548")
	if code == http.StatusSeeOther || !strings.Contains(body, "the keys are refused for the test") {
		t.Errorf("a refused save answered %d %q; the engineer must read the refusal", code, body)
	}
	if lift, wait, _ := fkFamilyKeys(t, db, a.FamilyID); lift != "" || wait != "" {
		t.Errorf("a refused save wrote %q / %q", lift, wait)
	}
}

// TestFamilyKeysSave_AFamilyGoneSaysSo: a family that went with its last type
// after the page was drawn is not saved, and the engineer is told it is gone.
func TestFamilyKeysSave_AFamilyGoneSaysSo(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	a := fkType(t, h, db, "FG-4548", "FG-4548")
	testutil.MustNoErr(t, db.DeleteBinType(a.ID), "delete the family's last type")
	if _, _, ok := fkFamilyKeys(t, db, a.FamilyID); ok {
		t.Fatal("the family outlived its last type; the fixture cannot make a family gone")
	}

	code, _, body := fkSave(t, h, a.FamilyID, "jackload_4548", "")
	if code != http.StatusNotFound || !strings.Contains(body, "This carrier family is gone") {
		t.Errorf("a save for a gone family answered %d %q, want 404 saying it is gone", code, body)
	}
}

// TestBinsPage_AMarkerIsALineUnderItsType: a bare marker is no row of its own
// and no count of its own: it is the quiet line under the type it stands for,
// with its own Edit and Delete. A tote carrier's line says its top is totes and
// how many per layer, and its marker's line is its bottom; "bare" is not said
// of it. Delete on a line is .btn .btn-sm, never .btn-danger (style guide 12).
func TestBinsPage_AMarkerIsALineUnderItsType(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	loadTestTemplates(t, h)
	cookie := loginCookie(t, h)
	cart := fkType(t, h, db, "BM-CART", "")
	tote := fkType(t, h, db, "BM-TOTE", "")
	_, err := db.Exec(`UPDATE bin_types SET totes_per_layer = 8 WHERE id = $1`, tote.ID)
	testutil.MustNoErr(t, err, "make BM-TOTE a tote carrier")
	marker := func(bt *bins.BinType) string {
		t.Helper()
		id, err := db.EnsureBareMarker(bt.ID)
		testutil.MustNoErr(t, err, "the marker of "+bt.Code)
		m, err := db.GetBinType(id)
		m = testutil.Must(t, m, err, "read the marker of "+bt.Code)
		return m.Code
	}
	cartMarker, toteMarker := marker(cart), marker(tote)

	page := binsPage(t, h, cookie)
	for _, c := range []struct {
		bt            *bins.BinType
		marker, line  string
		says, saysNot []string
	}{
		{cart, cartMarker, "Bare: the cart with no bin on it", nil, []string{"Top: totes"}},
		{tote, toteMarker, "Bottom: the cart with its totes off", []string{"Top: totes, 8 per layer"}, []string{"Bare:", ">bare<"}},
	} {
		key := "bt-family-" + strconv.FormatInt(c.bt.FamilyID, 10)
		if !strings.Contains(page, c.bt.FamilyCode+` <span class="text-muted">1</span>`) {
			t.Errorf("family %s's tab does not count one type (its marker is not a type of its own)", c.bt.FamilyCode)
		}
		panel := fkTabPanel(t, page, key)
		if n := strings.Count(panel, "<code>"+c.marker+"</code>"); n != 1 {
			t.Errorf("%s's marker %s is drawn %d times, want once", c.bt.Code, c.marker, n)
		}
		i := strings.Index(panel, `<tr class="bt-marker">`)
		if i < 0 || !strings.Contains(panel[i:], "<code>"+c.marker+"</code>") || !strings.Contains(panel[i:], c.line) {
			t.Errorf("%s's marker is not the quiet line %q under it", c.bt.Code, c.line)
		} else if j := strings.Index(panel, "<code>"+c.bt.Code+"</code>"); j < 0 || j > i {
			t.Errorf("%s's marker line is not under %s", c.bt.Code, c.bt.Code)
		} else if line := panel[i : i+strings.Index(panel[i:], "</tr>")]; strings.Contains(line, "<button") || strings.Contains(line, "/bin-types/delete") {
			t.Errorf("%s's marker line draws a button; the type's own Edit and Delete cover it", c.bt.Code)
		}
		for _, s := range c.says {
			if !strings.Contains(panel, s) {
				t.Errorf("family %s's tab does not say %q", c.bt.FamilyCode, s)
			}
		}
		for _, s := range c.saysNot {
			if strings.Contains(panel, s) {
				t.Errorf("family %s's tab says %q", c.bt.FamilyCode, s)
			}
		}
	}
	section := page[strings.Index(page, `id="bt-accordion"`):strings.Index(page, `id="bin-type-filter"`)]
	if strings.Contains(section, "btn-danger") {
		t.Error("the Bin Types section draws a .btn-danger: a Delete on a line is .btn .btn-sm")
	}
}
