// catid_autofill.go — retire redundant expected_catid stamps.
//
// The part-identity guard, auto-arm, and post-cutover verify now DERIVE a style's
// valid CATIDs from its produce claims' payloads (see catid_set.go), so the old
// single-value auto-fill/backfill stamps in styles.expected_catid are redundant.
// This clears the ones that merely duplicate the derived single value — leaving
// any pin that DIFFERS (a real human choice) or is a multi-value left/right pin
// untouched.

package engine

import (
	"log"
	"strings"
)

// ClearRedundantExpectedCATIDs clears every expected_catid pin that exactly
// equals the style's derived single-value set — those were the backfill/auto-fill
// stamps and are now redundant under derived-set membership. A pin that differs
// from the derived value, or is a multi-value list, is human intent and is left
// in place. Runs after a catalog sync (a freshly-derivable CATID retires its
// stamp); logs each clear.
func (e *Engine) ClearRedundantExpectedCATIDs() {
	styles, err := e.db.ListStyles()
	if err != nil {
		log.Printf("engine: clear redundant expected_catid: list styles: %v", err)
		return
	}
	for i := range styles {
		s := styles[i]
		pin := strings.TrimSpace(s.ExpectedCATID)
		if pin == "" {
			continue
		}
		derived := e.derivedCATIDSet(s.ID)
		// Redundant ONLY when the derived set is a single value AND the pin is that
		// exact value. A multi-value pin, or a pin that differs from the derived
		// value, is human intent — leave it.
		if len(derived) == 1 && pin == formatCATIDSet(derived) {
			if err := e.db.SetStyleExpectedCATID(s.ID, ""); err != nil {
				log.Printf("engine: clear redundant expected_catid on style %q: %v", s.Name, err)
				continue
			}
			log.Printf("engine: cleared redundant expected_catid=%s on style %q (now derived from its produce payload)", pin, s.Name)
		}
	}
}
