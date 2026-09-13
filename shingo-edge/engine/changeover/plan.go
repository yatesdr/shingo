// Package changeover holds the pure data shapes for the changeover order
// plan. The engine package builds a Plan from a set of node diffs and then
// applies it (creating orders, linking node tasks). Keeping the shapes in
// their own package lets the planner be exercised in tests without spinning
// up an Engine.
package changeover

import (
	"shingo/protocol"
	"shingoedge/domain"
)

// ComplexOrderSpec describes a single complex order to be created.
//
// ProcessNode is the line node the order belongs to (the node whose claim
// drove the plan). Distinct from DeliveryNode for swap orders that drop at
// a supermarket but conceptually "live" at the line. Threaded through to
// ComplexOrderRequest.ProcessNode so Core can pick the line bin for
// order.BinID and target the right bin at release-time fallback.
type ComplexOrderSpec struct {
	DeliveryNode string
	ProcessNode  string
	Steps        []protocol.ComplexOrderStep
	AutoConfirm  bool
	PayloadCode  string // when non-empty, overrides lookupPayloadMeta
}

// RetrieveOrderSpec describes a fallback retrieve order.
type RetrieveOrderSpec struct {
	RetrieveEmpty bool
	DeliveryNode  string
	// SourceNode names the supermarket node group Core should pull from.
	// Empty falls back to Core's global FIFO scan (legacy behaviour).
	// Changeover specs populate this from diff.ToClaim.InboundSource so
	// fallback retrieves honour the configured supermarket — same fix as
	// the bin_loader retrieve plumbing in orders/manager.go.
	SourceNode  string
	StagingNode string
	LoadType    string
	PayloadCode string
	AutoConfirm bool
}

// OrderSpec is one of Complex / Retrieve. Exactly one field is set.
type OrderSpec struct {
	Complex  *ComplexOrderSpec
	Retrieve *RetrieveOrderSpec
}

// NodeAction is the per-node plan: zero, one, or two orders to create plus
// the post-creation node-task state. If Err is non-nil the applier records
// the failure (and sets the node task to "error") without creating orders.
//
// SupplyOrder and EvacOrder are positional, not robot identities. A drop
// fills only EvacOrder (single robot, evac steps with a staged-wait gate).
// A swap fills both (resupply + removal). The names describe what the
// order does, not which "side" of a two-robot pair it belongs to.
type NodeAction struct {
	NodeID   int64
	NodeName string
	// CoreNodeName is the node's CORE name — the identity every routing
	// decision is keyed by. NodeName is the Edge's display name and the two
	// are free to differ, so a pass that edits legs by node (the tooling
	// decorator) has to read this one.
	CoreNodeName string
	Situation    string
	SupplyOrder  *OrderSpec           // "next material" — staging / delivery order. nil for drops.
	EvacOrder    *OrderSpec           // "old material release" — swap/evac/release order. nil for adds.
	NextState    domain.NodeTaskState // node-task state to set on success
	LogTag       string               // short tag used in success log line
	Err          error                // pre-flight validation failure (planning-time)
}

// Plan is the full set of per-node actions for a changeover.
type Plan struct {
	Actions []NodeAction
}

// OrderCount is how many ORDER ROWS applying this plan will create — the
// changeover episode's expected_orders.
//
// The design calls this "the sourcing plan's bin count", and this IS that
// count: one order per bin the plan intends to move. Taking it from the plan
// rather than from the node or action count is the same principle as the cell
// kind's — the system's own stated intent, captured once, in the unit the ratio
// is measured in. A node can contribute zero orders (unchanged), one (a drop or
// an add), or two (a swap), so counting nodes would be wrong on every
// changeover that mixes them.
//
// Actions with a planning-time error contribute nothing: the applier creates no
// orders for them, so counting them would make a changeover that failed
// pre-flight look as if it had under-delivered.
func (p Plan) OrderCount() int {
	n := 0
	for _, a := range p.Actions {
		if a.Err != nil {
			continue
		}
		if a.SupplyOrder != nil {
			n++
		}
		if a.EvacOrder != nil {
			n++
		}
	}
	return n
}

// PreviewAction is the wire shape of one NodeAction: what the desktop's
// changeover preview and the flow composer's preview both return. It mirrors
// NodeAction but turns the error into a string and flattens the OrderSpec
// union so a UI can render it without a discriminator dance.
//
// NodeName is the Edge display name (process_nodes.name, free text);
// CoreNodeName is the identity every routing decision and the composer's
// picture are keyed by. Both travel.
type PreviewAction struct {
	NodeID       int64        `json:"node_id"`
	NodeName     string       `json:"node_name"`
	CoreNodeName string       `json:"core_node_name"`
	Situation    string       `json:"situation"`
	SupplyOrder  *PreviewSpec `json:"supply_order,omitempty"`
	EvacOrder    *PreviewSpec `json:"evac_order,omitempty"`
	NextState    string       `json:"next_state,omitempty"`
	LogTag       string       `json:"log_tag,omitempty"`
	Error        string       `json:"error,omitempty"`
}

// PreviewSpec is one order of a PreviewAction, flattened.
type PreviewSpec struct {
	Kind         string `json:"kind"` // "complex" or "retrieve"
	DeliveryNode string `json:"delivery_node,omitempty"`
	StagingNode  string `json:"staging_node,omitempty"`
	// From / To are the order's REAL first pickup and last drop-off — where a
	// bin is fetched and where it ends up (U10 P1).
	//
	// ADDED FOR THE ORDERS SENTENCE, and it could not be built without them.
	// Both S9s read `Robot · from → to · what`, and DeliveryNode alone answers
	// half of that: a supply order's delivery node is where the bin GOES, and
	// the sentence also has to say where it came from. A complex order's ends
	// live inside Steps, which the wire carried only as a COUNT
	// (`step_count: 3`), so the UI had the shape of the order and neither end
	// of it — which is why S9 said "brings the new bin to PLN_02" and could
	// not say from where.
	//
	// Read off the plan rather than guessed from the claim: a complex order's
	// steps are what the robot is actually told to do, and the claim's
	// inbound_source is what an engineer asked for. When a decorator rewrites
	// a leg, these follow and the claim does not.
	//
	// Additive on an EDGE DTO. Nothing under protocol/ moves for a sentence.
	From        string `json:"from,omitempty"`
	To          string `json:"to,omitempty"`
	StepCount   int    `json:"step_count,omitempty"`
	PayloadCode string `json:"payload_code,omitempty"`
	AutoConfirm bool   `json:"auto_confirm"`
}

// ToPreviewAction flattens one planned action for the wire.
func ToPreviewAction(a NodeAction) PreviewAction {
	out := PreviewAction{
		NodeID:       a.NodeID,
		NodeName:     a.NodeName,
		CoreNodeName: a.CoreNodeName,
		Situation:    a.Situation,
		SupplyOrder:  ToPreviewSpec(a.SupplyOrder),
		EvacOrder:    ToPreviewSpec(a.EvacOrder),
		NextState:    string(a.NextState),
		LogTag:       a.LogTag,
	}
	if a.Err != nil {
		out.Error = a.Err.Error()
	}
	return out
}

// ToPreviewSpec flattens one OrderSpec; nil stays nil.
func ToPreviewSpec(spec *OrderSpec) *PreviewSpec {
	if spec == nil {
		return nil
	}
	if spec.Complex != nil {
		from, to := stepEnds(spec.Complex.Steps)
		if to == "" {
			// A complex order with no dropoff step still ends somewhere, and
			// the delivery node is where.
			to = spec.Complex.DeliveryNode
		}
		return &PreviewSpec{
			Kind:         "complex",
			DeliveryNode: spec.Complex.DeliveryNode,
			StepCount:    len(spec.Complex.Steps),
			AutoConfirm:  spec.Complex.AutoConfirm,
			From:         from,
			To:           to,
		}
	}
	if spec.Retrieve != nil {
		// A retrieve fetches from SourceNode — the supermarket group the claim
		// named — and lands at the staging node when there is one, because
		// that is where the robot actually stops. The delivery node is where
		// the bin is FOR, and on a two-robot swap the two are different nodes.
		to := spec.Retrieve.StagingNode
		if to == "" {
			to = spec.Retrieve.DeliveryNode
		}
		return &PreviewSpec{
			Kind:         "retrieve",
			DeliveryNode: spec.Retrieve.DeliveryNode,
			StagingNode:  spec.Retrieve.StagingNode,
			PayloadCode:  spec.Retrieve.PayloadCode,
			AutoConfirm:  spec.Retrieve.AutoConfirm,
			From:         spec.Retrieve.SourceNode,
			To:           to,
		}
	}
	return nil
}

// stepEnds is a complex order's first pickup and last dropoff.
//
// FIRST AND LAST, not first and second: a quarter-child-cart sequence picks up
// once and drops off two or three times, and the sentence is about where the
// bin ends up rather than about every stop on the way. A `wait` step is not an
// end of anything.
func stepEnds(steps []protocol.ComplexOrderStep) (from, to string) {
	for _, st := range steps {
		switch st.Action {
		case "pickup":
			if from == "" {
				from = st.Node
			}
		case "dropoff":
			if st.Node != "" {
				to = st.Node
			}
		}
	}
	return from, to
}
