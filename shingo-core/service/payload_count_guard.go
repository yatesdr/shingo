package service

import (
	"errors"
	"fmt"
	"strings"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/store/inventory"
	"shingocore/store/payloads"
)

// payload_count_guard.go — the two payload saves that touch what a standing
// count means (owner, 2026-11-19, rules 6 and the brief's step 6):
//
//   - A RATIO CHANGE rewrites no stored number: a bin of 800 parts reads 100 UoP
//     at 8 a cycle and 200 at 4, and no count moves. Core warns the engineer
//     how many bins and piles of the part hold a count and what they read
//     before and after (RatioChangeUnseen), and saves once the engineer has seen
//     it (UpdateWithSetsSeen's seen).
//   - A CHANGE BETWEEN ONE LINE AND SEVERAL changes the count's unit (800 parts
//     would read as 800 kits), so Core refuses it while any bin or pile of the
//     payload holds a count, and names them (ErrKitFlip). Orchestrator's
//     default; the owner may change it.

// ErrKitFlip refuses a manifest save that changes a payload between one line
// and several while a bin or pile of it holds a count.
var ErrKitFlip = errors.New("a payload whose bins or piles hold a count cannot change between one part and a kit")

// RatioChange is what a ratio change on a payload whose bins or piles hold a
// count changes in what they read. No stored count moves.
type RatioChange struct {
	Code  string           `json:"code"`
	From  int              `json:"from"`
	To    int              `json:"to"`
	Bins  int              `json:"bins"`
	Piles int              `json:"piles"`
	Rows  []RatioChangeRow `json:"rows"`
}

// RatioChangeRow is one bin or pile: its count in parts and its UoP reading
// before and after the change (protocol.SplitUOP).
type RatioChangeRow struct {
	What      string `json:"what"`
	Parts     int    `json:"parts"`
	UOPBefore int    `json:"uop_before"`
	UOPAfter  int    `json:"uop_after"`
	Pile      bool   `json:"pile,omitempty"`
}

// RatioChangeUnseen answers a save that changes the ratio of a payload whose
// bins or piles hold a count, before the engineer has seen what it changes:
// nothing was written. Its sentence is the warning.
type RatioChangeUnseen struct{ Change RatioChange }

func (e *RatioChangeUnseen) Error() string { return e.Change.Sentence() }

// Sentence is the warning an engineer reads before a ratio change saves.
func (c RatioChange) Sentence() string {
	var parts []string
	for _, r := range c.Rows {
		// OWNER-WORDS
		parts = append(parts, fmt.Sprintf("%s holds %d parts: %d UoP now, %d UoP after", r.What, r.Parts, r.UOPBefore, r.UOPAfter))
	}
	if shown := len(c.Rows); shown < c.Bins+c.Piles {
		parts = append(parts, fmt.Sprintf("and %d more", c.Bins+c.Piles-shown))
	}
	// OWNER-WORDS
	return fmt.Sprintf("Changing %s from %d to %d parts a cycle changes what %s and %s read. No count moves: %s.",
		c.Code, c.From, c.To, countPlural(c.Bins, "bin", "bins"), countPlural(c.Piles, "pile", "piles"), strings.Join(parts, "; "))
}

func countPlural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// kitFlip reports whether a save changes a payload between one manifest line
// and several (a part and a kit): its stored counts would then be read in the
// other unit. A payload with no line on either side is not a flip.
func kitFlip(before, after []protocol.ManifestItem) bool {
	return len(before) > 0 && len(after) > 0 && (len(before) == 1) != (len(after) == 1)
}

// countUnitGuard decides a manifest save against the counts that stand: a
// refusal when the payload changes between one line and several (a payload
// with no line is neither, and is let through), a RatioChange when the ratio
// changes (protocol.UOPRatio), nil and nil when neither or when nothing holds
// a count. Pure: TestCountUnitGuard.
func countUnitGuard(code string, before, after []protocol.ManifestItem, holders []inventory.Holder, bins, piles int) (error, *RatioChange) {
	if bins+piles == 0 {
		return nil, nil
	}
	if kitFlip(before, after) {
		var named []string
		for _, h := range holders {
			named = append(named, h.What)
		}
		if shown := len(named); shown < bins+piles {
			named = append(named, fmt.Sprintf("and %d more", bins+piles-shown))
		}
		// OWNER-WORDS
		return fmt.Errorf("%w: %s holds a count in %s and %s (%s). Changing it between one part and a kit would read their parts as kits. Count them to 0 or clear them first",
			ErrKitFlip, code, countPlural(bins, "bin", "bins"), countPlural(piles, "pile", "piles"), strings.Join(named, ", ")), nil
	}
	from, to := protocol.UOPRatio(before), protocol.UOPRatio(after)
	if from == to {
		return nil, nil
	}
	c := &RatioChange{Code: code, From: from, To: to, Bins: bins, Piles: piles}
	for _, h := range holders {
		b, _ := protocol.SplitUOP(h.Count, from)
		a, _ := protocol.SplitUOP(h.Count, to)
		c.Rows = append(c.Rows, RatioChangeRow{What: h.What, Parts: h.Count, UOPBefore: b, UOPAfter: a, Pile: h.Pile})
	}
	return nil, c
}

// checkCountUnit runs countUnitGuard for a save that states a manifest: the
// stored lines against the stated ones, and the holders read only when the
// line count's kind or the ratio would change.
func (s *PayloadService) checkCountUnit(p *payloads.Payload, sets PayloadSets, seen bool) error {
	if sets.Manifest == nil {
		return nil
	}
	stored, err := s.db.ListPayloadManifest(p.ID)
	if err != nil {
		return fmt.Errorf("read %s's manifest: %w", p.Code, err)
	}
	before := domain.ProtocolLines(stored)
	var after []protocol.ManifestItem
	for _, l := range *sets.Manifest {
		after = append(after, protocol.ManifestItem{PartNumber: l.PartNumber, PartsPerCycle: l.PartsPerCycle})
	}
	// A part at 1 a cycle and a kit both read at ratio 1, so the flip is asked
	// on its own: the holders are read for either.
	if !kitFlip(before, after) && protocol.UOPRatio(before) == protocol.UOPRatio(after) {
		return nil
	}
	code := p.Code
	if b, err := s.db.GetPayload(p.ID); err == nil && b != nil {
		code = b.Code
	}
	holders, bins, piles, err := s.db.PayloadCountHolders(code, 10)
	if err != nil {
		// A failed read must not become a silent allow.
		return fmt.Errorf("cannot read which bins and piles of %s hold a count: %w", code, err)
	}
	refuse, change := countUnitGuard(code, before, after, holders, bins, piles)
	if refuse != nil {
		return refuse
	}
	if change != nil && !seen {
		return &RatioChangeUnseen{Change: *change}
	}
	return nil
}
