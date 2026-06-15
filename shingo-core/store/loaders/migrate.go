package loaders

import "fmt"

// Loader refactor cutover — production migration helpers. The deployable
// migration reads the live Edge style_node_claims joined with the flag tables
// (home_location_loaders, transitional_loaders, loader_payload_thresholds) into
// []MigrationClaim, runs CheckHomeTripwire, derives the loader aggregate, and
// writes bin_loaders on Core. The pure derivation + the SLN_002 tripwire live
// here so they are unit-testable without a cross-system (Edge↔Core) connection;
// the cross-DB cmd wiring + the home-location → loader grouping heuristic are
// plant-specific and land with the rollout (see impl-questions.md Q5).

// MigrationClaim is one legacy manual_swap loader binding the migration derives
// from: a style_node_claim row joined with its flag-table state.
type MigrationClaim struct {
	CoreNode        string
	Role            string // produce | consume
	Payload         string // the primary payload (the home's payload for dedicated_positions)
	Payloads        []string
	ReorderPoint    int
	InboundSource   string
	OutboundDest    string
	AutoPush        bool
	OperatorStation string         // operator_stations.code — the home-location grouping key
	HomeLocation    bool           // home_location_loaders membership
	Transitional    bool           // transitional_loaders membership
	Thresholds      map[string]int // payload_code → uop threshold (loader_payload_thresholds)
}

// DerivedHome is one dedicated position (node NAME → one payload); the writer
// resolves the name to a Core nodes.id.
type DerivedHome struct {
	PositionNode string
	PayloadCode  string
	MinStock     int
	UOPThreshold int
}

// DerivedLoader is a bin_loaders aggregate row plus its payloads (shared_window)
// or homes (dedicated_positions), ready for the writer to persist.
type DerivedLoader struct {
	Loader   Loader
	Payloads []Payload
	Homes    []DerivedHome
}

// GroupIntoLoaders derives the loader aggregate from legacy migration claims:
//   - shared_window: one loader per (core_node, role) carrying the claim's
//     allowed payloads.
//   - dedicated_positions (home_location): one loader per (operator_station,
//     role) — the operator station IS the physical loader area — with each home
//     node a single-payload position. A home-location claim with no operator
//     station falls back to a one-position loader anchored at its own node.
//
// CheckHomeTripwire is enforced first, so a node carrying >1 home payload fails
// the whole migration rather than importing a self-spamming config.
func GroupIntoLoaders(claims []MigrationClaim) ([]DerivedLoader, error) {
	if err := CheckHomeTripwire(claims); err != nil {
		return nil, err
	}
	type key struct{ anchor, role string }
	idx := map[key]int{}
	homeNodeSeen := map[string]bool{}
	anchorHomeSeen := map[string]bool{}
	payloadSeen := map[string]bool{}
	var out []DerivedLoader

	ensure := func(k key, mk Loader) int {
		if i, ok := idx[k]; ok {
			return i
		}
		out = append(out, DerivedLoader{Loader: mk})
		idx[k] = len(out) - 1
		return len(out) - 1
	}

	for _, c := range claims {
		if c.HomeLocation {
			anchor := c.OperatorStation
			if anchor == "" {
				anchor = c.CoreNode
			}
			i := ensure(key{anchor, c.Role}, Loader{
				Name: anchor, Role: c.Role,
				Layout: LayoutDedicatedPositions, Replenishment: replenishment(c),
				OutboundDest: c.OutboundDest, InboundSource: c.InboundSource,
			})
			nk := anchor + "\x00" + c.Role + "\x00" + c.CoreNode
			if homeNodeSeen[nk] {
				continue
			}
			homeNodeSeen[nk] = true
			out[i].Homes = append(out[i].Homes, DerivedHome{
				PositionNode: c.CoreNode, PayloadCode: c.Payload,
				MinStock: c.ReorderPoint, UOPThreshold: c.Thresholds[c.Payload],
			})
			continue
		}
		i := ensure(key{c.CoreNode, c.Role}, LoaderFromClaim(c))
		// Materialise the anchor as the shared loader's sole window member (step 1: no
		// loader resolves to zero members, so the Edge empty-windows fallback is dead).
		// One window per (anchor, role); a shared window carries no per-position payload
		// (the shared set lives in Payloads). The writer resolves PositionNode → node id.
		ak := c.CoreNode + "\x00" + c.Role
		if !anchorHomeSeen[ak] {
			anchorHomeSeen[ak] = true
			out[i].Homes = append(out[i].Homes, DerivedHome{PositionNode: c.CoreNode, PayloadCode: ""})
		}
		for _, p := range DerivePayloadSet(c) {
			pk := c.CoreNode + "\x00" + c.Role + "\x00" + p.PayloadCode
			if payloadSeen[pk] {
				continue
			}
			payloadSeen[pk] = true
			out[i].Payloads = append(out[i].Payloads, p)
		}
	}
	return out, nil
}

// CheckHomeTripwire fails loud if any home-location node carries more than one
// distinct payload — the SLN_002 self-spamming misconfiguration (one node, many
// payloads, home flag on) the cutover must NEVER silently import. The migration
// runs this before writing anything; a tripped node is quarantined for manual
// resolution rather than imported. The invariant the aggregate enforces
// structurally (UNIQUE(position_node_id), one payload per home) becomes a
// migration-time gate for legacy data that predates it.
func CheckHomeTripwire(claims []MigrationClaim) error {
	byNode := map[string]map[string]struct{}{}
	for _, c := range claims {
		if !c.HomeLocation {
			continue
		}
		set := byNode[c.CoreNode]
		if set == nil {
			set = map[string]struct{}{}
			byNode[c.CoreNode] = set
		}
		for _, p := range c.Payloads {
			set[p] = struct{}{}
		}
	}
	for node, set := range byNode {
		if len(set) > 1 {
			return fmt.Errorf("migration tripwire: home-location node %s carries %d payloads — quarantine and resolve manually (one payload per home position)", node, len(set))
		}
	}
	return nil
}

// replenishment maps the legacy flags to the aggregate's replenishment mode
// (D4): transitional → operator; otherwise a consume loader follows auto_push
// (auto when pushing, operator when not), and a produce loader defaults to auto.
func replenishment(c MigrationClaim) string {
	if c.Transitional {
		return ReplenishmentOperator
	}
	if c.Role == RoleConsume && !c.AutoPush {
		return ReplenishmentOperator
	}
	return ReplenishmentAuto
}

// DerivePayloadSet returns the per-payload config (min_stock from reorder_point,
// uop_threshold from the threshold map) for a shared_window loader's claim.
// Exposed for the migration writer and tested directly.
func DerivePayloadSet(c MigrationClaim) []Payload {
	out := make([]Payload, 0, len(c.Payloads))
	for _, code := range c.Payloads {
		out = append(out, Payload{
			PayloadCode:  code,
			MinStock:     c.ReorderPoint,
			UOPThreshold: c.Thresholds[code],
		})
	}
	return out
}

// LoaderFromClaim builds the bin_loaders aggregate row (sans payloads/homes) for
// one shared_window claim. The home-location grouping is intentionally not here
// — it needs a plant-specific anchor/position heuristic (impl-questions.md Q5).
func LoaderFromClaim(c MigrationClaim) Loader {
	layout := LayoutSharedWindow
	if c.HomeLocation {
		layout = LayoutDedicatedPositions
	}
	return Loader{
		Name:          c.CoreNode,
		Role:          c.Role,
		Layout:        layout,
		Replenishment: replenishment(c),
		OutboundDest:  c.OutboundDest,
		InboundSource: c.InboundSource,
	}
}
