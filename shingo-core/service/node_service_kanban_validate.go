package service

import (
	"fmt"
	"strings"

	"shingocore/domain"
	"shingocore/kanban"
	"shingocore/store"
	"shingocore/store/loaders"
)

// Save-time rules for a kanban.
//
// ONE FUNCTION, EVALUATED AGAINST THE WHOLE POST-STATE, called by every endpoint
// that changes any part of the configuration. Not per-field checks at each
// endpoint: most of these rules are about how the parts AGREE — a level whose
// carrier type no child accepts, a Σwant past the positions, a level on an
// unloader's outbound with nowhere to spill — and
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

// KanbanCheck is what the rules found: what stops the save, and what
// the operator should see anyway.
type KanbanCheck struct {
	Refusals []string `json:"refusals,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Err returns the refusals as one error, or nil if there were none.
func (c KanbanCheck) Err() error {
	if len(c.Refusals) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(c.Refusals, "; "))
}

// typesAllowedSomewhere is which carrier types some enabled child will accept, read
// through the SAME predicate the resolver uses: an empty effective set means "no
// restriction", so it accepts everything. Copying that rule rather than sharing it is
// how a config screen ends up refusing what the floor would have allowed. Pins:
// TestKanban_RefusesLevelNoChildAccepts, TestKanban_UnrestrictedChildAcceptsAnyLevel.
func (s *NodeService) typesAllowedSomewhere(children []*domain.Node, levels []store.KanbanLevel) (map[int64]bool, error) {
	allowed := map[int64]bool{}
	for _, c := range children {
		if !c.Enabled {
			continue
		}
		bts, err := s.db.GetEffectiveBinTypes(c.ID)
		if err != nil {
			return nil, fmt.Errorf("read allowed bins at %s: %w", c.Name, err)
		}
		if len(bts) == 0 {
			// Unrestricted: this child takes any carrier, so every declared type
			// has a home and there is nothing left to check.
			for _, l := range levels {
				allowed[l.BinTypeID] = true
			}
			continue
		}
		for _, bt := range bts {
			allowed[bt.ID] = true
		}
	}
	return allowed, nil
}

// ValidateKanban runs every save-time rule against the configuration a
// save would leave behind.
//
// The error return is for READ failures — a database that could not answer. It
// is deliberately distinct from a refusal: "the rules say no" and "we could not
// find out" must never collapse into one another, because collapsing them either
// blocks saves during a blip or, far worse, lets a save through on the strength
// of a query that failed.
func (s *NodeService) ValidateKanban(cfg KanbanConfig) (KanbanCheck, error) {
	var chk KanbanCheck
	refuse := func(f string, a ...any) { chk.Refusals = append(chk.Refusals, fmt.Sprintf(f, a...)) }
	warn := func(f string, a ...any) { chk.Warnings = append(chk.Warnings, fmt.Sprintf(f, a...)) }

	group, err := s.db.GetNode(cfg.GroupNodeID)
	if err != nil {
		return chk, fmt.Errorf("read group %d: %w", cfg.GroupNodeID, err)
	}
	groupName := group.Name

	// ── Depth-1 ──────────────────────────────────────────────────────────────
	// A LEVEL is held in direct positions: no lanes, no nested groups. It is a
	// scope decision rather than a technical limit — depth dissolves the dig-lock
	// counting question and most of the ASRS interaction, and a level held over
	// buried carriers would be a number whose meaning changes with what is parked
	// in front of it. A group with no levels and no station counts nothing and keeps
	// nothing, so a plain lane group, whose modal posts its overflow and all-default
	// settings like any group's, or its strict row, is no such number. Pin:
	// TestKanbanSave_APlainGroupsOverflowIsAProperty.
	children, err := s.db.ListChildNodes(cfg.GroupNodeID)
	if err != nil {
		return chk, fmt.Errorf("read children of %s: %w", groupName, err)
	}
	var nested []string
	for _, c := range children {
		if c.IsSynthetic || c.NodeTypeCode == "LANE" {
			nested = append(nested, c.Name)
		}
	}
	if len(nested) > 0 && (len(cfg.Levels) > 0 || strings.TrimSpace(cfg.MaintenanceStation) != "") {
		refuse("%s is not flat — %s %s a lane or a group of its own, and a kanban holds its level in direct positions only",
			groupName, strings.Join(nested, ", "), plural(len(nested), "is", "are"))
	}

	// ── Supports need a station, or the orders run unseen ────────────────────
	// projectOrder no-ops on a blank StationID, so a top-up order minted without
	// one would run on the floor and appear on no board anywhere. That is the
	// phantom-order family, and it is worth a refusal rather than a warning
	// because nothing downstream can detect it afterwards. A kanban with no
	// supports declares what it holds and is refilled by others, and needs none.
	if len(cfg.Supports) > 0 && strings.TrimSpace(cfg.MaintenanceStation) == "" {
		refuse("%s has no station for its top-up orders — without one they would run on the floor and show on no board",
			groupName)
	}

	// ── Sources ──────────────────────────────────────────────────────────────
	if msg := s.sourceWarnings(cfg, groupName, refuse); msg != nil {
		chk.Warnings = append(chk.Warnings, msg...)
	}

	// ── Level rows ───────────────────────────────────────────────────────────
	allowedSomewhere, err := s.typesAllowedSomewhere(children, cfg.Levels)
	if err != nil {
		return chk, err
	}

	for _, l := range cfg.Levels {

		// A declared level nothing can hold is a permanent, silent shortfall:
		// the keeper would ask forever and every placement would be refused.
		if len(children) > 0 && !allowedSomewhere[l.BinTypeID] {
			refuse("%s is allowed at none of %s's enabled positions, so a level of it could never be held",
				l.BinTypeCode, groupName)
		}
	}

	if err := s.familyRefusal(cfg, refuse); err != nil {
		return chk, err
	}

	// ── An unloader's outbound, and where its repayments can go ──────────────
	feeding, err := s.unloadersSendingTo(groupName)
	if err != nil {
		return chk, err
	}
	repay, err := s.repaymentWarnings(cfg, groupName, len(feeding) > 0)
	if err != nil {
		return chk, err
	}
	chk.Warnings = append(chk.Warnings, repay...)

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

// sourceWarnings judges a kanban's sources: a source is not the kanban and is
// listed once (refuse), and a lineside kanban's keeper never takes from another
// kanban that has supports of its own, which is said here and never refused.
func (s *NodeService) sourceWarnings(cfg KanbanConfig, groupName string, refuse func(string, ...any)) []string {
	var warnings []string
	seen := make(map[int64]bool, len(cfg.Sources))
	for _, src := range cfg.Sources {
		if src.SourceNodeID == cfg.GroupNodeID {
			refuse("%s cannot source from itself", groupName)
			continue
		}
		if seen[src.SourceNodeID] {
			refuse("%s is listed twice in the sources of %s", src.SourceNodeName, groupName)
			continue
		}
		seen[src.SourceNodeID] = true
		if len(cfg.Supports) == 0 {
			continue
		}
		k, err := s.db.GetKanban(src.SourceNodeID)
		if err == nil && k != nil && k.HasSupports {
			warnings = append(warnings, fmt.Sprintf("%s has supports of its own, so the keeper of %s never takes from it",
				src.SourceNodeName, groupName))
		}
	}
	return warnings
}

// unloadersSendingTo names the live unloaders whose outbound is the group.
func (s *NodeService) unloadersSendingTo(groupName string) ([]string, error) {
	all, err := s.db.ListLoaders()
	if err != nil {
		return nil, fmt.Errorf("read loaders sending to %s: %w", groupName, err)
	}
	// A two-stage unloader's stage 2 is produce and sends its carts out as
	// empties all the same (loaders.ActsAsUnloaderSQL), so it counts.
	secondStage := map[int64]bool{}
	for _, l := range all {
		if l.SecondStageLoaderID != nil {
			secondStage[*l.SecondStageLoaderID] = true
		}
	}
	var feeding []string
	for _, l := range all {
		if (l.Role == loaders.RoleConsume || secondStage[l.ID]) && l.OutboundDest == groupName {
			feeding = append(feeding, l.Name)
		}
	}
	return feeding, nil
}

// repaymentWarnings says what the chooser cannot do for each type the kanban declares
// (plan.Planner.ChoosePush). Words, never refusals: a plant can want any of it.
//
//   - A source that declares no level of the type is short of nothing, so a cart of it never
//     repays that source. Sources of another family say nothing of the type and are left out:
//     a 32 x 30 zone is no lender of a 45 x 48 cart. A source with no level at all can hold any
//     family, so it is on every type's.
//   - A type with no source of its family: a cart of it, with no slot left here, waits on its
//     window. Said of a kanban a process draws from or an unloader sends to; one with neither
//     (a zone that only lends) has nothing to say.
//
// Pin: TestKanbanSave_WhatTheChooserCannotRepay.
func (s *NodeService) repaymentWarnings(cfg KanbanConfig, groupName string, feeding bool) ([]string, error) {
	if len(cfg.Levels) == 0 {
		return nil, nil
	}
	types, err := s.db.ListBinTypes()
	if err != nil {
		return nil, fmt.Errorf("read bin types for %s: %w", groupName, err)
	}
	family := make(map[string]int64, len(types))
	for _, t := range types {
		family[t.Code] = t.FamilyID
	}
	sourceLevels := make([][]store.KanbanLevel, len(cfg.Sources))
	for i, src := range cfg.Sources {
		if sourceLevels[i], err = s.db.ListDeclaredLevels(src.SourceNodeID); err != nil {
			return nil, fmt.Errorf("read levels of source %s: %w", src.SourceNodeName, err)
		}
	}
	var out []string
	for _, l := range cfg.Levels {
		routed := false
		for i, src := range cfg.Sources {
			declares, ofFamily := false, len(sourceLevels[i]) == 0
			for _, sl := range sourceLevels[i] {
				declares = declares || sl.BinTypeID == l.BinTypeID
				ofFamily = ofFamily || kanban.SameFamily(family[l.BinTypeCode], family[sl.BinTypeCode])
			}
			if declares {
				routed = true
				continue
			}
			if ofFamily {
				routed = true
				out = append(out, fmt.Sprintf("%s declares no level for %s — a cart of it never repays that source",
					src.SourceNodeName, l.BinTypeCode))
			}
		}
		if !routed && len(cfg.Sources) > 0 && (feeding || len(cfg.Supports) > 0) {
			out = append(out, fmt.Sprintf("%s declares %s with no source of its family — with no slot left here a cart of it waits on its window",
				groupName, l.BinTypeCode))
		}
	}
	return out, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
