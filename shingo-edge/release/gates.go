package release

// The gate table (SHAPE §3.3). Every verdict a release makes is one of these
// rows deciding, and Plan is the only code that applies them. Adding a gate
// is one row here, its facts in the snapshot and its read in the loader, and
// its matrix cells: TestEveryGateHasAMatrixRow (engine) fails until a cell
// exercises the row.
//
// The rows hold today's outcomes (S4 changes no behaviour): G7 is the pair's
// press-index collision refusal and the deferrals, as they were. G3 (the
// point fresh) and G7's lift rule over Core's releasePoints arrive with
// intents (S5).

// Gate ids.
const (
	G1 = "G1" // heading to the right wait
	G2 = "G2" // the wait's owner
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
		"go when Core will release it (staged, or in_transit); the per-order click refuses otherwise with Core's reason, every other door passes the leg over (pending, deferred or terminal)",
		[]string{"refuse:not-releasable"}},
	{G2, "the wait's owner",
		"the swap survivor (door 9)",
		"a station wait is the station's: a relay leg's or a changeover leg's wait is not the survivor rule's to release",
		[]string{"skip:not-the-survivors"}},
	{G4, "configuration readable",
		"every leg and door",
		"an order, node, runtime, claim, pair, steps or ledger that cannot be read, or a node with no claim, mode or destination to act on, refuses (fail closed)",
		[]string{"refuse:no-claim", "refuse:unclassifiable", "refuse:no-pair"}},
	{G5, "the line's pull",
		"a sequential A/B position the line feeds from",
		"the release moves the line to the partner first; an unreadable pull state, or a partner whose parts would land on a carrier still bound there, refuses; a plant-wide sweep declines a pulled position and names it",
		[]string{"refuse:pull", "refuse:outgoing-carrier"}},
	{G6, "the light curtain",
		"every curtained node the leg's release lets a bin cross",
		"not safe refuses with the sentence: release the light curtain at the node",
		[]string{"refuse:curtain"}},
	{G7, "the lift dependency",
		"a placing leg whose node another leg of the pair must clear first",
		"press-index: refuses while the sibling has not reached its wait (advisory); a pair click remembers the leg that could not go yet, and a changeover defers its paired supply to the evac's lift",
		[]string{"hold:collision", "deferred"}},
	{G8, "one robot per bin",
		"the Material page's release (a creation)",
		"refuses while the move an earlier tap created for the bin is still live",
		[]string{"refuse:in-flight"}},
}
