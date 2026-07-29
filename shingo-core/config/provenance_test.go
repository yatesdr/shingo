package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// provenance_test.go — the enforcement half of provenance.go.
//
// The binding rule is "every Phase 6 display constant is config, never a
// literal, and carries a recorded provenance note". A rule with no enforcement
// is a rule that decays, so each test below holds one clause of it:
//
//   TestDisplayProvenanceCoversEveryField  — no field without a record, and no
//                                            record without a field.
//   TestDisplayProvenanceVocabulary        — the record says something usable.
//   TestSimDerivedConstantsIsTheWatchList  — the watch list is derived, not kept.
//   TestRampStepsMatchesTokenSet           — the one STRUCTURAL claim is checked
//                                            against the structure it claims.
//   TestDisplayDefaultsAreTheShippedValues — the two SME numbers are pinned, so
//                                            a "tidy-up" cannot drift them away
//                                            from the provenance that names them.

// TestDisplayProvenanceCoversEveryField is the load-bearing one.
//
// It holds BOTH directions, and the second direction is the one that is easy to
// leave out: a registry entry with no matching field is a provenance note for a
// constant that no longer exists, which is precisely the "document asserting a
// state nobody verified" shape this repo keeps paying for. A watch list built
// from stale entries sends a plant to validate a number that is not on the
// surface any more.
func TestDisplayProvenanceCoversEveryField(t *testing.T) {
	fields := DisplayFieldPaths()
	if len(fields) == 0 {
		t.Fatal("DisplayFieldPaths returned nothing — the reflection walk is broken, " +
			"and every other assertion in this file would pass vacuously")
	}

	recorded := map[string]int{}
	for _, c := range DisplayProvenance() {
		recorded[c.Path]++
	}

	for _, f := range fields {
		switch recorded[f] {
		case 0:
			t.Errorf("display constant %q has no provenance record.\n"+
				"Add one to displayProvenance in provenance.go saying how the value was "+
				"chosen. If it was chosen without plant data — measured against the sim "+
				"or reasoned from the arithmetic — it is ProvenanceSimDerived and it goes "+
				"on the deploy watch list.", f)
		case 1:
			// exactly right
		default:
			t.Errorf("display constant %q has %d provenance records; it must have exactly one",
				f, recorded[f])
		}
	}

	known := map[string]bool{}
	for _, f := range fields {
		known[f] = true
	}
	for _, c := range DisplayProvenance() {
		if !known[c.Path] {
			t.Errorf("provenance record %q describes no field on DisplayConfig.\n"+
				"Either the field was renamed or removed and this record was left behind. "+
				"A stale record is worse than a missing one: it puts a number on the deploy "+
				"watch list that nothing renders.", c.Path)
		}
	}
}

func TestDisplayProvenanceVocabulary(t *testing.T) {
	for _, c := range DisplayProvenance() {
		if !c.Kind.Valid() {
			t.Errorf("%s: provenance kind %q is not one of the three known values", c.Path, c.Kind)
		}
		if c.Note == "" {
			t.Errorf("%s: empty note. The kind says what has to happen to the number; "+
				"the note is the only place that says why the number is what it is", c.Path)
		}

		switch c.Kind {
		case ProvenanceSME:
			// SME judgement is a claim about a PERSON's knowledge, so it must name
			// the person and the date. Anonymous SME judgement is indistinguishable
			// from a guess, and it is the one kind that exempts a constant from
			// re-derivation at a plant — the exemption has to be attributable.
			if c.Source == "" {
				t.Errorf("%s: SME-JUDGEMENT with no Source. Name who decided and when — "+
					"this is the kind that ships without plant re-derivation, and an "+
					"unattributed exemption is a guess wearing better clothes", c.Path)
			}
		case ProvenanceSimDerived, ProvenanceStructural:
			// Neither has a person behind it. A Source here would be a claim of
			// human judgement over a number that had none.
			if c.Source != "" {
				t.Errorf("%s: kind %s carries Source %q, but only SME-JUDGEMENT has a person behind it",
					c.Path, c.Kind, c.Source)
			}
		}
	}
}

// TestSimDerivedConstantsIsTheWatchList checks the enumeration Track A consumes.
//
// Two properties: it is exactly the SIM-DERIVED subset (not a hand-kept copy
// that can drift), and every entry is actionable — a watch-list line saying
// "re-derive this" without saying from what is a line a plant cannot act on.
func TestSimDerivedConstantsIsTheWatchList(t *testing.T) {
	want := map[string]bool{}
	for _, c := range DisplayProvenance() {
		if c.Kind == ProvenanceSimDerived {
			want[c.Path] = true
		}
	}

	got := SimDerivedConstants()
	if len(got) != len(want) {
		t.Errorf("SimDerivedConstants returned %d entries, but %d constants are SIM-DERIVED",
			len(got), len(want))
	}
	for _, c := range got {
		if !want[c.Path] {
			t.Errorf("SimDerivedConstants returned %q, which is %s, not SIM-DERIVED", c.Path, c.Kind)
		}
		if !c.Kind.NeedsPlantRederivation() {
			t.Errorf("%q is on the watch list but does not need re-derivation", c.Path)
		}
		// The note must say what to measure. Checked as a floor on length rather
		// than by keyword, because a keyword check is trivially satisfied by
		// writing the keyword.
		if len(c.Note) < 120 {
			t.Errorf("%q: SIM-DERIVED note is %d characters. It has to say what a plant "+
				"should measure INSTEAD, not just that the number is provisional",
				c.Path, len(c.Note))
		}
	}
}

// TestSMEExemptionsArePinned closes the hole the other tests leave open.
//
// FOUND BY VERIFY-RED, and worth recording because the mechanism looked complete
// without it. Relabelling a SIM-DERIVED constant as SME-JUDGEMENT — one word,
// plus any string in Source — removes it from SimDerivedConstants and therefore
// from the deploy watch list, and every other test in this file stayed GREEN
// through that edit. Both sides of TestSimDerivedConstantsIsTheWatchList are
// computed from the same registry, so they agree with each other about a
// registry that has just started lying.
//
// That is precisely the failure this whole mechanism exists to prevent: a
// sim-derived constant shipped as though it were a real one. It cannot be
// allowed to be a one-word edit.
//
// No test can verify that a human really made a judgement. What it can do is
// make claiming the exemption a DELIBERATE, REVIEWABLE act: the exempt set is
// pinned here, so a new SME-JUDGEMENT constant means editing the enforcement,
// in a diff, with this comment in view. Same shape as the contrast ratchets in
// shared/ — pinned at the known-good value, failing when they go stale, so
// improvement and regression both have to be recorded rather than absorbed.
func TestSMEExemptionsArePinned(t *testing.T) {
	// The ONLY constants exempt from plant re-derivation, and the only reason
	// they are: a named person's domain knowledge, recorded at the time.
	//
	// Adding a path here is a claim that someone who has stood on a plant floor
	// decided this number. Do not add one to silence a watch-list entry.
	exempt := map[string]bool{
		"display.worry_after":   true, // Stephen Brown, 2026-07-26
		"display.concern_after": true, // Stephen Brown, 2026-07-26
	}

	for _, c := range DisplayProvenance() {
		claimed := c.Kind == ProvenanceSME
		if claimed && !exempt[c.Path] {
			t.Errorf("%s claims SME-JUDGEMENT but is not in this test's pinned exempt set.\n"+
				"SME-JUDGEMENT is the one kind that ships without plant re-derivation, so it "+
				"is the one kind that can quietly empty the deploy watch list. If a named "+
				"person really decided this number, add the path above WITH their name and "+
				"the date. If nobody did, it is SIM-DERIVED.", c.Path)
		}
		if !claimed && exempt[c.Path] {
			t.Errorf("%s is pinned as SME-exempt but its record now says %s.\n"+
				"If the number lost its human provenance it belongs on the watch list — "+
				"remove it from the exempt set here too.", c.Path, c.Kind)
		}
	}
}

// TestRampStepsMatchesTokenSet turns the one STRUCTURAL claim into a check.
//
// ProvenanceStructural says "the value follows from the structure of something
// else and is checkable against it". This is that check: if the token set gains
// or loses a step and RampSteps does not follow, the ramp renders through
// tokens that do not exist (a colourless mark) or stops short of ones that do.
// Without this the "structural" label would be an assertion, which is the exact
// thing the provenance mechanism exists to stop.
func TestRampStepsMatchesTokenSet(t *testing.T) {
	// shared/ is a sibling module; the file is read as data, not imported.
	path := filepath.Join("..", "..", "shared", "tokens.css")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (this test is the only thing holding ramp_steps to reality; "+
			"if the file moved, repoint it rather than deleting the test)", path, err)
	}

	// Count DISTINCT indices. The token set is declared twice — once per theme —
	// so counting declarations would give ten for a five-step ramp.
	re := regexp.MustCompile(`--viz-seq-(\d+)\s*:`)
	seen := map[int]bool{}
	for _, m := range re.FindAllSubmatch(src, -1) {
		n, err := strconv.Atoi(string(m[1]))
		if err != nil {
			continue
		}
		seen[n] = true
	}
	if len(seen) == 0 {
		t.Fatal("found no --viz-seq-N declarations in tokens.css — the regex is stale, " +
			"and a zero count would otherwise fail this test for the wrong reason")
	}

	// Contiguous from 1, or "how many steps" is not a well-formed question.
	for i := 1; i <= len(seen); i++ {
		if !seen[i] {
			t.Fatalf("--viz-seq-%d is missing but %d steps were found; the ramp has a hole in it",
				i, len(seen))
		}
	}

	if got := DisplayDefaults().RampSteps; got != len(seen) {
		t.Errorf("display.ramp_steps is %d but shared/tokens.css defines %d --viz-seq steps.\n"+
			"These are the same fact written twice and they have diverged. Update "+
			"DisplayDefaults, and re-read the ramp's provenance note before assuming "+
			"which side is wrong.", got, len(seen))
	}
}

// TestDisplayDefaultsAreTheShippedValues pins the two numbers that have real
// provenance.
//
// Not a tautology, and worth saying why: 45 and 60 are SME judgement from a
// named person on a named date, and that record is only worth anything while
// the values it describes are still the values that ship. A later session
// retuning them against sim data would leave the provenance note asserting a
// human decision about numbers a machine picked — which is the failure this
// whole mechanism exists to prevent, arriving through the back door.
func TestDisplayDefaultsAreTheShippedValues(t *testing.T) {
	d := DisplayDefaults()
	if d.WorryAfter != 45*time.Minute {
		t.Errorf("display.worry_after = %s, want 45m (SME judgement, Stephen Brown, "+
			"2026-07-26). If this changed deliberately, the provenance record in "+
			"provenance.go must change with it — including whether it is still SME "+
			"judgement at all", d.WorryAfter)
	}
	if d.ConcernAfter != 60*time.Minute {
		t.Errorf("display.concern_after = %s, want 60m (SME judgement, Stephen Brown, 2026-07-26)",
			d.ConcernAfter)
	}
}

// TestDisplayValidateRejectsInvertedBands covers the one internal relationship
// a retune can break.
func TestDisplayValidateRejectsInvertedBands(t *testing.T) {
	if err := DisplayDefaults().Validate(); err != nil {
		t.Fatalf("shipped defaults do not validate: %v", err)
	}

	for _, tc := range []struct {
		name         string
		worry, conc  time.Duration
		wantRejected bool
	}{
		{"inverted", 60 * time.Minute, 45 * time.Minute, true},
		{"equal", 45 * time.Minute, 45 * time.Minute, true},
		{"ordered", 45 * time.Minute, 60 * time.Minute, false},
		{"ordered, retuned tighter", 10 * time.Minute, 11 * time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := DisplayDefaults()
			d.WorryAfter, d.ConcernAfter = tc.worry, tc.conc
			err := d.Validate()
			if tc.wantRejected && err == nil {
				t.Errorf("worry=%s concern=%s was accepted; the bands are unreachable in that order",
					tc.worry, tc.conc)
			}
			if !tc.wantRejected && err != nil {
				t.Errorf("worry=%s concern=%s was rejected: %v", tc.worry, tc.conc, err)
			}
		})
	}
}

// TestDisplayConstantsFallsBackPerField covers the unset-vs-zero rule.
//
// Zero is not a meaningful value for any of these four, so it means unset. The
// per-field part matters because YAML unmarshals onto the defaults struct, so a
// display block naming one key and omitting three is the normal case.
func TestDisplayConstantsFallsBackPerField(t *testing.T) {
	c := Defaults()
	c.Display = DisplayConfig{WorryAfter: 12 * time.Minute}

	got := c.DisplayConstants()
	if got.WorryAfter != 12*time.Minute {
		t.Errorf("explicit worry_after was overwritten: got %s, want 12m", got.WorryAfter)
	}
	def := DisplayDefaults()
	if got.ConcernAfter != def.ConcernAfter || got.RampSteps != def.RampSteps ||
		got.MinExpectedOrders != def.MinExpectedOrders {
		t.Errorf("omitted fields did not fall back to the shipped defaults: %+v", got)
	}

	// Negative is unset, not clamped — a typo must be visible as "nothing
	// changed", never honoured as "every row is now past the worry line".
	c.Display = DisplayConfig{WorryAfter: -1, MinExpectedOrders: -7}
	got = c.DisplayConstants()
	if got.WorryAfter != def.WorryAfter || got.MinExpectedOrders != def.MinExpectedOrders {
		t.Errorf("negative values were honoured rather than treated as unset: %+v", got)
	}
}
