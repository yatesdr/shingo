package www

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFleetMessagesAreRenderedAsMessages asserts that every page rendering the
// fleet's errors / warnings / notices reads the fields off them, rather than
// stringifying the object.
//
// They arrive as OBJECTS — {code, desc, times, timestamp}. orders.js ran
// escapeHtml over each one, and escaping an object yields the literal text
// "[object Object]". So every vendor refusal that page ever showed was that
// string, on the one surface an operator opens to find out why an order died.
//
// IT COST A REAL DIAGNOSIS. On 2026-09-22 a move was STOPPED by the fleet two
// seconds after dispatch. The page said "[object Object]". The answer was in the
// same payload the page already had: code 60009, "Cannot find path to
// [SMN_011]" — a destination the robot map cannot route to. Somebody had to read
// the database to learn what the screen was holding.
//
// TWO PAGES SHOW THESE NOW, which is the reason this is a test and not a fixed
// bug: mission-detail.js had it right the whole time and orders.js never did, so
// the same message rendered two ways on two screens and only one of them was
// worth reading. An operator who learns to distrust a screen does not go back to
// it.
//
// WHAT IT CANNOT SEE: whether the wording matches, only that the fields are
// read. A page that prints the code and drops the description passes here and is
// still worse than the one beside it. The wording rule lives in the comment at
// orders.js's fleetMessages; this is the floor under it.
func TestFleetMessagesAreRenderedAsMessages(t *testing.T) {
	t.Parallel()

	// The pages that consume a fleet message array. A new one that does must be
	// added here — which is the prompt to go and look at how it renders them.
	pages := []string{
		"static/pages/orders.js",
		"static/pages/mission-detail.js",
	}

	for _, rel := range pages {
		body, err := os.ReadFile(filepath.Clean(rel))
		if err != nil {
			t.Errorf("read %s: %v — if the page moved, say where", rel, err)
			continue
		}
		src := string(body)

		// The defect verbatim: escaping or interpolating the whole object.
		if strings.Contains(src, "map(escapeHtml)") {
			t.Errorf("%s stringifies a fleet message object with escapeHtml.\n\n"+
				"These are {code, desc, times, timestamp}; escaping the object renders "+
				"the literal text \"[object Object]\" and the operator learns nothing. "+
				"Read the fields — see fleetMessages in orders.js.", rel)
		}

		// And the positive: the description is the part a person reads, so a page
		// handling these must name it.
		if !strings.Contains(src, ".desc") {
			t.Errorf("%s renders fleet messages but never reads .desc.\n\n"+
				"The description is the whole message — \"Cannot find path to [X]\" — "+
				"and a code alone sends the operator to a vendor manual.", rel)
		}
	}
}
