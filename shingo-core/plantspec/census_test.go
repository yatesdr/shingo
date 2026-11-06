package plantspec

import (
	"strings"
	"testing"
)

// census_test.go — the census at birth (§R.78).
//
// WHAT IT IS FOR. The lane-stress rig's two worst air bubbles were read for
// weeks as evidence that digs fragment a group under churn. They were not
// drilled by anything: no bin was ever at either slot and no order ever named
// one — they came out of the seeder that way, in a group that started 73% full
// with a working margin of one slot in thirty-seven (§R.77). Every dig-vs-dig
// standoff the batch above exists to prevent grew in that soil, and nothing in
// the system had ever looked at a seed and asked whether it was survivable.

// censusPlant builds a one-zone spec from a lane layout: each string is a lane,
// each rune a slot from the MOUTH inward, 'X' occupied and '_' empty.
func censusPlant(t *testing.T, lanes ...string) *Plant {
	t.Helper()
	p := &Plant{Zones: []Zone{{Name: "Z"}}}
	for li, shape := range lanes {
		lane := Lane{Name: string(rune('A' + li))}
		for d, ch := range shape {
			slot := Slot{Name: lane.Name + string(rune('1'+d)), Depth: d + 1}
			lane.Slots = append(lane.Slots, slot)
			if ch == 'X' {
				p.Bins = append(p.Bins, Bin{Name: "b" + slot.Name, Slot: slot.Name})
			}
		}
		p.Zones[0].Lanes = append(p.Zones[0].Lanes, lane)
	}
	return p
}

func TestCensus_PackedLanesAreClean(t *testing.T) {
	// Filled from the BACK, levels varying lane to lane, mouths open. This is the
	// shape the re-cut mandates, and the fill LEVEL is deliberately uneven —
	// 4-of-4 next to 2-of-4 is fine, it is the arrangement that is fixed.
	c := censusPlant(t, "XXXX", "__XX", "____").CensusAtBirth()
	if !c.Clean() {
		t.Errorf("a packed seed reported %d bubble(s) and %d overfill(s): %v",
			len(c.Bubbles), len(c.Overfills), c.Findings())
	}
}

func TestCensus_AMouthFilledLaneIsABubble(t *testing.T) {
	// The tidy-looking mistake, and the one a human writing YAML makes: fill the
	// lane from the top of the list down. It buries every slot behind the first
	// bin just as thoroughly as a hole in the middle does.
	c := censusPlant(t, "XX__", "____").CensusAtBirth()
	if len(c.Bubbles) != 2 {
		t.Fatalf("got %d bubble(s), want 2 (A3 and A4, both walled by A1): %v", len(c.Bubbles), c.Findings())
	}
	for _, b := range c.Bubbles {
		if b.BlockedBy != "A1" {
			t.Errorf("bubble at %s says it is blocked by %s, want A1 — the SHALLOWEST bin is the one "+
				"a robot meets first and the one that has to move", b.Slot, b.BlockedBy)
		}
	}
}

func TestCensus_AHoleInTheMiddleIsABubble(t *testing.T) {
	c := censusPlant(t, "X_XX", "____").CensusAtBirth()
	if len(c.Bubbles) != 1 || c.Bubbles[0].Slot != "A2" {
		t.Fatalf("got %v, want exactly one bubble at A2", c.Findings())
	}
}

// TestCensus_HeadroomIsOneFullLanesWorth is the owner's number: 55 slots, 50
// bins, not 55. Here it is four lanes of three — twelve slots, deepest lane
// three, so nine bins is the ceiling.
func TestCensus_HeadroomIsOneFullLanesWorth(t *testing.T) {
	atCeiling := censusPlant(t, "XXX", "XXX", "XXX", "___").CensusAtBirth()
	if !atCeiling.Clean() {
		t.Errorf("nine bins in twelve slots was refused, leaving exactly one lane's worth free — "+
			"that is the rule satisfied, not broken: %v", atCeiling.Findings())
	}

	overFull := censusPlant(t, "XXX", "XXX", "XXX", "__X").CensusAtBirth()
	if len(overFull.Overfills) != 1 {
		t.Fatalf("ten bins in twelve slots was accepted. A group with less than one lane's worth "+
			"free cannot conduct an excavation at all — a dig has nowhere to stand its blockers — "+
			"so every dig it admits is a dig that parks: %v", overFull.Findings())
	}
}

// TestCensus_ASingleLaneGroupIsExemptFromHeadroom. A lone lane has nowhere to
// dig INTO at any fill level — findShuffleSlots refuses to park a blocker back
// into the lane being dug — so reserving slack there would report a shortage
// that is not the problem. The bubble rule still applies to it.
func TestCensus_ASingleLaneGroupIsExemptFromHeadroom(t *testing.T) {
	c := censusPlant(t, "XXX").CensusAtBirth()
	if len(c.Overfills) != 0 {
		t.Errorf("a full single-lane group was reported as short of digging room: %v", c.Findings())
	}
	if bubbles := censusPlant(t, "X_X").CensusAtBirth().Bubbles; len(bubbles) != 1 {
		t.Errorf("a hole in a single-lane group went unreported — it is just as unreachable")
	}
}

// TestCensus_HeadroomCanBeWaivedOnPurpose. Zero is a legal value and means "this
// group runs full", which an author has to write down rather than drift into.
// The DEFAULT is one lane's worth: headroom is opted out of, never forgotten.
func TestCensus_HeadroomCanBeWaivedOnPurpose(t *testing.T) {
	p := censusPlant(t, "XXX", "XXX")
	if len(p.CensusAtBirth().Overfills) != 1 {
		t.Fatal("the default did not apply — a spec that says nothing about headroom must still get it")
	}
	none := 0
	p.Headroom = Headroom{FreeLanes: &none}
	if !p.CensusAtBirth().Clean() {
		t.Errorf("free_lanes: 0 was not honoured: %v", p.CensusAtBirth().Findings())
	}
}

// TestCensus_TheShippedSeeds pins the shipped seed and the frozen baseline's two
// defects, which is the regression this file is really for.
//
// The master is clean. The frozen lane-stress baseline is gone from the tree; its
// two seeded bubbles are replayed here on the master's corridor, which is that
// rig's storage slot for slot: SYN_COMP's C3 and C5 filled from the MOUTH, as
// lane-stress.yaml had them. Frozen, the spec REPORTS and still loads (an A/B is
// only meaningful against an unchanged seed, so a spec a published number was
// measured on is not corrected); unfrozen, it is refused. Both defects are named.
func TestCensus_TheShippedSeeds(t *testing.T) {
	master, err := Load("../../plants/master.yaml")
	if err != nil {
		t.Fatalf("load the master: %v", err)
	}
	if master.BaselineFrozenAt != "" {
		t.Error("the master declares itself frozen. It is the seed every baseline is cut " +
			"from and it has nothing to excuse — a waiver here would hide a real regression")
	}
	if c := master.CensusAtBirth(); !c.Clean() {
		t.Errorf("the master is not clean: %v", c.Findings())
	}

	frozen := *master
	frozen.Bins = append([]Bin(nil), master.Bins...)
	mouthFirst := map[string]string{"LSC_010": "LSC_009", "LSC_011": "LSC_010", "LSC_012": "LSC_011", "LSC_018": "LSC_017", "LSC_019": "LSC_018"}
	moved := 0
	for i, b := range frozen.Bins {
		if to, ok := mouthFirst[b.Slot]; ok {
			frozen.Bins[i].Slot = to
			moved++
		}
	}
	if moved != 5 {
		t.Fatalf("moved %d carts, want C3's three and C5's two: the corridor's seed has changed under the replay", moved)
	}
	c := frozen.CensusAtBirth()
	var at []string
	for _, b := range c.Bubbles {
		at = append(at, b.Slot+" behind "+b.BlockedBy)
	}
	if got := strings.Join(at, ", "); len(c.Bubbles) != 2 || !strings.Contains(got, "LSC_012 behind LSC_009") ||
		!strings.Contains(got, "LSC_019 behind LSC_017") {
		t.Errorf("the replayed baseline reports %q, want the 2 §R.77 traced by hand (LSC_012 behind LSC_009, "+
			"LSC_019 behind LSC_017)", got)
	}
	if err := frozen.Validate(); err == nil || !strings.Contains(err.Error(), "AIR BUBBLE AT BIRTH") {
		t.Errorf("unfrozen, the replay validated (%v); a seed with an unreachable slot must be refused", err)
	}
	frozen.BaselineFrozenAt = "§R.78"
	if err := frozen.Validate(); err != nil {
		t.Errorf("frozen, the replay no longer validates: %v — a known defect reports, it does not refuse", err)
	}
}

// TestCensus_AnUnfrozenSpecWithABubbleFailsValidation is the assertion half: a
// new spec that ships pre-fragmented does not load at all.
//
// MUTATION (verified): drop the census call from Validate. This passes, and a
// seed can once again arrive with unreachable slots that get read months later
// as evidence about how the plant behaves.
func TestCensus_AnUnfrozenSpecWithABubbleFailsValidation(t *testing.T) {
	p := censusPlant(t, "X_XX", "____")
	err := p.Validate()
	if err == nil {
		t.Fatal("a spec seeding an unreachable slot validated cleanly")
	}
	if !strings.Contains(err.Error(), "AIR BUBBLE AT BIRTH") {
		t.Errorf("validation failed for some other reason and the bubble may be unreported: %v", err)
	}
}
