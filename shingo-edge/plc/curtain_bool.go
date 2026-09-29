package plc

import "strings"

// curtain_bool.go - the one interpreter for "is this WarLink tag value a
// BOOL, and which way". The FG light-curtain interlock reads it at the
// release gate (engine) and at the button's render stamp (www) - one
// spelling, because the two must never disagree about a value the same tag
// produced.
//
// The PLC's BOOL surfaces as JSON bool; tolerant of the 0/1 and "0"/"1"
// spellings the drivers have been known to hand back. Anything else refuses
// - a safety gate that guesses is a gate that has already failed.
func CurtainBool(raw any) (bool, bool) {
	switch v := raw.(type) {
	case bool:
		return v, true
	case int:
		if v == 0 || v == 1 {
			return v == 1, true
		}
	case int64:
		if v == 0 || v == 1 {
			return v == 1, true
		}
	case float64:
		if v == 0 || v == 1 {
			return v == 1, true
		}
	case string:
		s := strings.TrimSpace(v)
		switch s {
		case "0":
			return false, true
		case "1":
			return true, true
		}
	}
	return false, false
}
