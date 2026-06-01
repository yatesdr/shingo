package engine

import "strings"

// PLC tag derivation for the AMR-trial MES struct convention.
//
// Plant PLCs publish a per-process struct that carries the production
// counters, identity (CATID_NN), and control bits (Changeover_Active,
// RunTime_Active, DownTime_Active) together. The AMR trial confirmed
// the convention via WarLink Tag Republisher: a typical struct path is
//
//   MES_P42_Spot_Nut_Farm_2.Prod_Counter_01
//   MES_P42_Spot_Nut_Farm_2.Changeover_Active
//   MES_P42_Spot_Nut_Farm_2.CATID_01
//   ...
//
// Shingo's process row already configures counter_tag_name (the leaf
// PLC tag the production counter reads). The other tags in the same
// struct can be derived from that one configured value rather than
// asking the operator to type each tag name separately. This avoids
// typo opportunities and scales to "a ton of lines" without per-tag
// admin UI work, as long as the convention holds.
//
// The convention is documented; if a future plant uses a different
// layout, the derivation returns an empty tag and the caller (cutover
// monitor) logs and skips the subscription rather than crashing.

// deriveProcessTagPrefix strips the leaf segment from a fully-qualified
// PLC tag name, returning the parent struct path. Returns the empty
// string when the tag has no dot (no parent struct) or is empty.
//
// Example: "MES_P42_Spot_Nut_Farm_2.Prod_Counter_01" →
//
//	"MES_P42_Spot_Nut_Farm_2"
func deriveProcessTagPrefix(tagName string) string {
	if idx := strings.LastIndex(tagName, "."); idx > 0 {
		return tagName[:idx]
	}
	return ""
}

// deriveCutoverTag returns the Changeover_Active tag path under the
// same parent struct as the supplied counter tag. Returns empty when
// the input has no parent struct.
//
// Example: "MES_P42_Spot_Nut_Farm_2.Prod_Counter_01" →
//
//	"MES_P42_Spot_Nut_Farm_2.Changeover_Active"
func deriveCutoverTag(counterTagName string) string {
	prefix := deriveProcessTagPrefix(counterTagName)
	if prefix == "" {
		return ""
	}
	return prefix + ".Changeover_Active"
}
