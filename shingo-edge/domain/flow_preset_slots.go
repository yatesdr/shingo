package domain

import (
	"shingo/protocol"
	"shingoedge/domain/flowspec"
)

// flow_preset_slots.go — what a recipe asks the floor for, and what a style
// minted from it is called (pick-a-part brief, unit U3).

// Slot is one part a recipe needs: the positions that part runs on and the
// role it plays there. A mutual partner pair — two cells each naming the other
// (sequential A/B) — is ONE slot over two positions: the same part alternates
// between them. A press-index partner has no cell of its own, so its slot is
// the one row.
type Slot struct {
	Nodes []string           `json:"nodes"`
	Role  protocol.ClaimRole `json:"role"`
}

// PayloadSlots is a recipe's slots, in cell order. A cell whose mode does not
// require a part (a loader card) is not a slot. Read off flowspec, never off a
// mode name.
func PayloadSlots(cells []FlowCell) []Slot {
	by := make(map[string]FlowCell, len(cells))
	for _, c := range cells {
		by[c.CoreNodeName] = c
	}
	taken := map[string]bool{}
	var out []Slot
	for _, c := range cells {
		if taken[c.CoreNodeName] {
			continue
		}
		if flowspec.Steady(c.Role, c.SwapMode)[flowspec.PayloadCode] != flowspec.Required {
			continue
		}
		slot := Slot{Nodes: []string{c.CoreNodeName}, Role: c.Role}
		taken[c.CoreNodeName] = true
		if p, ok := by[c.PairedCoreNode]; ok && p.PairedCoreNode == c.CoreNodeName && !taken[p.CoreNodeName] {
			slot.Nodes = append(slot.Nodes, p.CoreNodeName)
			taken[p.CoreNodeName] = true
		}
		out = append(out, slot)
	}
	return out
}

// ProduceSlots counts the slots that make parts — the fact the station's
// "two off?" question reads (brief O-12: two PRODUCE slots, not two slots: a
// weld cell's consume + produce recipe has two slots and one produced part).
func ProduceSlots(slots []Slot) int {
	n := 0
	for _, s := range slots {
		if s.Role == protocol.ClaimRoleProduce {
			n++
		}
	}
	return n
}

// MintedStyleName is the name a style minted from parts takes (owner ruling
// R-D, 2026-10-17): one part → its code; two → the shared prefix collapsed,
// `1234-567/8`; three or more → the produced parts' codes, collapsed the same
// way (the outgoing parts — what the cell makes is what it is called by).
// `parts` are in slot order with their roles; an existing style is never
// renamed — this names new ones only.
func MintedStyleName(codes []string, roles []protocol.ClaimRole) string {
	switch len(codes) {
	case 0:
		return ""
	case 1:
		return codes[0]
	case 2:
		return collapsePair(codes[0], codes[1])
	}
	var made []string
	for i, c := range codes {
		if i < len(roles) && roles[i] == protocol.ClaimRoleProduce {
			made = append(made, c)
		}
	}
	if len(made) == 0 {
		made = codes
	}
	name := made[0]
	for _, c := range made[1:] {
		name = collapsePair(name, c)
	}
	return name
}

// collapsePair writes two codes as one: the second's differing tail after the
// prefix they share, `1234-567` + `1234-568` → `1234-567/8`. Codes that share
// nothing are joined whole, `A/B`, and so is a pair where one is a prefix of
// the other — a tail is never empty.
func collapsePair(a, b string) string {
	if a == b {
		return a
	}
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	if i == len(b) {
		// b is a prefix of a (or equal up to its end): nothing short to say.
		return a + "/" + b
	}
	if i == 0 {
		return a + "/" + b
	}
	return a + "/" + b[i:]
}
