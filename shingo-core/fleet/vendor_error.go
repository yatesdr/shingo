package fleet

import (
	"strconv"
	"strings"
)

// VendorError is a refusal the fleet ANSWERED with: its own code and text. A
// transport failure — a timeout, a refused connection — is not one, because
// nothing answered. The difference is what a person is told: "the robot system
// is not responding" is true of the second and false of the first, and it was
// the sentence for both until 2026-11-04 (dispatch.FleetRefusalCause).
//
// The vendor adapter's error type implements it (rds.APIError); callers find it
// with errors.As, so wrapping with %w keeps it.
type VendorError interface {
	error
	// VendorCode is the fleet's own code, or 0 when only the text is known.
	VendorCode() int
	// VendorText is the fleet's own description, as received.
	VendorText() string
}

// VendorTextMax bounds what the fleet's text can add to a sentence on a screen.
// The text is the vendor's and so is its length.
const VendorTextMax = 120

// FormatVendorError renders one fleet error the way a person quotes it to the
// vendor: the code first — RDS's logs and alarm tables are searched by it — then
// the text as received. Not lower-cased: RDS text carries node names
// ("[PLN_05]") that must stay as the map spells them.
//
// This is the ONE spelling of a fleet error on a screen: the stop reason
// (engine.fleetStopReason), the refusal wait sentence (dispatch.FleetRefusalCause)
// and the release-time refusals all print what it returns.
func FormatVendorError(code int, text string) string {
	text = strings.TrimSpace(text)
	if r := []rune(text); len(r) > VendorTextMax {
		text = string(r[:VendorTextMax]) + "…"
	}
	switch {
	case code != 0 && text != "":
		return "RDS error " + strconv.Itoa(code) + ": " + text
	case code != 0:
		return "RDS error " + strconv.Itoa(code)
	case text != "":
		return "RDS: " + text
	default:
		return ""
	}
}
