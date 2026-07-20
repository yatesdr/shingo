package engine

import (
	"fmt"
	"strings"

	"shingocore/fleet"
)

// LoadSequenceCheck is the outcome of validating a payload's advanced load
// sequence against the RDS binTask keys of the locations it loads at.
//
//   - Verified=true: every configured binTask name was confirmed present at
//     every checked location (or the sequence is empty = normal load).
//   - Missing: concrete (location, name) failures — RDS answered and a key is
//     absent. Populated only alongside a non-nil error from
//     ValidateAdvancedLoadSequence (a hard config-save rejection).
//   - Warnings: "couldn't verify" reasons (no binTask-checker backend, no
//     assigned nodes, an RDS error, or a location absent from the scene). The
//     save is allowed but flagged unverified; RDS's 50001 order-issue rejection
//     stays the loud runtime backstop.
type LoadSequenceCheck struct {
	Verified bool     `json:"verified"`
	Missing  []string `json:"missing,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// ValidateAdvancedLoadSequence checks that every binTask name in the payload's
// selected load sequence exists at each RDS location the payload is assigned to
// (node_payloads → node.Name = RDS location). It implements the owner's split
// rule (2026-07-18):
//
//   - A location that EXISTS in the RDS scene but is MISSING a configured key is
//     a hard failure: Missing is populated and a non-nil error is returned so the
//     caller rejects the save.
//   - An unknown sequence name is likewise a hard error (nothing to emit).
//   - Anything that merely PREVENTS verification — no binTask-checker backend
//     (the simulator), no assigned nodes yet, an RDS error, or a location absent
//     from the scene — is a WARNING: the check is returned with err=nil and the
//     caller saves the sequence flagged unverified.
//
// An empty seqName is the normal-load default: always verified, no RDS work.
// payloadID may be 0 (a not-yet-persisted payload on the create path); with no
// assigned nodes that resolves to a warning, never a rejection.
func (e *Engine) ValidateAdvancedLoadSequence(payloadID int64, seqName string) (*LoadSequenceCheck, error) {
	if seqName == "" {
		return &LoadSequenceCheck{Verified: true}, nil
	}

	seq, err := e.db.GetLoadSequence(seqName)
	if err != nil {
		return nil, fmt.Errorf("load sequence %q: %w", seqName, err)
	}
	if seq == nil {
		msg := fmt.Sprintf("unknown load sequence %q (not in the registry)", seqName)
		return &LoadSequenceCheck{Missing: []string{msg}}, fmt.Errorf("%s", msg)
	}

	checker, ok := e.fleet.(fleet.BinTaskChecker)
	if !ok {
		return &LoadSequenceCheck{Warnings: []string{
			"fleet backend cannot check binTask keys (e.g. the simulator) — sequence saved unverified",
		}}, nil
	}

	nodes, err := e.db.ListNodesForPayload(payloadID)
	if err != nil {
		return nil, fmt.Errorf("list nodes for payload %d: %w", payloadID, err)
	}
	if len(nodes) == 0 {
		return &LoadSequenceCheck{Warnings: []string{
			"payload has no assigned nodes yet — sequence saved unverified (re-check once a node is assigned)",
		}}, nil
	}
	locations := make([]string, 0, len(nodes))
	for _, n := range nodes {
		locations = append(locations, n.Name)
	}

	results, err := checker.CheckLocationTasks(locations)
	if err != nil {
		return &LoadSequenceCheck{Warnings: []string{
			fmt.Sprintf("could not reach RDS to verify (%v) — sequence saved unverified", err),
		}}, nil
	}

	byLoc := make(map[string]fleet.LocationTasks, len(results))
	for _, r := range results {
		byLoc[r.Location] = r
	}

	var missing, warnings []string
	for _, loc := range locations {
		r, seen := byLoc[loc]
		if !seen || !r.Exists {
			warnings = append(warnings, fmt.Sprintf("location %q not present in the RDS scene — unverified", loc))
			continue
		}
		have := make(map[string]bool, len(r.TaskNames))
		for _, name := range r.TaskNames {
			have[name] = true
		}
		for _, name := range seq.TaskNames {
			if !have[name] {
				missing = append(missing, fmt.Sprintf("location %q is missing binTask %q", loc, name))
			}
		}
	}

	if len(missing) > 0 {
		// RDS answered and a key is missing → hard reject (the whole point of the check).
		return &LoadSequenceCheck{Missing: missing, Warnings: warnings},
			fmt.Errorf("load sequence %q invalid: %s", seqName, strings.Join(missing, "; "))
	}
	if len(warnings) > 0 {
		return &LoadSequenceCheck{Warnings: warnings}, nil
	}
	return &LoadSequenceCheck{Verified: true}, nil
}
