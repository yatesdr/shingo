package dispatch

import (
	"fmt"
	"sort"
	"strings"

	"shingocore/store"
)

// lane_gate_census.go — the one line per group that says whether the gate is
// configured, and whether it is configured WRONG.
//
// ── WHY A CENSUS RATHER THAN A REFUSAL ────────────────────────────────────
//
// The two write doors (the seeder and the UI save) refuse a duplicate point,
// because that is where a person is typing one and can be told. The READ path
// deliberately refuses nothing: a config check on the dispatch hot path strands
// every robot already standing at that point, which punishes the floor for a
// mistake made in an office.
//
// So the bad config has to be VISIBLE somewhere that is neither. This is that
// place, and it does three jobs in one line each so that none of them can be the
// one nobody built:
//
//	the enablement checklist   — which groups are gated at all, and with how many
//	                             spots. "No plant sets a mark" was true for a year
//	                             and was discovered by reading code.
//	the duplicate report       — a point shared between two groups, named on both.
//	the config smell           — lanes still carrying their own legacy override,
//	                             counted. A part-migrated group is the worst
//	                             regression fixture there is, because its numbers
//	                             are an average of two behaviours.

// GateCensusLine is one group's gate configuration as the census sees it.
type GateCensusLine struct {
	Group      string
	Lanes      int
	WaitPoints []string
	// Overrides is how many of this group's lanes still carry their own
	// lane_gate_point. Non-zero means the group is part-migrated.
	Overrides int
	// DuplicatedWith names the other groups this group shares a point with, and
	// which point. Empty is the healthy case.
	DuplicatedWith []string
}

// String renders the line. Deliberately one line: a census that wraps is a
// census people stop reading.
func (l GateCensusLine) String() string {
	state := "UNGATED"
	if len(l.WaitPoints) > 0 {
		state = fmt.Sprintf("%d wait point(s): %s", len(l.WaitPoints), strings.Join(l.WaitPoints, " "))
	}
	out := fmt.Sprintf("group %s: %d lane(s), %s, %d lane override(s)", l.Group, l.Lanes, state, l.Overrides)
	if l.Overrides > 0 {
		out += " [PART-MIGRATED — an override is a set-of-one and disables the group's widening for that lane]"
	}
	if len(l.DuplicatedWith) > 0 {
		out += " [DUPLICATE: " + strings.Join(l.DuplicatedWith, "; ") +
			" — a wait point is a physical place beside ONE block of lanes; a robot standing at" +
			" another block's spot is in somebody's aisle]"
	}
	return out
}

// CensusGateConfig reads every node group that owns lanes and reports its gate
// configuration. Read-only; it changes nothing and never fails the boot.
func CensusGateConfig(db *store.DB) ([]GateCensusLine, error) {
	rows, err := db.DB.Query(`
		SELECT g.name                                       AS grp,
		       COUNT(l.id)                                  AS lanes,
		       COALESCE(MAX(gp.value), '')                  AS group_points,
		       COUNT(lp.value) FILTER (WHERE lp.value <> '') AS overrides
		FROM nodes g
		JOIN nodes l ON l.parent_id = g.id
		            AND l.node_type_id = (SELECT id FROM node_types WHERE code = 'LANE')
		LEFT JOIN node_properties gp ON gp.node_id = g.id AND gp.key = $1
		LEFT JOIN node_properties lp ON lp.node_id = l.id AND lp.key = $2
		GROUP BY g.name
		ORDER BY g.name`, PropGroupWaitPoints, PropLaneGatePoint)
	if err != nil {
		return nil, fmt.Errorf("census gate config: %w", err)
	}
	defer rows.Close()

	var out []GateCensusLine
	for rows.Next() {
		var l GateCensusLine
		var raw string
		if err := rows.Scan(&l.Group, &l.Lanes, &raw, &l.Overrides); err != nil {
			return nil, fmt.Errorf("census gate config: scan: %w", err)
		}
		l.WaitPoints = ParseWaitPoints(raw)
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("census gate config: %w", err)
	}
	annotateDuplicatePoints(out)
	return out, nil
}

// annotateDuplicatePoints fills DuplicatedWith. A point in two groups is named
// on BOTH lines, because whichever line the reader happens to see has to be the
// one that tells them.
func annotateDuplicatePoints(lines []GateCensusLine) {
	owners := map[string][]string{}
	for _, l := range lines {
		for _, p := range l.WaitPoints {
			owners[p] = append(owners[p], l.Group)
		}
	}
	for i := range lines {
		var notes []string
		for _, p := range lines[i].WaitPoints {
			others := make([]string, 0, len(owners[p]))
			for _, g := range owners[p] {
				if g != lines[i].Group {
					others = append(others, g)
				}
			}
			if len(others) == 0 {
				continue
			}
			sort.Strings(others)
			notes = append(notes, fmt.Sprintf("%s is also %s's", p, strings.Join(others, "/")))
		}
		lines[i].DuplicatedWith = notes
	}
}

// DuplicateWaitPointsAcrossGroups reports the points `candidate` would steal
// from another group, for the two WRITE doors — the seeder and the UI save.
//
// excludeGroupID is the group being written, so re-saving a group's own points
// is not a conflict with itself.
//
// The message names BOTH groups on purpose: "duplicate wait point" tells the
// person nothing they can act on, and the second name is the whole of what they
// need to go and look at.
func DuplicateWaitPointsAcrossGroups(db *store.DB, excludeGroupID int64, candidate []string) ([]string, error) {
	if len(candidate) == 0 {
		return nil, nil
	}
	rows, err := db.DB.Query(`
		SELECT g.name, p.value
		FROM node_properties p
		JOIN nodes g ON g.id = p.node_id
		WHERE p.key = $1 AND p.node_id <> $2`, PropGroupWaitPoints, excludeGroupID)
	if err != nil {
		return nil, fmt.Errorf("duplicate wait points: %w", err)
	}
	defer rows.Close()

	owner := map[string]string{}
	for rows.Next() {
		var group, raw string
		if err := rows.Scan(&group, &raw); err != nil {
			return nil, fmt.Errorf("duplicate wait points: scan: %w", err)
		}
		for _, p := range ParseWaitPoints(raw) {
			owner[p] = group
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("duplicate wait points: %w", err)
	}

	var conflicts []string
	for _, p := range candidate {
		if g, taken := owner[p]; taken {
			conflicts = append(conflicts, fmt.Sprintf("%q already belongs to group %s", p, g))
		}
	}
	return conflicts, nil
}

// DuplicateWaitPoints is DuplicateWaitPointsAcrossGroups for a caller that holds
// a Dispatcher rather than a store — the UI save door. The package function
// stays for the seeder, which runs before any dispatcher exists.
func (d *Dispatcher) DuplicateWaitPoints(groupID int64, candidate []string) ([]string, error) {
	return DuplicateWaitPointsAcrossGroups(d.db, groupID, candidate)
}
