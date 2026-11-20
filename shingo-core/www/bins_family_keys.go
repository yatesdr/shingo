package www

import (
	"cmp"
	"encoding/json"
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

// ── THE BIN TYPES SECTION'S RDS KEYS ────────────────────────────────────────
//
// A carrier family names the RDS binTask key a lift of its carts sends and the
// one a wait directly in front of such a lift sends (carrier_families
// .lift_bintask, .wait_bintask; dispatch/carrier_keys.go). Each family's tab of
// the Bin Types section shows its keys once, as two pickers with one Save, over
// the family's types (binTypeFamilies): one key per family, every type of it
// sends them.
//
// The options are the distinct keys Core's scene copy lists at bin locations
// (GeneralLocation rows), each with the operation RDS gives it there as the
// row's note: one scene_points read for the page (TestBinsPage_FamilyKeys). The
// default is offered first, stored blank and labelled with the key it sends. A
// saved key the copy no longer lists stays offered and selected, marked "not in
// RDS". Nothing is typed, and the save takes any key the copy lists.
//
// Each field is the shared picker (shared/picker.js, style guide §34) through
// the page's RDS keys adapter (static/pages/bins-rdskeys.js), with two views:
// Lift keys (or Wait keys), then All. A view keeps a key by the operation RDS
// gives it, never by its name: the operations the copy gives the default key
// this field sends (seerrds.BinTaskForAction, the one list of defaults). The
// default row and the saved row are in every view; a key with no operation is
// under All only.

// keyOption is one row of a key picker, as the adapter reads it.
type keyOption struct {
	Value string `json:"value"` // what Save stores: the key, "" for the default
	Label string `json:"label"`
	// Note is the row's quiet line: the operations RDS gives the key, or "not in
	// RDS" on a saved key the copy does not list.
	Note string `json:"note,omitempty"`
	// Ops are the operations RDS gives the key: what a view keeps it by.
	Ops []string `json:"ops,omitempty"`
	// Pinned is in every view: the default and the saved key.
	Pinned   bool `json:"pinned,omitempty"`
	Selected bool `json:"selected,omitempty"`
	// Missing marks a saved key the scene copy (read) does not list: the strip
	// says "not in RDS" beside it at rest.
	Missing bool `json:"missing,omitempty"`
}

// keyField is one key field of a row: the form name its pick is posted as,
// its first view, the operations that view keeps, and its rows.
type keyField struct {
	Name    string      `json:"name"`
	View    string      `json:"view"`
	ViewOps []string    `json:"viewOps"`
	Options []keyOption `json:"options"`
}

// Saved is the field's saved key, as its row: the selected one, else the
// default (the first).
func (f *keyField) Saved() keyOption {
	for _, o := range f.Options {
		if o.Selected {
			return o
		}
	}
	if len(f.Options) > 0 {
		return f.Options[0]
	}
	return keyOption{}
}

// JSON is the field as the adapter reads it from the host's data-field.
func (f *keyField) JSON() string {
	b, err := json.Marshal(f)
	if err != nil {
		log.Printf("bins page: encode the %s field: %v", f.Name, err)
		return "{}"
	}
	return string(b)
}

// sceneKey is one key the scene copy lists at some bin location, with every
// operation RDS gives it at any of them ("" left out), sorted.
type sceneKey struct {
	Name       string
	Operations []string
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

// keyOptions is one picker's rows: the default first (stored blank, labelled
// with the key it sends), then a saved key the copy does not list, then every
// key the copy lists. The saved key is the selected one, and it and the
// default are pinned to every view. A saved key is marked "not in RDS" only
// when the copy was read.
func keyOptions(def, saved string, scene sceneKeyCopy) []keyOption {
	opts := make([]keyOption, 0, len(scene.Keys)+2)
	opts = append(opts, keyOption{Value: "", Label: def + " (default)", Pinned: true, Selected: saved == ""})
	listed := slices.ContainsFunc(scene.Keys, func(k sceneKey) bool { return k.Name == saved })
	if saved != "" && !listed {
		o := keyOption{Value: saved, Label: saved, Pinned: true, Selected: true}
		if scene.Err == nil {
			o.Note, o.Missing = "not in RDS", true
		}
		opts = append(opts, o)
	}
	for _, k := range scene.Keys {
		mine := k.Name == saved
		opts = append(opts, keyOption{Value: k.Name, Label: k.Name, Note: strings.Join(k.Operations, ", "),
			Ops: k.Operations, Pinned: mine, Selected: mine})
	}
	return opts
}

// viewOps is what a field's first view keeps keys by: the operations the copy
// gives the default key the field sends. None when the copy does not list the
// default key, and the view then holds the default and the saved key.
func viewOps(def string, scene sceneKeyCopy) []string {
	for _, k := range scene.Keys {
		if k.Name == def {
			return k.Operations
		}
	}
	return []string{}
}

// pageFamilyKeys is a carrier family's two keys as the page's families read
// gives them.
type pageFamilyKeys struct {
	lift, wait string
}

// ── THE BIN TYPES SECTION: A TAB PER CARRIER FAMILY (owner, 2026-11-18) ────
//
// One tab per family, in family code order, each with its count of types. At
// the top of a tab, the family's Lift and Wait keys once, with one Save; under
// it, the family's types. A bare marker is no row of its own: it is a quiet
// line under the type it stands for (BareOf). A tote carrier's line says its
// top is totes, and its marker's line is its bottom. Everything is drawn from
// the types and the families the page already reads; a family's tab goes with
// its last type, as the family does.

// binTypeFamilyTab is one family's tab.
type binTypeFamilyTab struct {
	ID   int64
	Code string
	// Types are the family's types, bare markers left out, in code order: the
	// tab's count.
	Types []binTypeLine
	// LiftField and WaitField are the family's keys; nil when the page's read
	// of the families failed, and the tab then offers nothing to save.
	LiftField, WaitField *keyField
}

// keyStripField is one field of a tab's key strip: the strip's name for it
// (bins-keystrip.js), its label, and the field.
type keyStripField struct {
	Key, Label string
	Field      *keyField
}

// KeyStrip is the tab's key strip, lift then wait; nil when the family's keys
// were not read, and the tab then offers nothing to save.
func (t binTypeFamilyTab) KeyStrip() []keyStripField {
	if t.LiftField == nil || t.WaitField == nil {
		return nil
	}
	return []keyStripField{{Key: "lift", Label: "Lift", Field: t.LiftField}, {Key: "wait", Label: "Wait", Field: t.WaitField}}
}

// Key is the tab's key: its tab carries it, its panel is tab-<key> (shared
// switchTab, style guide 22).
func (t binTypeFamilyTab) Key() string { return "bt-family-" + strconv.FormatInt(t.ID, 10) }

// binTypeLine is one type of a tab, with the bare marker that stands for it
// drawn as the line under it (nil for none). A marker whose type the page did
// not read is a line of its own, with Bare set.
type binTypeLine struct {
	*domain.BinType
	Marker *domain.BinType
}

// binTypeCount is the section's count of types: its tabs' lines, a bare marker
// drawn under its type not counted again.
func binTypeCount(tabs []binTypeFamilyTab) int {
	n := 0
	for _, t := range tabs {
		n += len(t.Types)
	}
	return n
}

// binTypeFamilies is the Bin Types section: each family that has a type, as a
// tab, from the page's one read of the types and its one read of the families
// (families nil when that read failed).
func binTypeFamilies(types []*domain.BinType, families map[int64]pageFamilyKeys, scene sceneKeyCopy) []binTypeFamilyTab {
	marker := map[int64]*domain.BinType{} // carrier type id -> its bare marker
	read := map[int64]bool{}
	for _, bt := range types {
		read[bt.ID] = true
	}
	for _, bt := range types {
		if bt.Bare && bt.BareOf != nil && read[*bt.BareOf] {
			marker[*bt.BareOf] = bt
		}
	}
	lift, wait := seerrds.BinTaskForAction(protocol.ActionPickup), seerrds.BinTaskForAction(protocol.ActionWait)
	liftOps, waitOps := viewOps(lift, scene), viewOps(wait, scene)
	byFamily := map[int64]*binTypeFamilyTab{}
	var tabs []*binTypeFamilyTab
	for _, bt := range types {
		if bt.Bare && bt.BareOf != nil && read[*bt.BareOf] {
			continue // drawn under its type
		}
		tab := byFamily[bt.FamilyID]
		if tab == nil {
			tab = &binTypeFamilyTab{ID: bt.FamilyID, Code: bt.FamilyCode}
			if keys, ok := families[bt.FamilyID]; ok {
				tab.LiftField = &keyField{Name: "lift_bintask", View: "Lift keys", ViewOps: liftOps,
					Options: keyOptions(lift, keys.lift, scene)}
				tab.WaitField = &keyField{Name: "wait_bintask", View: "Wait keys", ViewOps: waitOps,
					Options: keyOptions(wait, keys.wait, scene)}
			}
			byFamily[bt.FamilyID] = tab
			tabs = append(tabs, tab)
		}
		tab.Types = append(tab.Types, binTypeLine{BinType: bt, Marker: marker[bt.ID]})
	}
	out := make([]binTypeFamilyTab, len(tabs))
	for i, t := range tabs {
		slices.SortFunc(t.Types, func(a, b binTypeLine) int { return strings.Compare(a.Code, b.Code) })
		out[i] = *t
	}
	slices.SortFunc(out, func(a, b binTypeFamilyTab) int {
		return cmp.Or(strings.Compare(a.Code, b.Code), cmp.Compare(a.ID, b.ID))
	})
	return out
}

// handleBinTypeFamilyKeys is a family tab's Save of the family's keys,
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
