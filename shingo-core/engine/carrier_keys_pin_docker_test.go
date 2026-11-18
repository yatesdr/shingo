//go:build docker

package engine

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store"
)

// carrier_keys_pin_docker_test.go — brief B's Unit 0, against brief A's pin.
//
// Brief A's characterisation (block_record_pin_docker_test.go) is the pin: at a
// plant with no key on any family its 27 transcripts do not change. This file
// runs the same 27 cases, through the pin's own run functions, twice: plain,
// and with every carrier family keyed and every location's scene copy listing
// every key. The two transcripts must agree line for line except for the
// binTask a block carries (and the finish line's echo of it), and the change
// must be the brief's rule exactly:
//
//   - every JackLoad becomes its cart's family lift key: a lift that stays
//     JackLoad is a block whose cart was not on record when it was built (the
//     brief's STOP);
//   - a Wait becomes a family wait key only where the order's next block is a
//     lift at the same location, and then the key of that lift's family;
//     every other Wait (a lane gate point, a dig dwell, a turn in front of a
//     set-down) stays Wait;
//   - nothing else changes: JackUnload, a load sequence's names, an orderless
//     block; and no finish line changes but for the name it echoes (the arm,
//     steps_done and every cart's node and holder are the plain run's).

const keyedPinPrefix = "TestCarrierKeysPin_EveryPinShapeKeyed/"

// keyEveryFamilyAndLocation is the keyed run's boot hook (engineBootHooks):
// every family gets lift key jackload_<code> and wait key wait_<code>, and
// every node, those made later included, gets a scene location listing the
// three default keys and every family's two.
func keyEveryFamilyAndLocation(t *testing.T, db *store.DB) {
	t.Helper()
	if !strings.HasSuffix(t.Name(), "/keyed") {
		return
	}
	res, err := db.Exec(`UPDATE carrier_families SET lift_bintask = 'jackload_' || lower(code), wait_bintask = 'wait_' || lower(code)`)
	testutil.MustNoErr(t, err, "key every family")
	keyedFamilies, err := res.RowsAffected() // none for a shape with no cart (the cleaning door): its prediction is "no change"
	testutil.MustNoErr(t, err, "count the keyed families")
	tasks := []map[string]map[string]any{
		{"JackLoad": {"operation": "JackLoad"}}, {"JackUnload": {"operation": "JackUnload"}}, {"Wait": {"operation": "Wait"}},
	}
	rows, err := db.Query(`SELECT lift_bintask, wait_bintask FROM carrier_families ORDER BY id`)
	testutil.MustNoErr(t, err, "read the keys")
	for rows.Next() {
		var lift, wait string
		testutil.MustNoErr(t, rows.Scan(&lift, &wait), "scan the keys")
		tasks = append(tasks, map[string]map[string]any{lift: {"operation": "JackLoad"}}, map[string]map[string]any{wait: {"operation": "Wait"}})
	}
	testutil.MustNoErr(t, rows.Close(), "close")
	value, err := json.Marshal(tasks)
	testutil.MustNoErr(t, err, "marshal binTask")
	props, err := json.Marshal([]map[string]string{{"key": "binTask", "type": "json", "stringValue": string(value)}})
	testutil.MustNoErr(t, err, "marshal properties")
	lit := strings.ReplaceAll(string(props), "'", "''")
	for _, stmt := range []string{
		`INSERT INTO scene_points (area_name, instance_name, class_name, point_name, properties_json)
		 SELECT 'Area-01', name, 'GeneralLocation', 'AP-' || name, '` + lit + `'::jsonb FROM nodes
		 ON CONFLICT (area_name, instance_name) DO UPDATE SET properties_json = EXCLUDED.properties_json`,
		`CREATE FUNCTION keyed_pin_location() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		   INSERT INTO scene_points (area_name, instance_name, class_name, point_name, properties_json)
		   VALUES ('Area-01', NEW.name, 'GeneralLocation', 'AP-' || NEW.name, '` + lit + `'::jsonb)
		   ON CONFLICT (area_name, instance_name) DO NOTHING;
		   RETURN NEW;
		 END $$`,
		`CREATE TRIGGER keyed_pin_location AFTER INSERT ON nodes FOR EACH ROW EXECUTE FUNCTION keyed_pin_location()`,
	} {
		_, err := db.Exec(stmt)
		testutil.MustNoErr(t, err, "seed the keyed scene")
	}
	var locations int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM scene_points WHERE class_name = 'GeneralLocation'`).Scan(&locations), "count")
	t.Logf("keyed run: %d famil(ies) keyed, %d location(s) listing %d keys", keyedFamilies, locations, len(tasks))
}

// pinLine is one transcript line read for the comparison.
type pinLine struct {
	label string // the order's label, which may hold spaces
	verb  string // create, append, finish, replay; "" for any other line
	block string
	loc   string
	task  string
	at    int // the task's token index
}

func readPinLine(s string) pinLine {
	f := strings.Fields(s)
	for j, tok := range f {
		switch tok {
		case "create", "append":
			if len(f) == j+4 {
				return pinLine{label: strings.Join(f[:j], " "), verb: tok, block: f[j+1], loc: f[j+2], task: f[j+3], at: j + 3}
			}
		case "finish", "replay":
			if len(f) > j+4 && f[j+4] == "->" {
				return pinLine{label: strings.Join(f[:j], " "), verb: tok, block: f[j+1], loc: f[j+2], task: f[j+3], at: j + 3}
			}
		}
	}
	return pinLine{}
}

// keyedDiff compares a case's plain and keyed transcripts and returns what the
// keyed run changed, one "label block: from -> to" per changed wire line, and
// every way the change breaks the rule.
func keyedDiff(plain, keyed string) (changes, faults []string) {
	pl, kl := strings.Split(plain, "\n"), strings.Split(keyed, "\n")
	if len(pl) != len(kl) {
		return nil, []string{fmt.Sprintf("the keyed transcript has %d lines, the plain one %d", len(kl), len(pl))}
	}
	type wire struct{ p, k pinLine }
	seq := map[string][]wire{}
	sent := map[string]string{} // label+block -> the keyed task sent
	var labels []string
	for i := range pl {
		p, k := readPinLine(pl[i]), readPinLine(kl[i])
		if pl[i] == kl[i] && p.verb == "" {
			continue
		}
		pf, kf := strings.Fields(pl[i]), strings.Fields(kl[i])
		if p.verb == "" || len(pf) != len(kf) {
			faults = append(faults, fmt.Sprintf("line %d changed beyond its binTask:\n    plain %s\n    keyed %s", i+1, pl[i], kl[i]))
			continue
		}
		for j := range pf {
			if pf[j] != kf[j] && j != p.at {
				faults = append(faults, fmt.Sprintf("line %d changed beyond its binTask:\n    plain %s\n    keyed %s", i+1, pl[i], kl[i]))
				break
			}
		}
		switch p.verb {
		case "create", "append":
			if _, ok := seq[p.label]; !ok {
				labels = append(labels, p.label)
			}
			seq[p.label] = append(seq[p.label], wire{p, k})
			sent[p.label+" "+p.block] = k.task
		default:
			if want, ok := sent[p.label+" "+p.block]; ok && k.task != want {
				faults = append(faults, fmt.Sprintf("line %d: the finish echoes %s, the block was sent %s", i+1, k.task, want))
			}
		}
	}
	for _, label := range labels {
		ws := seq[label]
		for n, w := range ws {
			from, to := w.p.task, w.k.task
			if from != to {
				changes = append(changes, fmt.Sprintf("%s %s: %s -> %s", label, w.p.block, from, to))
			}
			switch from {
			case "JackLoad":
				if !strings.HasPrefix(to, "jackload_") {
					faults = append(faults, fmt.Sprintf("STOP: %s %s at %s lifts and went out %s: its cart was not on record when the block was built",
						label, w.p.block, w.p.loc, to))
				}
			case "Wait":
				var next *wire
				if n+1 < len(ws) {
					next = &ws[n+1]
				}
				inFront := next != nil && next.p.task == "JackLoad" && next.p.loc == w.p.loc
				switch {
				case inFront && strings.HasPrefix(next.k.task, "jackload_"):
					if want := "wait_" + strings.TrimPrefix(next.k.task, "jackload_"); to != want {
						faults = append(faults, fmt.Sprintf("%s %s: the wait in front of the lift %s went out %s, want %s",
							label, w.p.block, next.p.block, to, want))
					}
				case inFront && to != "Wait":
					faults = append(faults, fmt.Sprintf("%s %s: the wait in front of the lift %s went out %s while the lift went out %s",
						label, w.p.block, next.p.block, to, next.k.task))
				case !inFront && to != "Wait":
					faults = append(faults, fmt.Sprintf("%s %s at %s: a wait not in front of a lift there went out %s, want Wait",
						label, w.p.block, w.p.loc, to))
				}
			default:
				if from != to {
					faults = append(faults, fmt.Sprintf("%s %s: %s changed to %s; only a lift and a wait in front of one change",
						label, w.p.block, from, to))
				}
			}
		}
	}
	return changes, faults
}

func TestCarrierKeysPin_EveryPinShapeKeyed(t *testing.T) {
	t.Parallel()
	engineBootHooks.Store(keyedPinPrefix, keyEveryFamilyAndLocation)
	// want is the prediction, written before the builder existed
	// (work/carrier-bintask-keys-2026-11-17/build-b/PREDICTIONS-unit0.md): the
	// wire lines the keyed run changes, as "<order> <block> lift|wait".
	cases := []struct {
		name string
		run  func(t *testing.T) *pinRig
		want []string
	}{
		{"plain retrieve", pinPlainRetrieve, []string{"retrieve b1 lift"}},
		{"plain retrieve, a replayed block", pinPlainRetrieveReplay, []string{"retrieve b1 lift"}},
		{"plain retrieve, a block finishing after the order ended", pinPlainRetrieveAfterEnd, []string{"retrieve b1 lift"}},
		{"empty-carrier retrieve", pinEmptyRetrieve, []string{"empty b1 lift"}},
		{"staged release [wait X, pickup X, drop]", pinStagedRelease, []string{"removal b1 wait", "removal b2 lift"}},
		{"single-robot swap", pinSingleRobotSwap, []string{"swap b1 lift", "swap b3 wait", "swap b4 lift", "swap b6 lift", "swap b8 lift"}},
		{"carryover round trip, one cart lifted twice", pinCarryover, []string{"carry b1 lift", "carry b3 wait", "carry b4 lift"}},
		{"advanced load sequence, sealed", pinLoadSequenceSealed, nil},
		{"advanced load sequence, gated", pinLoadSequenceGated, nil},
		{"manual move", pinManualMove, []string{"move b1 lift"}},
		{"gated store", pinGatedStore, []string{"store b1 lift"}},
		{"gated retrieve", pinGatedRetrieve, []string{"retrieve b2 lift"}},
		{"two-robot swap, both legs", pinTwoRobotSwap, []string{"supply b1 lift", "supply b3 wait", "supply b4 lift", "evac b1 wait", "evac b2 lift"}},
		{"keep-staged swap", pinKeepStagedSwap, []string{"supply b1 wait", "supply b2 lift", "evac b1 wait", "evac b2 lift"}},
		{"press-index pair", pinPressIndexPair, []string{"r1 b1 wait", "r1 b2 lift", "r1 b4 lift", "r2 b1 wait", "r2 b2 lift"}},
		{"sequential", pinSequential, []string{"removal b1 wait", "removal b2 lift", "backfill b1 lift"}},
		{"changeover per-position, two carrier families", pinChangeoverTwoFamilies, []string{"position b1 wait", "position b2 lift", "position b4 lift"}},
		{"table turn, plain order, lift side", pinTurnPlainLift, []string{"move b1 wait", "move b2 lift"}},
		{"table turn, plain order, set-down side", pinTurnPlainSetDown, []string{"move b1 lift"}},
		{"table turn, complex order, lift side", pinTurnComplexLift, []string{"swap b1 wait", "swap b2 lift", "swap b4 lift"}},
		{"table turn, complex order, set-down side", pinTurnComplexSetDown, []string{"swap b1 lift", "swap b3 lift"}},
		{"dig: the reshuffle legs and the dwell", pinDig, []string{"leg1 b2 lift", "leg2 b2 lift"}},
		{"home-clear trade", pinHomeClearTrade, []string{"P S->H b1 lift", "E H->S b1 lift"}},
		{"carried-bin recovery", pinCarriedBinRecovery, nil},
		{"cancel-return", pinCancelReturn, nil},
		{"redirect", pinRedirect, []string{"retrieve b1 lift"}},
		{"orderless door: the cleaning mission", pinCleaningDoor, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var plain, keyed string
			t.Run("plain", func(t *testing.T) { plain = c.run(t).transcript() })
			t.Run("keyed", func(t *testing.T) { keyed = c.run(t).transcript() })
			if plain == "" || keyed == "" {
				t.Fatal("a run left no transcript")
			}
			changes, faults := keyedDiff(plain, keyed)
			t.Logf("keyed changes: %d\n  %s", len(changes), strings.Join(changes, "\n  "))
			for _, f := range faults {
				t.Error(f)
			}
			var got []string
			for _, ch := range changes {
				head, from, _ := strings.Cut(ch, ": ")
				kind := "lift"
				if strings.HasPrefix(from, "Wait ") {
					kind = "wait"
				}
				got = append(got, head+" "+kind)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("the keyed run changed %v, the prediction was %v", got, c.want)
			}
			if c.name == "changeover per-position, two carrier families" {
				var lifts []string
				for _, ch := range changes {
					if _, to, ok := strings.Cut(ch, "-> "); ok && strings.HasPrefix(to, "jackload_") && !slices.Contains(lifts, to) {
						lifts = append(lifts, to)
					}
				}
				if len(lifts) != 2 {
					t.Errorf("the changeover lifts carts of two families, sent %v; want two different lift keys", lifts)
				}
			}
		})
	}
}

// The comparison itself, on transcripts written by hand: it must fail each way
// the rule can be broken, or a green keyed run proves nothing.
func TestCarrierKeysPin_TheComparisonCatchesEachBreak(t *testing.T) {
	t.Parallel()
	plain := strings.Join([]string{
		"swap create b1 SRC JackLoad",
		"swap create b2 STG JackUnload",
		"swap create b3 LINE Wait",
		"swap append b4 LINE JackLoad",
		"swap append b5 GATE Wait",
		"swap append b6 OUT JackUnload",
		"swap finish b1 SRC JackLoad -> lift A | steps_done 1 | A@_TRANSIT[swap]",
	}, "\n")
	good := strings.Join([]string{
		"swap create b1 SRC jackload_a",
		"swap create b2 STG JackUnload",
		"swap create b3 LINE wait_b",
		"swap append b4 LINE jackload_b",
		"swap append b5 GATE Wait",
		"swap append b6 OUT JackUnload",
		"swap finish b1 SRC jackload_a -> lift A | steps_done 1 | A@_TRANSIT[swap]",
	}, "\n")
	if changes, faults := keyedDiff(plain, good); len(faults) != 0 || len(changes) != 3 {
		t.Fatalf("the rule kept: changes %v faults %v, want 3 changes and no fault", changes, faults)
	}
	for name, keyed := range map[string]string{
		"a lift left default (the STOP)": strings.Replace(good, "b4 LINE jackload_b", "b4 LINE JackLoad", 1),
		"a wait sized in front of a default lift": strings.Replace(strings.Replace(good, "b4 LINE jackload_b", "b4 LINE JackLoad", 1),
			"b1 SRC jackload_a", "b1 SRC jackload_a", 1),
		"a carrying wait sized":             strings.Replace(good, "b5 GATE Wait", "b5 GATE wait_b", 1),
		"the wait sized by another family":  strings.Replace(good, "b3 LINE wait_b", "b3 LINE wait_a", 1),
		"a set-down sized":                  strings.Replace(good, "b6 OUT JackUnload", "b6 OUT jackunload_b", 1),
		"a finish that does something else": strings.Replace(good, "-> lift A", "-> none", 1),
		"a finish echoing another name":     strings.Replace(good, "finish b1 SRC jackload_a", "finish b1 SRC JackLoad", 1),
		"a location that moved":             strings.Replace(good, "b1 SRC jackload_a", "b1 SRC2 jackload_a", 1),
		"a line lost":                       strings.Join(strings.Split(good, "\n")[1:], "\n"),
	} {
		if _, faults := keyedDiff(plain, keyed); len(faults) == 0 {
			t.Errorf("%s: the comparison found nothing", name)
		}
	}
}
