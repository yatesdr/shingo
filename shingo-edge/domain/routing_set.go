package domain

import (
	"strings"
	"time"

	"shingoedge/domain/flowspec"
)

// ── THE ROUTING SET ──────────────────────────────────────────────────────────
//
// A process's routing set is the list of nodes it may route material through
// that are not its own positions: where bins come from (source), where they
// wait on the way (staging) and where they go (destination). The composer
// offers a press only these, never the plant. Positions stay in
// process_nodes; the effective routing picture is the union of the two.
//
// It is a TABLE (process_routing_nodes), not a role column on process_nodes,
// because membership in process_nodes is runtime behaviour — the delivered
// fallback treats "not a process node" as the correct silent answer for a
// supermarket delivery — and because two writers (changeover_service.Create,
// StationService.SetNodes) mint and delete process_nodes rows with no role
// concept. One row per (process, node, role): a buffer is legitimately both a
// source and a destination.
//
// 'waypoint' is deliberately not a role. A key route is ordered and validated
// against the vendor map, not against this list.

// Routing roles — the three ways a node can take part in a process's flow.
const (
	RoutingRoleSource      = "source"
	RoutingRoleStaging     = "staging"
	RoutingRoleDestination = "destination"
)

// Routing origins — where a routing row came from.
const (
	// RoutingOriginEngineer: typed on the desktop Processes page.
	RoutingOriginEngineer = "engineer"
	// RoutingOriginBackfill: derived from live claims' source / staging /
	// destination fields, once, by migration v25. Lands enabled: the press
	// already routes material through every one of them.
	RoutingOriginBackfill = "backfill"
)

// RoutingRoles lists the roles in the order the desktop groups them.
func RoutingRoles() []string {
	return []string{RoutingRoleSource, RoutingRoleStaging, RoutingRoleDestination}
}

// IsRoutingRole reports whether r is one of the three roles.
func IsRoutingRole(r string) bool {
	switch r {
	case RoutingRoleSource, RoutingRoleStaging, RoutingRoleDestination:
		return true
	}
	return false
}

// RoutingNode is one row of a process's routing set.
type RoutingNode struct {
	ID           int64     `json:"id"`
	ProcessID    int64     `json:"process_id"`
	CoreNodeName string    `json:"core_node_name"`
	Role         string    `json:"role"`
	Label        string    `json:"label"`
	Sequence     int       `json:"sequence"`
	Enabled      bool      `json:"enabled"`
	Origin       string    `json:"origin"`
	CalledBy     string    `json:"called_by"`
	CreatedAt    time.Time `json:"created_at"`
	// StyleCount is how many of the process's LIVE styles name this node in
	// THIS role — the evidence the Routing picker puts beside a backfilled name
	// ("inbound source on 10 styles"). It is a read-side count and no column,
	// taken from the live claims the routing read already holds.
	//
	// PER ROLE, not per name, and by the same predicate the delete guard
	// matches on: a node that is a live source may be nobody's destination,
	// and one count over every field would put "on 10 styles" beside a
	// destination row nothing sends to.
	StyleCount int `json:"style_count"`
}

// RoutingFieldsFor is the claim-side rule for one role: which of a claim's
// fields name a node in that role.
//
// ONE STATEMENT OF THE RULE, read by the count and by the delete guard. They
// must agree — "10 styles route through this" and "you may not delete it,
// these styles route through it" are the same question asked twice — and
// before this they were two hand-written predicates, one in SQL and one in a
// correlated subquery, that had already drifted on whether to TRIM the
// routing row's own name.
func RoutingFieldsFor(c NodeClaim, role string) []string {
	switch role {
	case RoutingRoleSource:
		return []string{c.InboundSource}
	case RoutingRoleStaging:
		return []string{c.InboundStaging, c.OutboundStaging}
	default:
		return []string{c.OutboundDestination, c.ChangeoverEvacDestination}
	}
}

// CountRoutingStyles fills each row's StyleCount from claims already read:
// how many DISTINCT styles name that row's node in that row's role.
//
// IN GO, NOT IN SQL. The SQL form is a correlated COUNT(DISTINCT) with TRIM()
// on both sides of every comparison, which no index can serve, and it was
// running on every station poll, every preview and twice per save for a
// number one panel reads. The caller has the claims in hand; this is a pass
// over them.
//
// Both sides are trimmed, matching the count's own historic behaviour: plant
// data carries padded names and a row that does not match its claims reads as
// unused evidence, which is the one answer that must not be invented.
func CountRoutingStyles(rows []RoutingNode, claims []NodeClaim) {
	if len(rows) == 0 {
		return
	}
	// (role, trimmed name) -> the styles naming it.
	seen := map[string]map[int64]bool{}
	for _, c := range claims {
		for _, role := range RoutingRoles() {
			for _, field := range RoutingFieldsFor(c, role) {
				name := strings.TrimSpace(field)
				if name == "" {
					continue
				}
				key := role + "\x00" + name
				if seen[key] == nil {
					seen[key] = map[int64]bool{}
				}
				seen[key][c.StyleID] = true
			}
		}
	}
	for i := range rows {
		rows[i].StyleCount = len(seen[rows[i].Role+"\x00"+strings.TrimSpace(rows[i].CoreNodeName)])
	}
}

// RoutingNodeInput is the write shape. ProcessID comes from the URL and
// Origin / CalledBy are stamped by the server, so none of the three is
// accepted from a request body.
type RoutingNodeInput struct {
	ProcessID    int64  `json:"-"`
	CoreNodeName string `json:"core_node_name"`
	Role         string `json:"role"`
	Label        string `json:"label"`
	Sequence     int    `json:"sequence"`
	Enabled      bool   `json:"enabled"`
	Origin       string `json:"-"`
	CalledBy     string `json:"-"`
}

// RoutingGaps is the desktop's word for the fields a new row takes from the
// routing set — its inbound source and outbound destination, the lowest
// enabled row of each role (composer-model's defaultRouting, the same rule as
// lowestOfRole) — when the set has no such row, keyed by the field's wire
// name. A recipe draft draws the sentence beside the row's empty field, so the
// engineer reads where to fill it from. Nil when the set has both.
func RoutingGaps(processName string, routing []RoutingNode) map[string]string {
	var out map[string]string
	for _, g := range []struct {
		field flowspec.Field
		role  string
	}{
		{flowspec.InboundSource, RoutingRoleSource},
		{flowspec.OutboundDestination, RoutingRoleDestination},
	} {
		if lowestOfRole(routing, g.role) != "" {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[string(g.field)] = processName + "'s routing set names no " +
			strings.ToLower(flowspec.Label(g.field)) + " — add one under Routing set"
	}
	return out
}
