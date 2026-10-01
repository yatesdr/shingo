package release

// Origin is the door an act came through. Each door is one adapter in
// engine/release_doors.go; the matrix numbers them (engine/release_matrix_test.go).
type Origin string

const (
	OriginStationPair     Origin = "station_pair"     // door 1: RELEASE on a paired node
	OriginStationOrder    Origin = "station_order"    // door 2: RELEASE on one order (the API, the sim's auto-operator)
	OriginMaterialPage    Origin = "material_page"    // door 3: the Material page's release of a node's bin
	OriginPositionEvac    Origin = "position_evac"    // door 4: a fanned-out position's changeover evac
	OriginChangeoverSweep Origin = "changeover_sweep" // door 5: the changeover's release of every node
	OriginChangeoverNode  Origin = "changeover_node"  // doors 5 and 6: the changeover's release of one node
	OriginDeferral        Origin = "deferral"         // door 8: a pair leg the click deferred, at its staging
	OriginSurvivor        Origin = "survivor"         // door 9: a swap leg whose partner finished
	OriginPickup          Origin = "pickup"           // door 10: a changeover supply deferred to its evac's lift
)

// Purpose is the station decision a station wait belongs to, from a closed
// set the step builders own (stationWait(node, purpose)). A coordination wait
// — one robot holding while its partner decides — takes the purpose of the
// decision it follows. The button that releases a wait is labelled with it.
type Purpose string

const (
	// PurposeSwap: the steady-state cycle's decision — the bin is done (a
	// produce bin full, a consume bin spent): swap it.
	PurposeSwap Purpose = "swap"
	// PurposeReady: a changeover's first decision — the cell is ready for the
	// changeover's robots.
	PurposeReady Purpose = "ready"
	// PurposeToolingDone: a changeover's second decision — the tooling change
	// is done: set the new bins down.
	PurposeToolingDone Purpose = "tooling_done"
)

// Purposes is the closed set, in the order a cell meets them.
var Purposes = []Purpose{PurposeSwap, PurposeReady, PurposeToolingDone}

// Act is one decision to let robots go: an operator click or a system rule.
type Act struct {
	Origin   Origin
	Node     int64 // the process node the act was made at; 0 when it names an order
	CalledBy string
}

// passesOverUnreleasable reports whether the act skips a leg Core will not
// take yet rather than refusing the act. Only the per-order click refuses: it
// names one order, and an operator who pressed it for a leg that is not at a
// wait is told so. Every other door covers legs it did not name one by one,
// and a leg not yet at its wait is pending, deferred, or someone else's.
func (a Act) passesOverUnreleasable() bool { return a.Origin != OriginStationOrder }
