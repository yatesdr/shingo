package domain

import (
	"fmt"
	"strings"
	"time"
)

// ── THE ROUTING SET ──────────────────────────────────────────────────────────
//
// A process's routing set is the list of nodes it may route material through
// that are not its own positions: where bins come from (source), where they
// wait on the way (staging) and where they go (destination). The HMI flow
// composer offers a press only these, never the plant. Positions stay in
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
	// destination fields. Lands DISABLED until an engineer adopts it.
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
	// THIS role — the evidence the Routing panel puts under a backfilled name
	// ("inbound source on 10 styles"). It is a read-side count and no column:
	// the derivation already reads exactly these claims, and this is that read
	// exposed rather than a second one.
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

// RoutingDeriveReport is one process's line of the routing-set backfill: what
// the derivation read, what it produced, and how many names still need an
// engineer's decision.
type RoutingDeriveReport struct {
	ProcessID   int64  `json:"process_id"`
	ProcessName string `json:"process_name"`
	// FlowComposerEnabled is the process's gate, and it is why nothing was
	// derived when it is set: a reviewed set is never re-seeded from claims.
	//
	// IT WAS CALLED Skipped, which named the CONSEQUENCE and not the fact —
	// so the routing panel's `flow_composer_enabled` was assigned from a
	// field called "skipped", and a reader had to know the two were the same
	// thing. A field is called what it is.
	FlowComposerEnabled bool `json:"flow_composer_enabled"`
	// Nodes is the number of backfill-origin rows in the set after the run —
	// what the derivation produced, positions already dropped.
	Nodes int `json:"nodes"`
	// Claims is the number of live claims the derivation read.
	Claims int `json:"claims"`
	// NeedDecision counts the distinct names in the set that Core does not
	// know. It is 0, not a finding, when Core's list was not available —
	// absence of data is never evidence.
	NeedDecision int      `json:"need_decision"`
	Unknown      []string `json:"unknown,omitempty"`
}

// Line renders the report as the one log line the backfill emits per process
// and the Routing panel shows as its first line.
func (r RoutingDeriveReport) Line() string {
	if r.FlowComposerEnabled {
		return fmt.Sprintf("routing set: %s — flow composer enabled, not re-derived (%d nodes from %d claims; %d need a decision)",
			r.ProcessName, r.Nodes, r.Claims, r.NeedDecision)
	}
	return fmt.Sprintf("routing set: %s — derived %d nodes from %d claims; %d need a decision",
		r.ProcessName, r.Nodes, r.Claims, r.NeedDecision)
}
