package deviceconfig

import "strings"

// Row kinds. Every subsection renders its device's `name` and `enabled`
// rows before its params — both compare like any value: a renamed device
// reds on its name row, a switched-off device reds on its enabled row.
const (
	KindParam   = "param"
	KindName    = "name"
	KindEnabled = "enabled"
)

// Leaf is one comparison row of one robot: the flattened, sanitized unit
// the matrix is built from and the pull row persists.
//
// Path is the DISPLAY path ("basic: terminalResistance", "mode: type",
// "func: name", "name"); Ident is the full identity, which KEEPS the combo
// choice segment ("func/runScript: name") even though the display drops
// it — the choice returns to the display only to disambiguate two choices
// exposing the same key (the naming rule, UI spec).
type Leaf struct {
	Section    string `json:"section"`
	Subsection string `json:"subsection"`
	Kind       string `json:"kind"`
	Path       string `json:"path"`
	Ident      string `json:"ident"`
	Value      string `json:"value"`
	Desc       string `json:"desc"`
	Type       string `json:"type"`
	Unit       string `json:"unit"`
	Range      string `json:"range"`
	Credential bool   `json:"credential"`
}

// Leaves flattens a (sanitized) model file into comparison rows, in
// document order: sections by file order, then devices, then each device's
// name, enabled and param rows.
//
// The counting rule the census pinned (brief §Trigger): LIVE LEAVES include
// the selected choice's subparams — Trigger is 16 live leaves (4+3+3+6),
// not its 12 top-level params; unselected choices' subparams are kept in
// the parse tree for the v4 edit UI and never become rows.
func Leaves(m *ModelFile) []Leaf {
	var out []Leaf
	for i := range m.DeviceTypes {
		dt := &m.DeviceTypes[i]
		for j := range dt.Devices {
			dev := &dt.Devices[j]
			out = append(out,
				Leaf{
					Section: dt.Name, Subsection: dev.Name, Kind: KindName,
					Path: "name", Ident: "name", Value: dev.Name, Type: "string",
				},
				Leaf{
					Section: dt.Name, Subsection: dev.Name, Kind: KindEnabled,
					Path: "enabled", Ident: "enabled", Value: fmtBool(dev.IsEnabled), Type: "bool",
				},
			)
			var rows []Leaf
			walkParams(dt.Name, dev.Name, nil, dev.DeviceParams, &rows)
			out = append(out, disambiguate(rows)...)
		}
	}
	return out
}

// walkParams emits rows for params under an optional group path ("basic").
func walkParams(section, subsection string, prefix []string, ps []Param, out *[]Leaf) {
	for i := range ps {
		p := &ps[i]
		base := append(append([]string{}, prefix...), p.Key)
		switch {
		case p.Array != nil:
			// A group level: its key folds into the path and its params
			// recurse (one level measured, "basic"; deeper recurses the same).
			walkParams(section, subsection, base, p.Array.Params, out)
		case p.Combo != nil && p.hasSubparams():
			// Selection row `param: type`, then the SELECTED choice's
			// subparams as `param: key` (choice kept in the identity).
			*out = append(*out, leafOf(section, subsection, p,
				joinPath(base)+": type", joinPath(base)+": type"))
			choice := p.Combo.ChildKey
			for _, sub := range p.choiceParams() {
				emitSub(section, subsection, p, base, choice, sub, out)
			}
		default:
			// A plain leaf or a combo without subparams (bare name: func,
			// brand, baudrate — the choice IS the value).
			*out = append(*out, leafOf(section, subsection, p, joinPath(base), joinPath(base)))
		}
	}
}

// emitSub emits one live subparam of a combo's selected choice.
//
// IDENTITY IS THE FULL CHAIN (round-2, cinder-otter F3): `base` carries the
// group-path segments (an arrayParam under a choice folds its key in, as
// walkParams folds top-level groups) and `choice` carries the WHOLE chain of
// selected choices walked so far ("mode/omni/inner"). A shorter identity —
// only the outermost choice, as this used to be — collapses two different
// nested-choice subparams into one row identity, and the matrix then ties
// them into a false red. The DISPLAY stays folded: `param: key`, the choice
// segment returning only on collision (BuildMatrix's union rule).
func emitSub(section, subsection string, p *Param, base []string, choice string, sub Param, out *[]Leaf) {
	ident := joinPath(base) + "/" + choice + ": " + sub.Key
	display := joinPath(base) + ": " + sub.Key
	if sub.Array != nil {
		// The group's key folds into the path — dropping it (as this once
		// did) merges the group's leaves into the choice's own namespace.
		grouped := append(append([]string{}, base...), sub.Key)
		for _, s2 := range sub.Array.Params {
			emitSub(section, subsection, p, grouped, choice, s2, out)
		}
		return
	}
	if sub.Combo != nil && sub.hasSubparams() {
		inner := choice + "/" + sub.Combo.ChildKey
		*out = append(*out, leafOf(section, subsection, &sub, display+": type", ident+": type"))
		for _, s2 := range sub.choiceParams() {
			emitSub(section, subsection, p, base, inner, s2, out)
		}
		return
	}
	*out = append(*out, leafOf(section, subsection, &sub, display, ident))
}

func leafOf(section, subsection string, p *Param, path, ident string) Leaf {
	return Leaf{
		Section: section, Subsection: subsection, Kind: KindParam,
		Path: path, Ident: ident,
		Value: p.Value(), Desc: p.Desc, Type: p.Type, Unit: p.Unit,
		Range: p.Range(), Credential: CredentialKey(p.Key),
	}
}

func joinPath(parts []string) string { return strings.Join(parts, ": ") }

// disambiguate is the PER-ROBOT half of the collision rule — defensive
// only, since one robot emits just its selected choice's subparams and so
// cannot collide with itself. The LIVE half runs over the union, in
// BuildMatrix: that is where a mixed fleet's RS485 and RS232 rows share a
// display (round-2).
func disambiguate(rows []Leaf) []Leaf {
	seen := map[string]int{}
	for _, r := range rows {
		seen[r.Path]++
	}
	for i := range rows {
		if seen[rows[i].Path] > 1 && rows[i].Ident != rows[i].Path {
			rows[i].Path = rows[i].Ident
		}
	}
	return rows
}

func fmtBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
