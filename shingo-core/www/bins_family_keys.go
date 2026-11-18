package www

import (
	"cmp"
	"errors"
	"log"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/domain"
	"shingocore/fleet/seerrds"
)

// ── THE BIN TYPES TABLE'S RDS KEYS ──────────────────────────────────────────
//
// A carrier family names the RDS binTask key a lift of its carts sends and the
// one a wait directly in front of such a lift sends (carrier_families
// .lift_bintask, .wait_bintask; dispatch/carrier_keys.go). The Bin Types table
// shows each type's family keys as two pickers with one Save. The keys are the
// family's, so a Save on one row sets every type of the family, and the row
// names them.
//
// The options are the distinct keys Core's scene copy lists at bin locations
// (GeneralLocation rows), each with the operation RDS gives it there: one
// scene_points read for the page (TestBinsPage_FamilyKeys). The default is
// offered first, stored blank and labelled with the key it sends. A saved key
// the copy no longer lists stays offered and selected, marked "not in RDS".
// Nothing is typed, and no key is left out for its name or its operation.

// keyOption is one option of a key picker.
type keyOption struct {
	Value    string // what Save stores: the key, "" for the default
	Label    string
	Selected bool
}

// sceneKey is one key the scene copy lists at some bin location, with every
// operation RDS gives it at any of them ("" left out), sorted.
type sceneKey struct {
	Name       string
	Operations []string
}

// label is the key as its option shows it: "jackload_4548 · JackLoad".
func (k sceneKey) label() string {
	if len(k.Operations) == 0 {
		return k.Name
	}
	return k.Name + " · " + strings.Join(k.Operations, ", ")
}

// sceneKeyCopy is what the page's one read of the scene copy's bin locations
// tells the pickers.
type sceneKeyCopy struct {
	Keys     []sceneKey // each key once, by name ignoring case, then as written
	SyncedAt time.Time  // the newest synced_at among the locations; zero for none
	Err      error      // the read failed: nothing is known of the scene's keys
}

// readSceneKeys is the page's one scene_points read: the bin locations, each
// read with domain.ScenePoint.BinTasks.
func (h *Handlers) readSceneKeys() sceneKeyCopy {
	points, err := h.engine.NodeService().ListScenePointsByClass("GeneralLocation")
	if err != nil {
		log.Printf("bins page: read the scene copy's bin locations: %v", err)
		return sceneKeyCopy{Err: err}
	}
	return gatherSceneKeys(points)
}

// gatherSceneKeys is the distinct keys the locations list and the newest sync
// time among them. A location whose binTask property has no value lists none.
func gatherSceneKeys(points []*domain.ScenePoint) sceneKeyCopy {
	var out sceneKeyCopy
	ops := map[string]map[string]bool{}
	for _, p := range points {
		if p.SyncedAt.After(out.SyncedAt) {
			out.SyncedAt = p.SyncedAt
		}
		for _, t := range p.BinTasks() {
			if ops[t.Name] == nil {
				ops[t.Name] = map[string]bool{}
			}
			if t.Operation != "" {
				ops[t.Name][t.Operation] = true
			}
		}
	}
	for name, set := range ops {
		out.Keys = append(out.Keys, sceneKey{Name: name, Operations: slices.Sorted(maps.Keys(set))})
	}
	slices.SortFunc(out.Keys, func(a, b sceneKey) int {
		return cmp.Or(strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), strings.Compare(a.Name, b.Name))
	})
	return out
}

// keyOptions is one picker: the default first (stored blank, labelled with
// the key it sends), then a saved key the copy does not list, then every key
// the copy lists. The saved key is the selected one. A saved key is marked
// "not in RDS" only when the copy was read.
func keyOptions(def, saved string, scene sceneKeyCopy) []keyOption {
	opts := make([]keyOption, 0, len(scene.Keys)+2)
	opts = append(opts, keyOption{Value: "", Label: def + " (default)", Selected: saved == ""})
	listed := slices.ContainsFunc(scene.Keys, func(k sceneKey) bool { return k.Name == saved })
	if saved != "" && !listed {
		label := saved
		if scene.Err == nil {
			label += " · not in RDS"
		}
		opts = append(opts, keyOption{Value: saved, Label: label, Selected: true})
	}
	for _, k := range scene.Keys {
		opts = append(opts, keyOption{Value: k.Name, Label: k.label(), Selected: k.Name == saved})
	}
	return opts
}

// pageFamilyKeys is a carrier family's two keys as the page's families read
// gives them.
type pageFamilyKeys struct {
	lift, wait string
}

// binTypeRow is one row of the Bin Types table: the type, its family's two
// pickers, and the family's other types, which a Save sets too. A row whose
// family the page did not read has no pickers, so it offers nothing to save.
type binTypeRow struct {
	*domain.BinType
	Lift, Wait   []keyOption
	FamilyOthers []string
}

// binTypeRows is the Bin Types table: each type with its family's keys.
func binTypeRows(types []*domain.BinType, families map[int64]pageFamilyKeys, scene sceneKeyCopy) []binTypeRow {
	members := map[int64][]string{}
	for _, bt := range types {
		members[bt.FamilyID] = append(members[bt.FamilyID], bt.Code)
	}
	lift, wait := seerrds.BinTaskForAction(protocol.ActionPickup), seerrds.BinTaskForAction(protocol.ActionWait)
	rows := make([]binTypeRow, len(types))
	for i, bt := range types {
		rows[i] = binTypeRow{BinType: bt}
		keys, read := families[bt.FamilyID]
		if !read {
			continue
		}
		rows[i].Lift = keyOptions(lift, keys.lift, scene)
		rows[i].Wait = keyOptions(wait, keys.wait, scene)
		rows[i].FamilyOthers = slices.DeleteFunc(slices.Clone(members[bt.FamilyID]),
			func(code string) bool { return code == bt.Code })
	}
	return rows
}

// handleBinTypeFamilyKeys is the Bin Types table's Save of a family's keys,
// through the dispatcher's one door (SaveCarrierFamilyKeys), so the next block
// built sends them. A refusal is answered as the page's other bin-type forms
// answer theirs: its sentence, which the browser shows.
func (h *Handlers) handleBinTypeFamilyKeys(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	familyID, err := strconv.ParseInt(r.FormValue("family_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid family", http.StatusBadRequest)
		return
	}
	lift, wait := r.FormValue("lift_bintask"), r.FormValue("wait_bintask")
	if err := h.engine.Dispatcher().SaveCarrierFamilyKeys(familyID, lift, wait); err != nil {
		log.Printf("bins: carrier family %d keys refused: actor=%s lift=%q wait=%q: %v",
			familyID, h.getUsername(r), lift, wait, err)
		status, sentence := familyKeysRefusal(err)
		http.Error(w, sentence, status)
		return
	}
	log.Printf("bins: carrier family %d keys saved: actor=%s lift=%q wait=%q", familyID, h.getUsername(r), lift, wait)
	http.Redirect(w, r, "/bins#bt-accordion", http.StatusSeeOther)
}

// familyKeysRefusal is what the engineer is told when a family's keys were not
// saved, and its status.
func familyKeysRefusal(err error) (int, string) {
	switch {
	case errors.Is(err, dispatch.ErrTableTurnRefused):
		return http.StatusConflict, err.Error()
	case errors.Is(err, dispatch.ErrNoSuchFamily):
		return http.StatusNotFound, "This carrier family is gone: its last bin type was deleted or moved to another " +
			"family after the page was drawn. Reload the Bins page."
	}
	return http.StatusInternalServerError, err.Error()
}
