package domain

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"shingoedge/domain/flowspec"

	"shingo/protocol"
)

// TestSwapModeWordsMatchTheModel holds SwapModeWord to composer-model.js's
// MODES block, which is the authority: both surfaces render their chips and
// cards from it, and the server only needs the words to build a preset's
// suggested name. Two spellings of "2-robot index" would put one of them on
// the Presets tab and the other on every other screen.
func TestSwapModeWordsMatchTheModel(t *testing.T) {
	path := filepath.Join("..", "www", "static", "operator-station", "composer-model.js")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	block := regexp.MustCompile(`(?s)const MODES = \{(.*?)\};`).FindSubmatch(body)
	if block == nil {
		t.Fatal("composer-model.js has no MODES block — the recogniser has drifted and this test is a no-op")
	}
	pairs := regexp.MustCompile(`(\w+):\s*'([^']*)'`).FindAllStringSubmatch(string(block[1]), -1)
	if len(pairs) == 0 {
		t.Fatal("parsed no modes out of the MODES block")
	}
	seen := 0
	for _, p := range pairs {
		mode, want := p[1], p[2]
		if got := SwapModeWord(protocol.SwapMode(mode)); got != want {
			t.Errorf("SwapModeWord(%q) = %q, the model says %q", mode, got, want)
		}
		seen++
	}
	if seen != 4 {
		t.Errorf("the model declares %d modes; SwapModeWord was written against 4", seen)
	}
	// A mode nothing names gets no word, so a suggested name says the
	// positions alone rather than inventing a label.
	if w := SwapModeWord(protocol.SwapModeManualSwap); w != "" {
		t.Errorf("SwapModeWord(manual_swap) = %q; a loader board is not a choreography a preset names", w)
	}
}

// TestShapeFieldWordsMatchTheModel holds shapeFields to composer-model.js's
// SHAPE_FIELDS — the same eleven fields, the same words, in the same order.
//
// BOTH SIDES WORD A DIFF FOR THE SAME ENGINEER. The server words a drifted
// member's field list (`PLN_04 · outbound destination`) on the Presets tab's rows; the
// desktop words an apply's previewed diff in the modal one click away. Two
// spellings of one field would read as two different fields on two screens
// that are describing the same change, and an order that disagreed would put
// the wrong word beside the right value.
//
// The words themselves are THE CLAIM'S OWN FIELD NAMES since owner ruling F3
// (2026-09-12) — see TestShapeFieldWordsAreTheClaimsOwnNames below, which is
// the half of this that says WHICH words rather than only that the two lists
// agree on them.
// TestShapeFieldsAreRealClaimFields is owner ruling F3 (2026-09-12):
// A FIELD IS CALLED WHAT THE CLAIM CALLS IT; INVENT NO SECOND NAME.
//
// The shape words were D1's column headings and the headings were invented —
// `old bins to` for `outbound_destination`, `new bins from` for
// `inbound_source`, `stage new at` for `inbound_staging`. Shingo has had names
// for these since the claim did, and two names for one field is a second thing
// to learn and a thing to get wrong: an engineer reading a refusal that says
// `outbound_destination` and a table that says `old bins to` has to work out
// they are the same field.
//
// THE FIRST FIX WROTE BETTER WORDS IN A SECOND TABLE, which is what this test
// used to hold: a list of good words, checked against a list of field names.
// That left three tables — this package's, flowspec's, and the model's — so
// "paired position" here was "Paired Core Node" in the refusal about it.
//
// shapeFields names FIELDS now and asks flowspec.Label for the word, so there
// is no word here to drift. What is left to check is that every field it names
// is a field flowspec actually knows: a typo would silently render as the raw
// string and read like a word somebody chose.
func TestShapeFieldsAreRealClaimFields(t *testing.T) {
	for _, f := range shapeFields {
		if flowspec.Label(f.field) == string(f.field) {
			t.Errorf("shapeFields names %q and flowspec has no label for it, so the diff would show "+
				"the raw column name — add it to fieldLabels or fix the name", f.field)
		}
	}
	// AND THE INVENTED WORDS ARE GONE, by name, so a revert is red rather than
	// quietly re-passing. They are checked against the LABELS now, because the
	// words no longer live in this package at all.
	for _, f := range shapeFields {
		for _, gone := range []string{"old bins to", "new bins from", "stage new at",
			"park old at", "robot drives via", "third position"} {
			if flowspec.Label(f.field) == gone {
				t.Errorf("%q is worded %q again — F3 replaced it with the claim's own name", f.field, gone)
			}
		}
	}
	// The part is in the list of neither, and that is the ruling rather than
	// an omission: a preset is a shape, never a part.
	for _, f := range shapeFields {
		if f.field == flowspec.PayloadCode {
			t.Error("shapeFields names payload_code — a preset that carried the part would report " +
				"every member as drifted the moment a second part used it")
		}
	}
}

// TestShapeFieldsMatchTheModel holds the two LISTS together: the same eleven
// fields, in the same order, on both surfaces. The WORDS are no longer either
// list's business — both look them up — so this compares field names.
func TestShapeFieldsMatchTheModel(t *testing.T) {
	path := filepath.Join("..", "www", "static", "operator-station", "composer-model.js")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	block := regexp.MustCompile(`(?s)const SHAPE_FIELDS = \[(.*?)
\];`).FindSubmatch(body)
	if block == nil {
		t.Fatal("composer-model.js has no SHAPE_FIELDS block — the recogniser has drifted and this test is a no-op")
	}
	fields := regexp.MustCompile(`field:\s*'([^']*)'`).FindAllStringSubmatch(string(block[1]), -1)
	if len(fields) == 0 {
		t.Fatal("parsed no field names out of the SHAPE_FIELDS block")
	}
	if len(fields) != len(shapeFields) {
		t.Fatalf("the model declares %d shape fields, shapeFields has %d — a shape is one thing or it is not a shape",
			len(fields), len(shapeFields))
	}
	for i, f := range fields {
		if got, want := string(shapeFields[i].field), f[1]; got != want {
			t.Errorf("shapeFields[%d] is %q, the model says %q", i, got, want)
		}
	}
}
