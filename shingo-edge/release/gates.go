package release

// The gate table (SHAPE §3.3). Every verdict a release makes is one of these
// rows deciding, and Plan is the only code that applies them. Adding a gate
// is one row here, its facts in the snapshot and its read in the loader, and
// its matrix cells: TestEveryGateHasAMatrixRow (engine) fails until a cell
// exercises the row.
//
// S5: the act's gates read Core's point (releasePoints) and hold with an
// intent rather than refuse: G1 for a leg not yet at its wait whose partner
// goes, G3 with no point, G6 on a live curtain, G7 on a lift dependency.

// Gate ids.
const (
	G1 = "G1" // heading to the right wait
	G2 = "G2" // the wait's owner
	G3 = "G3" // the point fresh
	G4 = "G4" // configuration readable
	G5 = "G5" // the line's pull (sequential A/B)
	G6 = "G6" // the light curtain
	G7 = "G7" // the lift dependency
	G8 = "G8" // one robot per bin
)

// Gate is one row of the table.
type Gate struct {
	ID   string
	Name string
	// Applies says which legs and doors the row is asked of.
	Applies string
	// AtAct is what it answers at an act; Verdicts are the outcome classes
	// the release matrix prints for it.
	AtAct    string
	Verdicts []string
}

// Gates is the table, in id order. Each stage evaluates the rows that apply
// to it in the order its plan function states.
var Gates = []Gate{
	{G1, "heading to the right wait",
		"every leg in scope",
		"go when staged at the act's station wait or still driving to it; not yet moving: the per-order click refuses with Core's reason, a leg whose partner on the same press goes holds (remembered), any other is pending",
		[]string{"refuse:not-releasable", "hold:wait"}},
	{G2, "the wait's owner",
		"every leg",
		"a leg staged at a lane wait is Core's to release: out of the act's scope",
		[]string{"out:lane-wait"}},
	{G3, "the point fresh",
		"every leg that could go",
		"Core's point for the act could not be read: hold, waiting for Core (wake: Core reachable, the floor)",
		[]string{"hold:core"}},
	{G4, "configuration readable",
		"every leg and door",
		"an order, node, runtime, claim, pair, steps or ledger that cannot be read, or a node with no claim, mode or destination to act on, refuses (fail closed)",
		[]string{"refuse:no-claim", "refuse:unclassifiable", "refuse:no-pair"}},
	{G5, "the line's pull",
		"a sequential A/B position the line feeds from",
		"the release moves the line to the partner first; an unreadable pull state, or a partner whose parts would land on a carrier still bound there, refuses; a plant-wide sweep declines a pulled position and names it",
		[]string{"refuse:pull", "refuse:outgoing-carrier"}},
	{G6, "the light curtain",
		"every curtained node the leg's next segment picks up or drops at (Core's point)",
		"not safe holds: the press is remembered, the chip says release the light curtain, the robot goes when it clears (Q8)",
		[]string{"hold:curtain", "refuse:curtain"}},
	{G7, "the lift dependency",
		"a leg with AwaitsLift on Core's point",
		"holds unless its lifter goes in this act and Core says they co-release (SHAPE §3.4); wake: the lifter's BinPickedUp, the lifter terminal, the floor",
		[]string{"hold:lift"}},
	{G8, "one robot per bin",
		"the Material page's release (a creation)",
		"refuses while the move an earlier tap created for the bin is still live",
		[]string{"refuse:in-flight"}},
}
