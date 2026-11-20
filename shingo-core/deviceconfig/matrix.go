package deviceconfig

import (
	"sort"
	"time"
)

// Cell states — the five the round-1 designs converged on, with the absent
// dash split by whether it differs (ABSENCE IS A COMPARED STATE: a dash on
// a row whose plurality is a real value is a RED dash; the difference may
// be purely structural, but the field needs checking the moment the type is
// aligned again). Yellow — no value from pull — is the only non-voter.
const (
	StateOK            = "ok"             // holds the plurality value
	StateDiffers       = "differs"        // valued, differs from plurality (RED)
	StateCalibration   = "calibration"    // valued, differs, expected per-unit (LIGHT BLUE)
	StateMissing       = "missing"        // no pull for this robot (YELLOW, non-voting)
	StateAbsent        = "absent"         // dash; plurality is also absence (plain)
	StateAbsentDiffers = "absent_differs" // dash against a valued plurality (RED dash)
)

// absentToken is the plurality bucket for structural absence.
const absentToken = "\x00absent"

// Exclusion is one RoboShop-parity exclusion: a row the vendor's Device
// Config does not expose is not rendered, not compared, not counted — but
// still STORED (the pull keeps the model whole; exclusion is a display and
// comparison rule applied here, at serve time, so the list can grow without
// re-pulling. SYNTH-round1).
type Exclusion struct {
	Section string
	Ident   string
}

// ExcludedV1 is the v1 list: rollAngle exists only under chassis>mode's
// omni choice and is not configurable in RoboShop (brief, section walk).
var ExcludedV1 = []Exclusion{
	{Section: "chassis", Ident: "mode/omni: rollAngle"},
}

// paritySectionsV1 is the RoboShop-parity SECTION allow-list: the sections
// the vendor's Device Config tree actually renders, as walked in the brief
// (the other 33 of the model file's 51 — Gripper, arm, fork, mailsender,
// empty container shells… — are file internals or hardware this robot does
// not carry, and the floor ruled them out of the matrix on the first live
// sitting, 2026-11-20). Rows outside the list are dropped at SERVE time
// only — the pull stores the whole model, so a section joins the matrix by
// being added here, with no re-pull. An allow-list (not a block-list)
// deliberately: a heterogeneous robot's UNKNOWN section stays hidden until
// someone rules it in, rather than spraying new rows the moment it arrives.
var paritySectionsV1 = map[string]bool{
	"CAN": true, "DI": true, "DISensor": true, "DO": true, "EMC": true,
	"ESP32": true, "Manipulators": true, "Trigger": true, "battery": true,
	"charger": true, "chassis": true, "controller": true, "jack": true,
	"laser": true, "led": true, "motor": true, "pgv": true, "soundplayer": true,
}

func sectionShown(section string) bool {
	return paritySectionsV1[section]
}

func excluded(section, ident string) bool {
	for _, e := range ExcludedV1 {
		if e.Section == section && e.Ident == ident {
			return true
		}
	}
	return false
}

// calibrationV1 is the curated light-blue list (Eric, 2026-11-19; the motor
// entries pending his veto): per-device constants whose differences are
// expected. Entries are "section/subsection-scope ident" against the leaf's
// IDENT (which keeps the combo choice, so func/linear: offset and
// func/spin: offset are addressable separately); the bare path is matched
// too so a list entry written without the choice still lands.
var calibrationV1 = map[string]bool{
	"controller/basic: SSF":      true,
	"controller/basic: Bax":      true,
	"controller/basic: Bay":      true,
	"controller/basic: Baz":      true,
	"motor/basic: currentFactor": true,
	"motor/func/walk: motorP":    true,
	"motor/func/walk: motorI":    true,
	"motor/func/linear: upKP":    true,
	"motor/func/linear: upKI":    true,
	"motor/func/linear: upKD":    true,
	"motor/func/linear: downKP":  true,
	"motor/func/linear: downKI":  true,
	"motor/func/linear: downKD":  true,
	"motor/func/linear: offset":  true,
	"motor/func/spin: offset":    true,
}

func calibrationRow(l Leaf) bool {
	return calibrationV1[l.Section+"/"+l.Ident] || calibrationV1[l.Section+"/"+l.Path]
}

// Pull is one robot's stored pull as the matrix consumes it.
type Pull struct {
	Vehicle  string
	PulledAt time.Time
	Leaves   []Leaf
}

// RobotColumn is one AMR column. A robot with no pull is a column of yellow
// cells (HasPull=false); the column stays because the question the page
// answers is fleet-wide.
type RobotColumn struct {
	Vehicle  string    `json:"vehicle"`
	PulledAt time.Time `json:"pulled_at"`
	HasPull  bool      `json:"has_pull"`
}

// Cell is one robot's state on one row. Value is "" on credential rows: the
// fingerprint is the comparison string, computed here, never shipped — the
// browser only ever learns that a credential agrees or differs.
type Cell struct {
	State string `json:"state"`
	Value string `json:"value"`
}

// RowView is one comparison row across every robot column, with the
// plurality the reds refer to and the counts the hover quotes.
type RowView struct {
	Section     string          `json:"section"`
	Subsection  string          `json:"subsection"`
	Kind        string          `json:"kind"`
	Path        string          `json:"path"`
	Ident       string          `json:"ident"`
	Desc        string          `json:"desc"`
	Type        string          `json:"type"`
	Unit        string          `json:"unit"`
	Range       string          `json:"range"`
	Credential  bool            `json:"credential"`
	Calibration bool            `json:"calibration"`
	Plurality   string          `json:"plurality"` // "" on a tie: no fleet value exists
	Tie         bool            `json:"tie"`
	Counts      map[string]int  `json:"counts"` // value/"absent" -> voters, for the hover
	Cells       map[string]Cell `json:"cells"`  // vehicle -> cell
}

// MatrixView is GET /api/robots/device-config's whole answer.
type MatrixView struct {
	Robots []RobotColumn `json:"robots"`
	Rows   []RowView     `json:"rows"`
	// CanPull reports whether the fleet backend implements
	// fleet.DeviceConfigReader — the same type assertion the pull handler
	// answers 501 with, surfaced so the page can hide its Pull button on a
	// fleet (the simulator) that cannot pull at all.
	CanPull bool `json:"can_pull"`
}

// rowIdent is the subsection-scoped identity of a leaf: the same param in
// two subsections is two rows, and every subsection's name/enabled rows are
// their own rows (they compare like any value).
func rowIdent(l Leaf) string {
	return l.Subsection + "/" + l.Ident
}

// BuildMatrix computes the fleet matrix.
//
// vehicles is the fleet — columns exist for unpulled robots too, all
// yellow. pulls maps vehicle -> stored pull for those that have one. Rows
// are the UNION across pulled robots: subsection-scoped identities in the
// first (alphabetically) pulled robot's document order, then rows only
// later robots have, appended. Plurality is over valued cells AND absences
// together — a value-holder can be the minority on a mostly-absent row and
// reds by the same rule. A tie sends every VALUED cell red (light blue on
// calibration rows) and leaves absent cells plain dashes. One pulled robot
// compares nothing: its own value is its own plurality, so nothing reds.
//
// The body is a driver over the phases, each named below; the semantics
// live with the phase that owns them.
func BuildMatrix(vehicles []string, pulls map[string]Pull) *MatrixView {
	view := &MatrixView{}
	for _, v := range vehicles {
		col := RobotColumn{Vehicle: v}
		if p, ok := pulls[v]; ok {
			col.HasPull, col.PulledAt = true, p.PulledAt
		}
		view.Robots = append(view.Robots, col)
	}

	pulled := pulledVoters(vehicles, pulls)
	order, leafOf, values := unionRows(pulled, pulls)
	disambiguateCollisions(order, leafOf)

	for _, id := range order {
		l := leafOf[id]
		row := RowView{
			Section: l.Section, Subsection: l.Subsection, Kind: l.Kind,
			Path: l.Path, Ident: l.Ident, Desc: l.Desc, Type: l.Type,
			Unit: l.Unit, Range: l.Range, Credential: l.Credential,
			Calibration: calibrationRow(l),
			Counts:      map[string]int{},
			Cells:       map[string]Cell{},
		}
		counts, best, tie := tallyRow(id, pulled, values)
		row.Tie = tie
		if !tie && best != absentToken {
			row.Plurality = best
		}
		fillCounts(&row, counts, best, tie)
		row.Cells = rowCells(id, row, vehicles, pulls, values, best, tie)
		view.Rows = append(view.Rows, row)
	}
	return view
}

// pulledVoters intersects the stored pulls to the fleet list handed in: a
// pull whose vehicle has no column must not vote. The GET handler
// deliberately unions stored-pull robots into that list, so a
// decommissioned robot keeps its column; the guard makes BuildMatrix total
// for any other caller (a stale store row for a robot the fleet no longer
// names would otherwise bend every plurality it ever held, with no cell to
// see it by — round-2, cinder-otter F4).
func pulledVoters(vehicles []string, pulls map[string]Pull) []string {
	vset := make(map[string]bool, len(vehicles))
	for _, v := range vehicles {
		vset[v] = true
	}
	pulled := make([]string, 0, len(pulls))
	for v := range pulls {
		if vset[v] {
			pulled = append(pulled, v)
		}
	}
	sort.Strings(pulled)
	return pulled
}

// unionRows walks the pulled robots (alphabetical, so the union order is
// deterministic) and collects the row union in document order of the FIRST
// robot that has each row, one representative leaf per row (desc/type/
// range come from it; they are vendor-static), and every robot's value per
// row. The RoboShop-parity filters drop their rows here, so an excluded
// section or param never reaches the matrix at all — section first (every
// row kind), then the per-ident exclusions (params).
func unionRows(pulled []string, pulls map[string]Pull) (order []string, leafOf map[string]Leaf, values map[string]map[string]string) {
	leafOf = map[string]Leaf{}
	values = map[string]map[string]string{} // rowIdent -> vehicle -> value or absentToken
	for _, v := range pulled {
		for _, l := range pulls[v].Leaves {
			if !sectionShown(l.Section) {
				continue
			}
			if l.Kind == KindParam && excluded(l.Section, l.Ident) {
				continue
			}
			id := rowIdent(l)
			if _, ok := leafOf[id]; !ok {
				order = append(order, id)
				leafOf[id] = l
				values[id] = map[string]string{}
			}
			values[id][v] = l.Value
		}
	}
	return order, leafOf, values
}

// disambiguateCollisions runs the collision rule over the UNION, never
// over one robot: a single robot emits only its SELECTED choice's
// subparams, so two rows with the same display can only be cross-robot —
// the brief's named RS485/RS232 devName case, one mixed pull away. Where
// one subsection's rows share a display Path, both fall back to the
// identity, which keeps the choice segment (round-2, cinder-otter F2 /
// argent-condor). It rewrites leafOf's Paths in place.
func disambiguateCollisions(order []string, leafOf map[string]Leaf) {
	type displayKey struct{ sec, sub, path string }
	collisions := map[displayKey]int{}
	for _, id := range order {
		l := leafOf[id]
		collisions[displayKey{l.Section, l.Subsection, l.Path}]++
	}
	for _, id := range order {
		l := leafOf[id]
		if collisions[displayKey{l.Section, l.Subsection, l.Path}] > 1 && l.Ident != l.Path {
			l.Path = l.Ident
			leafOf[id] = l
		}
	}
}

// tallyRow counts the row's votes over the PULLED robots only: a pulled
// robot without the row is structurally absent (the param does not exist
// under its selection), which is a compared state, not an abstention —
// absence votes as absentToken. Plurality is the strict maximum; a shared
// maximum is a tie.
func tallyRow(id string, pulled []string, values map[string]map[string]string) (counts map[string]int, best string, tie bool) {
	counts = map[string]int{}
	for _, v := range pulled {
		val, ok := values[id][v]
		if !ok {
			val = absentToken
		}
		counts[val]++
	}
	bestN := 0
	for val, n := range counts {
		if n > bestN {
			best, bestN, tie = val, n, false
		} else if n == bestN && val != best {
			tie = true
		}
	}
	return counts, best, tie
}

// fillCounts publishes the hover's counts. A credential row speaks in
// agree/differ buckets only: the fingerprint never ships (rule 4) — and
// the GET is public, where an unsalted SHA-256 of a factory default is a
// dictionary lookup away from the value; the per-cell states already carry
// agreement (round-2, 3-of-4 reviewers). Every other row shows
// value -> voters, with absences bucketed as "absent".
func fillCounts(row *RowView, counts map[string]int, best string, tie bool) {
	if row.Credential {
		row.Plurality = ""
		agree, differ, absent := 0, 0, 0
		for val, n := range counts {
			switch {
			case val == absentToken:
				absent += n
			case !tie && val == best:
				agree += n
			default:
				differ += n
			}
		}
		row.Counts["agree"], row.Counts["differ"], row.Counts["absent"] = agree, differ, absent
		return
	}
	for val, n := range counts {
		if val == absentToken {
			row.Counts["absent"] = n
		} else {
			row.Counts[val] = n
		}
	}
}

// rowCells builds one cell per fleet column: yellow for a robot with no
// pull (the only non-voter), the absent dash split by whether absence is
// the plurality's own state (a dash against a valued plurality is a RED
// dash), and valued cells compared to the plurality — a differing value on
// a calibration row softens to light blue, and a credential cell ships no
// value at all. best/tie are tallyRow's answers, NOT row.Plurality/Tie:
// the comparison always runs against the raw plurality even where the
// published row redacts it (credential rows, ties).
func rowCells(id string, row RowView, vehicles []string, pulls map[string]Pull, values map[string]map[string]string, best string, tie bool) map[string]Cell {
	cells := map[string]Cell{}
	for _, v := range vehicles {
		if _, has := pulls[v]; !has {
			cells[v] = Cell{State: StateMissing}
			continue
		}
		val, ok := values[id][v]
		if !ok {
			val = absentToken
		}
		if val == absentToken {
			if tie || best == absentToken {
				cells[v] = Cell{State: StateAbsent}
			} else {
				cells[v] = Cell{State: StateAbsentDiffers}
			}
			continue
		}
		cell := Cell{State: StateOK}
		if tie || val != best {
			cell.State = StateDiffers
		}
		if cell.State == StateDiffers && row.Calibration {
			cell.State = StateCalibration
		}
		if !row.Credential {
			cell.Value = val
		}
		cells[v] = cell
	}
	return cells
}
