// fleet_stop_reason.go — the sentence written when the fleet stops an order
// Core did not ask it to stop.

package engine

import (
	"sort"
	"strconv"
	"strings"

	"shingocore/domain"
	"shingocore/fleet"
)

// fleetStopDetail opens every fleet-stop reason. It is a prefix, not decoration:
// the cancel classifiers key an uncoded cancel as an RDS-origin cancel on it
// (domain.IsFleetStopDetail), so the fleet's own words go AFTER it, never in
// place of it. Pinned by TestFleetStopReason_StaysRDSOrigin.
const fleetStopDetail = domain.FleetStopDetail

// fleetStopReason renders why the fleet stopped an order, in the fleet's own
// words when it gave any.
//
// Until 2026-11-04 this was the literal "fleet order stopped" whatever RDS
// said. HK's 400t press lost seven swaps in an hour that morning (core orders
// 3166–3191) to RDS 60009 "Cannot find path to new block target [PLN_05]" — RDS
// stopped each evac about four seconds after its release appended the next
// blocks, and recorded the reason on the order's errors[]. The poll snapshot
// carried that list into handleVendorStatusChange and the cancel threw it away,
// so the order row, its history, the Edge and the swap partner all said the
// same four words, and the cause was one /orderDetails call away the whole time.
//
// ONE error, the newest, with a count of the rest: this string is toasted on
// the press and shown on the order page, and the owner's rule for those is short
// and searchable — the code and node a person looks up, not a paragraph. The
// full list stays on the mission telemetry row.
//
// Without errors (no snapshot, or a hand terminate in RoboShop, which carries
// none) it is the bare four words it always was: an absent reason is not dressed
// up as a sentence (owner ruling, 2026-11-04).
func fleetStopReason(snap *fleet.OrderSnapshot) string {
	if snap == nil {
		return fleetStopDetail
	}
	errs := newestDistinctErrors(snap.Errors)
	if len(errs) == 0 {
		return fleetStopDetail
	}
	s := fleetStopDetail + " — " + fleet.FormatVendorError(errs[0].Code, errs[0].Desc)
	if n := len(errs) - 1; n > 0 {
		s += " (+" + strconv.Itoa(n) + " more)"
	}
	return s
}

// newestDistinctErrors orders the fleet's errors newest first and drops exact
// repeats and empty entries. Newest first because the error that coincides with
// the stop is the one that explains it; RDS keeps errors[] for the order's whole
// life, so a replan it recovered from an hour earlier is still in the list.
func newestDistinctErrors(in []fleet.OrderMessage) []fleet.OrderMessage {
	out := make([]fleet.OrderMessage, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, m := range in {
		m.Desc = strings.TrimSpace(m.Desc)
		if m.Code == 0 && m.Desc == "" {
			continue
		}
		key := strconv.Itoa(m.Code) + "\x00" + m.Desc
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Timestamp > out[j].Timestamp })
	return out
}
