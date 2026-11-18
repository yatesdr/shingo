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
var fkFieldRe = regexp.MustCompile(`<div class="bt-key-pick" id="(bt-[a-z]+-\d+)" data-label="[^"]*" data-field="([^"]*)"></div>`)

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
	if !strings.Contains(page, "Core has no copy of the RDS scene yet") {
		t.Error("with no scene copy the table does not say so")
	}
	if got, want := fkPickers(t, page)["bt-lift-"+strconv.FormatInt(a.ID, 10)],
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
	pickers := fkPickers(t, page)
	for _, row := range []struct {
		bt   *bins.BinType
		want map[string][]string
	}{{a, keyed}, {b, keyed}, {c, blank}} {
		for _, key := range []string{"lift", "wait"} {
			id := "bt-" + key + "-" + strconv.FormatInt(row.bt.ID, 10)
			if got := pickers[id]; !reflect.DeepEqual(got, row.want[key]) {
				t.Errorf("%s's %s picker:\n got  %q\n want %q", row.bt.Code, key, got, row.want[key])
			}
		}
	}
	for _, say := range []string{
		"Saves for family <code>FK-4548</code>, also <code>FK-4548-B</code>",
		"Saves for family <code>FK-4548</code>, also <code>FK-4548-A</code>",
		"Saves for family <code>" + c.FamilyCode + "</code></div>",
	} {
		if !strings.Contains(page, say) {
			t.Errorf("no row says %q", say)
		}
	}

	var synced time.Time
	testutil.MustNoErr(t, db.QueryRow(`SELECT MAX(synced_at) FROM scene_points WHERE class_name = 'GeneralLocation'`).
		Scan(&synced), "read the newest sync time")
	if want := `synced <time data-utc="` + synced.UTC().Format(time.RFC3339) + `"`; !strings.Contains(page, want) {
		t.Errorf("the table does not show the last scene sync (%s)", want)
	}
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
	for _, bt := range []*bins.BinType{a, b} {
		for key, want := range map[string]string{"lift": "jackload_4548", "wait": "wait_4548"} {
			id := "bt-" + key + "-" + strconv.FormatInt(bt.ID, 10)
			if got := pickers[id]; len(got) < 2 || got[1] != want+"|"+want+" · not in RDS*" {
				t.Errorf("%s's %s picker = %q, want %s selected", bt.Code, key, got, want)
			}
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
