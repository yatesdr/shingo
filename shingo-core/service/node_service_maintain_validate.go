package service

import (
	"fmt"
	"strings"

	"shingocore/store/nodes"
)

// Save-time rules for a maintained group.
//
// ONE FUNCTION, EVALUATED AGAINST THE WHOLE POST-STATE, called by every endpoint
// that changes any part of the configuration. Not per-field checks at each
// endpoint: most of these rules are about how the parts AGREE — a level whose
// carrier type no child accepts, a Σwant that leaves no room for a return — and
// a per-field check has no view of the other fields to compare against. Every
// mutating path therefore builds the configuration its write WOULD leave behind
// and asks this.
//
// REFUSALS vs WARNINGS is the difference between "this cannot work" and "this
// probably is not what you meant". A refusal blocks the save and names the
// setting it refused. A warning rides back with a successful save and is
// rendered; it never blocks, because every one of them is a state a plant can
// legitimately be in mid-configuration and an operator who cannot save a
// half-finished screen simply loses the work.
//
// EVERY MESSAGE NAMES THE THING. "level refused" sends somebody to read code;
// "45x58x32 is allowed at none of GRP-PRESS-A's positions" does not.

// MaintainedGroupCheck is what the rules found: what stops the save, and what
// the operator should see anyway.
type MaintainedGroupCheck struct {
	Refusals []string `json:"refusals,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Err returns the refusals as one error, or nil if there were none.
func (c MaintainedGroupCheck) Err() error {
	if len(c.Refusals) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(c.Refusals, "; "))
}

// ValidateMaintainedGroup runs every save-time rule against the configuration a
// save would leave behind.
//
// The error return is for READ failures — a database that could not answer. It
// is deliberately distinct from a refusal: "the rules say no" and "we could not
// find out" must never collapse into one another, because collapsing them either
// blocks saves during a blip or, far worse, lets a save through on the strength
// of a query that failed.
func (s *NodeService) ValidateMaintainedGroup(cfg MaintainedGroupConfig) (MaintainedGroupCheck, error) {
	var chk MaintainedGroupCheck
	refuse := func(f string, a ...any) { chk.Refusals = append(chk.Refusals, fmt.Sprintf(f, a...)) }
	warn := func(f string, a ...any) { chk.Warnings = append(chk.Warnings, fmt.Sprintf(f, a...)) }

	group, err := s.db.GetNode(cfg.GroupNodeID)
	if err != nil {
		return chk, fmt.Errorf("read group %d: %w", cfg.GroupNodeID, err)
	}
	groupName := group.Name

	// ── Depth-1 ──────────────────────────────────────────────────────────────
	// Maintained groups are FLAT: direct children only, no lanes, no nested
	// groups. It is a scope decision rather than a technical limit — depth
	// dissolves the dig-lock counting question and most of the ASRS interaction,
	// and a level held over buried carriers would be a number whose meaning
	// changes with what is parked in front of it.
	children, err := s.db.ListChildNodes(cfg.GroupNodeID)
	if err != nil {
		return chk, fmt.Errorf("read children of %s: %w", groupName, err)
	}
	var nested []string
	enabledChildren := 0
	for _, c := range children {
		if c.IsSynthetic || c.NodeTypeCode == "LANE" {
			nested = append(nested, c.Name)
		}
		if c.Enabled {
			enabledChildren++
		}
	}
	if len(nested) > 0 {
		refuse("%s is not flat — %s %s a lane or a group of its own, and a maintained group holds its level in direct positions only",
			groupName, strings.Join(nested, ", "), plural(len(nested), "is", "are"))
	}

	// ── A station, or the orders run unseen ──────────────────────────────────
	// projectOrder no-ops on a blank StationID, so a top-up order minted without
	// one would run on the floor and appear on no board anywhere. That is the
	// phantom-order family, and it is worth a refusal rather than a warning
	// because nothing downstream can detect it afterwards.
	if cfg.Enabled && strings.TrimSpace(cfg.MaintenanceStation) == "" {
		refuse("%s has no station for its top-up orders — without one they would run on the floor and show on no board",
			groupName)
	}

	// ── Level rows ───────────────────────────────────────────────────────────
	// Which carrier types each enabled child will accept, read through the SAME
	// predicate the resolver uses: an empty effective set means "no restriction",
	// so it accepts everything. Copying that rule rather than sharing it is how
	// a config screen ends up refusing what the floor would have allowed.
	allowedSomewhere := map[int64]bool{}
	for _, c := range children {
		if !c.Enabled {
			continue
		}
		bts, err := s.db.GetEffectiveBinTypes(c.ID)
		if err != nil {
			return chk, fmt.Errorf("read allowed bins at %s: %w", c.Name, err)
		}
		if len(bts) == 0 {
			// Unrestricted: this child takes any carrier, so every declared type
			// has a home and there is nothing left to check.
			for _, l := range cfg.Levels {
				allowedSomewhere[l.BinTypeID] = true
			}
			continue
		}
		for _, bt := range bts {
			allowedSomewhere[bt.ID] = true
		}
	}

	total := 0
	for _, l := range cfg.Levels {
		total += l.Want

		// The episode key is `mnt|<group>|<type code>`, and a code carrying the
		// separator would produce a key that parses back into different
		// components than it was built from. Refused at the only point where a
		// person can still choose a different code.
		if strings.Contains(l.BinTypeCode, "|") {
			refuse("carrier type %q contains a %q, which cannot be used in a maintained level", l.BinTypeCode, "|")
		}

		// A declared level nothing can hold is a permanent, silent shortfall:
		// the keeper would ask forever and every placement would be refused.
		if len(children) > 0 && !allowedSomewhere[l.BinTypeID] {
			refuse("%s is allowed at none of %s's enabled positions, so a level of it could never be held",
				l.BinTypeCode, groupName)
		}
	}

	// ── Room for a return ────────────────────────────────────────────────────
	// A warning, not a refusal: the group can be reconfigured, positions added,
	// or children re-enabled, and the runtime guard is the resolver refusing a
	// push at level — not this arithmetic. What it catches is the operator who
	// filled every position with declared level and left the unloader nowhere to
	// put the next carrier it drains.
	if cfg.Enabled && enabledChildren > 0 && total >= enabledChildren {
		warn("%s declares %d carriers across %d position%s — nothing is left free for a carrier coming back in",
			groupName, total, enabledChildren, plural(enabledChildren, "", "s"))
	}

	// ── Untyped supported positions ──────────────────────────────────────────
	// A warning because it is a data gap, not a contradiction, and because the
	// gap is the normal state today: node_bin_types is unpopulated at most
	// positions. It matters because typing a press's empty pull from its
	// position's allowed bins is how the pull stops being type-blind — a
	// position with no row stays blind, and the level churns while reading as
	// numerically perfect.
	var untyped []string
	for _, sup := range cfg.Supports {
		bts, err := s.db.ListBinTypesForNode(sup.ProcessNodeID)
		if err != nil {
			return chk, fmt.Errorf("read allowed bins at %s: %w", sup.ProcessNodeName, err)
		}
		if len(bts) == 0 {
			untyped = append(untyped, sup.ProcessNodeName)
		}
	}
	if len(untyped) > 0 {
		warn("no carrier types are set on %s — an empty pull to %s cannot be typed from the position",
			strings.Join(untyped, ", "), plural(len(untyped), "it", "them"))
	}

	return chk, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ensureExplicitBinTypeMode writes bin_type_mode on a group that has never had
// one, at the first save that declares a level.
//
// INHERIT-BY-DEFAULT CONTRADICTS A DECLARED MIX. An unset mode resolves by
// walking up the parent chain, so a group that has just been told to hold four
// of one type and two of another can silently be governed by an ancestor's
// allowed-bins list that permits neither. The default is written as "all" —
// no restriction — because that is what an unconfigured group behaves as today
// and because narrowing on the operator's behalf is a decision they did not
// make.
//
// Only when ABSENT. A mode somebody chose is never overwritten.
func (s *NodeService) ensureExplicitBinTypeMode(groupNodeID int64) error {
	if s.db.GetNodeProperty(groupNodeID, nodes.PropBinTypeMode) != "" {
		return nil
	}
	return s.db.SetNodeProperty(groupNodeID, nodes.PropBinTypeMode, nodes.BinTypeModeAll)
}
