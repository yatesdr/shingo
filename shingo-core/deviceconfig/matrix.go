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
func BuildMatrix(vehicles []string, pulls map[string]Pull) *MatrixView {
	view := &MatrixView{}
	for _, v := range vehicles {
		col := RobotColumn{Vehicle: v}
		if p, ok := pulls[v]; ok {
			col.HasPull, col.PulledAt = true, p.PulledAt
		}
		view.Robots = append(view.Robots, col)
	}

	pulled := make([]string, 0, len(pulls))
	for v := range pulls {
		pulled = append(pulled, v)
	}
	sort.Strings(pulled)

	// Union order + one representative leaf (desc/type/range come from the
	// first robot that has the row; they are vendor-static).
	order := []string{}
	leafOf := map[string]Leaf{}
	values := map[string]map[string]string{} // rowIdent -> vehicle -> value or absentToken
	for _, v := range pulled {
		for _, l := range pulls[v].Leaves {
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

		// Absence votes: a pulled robot without the row is structurally
		// absent (the param does not exist under its selection), which is a
		// compared state, not an abstention.
		counts := map[string]int{}
		for _, v := range pulled {
			val, ok := values[id][v]
			if !ok {
				val = absentToken
			}
			counts[val]++
		}
		// Plurality: the strict maximum; a shared maximum is a tie.
		best, bestN, tie := "", 0, false
		for val, n := range counts {
			if n > bestN {
				best, bestN, tie = val, n, false
			} else if n == bestN && val != best {
				tie = true
			}
		}
		row.Tie = tie
		if !tie && best != absentToken {
			row.Plurality = best
		}
		for val, n := range counts {
			if val == absentToken {
				row.Counts["absent"] = n
			} else {
				row.Counts[val] = n
			}
		}

		for _, v := range vehicles {
			_, has := pulls[v]
			if !has {
				row.Cells[v] = Cell{State: StateMissing}
				continue
			}
			val, ok := values[id][v]
			if !ok {
				val = absentToken
			}
			if val == absentToken {
				if tie || best == absentToken {
					row.Cells[v] = Cell{State: StateAbsent}
				} else {
					row.Cells[v] = Cell{State: StateAbsentDiffers}
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
			row.Cells[v] = cell
		}
		view.Rows = append(view.Rows, row)
	}
	return view
}
