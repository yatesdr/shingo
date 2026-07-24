// catid_autofill.go — auto-fill a style's expected_catid from the CATID that
// rides the payload catalog.
//
// Operators enter each payload's part id (CATID) once, on the payload's manifest
// in the Core web UI. Core sends it down with the payload catalog; the edge then
// stamps a style's expected_catid from the CATID of the payload its produce claim
// runs — so the PLC part-identity guard and auto-arm configure themselves with no
// script and no hand-typing. Blank-only (never overwrites an engineer's value)
// and never guesses (only fills from a produce claim whose payload maps to a
// single CATID).

package engine

import "log"

// AutoFillExpectedCATIDs stamps every not-yet-configured style's expected_catid
// from its produce payload's CATID. Runs after a catalog sync (a freshly-synced
// CATID fills existing styles) and after a claim is saved (a newly-chosen payload
// fills its style).
func (e *Engine) AutoFillExpectedCATIDs() {
	styles, err := e.db.ListStyles()
	if err != nil {
		log.Printf("engine: auto-fill expected_catid: list styles: %v", err)
		return
	}
	for _, s := range styles {
		e.autoFillExpectedCATIDForStyle(s.ID, s.Name, s.ExpectedCATID)
	}
}

// autoFillExpectedCATIDForStyle fills one style's expected_catid when it is blank
// and its produce payload maps to a single CATID. currentExpected is passed in so
// callers that already hold the style don't re-read it.
func (e *Engine) autoFillExpectedCATIDForStyle(styleID int64, styleName, currentExpected string) {
	if currentExpected != "" {
		return // never overwrite an engineer-set value
	}
	catid := e.produceCATIDForStyle(styleID)
	if catid == "" {
		return
	}
	if err := e.db.SetStyleExpectedCATID(styleID, catid); err != nil {
		log.Printf("engine: auto-fill expected_catid for style %q: %v", styleName, err)
		return
	}
	log.Printf("engine: auto-filled expected_catid=%s for style %q from its produce payload", catid, styleName)
}

// AutoFillExpectedCATIDForStyle fills a single style's expected_catid (blank-only)
// from its produce payload's CATID — called after a claim is saved so choosing a
// payload configures the part-identity guard immediately.
func (e *Engine) AutoFillExpectedCATIDForStyle(styleID int64) {
	s, err := e.db.GetStyle(styleID)
	if err != nil || s == nil {
		return
	}
	e.autoFillExpectedCATIDForStyle(s.ID, s.Name, s.ExpectedCATID)
}

// produceCATIDForStyle returns the CATID of the payload the style's produce
// claim(s) run when it is unambiguous — a single distinct CATID across the
// produce claims' payloads (via the synced catalog). Empty when there is no
// produce claim, no catalog CATID, or several distinct CATIDs (never guess).
func (e *Engine) produceCATIDForStyle(styleID int64) string {
	claims, err := e.db.ListStyleNodeClaims(styleID)
	if err != nil {
		return ""
	}
	seen := map[string]struct{}{}
	for _, c := range claims {
		if c.Role != "produce" || c.PayloadCode == "" {
			continue
		}
		ce, err := e.db.GetPayloadCatalogByCode(c.PayloadCode)
		if err != nil || ce == nil || ce.CATID == "" {
			continue
		}
		seen[ce.CATID] = struct{}{}
	}
	if len(seen) != 1 {
		return ""
	}
	for k := range seen {
		return k
	}
	return ""
}
