package deviceconfig

import "testing"

// The matrix rules under test are Eric's decided UI-spec semantics, each
// with its one-line reason. Synthetic pulls — the rules are the product;
// the fixture proves the parse, these prove the comparison.

func leaf(sub, path, value string) Leaf {
	return Leaf{Section: "S", Subsection: sub, Kind: KindParam, Path: path, Ident: path, Value: value}
}

func pull(v string, ls ...Leaf) Pull {
	return Pull{Vehicle: v, Leaves: append([]Leaf{
		{Section: "S", Subsection: "d", Kind: KindName, Path: "name", Ident: "name", Value: v},
	}, ls...)}
}

func cellOf(t *testing.T, view *MatrixView, path, vehicle string) Cell {
	t.Helper()
	for _, r := range view.Rows {
		if r.Path == path { // paths are unique within these tests' sections
			c, ok := r.Cells[vehicle]
			if !ok {
				t.Fatalf("row %s has no cell for %s", path, vehicle)
			}
			return c
		}
	}
	t.Fatalf("row %s not found", path)
	return Cell{}
}

// PLURALITY: of 10 where 7 hold one value, the 3 go red and the 7 stay
// clear — and the plurality line quotes the counts.
func TestPluralityMinorityReds(t *testing.T) {
	pulls := map[string]Pull{}
	fleet := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		v := string(rune('A' + i))
		fleet = append(fleet, v)
		val := "enabled"
		if i >= 7 {
			val = "disabled"
		}
		pulls[v] = pull(v, leaf("d", "state", val))
	}
	view := BuildMatrix(fleet, pulls)
	if c := cellOf(t, view, "state", "A"); c.State != StateOK {
		t.Errorf("majority cell = %s, want ok", c.State)
	}
	if c := cellOf(t, view, "state", "H"); c.State != StateDiffers {
		t.Errorf("minority cell = %s, want differs (RED)", c.State)
	}
	row := view.Rows[1] // [0] is the name row
	if row.Plurality != "enabled" || row.Counts["enabled"] != 7 || row.Counts["disabled"] != 3 {
		t.Errorf("plurality/counts = %q %v, want enabled 7/3", row.Plurality, row.Counts)
	}
}

// ABSENCE IS A COMPARED STATE: a dash against a valued plurality is a RED
// dash; a dash where the plurality is absence stays plain; a value-holder
// can be the minority on a mostly-absent row and reds by the same rule.
func TestAbsenceCompares(t *testing.T) {
	// A and B hold the param; C does not (structural absence).
	view := BuildMatrix([]string{"A", "B", "C"}, map[string]Pull{
		"A": pull("A", leaf("d", "onlySome", "7")),
		"B": pull("B", leaf("d", "onlySome", "7")),
		"C": pull("C"),
	})
	if c := cellOf(t, view, "onlySome", "C"); c.State != StateAbsentDiffers {
		t.Errorf("absent-against-valued = %s, want absent_differs (RED dash)", c.State)
	}

	// Mostly-absent row: two absent, one holder — absence is the plurality,
	// the HOLDER reds.
	view = BuildMatrix([]string{"A", "B", "C"}, map[string]Pull{
		"A": pull("A", leaf("d", "rare", "1")),
		"B": pull("B"),
		"C": pull("C"),
	})
	if c := cellOf(t, view, "rare", "B"); c.State != StateAbsent {
		t.Errorf("absent-with-absent-plurality = %s, want absent (plain dash)", c.State)
	}
	if c := cellOf(t, view, "rare", "A"); c.State != StateDiffers {
		t.Errorf("lone holder on mostly-absent row = %s, want differs", c.State)
	}
}

// TIE: no fleet value exists — every VALUED cell goes red, absent cells
// stay plain dashes, and the row says it is split.
func TestTieAllValuedRed(t *testing.T) {
	view := BuildMatrix([]string{"A", "B", "C", "D"}, map[string]Pull{
		"A": pull("A", leaf("d", "split", "x")),
		"B": pull("B", leaf("d", "split", "x")),
		"C": pull("C", leaf("d", "split", "y")),
		"D": pull("D", leaf("d", "split", "y")),
	})
	for _, v := range []string{"A", "B", "C", "D"} {
		if c := cellOf(t, view, "split", v); c.State != StateDiffers {
			t.Errorf("tie cell %s = %s, want differs", v, c.State)
		}
	}
	for _, r := range view.Rows {
		if r.Path == "split" && (!r.Tie || r.Plurality != "") {
			t.Errorf("tie row flags = tie:%v plurality:%q, want tie:true plurality:\"\"", r.Tie, r.Plurality)
		}
	}
}

// ONE PULLED ROBOT: nothing red — there is nothing to compare against.
func TestSingleRobotNothingReds(t *testing.T) {
	view := BuildMatrix([]string{"A", "B"}, map[string]Pull{
		"A": pull("A", leaf("d", "solo", "1")),
	})
	if c := cellOf(t, view, "solo", "A"); c.State != StateOK {
		t.Errorf("single pulled robot's own cell = %s, want ok", c.State)
	}
	if c := cellOf(t, view, "solo", "B"); c.State != StateMissing {
		t.Errorf("never-pulled robot's cell = %s, want missing (YELLOW, non-voting)", c.State)
	}
}

// YELLOW NEVER VOTES: a robot with no pull does not sway the plurality —
// three holders with a 2-1 split red the one even with a fourth robot
// missing entirely.
func TestMissingDoesNotVote(t *testing.T) {
	view := BuildMatrix([]string{"A", "B", "C", "D"}, map[string]Pull{
		"A": pull("A", leaf("d", "v", "1")),
		"B": pull("B", leaf("d", "v", "1")),
		"C": pull("C", leaf("d", "v", "2")),
		// D: no pull
	})
	if c := cellOf(t, view, "v", "C"); c.State != StateDiffers {
		t.Errorf("1-of-3 holder = %s, want differs; the missing robot must not vote", c.State)
	}
}

// CALIBRATION: on a curated light-blue row a differing cell renders light
// blue instead of red — and on a tie, every valued cell is blue.
func TestCalibrationLightBlue(t *testing.T) {
	mk := func(v, val string) Pull {
		p := Pull{Vehicle: v}
		l := Leaf{Section: "controller", Subsection: "controller", Kind: KindParam,
			Path: "basic: SSF", Ident: "basic: SSF", Value: val}
		p.Leaves = []Leaf{l}
		return p
	}
	view := BuildMatrix([]string{"A", "B"}, map[string]Pull{
		"A": mk("A", "16.03556"),
		"B": mk("B", "16.04000"),
	})
	if c := cellOf(t, view, "basic: SSF", "B"); c.State != StateCalibration {
		t.Errorf("differing calibration cell = %s, want calibration (LIGHT BLUE)", c.State)
	}

	// Tie on a calibration row: every valued cell blue, none red.
	view = BuildMatrix([]string{"A", "B"}, map[string]Pull{
		"A": mk("A", "1"),
		"B": mk("B", "2"),
	})
	for _, v := range []string{"A", "B"} {
		if c := cellOf(t, view, "basic: SSF", v); c.State != StateCalibration {
			t.Errorf("tie calibration cell %s = %s, want calibration", v, c.State)
		}
	}
}

// CREDENTIAL: the fingerprint is the comparison string (equal fingerprints
// = equal values, masking never suppresses a red), and the fingerprint
// itself never ships to the browser.
func TestCredentialComparesOnFingerprint(t *testing.T) {
	mk := func(v, val string) Pull {
		return Pull{Vehicle: v, Leaves: []Leaf{{
			Section: "ESP32", Subsection: "esp-wifi-mesh", Kind: KindParam,
			Path: "password", Ident: "password", Value: val, Credential: true,
		}}}
	}
	same1, same2, other := Fingerprint("12345678"), Fingerprint("12345678"), Fingerprint("99999999")
	view := BuildMatrix([]string{"A", "B", "C"}, map[string]Pull{
		"A": mk("A", same1),
		"B": mk("B", same2),
		"C": mk("C", other),
	})
	if c := cellOf(t, view, "password", "B"); c.State != StateOK {
		t.Errorf("equal fingerprints = %s, want ok — masking must never suppress agreement", c.State)
	}
	if c := cellOf(t, view, "password", "C"); c.State != StateDiffers {
		t.Errorf("different fingerprints = %s, want differs — masking must never suppress a red", c.State)
	}
	for _, r := range view.Rows {
		if r.Credential {
			for _, c := range r.Cells {
				if c.Value != "" {
					t.Errorf("credential value %q shipped to the browser — only the state may", c.Value)
				}
			}
		}
	}
}

// The union: rows only one robot has enter by themselves; a uniform fleet
// shows only its own family's rows. Two holders against one absent make the
// absence the minority — a RED dash (with one holder vs one absent the row
// ties, and a tie's dashes stay plain by rule).
func TestUnionRows(t *testing.T) {
	view := BuildMatrix([]string{"A", "B", "C"}, map[string]Pull{
		"A": pull("A", leaf("d", "common", "1"), leaf("d", "aOnly", "1")),
		"B": pull("B", leaf("d", "common", "2"), leaf("d", "aOnly", "1")),
		"C": pull("C", leaf("d", "common", "3")),
	})
	paths := map[string]bool{}
	for _, r := range view.Rows {
		paths[r.Path] = true
	}
	if !paths["aOnly"] || !paths["common"] {
		t.Errorf("union rows = %v, want aOnly and common", paths)
	}
	if c := cellOf(t, view, "aOnly", "C"); c.State != StateAbsentDiffers {
		t.Errorf("structural absence on union row = %s, want absent_differs", c.State)
	}
}
