package plantspec

import (
	"fmt"
	"sort"
)

// carrier_pairs.go — the A/B two-container rule, as a census at birth.
//
// OWNER'S RULING (verbatim intent — this file is that sentence as a check):
//
//	"Not just an A/B press: if a process wants to run an A/B swap it needs at
//	 least two containers in the system. Two nodes lineside holding the same
//	 part — it can't function with less than 2."
//
// As an invariant: for every (style, payload) a claim's PAIRED positions hold
// lineside, the carriers of that payload's bin type available to the loop must
// number at least the paired lineside positions — two paired A/B positions on
// one part means a minimum of two carriers of its type, because the pair only
// ever works by holding one carrier's worth of slack between the two positions.

// THE MOTIVATING INCIDENT IS NOT A BIRTH DEFECT, AND SAYING SO IS THE HONEST
// LIMIT OF THIS CHECK. The demo STANDARD-SM jam every number in this file's
// neighbourhood traces to (13 carriers, 12 full, zero empty at end of run) is a
// RUNTIME drain — at birth the same pool holds 13 carriers with 9 empty, which
// passes here. The rule as ruled refuses a seed that STARTS without enough
// containers to rotate; it does not and cannot see a pool that drains to that
// state while running. The runtime half is soakstat's carrier check (0401b897)
// and simcalc's coupling drain (4128462d, 13a07441).
//
// THE PAIR IS MEASURED, NOT NAMED. The arm does not switch on the mode string:
// it counts, per (style, payload), the distinct core nodes of claims that name
// a paired_core_node — the measured position geometry. Any mode that pairs two
// lineside positions inherits the check by construction, and a mode name that
// does not pair positions cannot trip it. Two modes measure as paired today:
// sequential (the A/B pair, two claims naming each other) and
// two_robot_press_index (one claim naming its back position — bins index
// C→B→A, so both positions hold the part simultaneously, which is "two nodes
// lineside holding the same part" exactly as ruled).

// CarrierShortage is one (style, payload) whose paired positions outnumber the
// seeded carriers of the payload's bin type.
type CarrierShortage struct {
	Style     string
	Payload   string
	BinType   string
	Positions []string // the paired lineside positions, sorted, for the message
	Carriers  int      // seeded carriers of BinType
}

// CarrierPairs is the paired-position half of the census: one entry per
// (style, payload) held by paired claims. Empty means no paired positions.
type CarrierPairs struct {
	Shortages []CarrierShortage
}

// Clean reports whether every paired (style, payload) has its carriers.
func (c CarrierPairs) Clean() bool { return len(c.Shortages) == 0 }

// Findings renders the shortage lines, quoting the ruling.
func (c CarrierPairs) Findings() []string {
	var out []string
	for _, s := range c.Shortages {
		out = append(out, fmt.Sprintf(
			"CARRIER SHORTAGE AT BIRTH: style %s pairs %d lineside positions on %s (%s) but the seed "+
				"holds only %d carrier(s) of its bin type %s. \"If a process wants to run an A/B swap it "+
				"needs at least two containers in the system. Two nodes lineside holding the same part — "+
				"it can't function with less than 2.\"",
			s.Style, len(s.Positions), s.Payload, joinNames(s.Positions), s.Carriers, s.BinType))
	}
	return out
}

func joinNames(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}

// CarrierPairsAtBirth walks the claims and the seed and reports every paired
// (style, payload) whose positions outnumber the carriers of its bin type.
func (p *Plant) CarrierPairsAtBirth() CarrierPairs {
	binTypeByPayload := make(map[string]string, len(p.Payloads))
	for _, pl := range p.Payloads {
		binTypeByPayload[pl.Code] = pl.BinType
	}
	carriersByType := make(map[string]int, len(p.Bins))
	for _, b := range p.Bins {
		// The seeder defaults a blank bin_type to BinTypes[0] (seed_core.go);
		// count what is born, not what is spelled.
		bt := b.BinType
		if bt == "" && len(p.BinTypes) > 0 {
			bt = p.BinTypes[0]
		}
		if bt != "" {
			carriersByType[bt]++
		}
	}

	// Distinct paired positions per (style, payload). A claim names its pair by
	// paired_core_node; both halves of a pair name each other, so dedupe by node
	// and not by claim or every position would count twice.
	positions := map[string]map[string]bool{} // style|payload → node set
	for _, c := range p.Claims {
		if c.PairedCoreNode == "" {
			continue
		}
		key := c.Style + "|" + c.Payload
		set := positions[key]
		if set == nil {
			set = map[string]bool{}
			positions[key] = set
		}
		set[c.CoreNode] = true
	}

	var cp CarrierPairs
	for key, set := range positions {
		style, payload := splitKey(key)
		binType := binTypeByPayload[payload]
		want := len(set)
		have := carriersByType[binType]
		if have < want {
			names := make([]string, 0, len(set))
			for n := range set {
				names = append(names, n)
			}
			sort.Strings(names)
			cp.Shortages = append(cp.Shortages, CarrierShortage{
				Style: style, Payload: payload, BinType: binType,
				Positions: names, Carriers: have,
			})
		}
	}
	sort.Slice(cp.Shortages, func(i, j int) bool {
		if cp.Shortages[i].Style != cp.Shortages[j].Style {
			return cp.Shortages[i].Style < cp.Shortages[j].Style
		}
		return cp.Shortages[i].Payload < cp.Shortages[j].Payload
	})
	return cp
}

func splitKey(key string) (style, payload string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '|' {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}
