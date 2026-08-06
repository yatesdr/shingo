// wiredrift_test.go — the guard for the bug this package keeps having.
//
// Five fields have been rediscovered on the /robotsStatus wire after months
// of being silently discarded: confidence, area_ids, the battery/thermal
// block, disable_paths and undispatchable_reason. Every one was lost at the
// same place — mapRobotStatus, one function deciding which of 55 wire keys
// become struct fields — and every one was invisible for the same reason: a
// Go struct cannot notice a field it never declared, so no test written
// against the struct will ever catch the next one.
//
// These tests are written against the WIRE instead. The fixtures are real
// captures, one per plant, taken 2026-08-06 and kept verbatim.
//
// Two rules, both learned the expensive way:
//
//  1. An unknown wire key FAILS rather than passing silently. A vendor
//     firmware update that adds a field should be a reviewed decision, not a
//     discovery in six months.
//
//  2. A field may not be satisfied by a hardcoded constant. Suspended: false
//     sat in mapRobotStatus behind a "Phase 2" comment while RDS published
//     the answer on every poll. That is worse than a drop: a dropped field
//     looks like never-having-had-it, which is honest, while a constant
//     looks like a measurement.

package seerrds

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"shingocore/fleet"

	"shingocore/rds"
)

// fixtures are verbatim /robotsStatus captures. Both plants, because a
// single plant's payload cannot tell a vendor-wide field apart from a
// site-configured one.
var fixtures = map[string]string{
	"springfield":  "testdata/robotsstatus_spr_2026-08-06.json",
	"hopkinsville": "testdata/robotsstatus_hk_2026-08-06.json",
}

// notCarried is the explicit allow-list of wire keys we have LOOKED AT and
// decided not to map. Every entry is a decision with a reason; a key that is
// on the wire and in neither this list nor a struct fails the test.
//
// Keyed by the JSON path of the containing object.
var notCarried = map[string]map[string]string{
	"": {
		"model_md5":            "hashes the robot MODEL definition, not the map — byte-identical at both plants, which is the proof. Carrying it as a map gate would be wrong in a way that looks right",
		"upload_scene_status":  "RDS's own scene-upload progress; transient UI state with no consumer here",
		"dynamic_obstacle":     "empty at both plants",
		"hk_area_state":        "empty at both plants",
		"traffic_control_info": "empty at both plants",
		"is_error":             "fleet-wide error flag; per-robot is_error is carried and is the actionable one",
		"alarms":               "fleet-level duplicate of the per-robot alarms already carried",
		"errors":               "fleet-level severity bucket; see alarms",
		"fatals":               "fleet-level severity bucket; see alarms",
		"warnings":             "fleet-level severity bucket; see alarms",
		"notices":              "fleet-level severity bucket; see alarms",
	},
	"report[]": {
		"chassis":                 "static polygon per robot model",
		"lock_info":               "RDS resource bookkeeping, not robot state",
		"labels":                  "RDS grouping metadata",
		"changes":                 "RDS internal diff bookkeeping",
		"src_release":             "RDS build string",
		"isLoaded":                "duplicate of jack/container state already carried",
		"remaining_time":          "RDS's own ETA; Core computes its own from mission telemetry",
		"area_resources_occupied": "traffic-control bookkeeping, owned by the count-group loop",
		"finished_path":           "route actually driven — WANTED, and deliberately deferred: it belongs with the per-traversal tier, which is not built",
		"unfinished_path":         "route remaining — same as finished_path",
	},
	"report[].basic_info": {
		"current_group": "RDS dispatch grouping; robot_group is read from the scene instead",
		"current_area":  "the RDS SCENE area (\"Area-01\"), a single value across a whole plant. rbk_report.area_ids is the robot's own map area and is the one that carries information",
		// These three were found by this test on its first run, which is
		// the test doing its job: no prior audit named them, including the
		// one that enumerated every rbk_report key.
		"current_label": "empty array at both plants; RDS labelling with no consumer",
		"dsp_version":   "motor-controller firmware string. Inventory, not state — worth carrying the day anyone correlates a fault with a firmware level, and not before",
		"robot_note":    "free-text note, identical to model at both plants",
	},
	"report[].rbk_report": {
		"arm_info":             "no arms fitted at either plant",
		"roller":               "unfitted",
		"fork":                 "unfitted",
		"brake":                "chassis detail with no consumer",
		"soft_emc":             "duplicate of emergency, already carried",
		"spin":                 "chassis articulation; vx/vy/w already carry motion",
		"steer":                "chassis articulation; see spin",
		"received_on":          "RDS receive timestamp, not a robot fact",
		"lock_info":            "RDS resource bookkeeping",
		"battery_extra":        "empty string at both plants",
		"requestCurrent":       "charger handshake detail; charge current/voltage are carried",
		"requestVoltage":       "charger handshake detail; see requestCurrent",
		"DI":                   "digital inputs — WANTED for the charge-DI incident family, deliberately deferred to its own change",
		"DO":                   "digital outputs — see DI",
		"info":                 "nested block detail; currentBlockId duplicates the mission-table lookup Core already does",
		"errors":               "flat duplicate of alarms.errors, carrying a dynamic \"<code>\":<ts> key",
		"fatals":               "flat duplicate of alarms.fatals",
		"warnings":             "flat duplicate of alarms.warnings",
		"notices":              "flat duplicate of alarms.notices",
		"containers":           "carried as Containers on the struct",
		"available_containers": "carried",
		"total_containers":     "carried",
	},
	// Six of the nine jack fields are unmapped. Load count, height and error
	// code are the three with consumers; the rest describe a lift mechanism
	// nothing in Core reasons about.
	//
	// The names here are read off the wire, not off a design note. An
	// earlier draft of this list guessed jack_pos and jack_max_pos from a
	// prose description and this test rejected both — which is the same
	// class of error it exists to catch, caught on itself.
	"report[].rbk_report.jack": {
		"jack_state":  "lift state machine; no consumer",
		"jack_isFull": "no consumer",
		"jack_enable": "no consumer",
		"jack_speed":  "no consumer",
		"jack_emc":    "lift emergency stop; the chassis-level emergency flag is the one Core acts on",
		"jack_mode":   "no consumer",
	},
}

// structFor maps a JSON path to the Go type that claims to model it.
func structFor(path string) any {
	switch path {
	case "":
		return rds.RobotsStatusResponse{}
	case "report[]":
		return rds.RobotStatus{}
	case "report[].basic_info":
		return rds.RobotBasicInfo{}
	case "report[].rbk_report":
		return rds.RbkReport{}
	case "report[].rbk_report.jack":
		return rds.JackReport{}
	case "report[].undispatchable_reason":
		return rds.UndispatchableReason{}
	case "report[].rbk_report.alarms":
		return rds.RbkAlarms{}
	}
	return nil
}

// jsonTags returns the set of json tag names declared on a struct, including
// those promoted from embedded structs (rds.Response is embedded in the
// robots-status envelope and its keys arrive at the top level).
func jsonTags(v any) map[string]bool {
	out := map[string]bool{}
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.Anonymous && f.Type.Kind() == reflect.Struct {
				walk(f.Type)
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name != "" && name != "-" {
				out[name] = true
			}
		}
	}
	walk(reflect.TypeOf(v))
	return out
}

// wireKeys walks one captured payload and returns the key set observed at
// each modelled JSON path. Every robot in the payload is unioned, because a
// field can be absent on one robot and present on another.
func wireKeys(t *testing.T, path string) map[string]map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(path))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	seen := map[string]map[string]bool{}
	add := func(p string, m map[string]any) {
		if seen[p] == nil {
			seen[p] = map[string]bool{}
		}
		for k := range m {
			seen[p][k] = true
		}
	}
	add("", doc)
	report, _ := doc["report"].([]any)
	for _, r := range report {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		add("report[]", rm)
		for _, sub := range []string{"basic_info", "rbk_report", "undispatchable_reason"} {
			if m, ok := rm[sub].(map[string]any); ok {
				add("report[]."+sub, m)
			}
		}
		if rb, ok := rm["rbk_report"].(map[string]any); ok {
			for _, sub := range []string{"jack", "alarms"} {
				if m, ok := rb[sub].(map[string]any); ok {
					add("report[].rbk_report."+sub, m)
				}
			}
		}
	}
	return seen
}

// TestWireKeysAreCarriedOrExplained is the drift guard. For every key on the
// wire at every modelled level, the key is either on the Go struct or named
// in notCarried with a reason. Anything else fails.
func TestWireKeysAreCarriedOrExplained(t *testing.T) {
	for plant, path := range fixtures {
		t.Run(plant, func(t *testing.T) {
			for jsonPath, keys := range wireKeys(t, path) {
				target := structFor(jsonPath)
				if target == nil {
					t.Fatalf("no struct registered for wire path %q — a new nesting level appeared on the wire and nothing models it", jsonPath)
				}
				carried := jsonTags(target)
				allowed := notCarried[jsonPath]
				var unexplained []string
				for k := range keys {
					if carried[k] || allowed[k] != "" {
						continue
					}
					unexplained = append(unexplained, k)
				}
				sort.Strings(unexplained)
				if len(unexplained) > 0 {
					t.Errorf("wire path %q carries keys that are neither mapped nor explained: %v\n"+
						"Add each to the struct, or to notCarried with the reason it is not worth carrying. "+
						"Do not delete this test to make it pass — it exists because five fields were lost exactly here.",
						jsonPath, unexplained)
				}
			}
		})
	}
}

// TestNotCarriedListIsNotStale fails when the allow-list names a key that is
// no longer on the wire. A stale exemption is how a field quietly stops
// being reviewed: it reads as "we decided about this" when nobody has looked
// since the vendor removed it.
func TestNotCarriedListIsNotStale(t *testing.T) {
	observed := map[string]map[string]bool{}
	for _, path := range fixtures {
		for jsonPath, keys := range wireKeys(t, path) {
			if observed[jsonPath] == nil {
				observed[jsonPath] = map[string]bool{}
			}
			for k := range keys {
				observed[jsonPath][k] = true
			}
		}
	}
	for jsonPath, allowed := range notCarried {
		for k := range allowed {
			if !observed[jsonPath][k] {
				t.Errorf("notCarried[%q][%q] names a key that is on neither plant's wire — "+
					"either the fixture is stale or the exemption is", jsonPath, k)
			}
		}
	}
}

// TestBothPlantsAgreeOnTheWireShape asserts the two captures have identical
// key sets.
//
// This is what makes a single-plant observation generalisable. It is also
// the check that settled whether ref_pos / update_reason / loc_state are
// site configuration or RDS behaviour: absent at BOTH plants with otherwise
// identical key sets means RDS does not forward them, so the answer is not
// "check the other plant" — it is "they are robot-push-only."
func TestBothPlantsAgreeOnTheWireShape(t *testing.T) {
	var plants []string
	for p := range fixtures {
		plants = append(plants, p)
	}
	sort.Strings(plants)
	base := wireKeys(t, fixtures[plants[0]])
	for _, p := range plants[1:] {
		other := wireKeys(t, fixtures[p])
		for jsonPath, keys := range base {
			for k := range keys {
				if !other[jsonPath][k] {
					t.Errorf("%q has %s.%s and %s does not — the wire shape is site-dependent, "+
						"which means a field audited at one plant says nothing about the other",
						plants[0], jsonPath, k, p)
				}
			}
			for k := range other[jsonPath] {
				if !keys[k] {
					t.Errorf("%q has %s.%s and %s does not", p, jsonPath, k, plants[0])
				}
			}
		}
	}
}

// TestLocalizationDetailIsNotOnTheWire pins the negative result that closed
// the biggest open question in this design.
//
// ref_pos (the reflectors a robot currently sees), update_reason (odometry
// vs laser correction) and loc_state (normal / skidding / low-confidence)
// would make the reflector mechanism directly measurable instead of
// inferred. They are absent from RDS's rbk_report at both plants, so they
// are robot-push-only and reaching them means opening a second connection to
// every robot — a decision the original design deliberately refused.
//
// The test exists so that if RDS ever starts forwarding them, somebody finds
// out on the next run instead of never. A failure here is GOOD NEWS.
func TestLocalizationDetailIsNotOnTheWire(t *testing.T) {
	for plant, path := range fixtures {
		keys := wireKeys(t, path)["report[].rbk_report"]
		for _, f := range []string{"ref_pos", "update_reason", "loc_state", "similarity"} {
			if keys[f] {
				t.Errorf("%s: rbk_report now carries %q — RDS has started forwarding the "+
					"localization detail this design was built without. Revisit: it makes the "+
					"reflector mechanism directly measurable and may delete derived work.", plant, f)
			}
		}
	}
}

// TestMapperAssignsFromTheWire is rule 2, and it is the one a key-set test
// cannot express.
//
// Every field in mapRobotStatus's composite literal must derive from the
// function's parameter. A field assigned a constant is a hardcoded answer
// wearing the shape of a measurement, and it is strictly worse than not
// carrying the field at all.
func TestMapperAssignsFromTheWire(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "mappers.go", nil, 0)
	if err != nil {
		t.Fatalf("parse mappers.go: %v", err)
	}

	var lit *ast.CompositeLit
	var param string
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "mapRobotStatus" {
			return true
		}
		if len(fn.Type.Params.List) > 0 && len(fn.Type.Params.List[0].Names) > 0 {
			param = fn.Type.Params.List[0].Names[0].Name
		}
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			if cl, ok := m.(*ast.CompositeLit); ok && lit == nil {
				lit = cl
				return false
			}
			return true
		})
		return false
	})
	if lit == nil || param == "" {
		t.Fatal("could not find mapRobotStatus's return literal — this test is structural " +
			"and needs updating if the mapper was reshaped")
	}

	// referencesParam reports whether an expression reads anything off the
	// mapper's input. Constants, and only constants, do not.
	referencesParam := func(e ast.Expr) bool {
		found := false
		ast.Inspect(e, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == param {
				found = true
			}
			return !found
		})
		return found
	}

	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		name := kv.Key.(*ast.Ident).Name
		if referencesParam(kv.Value) {
			continue
		}
		t.Errorf("mapRobotStatus assigns %s from something other than the wire. "+
			"If the fleet does not publish it, leave the field off rather than asserting a "+
			"value: a dropped field reads as never-having-had-it, a constant reads as an answer. "+
			"(This is the check Suspended: false would have failed for months.)", name)
	}
}

// ── The scene envelope, and the state it must not be confused with ─────────

// GetSceneState must report "never polled" as something other than "nothing
// disabled".
//
// Both are an empty DisabledPaths, and Springfield has four lanes switched
// off right now — so a consumer that reads the slice alone renders those four
// as enabled for every window before the first successful poll: boot,
// reconnect, or a backend that does not implement this at all. That is
// exactly the false reassurance the field was carried to prevent, and it is
// the guide's own rule ("no data, zero and not applicable must look
// different") failing at the type rather than at the CSS.
//
// Asserted through the interface rather than the concrete adapter, because
// the property belongs to the contract: any backend added later has to keep
// it.
func TestSceneState_NeverPolledIsNotNothingDisabled(t *testing.T) {
	var p fleet.SceneStateProvider = New(Config{BaseURL: "http://127.0.0.1:1", Timeout: time.Millisecond})

	state, ok := p.GetSceneState()
	if ok {
		t.Fatal("a freshly constructed adapter has never seen the envelope and must report so")
	}
	if len(state.DisabledPaths) != 0 || !state.ObservedAt.IsZero() {
		t.Errorf("unobserved state should be empty, got %+v", state)
	}

	// After an observation with genuinely nothing disabled, the VALUE is
	// identical and only the flag separates them. That is the whole point:
	// if the flag were dropped these two cases would be one.
	a := p.(*Adapter)
	a.sceneMu.Lock()
	a.sceneState, a.sceneSeen = mapSceneState(&rds.RobotsStatusResponse{SceneMD5: "abc"}, time.Now()), true
	a.sceneMu.Unlock()

	observed, ok := p.GetSceneState()
	if !ok {
		t.Fatal("after an observation the flag must be true")
	}
	if len(observed.DisabledPaths) != 0 {
		t.Fatalf("fixture: this case must also have an empty slice, got %v", observed.DisabledPaths)
	}
	if observed.ObservedAt.IsZero() {
		t.Error("an observed state must carry when it was observed")
	}
}

// The envelope's disabled lanes are unwrapped from the vendor's object form,
// and blanks are dropped rather than becoming empty ids.
//
// Four lanes are disabled at Springfield as of 2026-08-06 and none at
// Hopkinsville, so this is read straight off the captured fixtures: the
// numbers are the plant's, not a hand-built literal.
func TestSceneState_DisabledPathsComeOffTheWire(t *testing.T) {
	want := map[string]int{"springfield": 4, "hopkinsville": 0}
	for plant, path := range fixtures {
		raw, err := os.ReadFile(filepath.FromSlash(path))
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
		var resp rds.RobotsStatusResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatalf("%s: parse: %v", plant, err)
		}
		state := mapSceneState(&resp, time.Now())
		if got := len(state.DisabledPaths); got != want[plant] {
			t.Errorf("%s: %d disabled paths, want %d", plant, got, want[plant])
		}
		if state.SceneMD5 == "" {
			t.Errorf("%s: scene_md5 did not survive the envelope", plant)
		}
		for _, id := range state.DisabledPaths {
			if id == "" {
				t.Errorf("%s: an empty id reached the list", plant)
			}
		}
	}
}
