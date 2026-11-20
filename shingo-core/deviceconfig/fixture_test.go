package deviceconfig

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The fixture is a REAL robot's #1500 model file (SJV-SW600, saved by Eric
// 2026-11-19; the same document RoboShop's Device Config renders), with the
// one credential value already fingerprinted — the raw bytes are never
// committed, for the same reason the pull path sanitizes before storing.
const fixturePath = "testdata/SJV-SW600.model"

func fixture(t *testing.T) *ModelFile {
	t.Helper()
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return m
}

// TestGoldenCensus pins the live-leaf counting rule the brief ordered pinned
// at build: LIVE LEAVES INCLUDE THE SELECTED CHOICE'S SUBPARAMS. Trigger is
// the rule's proof — 16 live leaves (4+3+3+6), not its 12 top-level params,
// and it matches the brief's hand count exactly.
//
// The brief's earlier "530 leaves" is not a rival count: it is EXACT under
// the pre-pin rule (array params recursed, combo params FLAT — the selected
// choice's subparams unwalked), and the pinned rule adds exactly those,
// 714 - 530 = 184. The round-2 dedup disagreement (615 vs 513) was two
// different keys: by (section, path) — CAN1's and CAN2's canPort collapsing
// to one — the 714 rows are 513 distinct params, pinned below. One rule,
// one story: 51 sections, 80 subsections, 714 live param rows, 874
// rows including every subsection's name and enabled rows.
func TestGoldenCensus(t *testing.T) {
	m := fixture(t)
	rows := Leaves(m)

	sections, subs := map[string]bool{}, map[string]bool{}
	params, deviceRows := 0, 0
	distinct := map[string]bool{}
	for _, r := range rows {
		sections[r.Section] = true
		subs[r.Section+"/"+r.Subsection] = true
		if r.Kind == KindParam {
			params++
			distinct[r.Section+"/"+r.Path] = true
		} else {
			deviceRows++
		}
	}
	if len(sections) != 51 {
		t.Errorf("sections = %d, want 51", len(sections))
	}
	if len(subs) != 80 {
		t.Errorf("subsections = %d, want 80", len(subs))
	}
	if params != 714 {
		t.Errorf("live param rows = %d, want 714 (the pinned-rule recount)", params)
	}
	if len(distinct) != 513 {
		t.Errorf("distinct (section, path) params = %d, want 513 (the comment's dedup number)", len(distinct))
	}
	if deviceRows != 160 { // 80 subsections × name + enabled
		t.Errorf("name/enabled rows = %d, want 160", deviceRows)
	}
	if len(rows) != 874 {
		t.Errorf("total rows = %d, want 874", len(rows))
	}
}

// TestGoldenTrigger pins the rule's named example: the four Trigger devices
// are 4+3+3+6 live leaves, liftTrigger3 carries the live script reference
// under the naming rule (func: name / func: args), and a source selection
// renders as `source: type`.
func TestGoldenTrigger(t *testing.T) {
	rows := Leaves(fixture(t))
	perDevice := map[string]int{}
	paths := map[string]string{} // path -> value, on liftTrigger3
	for _, r := range rows {
		if r.Section != "Trigger" || r.Kind != KindParam {
			continue
		}
		perDevice[r.Subsection]++
		if r.Subsection == "liftTrigger3" {
			paths[r.Path] = r.Value
		}
	}
	want := map[string]int{"BumperTrigger": 4, "EMCTrigger1": 3, "EMCTrigger2": 3, "liftTrigger3": 6}
	for dev, n := range want {
		if perDevice[dev] != n {
			t.Errorf("Trigger/%s live leaves = %d, want %d", dev, perDevice[dev], n)
		}
	}
	for _, p := range []string{"source: type", "source: DI", "func: type", "func: name", "func: args"} {
		if _, ok := paths[p]; !ok {
			t.Errorf("liftTrigger3 missing row %q; have %v", p, paths)
		}
	}
	if v := paths["func: name"]; v != "jackDoMotor.py" {
		t.Errorf("func: name = %q, want jackDoMotor.py", v)
	}
	if v := paths["func: args"]; v != "{}" {
		t.Errorf("func: args = %q, want {}", v)
	}
}

// TestGoldenKnownRows pins rows the section walk verified by hand: the June
// incident's own row (charger > basic: chargeDI = 7), RoboShop's
// func:suspendTask on EMCTrigger1, a bare combo (CAN1's canPort = port1),
// and the device name/enabled rows.
func TestGoldenKnownRows(t *testing.T) {
	rows := Leaves(fixture(t))
	find := func(sec, sub, path string) (Leaf, bool) {
		for _, r := range rows {
			if r.Section == sec && r.Subsection == sub && r.Path == path {
				return r, true
			}
		}
		return Leaf{}, false
	}
	if r, ok := find("charger", "charger", "basic: chargeDI"); !ok {
		t.Errorf("charger basic: chargeDI row missing")
	} else if r.Value != "7" {
		t.Errorf("chargeDI = %q, want 7 (the June incident's reset value is -1)", r.Value)
	}
	if r, ok := find("Trigger", "EMCTrigger1", "func: type"); !ok {
		t.Errorf("Trigger func selection row missing (func's runScript choice carries subparams, so func is a with-subparams combo)")
	} else if r.Value != "suspendTask" {
		t.Errorf("EMCTrigger1 func = %q, want suspendTask", r.Value)
	}
	if r, ok := find("CAN", "CAN1", "canPort"); !ok {
		t.Errorf("CAN1 canPort row missing")
	} else if r.Value != "port1" {
		t.Errorf("canPort = %q, want port1", r.Value)
	}
	if r, ok := find("controller", "controller", "name"); !ok {
		t.Errorf("controller name row missing (device name renders as the subsection's first row)")
	} else if r.Value != "controller" {
		t.Errorf("controller name = %q, want controller", r.Value)
	}
	if r, ok := find("Trigger", "liftTrigger3", "enabled"); !ok {
		t.Errorf("liftTrigger3 enabled row missing")
	} else if r.Value != "false" {
		t.Errorf("liftTrigger3 enabled = %q, want false (device is disabled)", r.Value)
	}
}

// TestFixtureCredentialFingerprinted: every credential row with a value
// holds the deterministic SHA-256 fingerprint, never the plaintext — which
// is what the pull path stores. Empty-valued credential rows (the email
// password, unset on this robot) are fine: there is nothing to hide.
func TestFixtureCredentialFingerprinted(t *testing.T) {
	rows := Leaves(fixture(t))
	saw := false
	for _, r := range rows {
		if !r.Credential {
			continue
		}
		if r.Value == "" {
			continue
		}
		saw = true
		if r.Value == "12345678" {
			t.Errorf("credential row %s/%s holds PLAINTEXT", r.Section, r.Path)
		} else if r.Value != Fingerprint("12345678") {
			t.Errorf("credential row %s value = %q, want the deterministic fingerprint", r.Path, r.Value)
		}
	}
	if !saw {
		t.Fatal("no valued credential row found in the fixture — the ESP32 password row should be one")
	}
}

// TestExcludedAtServeOnly: the RoboShop-parity exclusion drops the row from
// the matrix while storage keeps it — exclusion is a display and comparison
// rule, never a storage one. SJV-SW600 is a differential robot, so its own
// leaves never contain omni's rollAngle (only the selected family's
// subparams are rows); the excluded row is therefore tested with a
// synthetic omni leaf, the day a real omni robot joins the pull.
func TestExcludedAtServeOnly(t *testing.T) {
	rows := Leaves(fixture(t))
	for _, r := range rows {
		if r.Ident == "mode/omni: rollAngle" {
			t.Fatal("differential fixture carries an omni leaf — only the selected choice's subparams may be rows")
		}
	}
	omni := append(rows, Leaf{
		Section: "chassis", Subsection: "chassis", Kind: KindParam,
		Path: "mode: rollAngle", Ident: "mode/omni: rollAngle", Value: "45",
	})
	view := BuildMatrix([]string{"AMR-01", "AMR-02"}, map[string]Pull{
		"AMR-01": {Vehicle: "AMR-01", Leaves: omni},
		"AMR-02": {Vehicle: "AMR-02", Leaves: rows},
	})
	for _, rv := range view.Rows {
		if rv.Section == "chassis" && rv.Ident == "mode/omni: rollAngle" {
			t.Error("rollAngle row reached the matrix — the exclusion list must drop it at serve time")
		}
	}
}

// TestSanitizeNoPlaintextInColumns: after Sanitize, neither JSONB column a
// pull writes can contain the credential. params comes from Leaves of the
// sanitized tree; config IS the sanitized bytes.
func TestSanitizeNoPlaintextInColumns(t *testing.T) {
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Re-introduce a plaintext credential exactly where the live payload
	// carries it, so the test proves the scrub rather than the fixture.
	for i := range m.DeviceTypes {
		if m.DeviceTypes[i].Name != "ESP32" {
			continue
		}
		for j := range m.DeviceTypes[i].Devices {
			for k := range m.DeviceTypes[i].Devices[j].DeviceParams {
				p := &m.DeviceTypes[i].Devices[j].DeviceParams[k]
				if CredentialKey(p.Key) {
					v := "12345678"
					p.StringValue = &v
				}
			}
		}
	}
	config, err := Sanitize(m)
	if err != nil {
		t.Fatal(err)
	}
	if jsonContains(config, "12345678") {
		t.Error("plaintext credential in the config JSONB column")
	}
	params, err := json.Marshal(Leaves(m))
	if err != nil {
		t.Fatal(err)
	}
	if jsonContains(params, "12345678") {
		t.Error("plaintext credential in the params JSONB column")
	}
	if !jsonContains(config, "sha256:") {
		t.Error("fingerprint missing from the sanitized config")
	}
	// Deterministic: same value, same fingerprint, every time.
	if Fingerprint("12345678") != Fingerprint("12345678") {
		t.Error("fingerprint not deterministic")
	}
	if Fingerprint("a") == Fingerprint("b") {
		t.Error("fingerprint collides on different values")
	}
}

// TestSanitizeRecursesChoices: a credential nested inside a combo choice's
// subparams is scrubbed too — the walker is total, not top-level-only.
func TestSanitizeRecursesChoices(t *testing.T) {
	v1, v2 := "topsecret", "nestedsecret"
	m := &ModelFile{Model: "T", DeviceTypes: []DeviceType{{
		Name: "S", Devices: []Device{{
			Name: "D", DeviceParams: []Param{
				{Key: "password", Type: "string", StringValue: &v1},
				{Key: "auth", Type: "comboParam", Combo: &ComboParam{
					ChildKey: "yes",
					ChildParams: []ChildParam{{Key: "yes", Params: []Param{
						{Key: "token", Type: "string", StringValue: &v2},
					}}},
				}},
			},
		}},
	}}}
	out, err := Sanitize(m)
	if err != nil {
		t.Fatal(err)
	}
	if jsonContains(out, "topsecret") || jsonContains(out, "nestedsecret") {
		t.Error("credential survived sanitize (top-level or nested-in-choice)")
	}
	if !jsonContains(out, Fingerprint("nestedsecret")) {
		t.Error("nested credential not fingerprinted")
	}
}

func jsonContains(raw []byte, needle string) bool {
	return strings.Contains(string(raw), needle)
}
