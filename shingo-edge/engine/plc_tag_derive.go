package engine

import "strings"

// PLC tag derivation for the AMR-trial MES struct convention.
//
// Plant PLCs publish a per-process struct that carries the production
// counters, identity (CATID_NN), and control bits (Changeover_Active,
// RunTime_Active, DownTime_Active) together. The AMR trial confirmed
// the convention via WarLink Tag Republisher: a typical struct path is
//
//   MES_Press_A1.Prod_Counter_01
//   MES_Press_A1.Changeover_Active
//   MES_Press_A1.CATID_01
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
// Example: "MES_Press_A1.Prod_Counter_01" →
//
//	"MES_Press_A1"
func deriveProcessTagPrefix(tagName string) string {
	if idx := strings.LastIndex(tagName, "."); idx > 0 {
		return tagName[:idx]
	}
	return ""
}

// deriveIdentityTag returns the CATID_0N part-identity tag path under the same
// parent struct as the supplied counter tag, where _0N is the counter leaf's
// index: an output counted on Prod_Counter_02 is identified by CATID_02. A leaf
// with no _NN index reads the first slot, CATID_01. Returns empty when the
// input has no parent struct. This is the ground-truth part signal the A5
// guard reads.
//
// Example: "MES_Press_A1.Prod_Counter_02" →
//
//	"MES_Press_A1.CATID_02"
func deriveIdentityTag(counterTagName string) string {
	prefix := deriveProcessTagPrefix(counterTagName)
	if prefix == "" {
		return ""
	}
	return prefix + ".CATID_" + counterLeafIndex(counterTagName[len(prefix)+1:])
}

// counterLeafIndex returns the digits after a leaf's last underscore ("02" for
// Prod_Counter_02), or "01" when the leaf carries no such index.
func counterLeafIndex(leaf string) string {
	i := strings.LastIndex(leaf, "_")
	if i < 0 || i == len(leaf)-1 {
		return "01"
	}
	for _, r := range leaf[i+1:] {
		if r < '0' || r > '9' {
			return "01"
		}
	}
	return leaf[i+1:]
}
