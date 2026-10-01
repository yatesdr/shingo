package release

import "shingo/protocol"

// Need names a group of facts the loader reads on request. A plan that
// reaches a gate whose facts are not loaded returns the group it needs, and
// the door's loop has the loader read it and plans again. So reads happen in
// gate order and stop at the gate that decides: a refused leg reads nothing
// past its refusal, exactly as the code before the extraction read lazily.
type Need uint32

const (
	NeedOrder    Need = 1 << iota // the order row (and its release facts)
	NeedCurtain                   // the curtained nodes the leg's release lets a bin cross
	NeedNode                      // the process node row
	NeedPull                      // whether the line is pulling from the node
	NeedRuntime                   // the node's runtime row
	NeedKind                      // the drop task, the claim the release resets against, its role
	NeedSupply                    // whether the leg is the supply of a two-robot swap
	NeedDeparts                   // whether releasing the leg takes a produce bin away
	NeedFlip                      // the sequential partner the release moves the line to
	NeedRoute                     // pair door: the node's changeover task
	NeedActive                    // pair and Material doors: the node, its runtime and its claim
	NeedPair                      // pair door: the two legs and their classification
	NeedInFlight                  // Material door: the order already working the node's bin
)

// Has reports whether every group in n is loaded.
func (l Need) Has(n Need) bool { return l&n == n }

// Sink is where a log line goes: the release subsystem of the debug log, the
// engine's log, or the process log.
type Sink int

const (
	SinkRelease Sink = iota
	SinkEngine
	SinkStd
)

// Log is one line a decision writes.
type Log struct {
	Sink Sink
	Text string
}

// Leg is what the loader has read about one leg for the trunk: the per-order
// release every door runs each of its legs through.
type Leg struct {
	Loaded Need

	OrderID int64
	// Label names the leg in the door's account ("evac", "supply", a re-fire's
	// name); TaskNode is the changeover task's node. Both only shape the text.
	Label    string
	TaskNode string
	Mode     string // the disposition's mode

	// NeedOrder
	Intent      *Intent // the leg's release intent, if any
	ReadErr     error
	Status      protocol.Status
	QueueReason string
	QueueCode   string
	// HasProcessNode: the order names a process node. ProcessNodeID is it.
	HasProcessNode bool
	ProcessNodeID  int64

	// NeedCurtain: the first curtained node, in the order the release lets a
	// bin cross them (Core's point when the act has it), that is not safe;
	// nil when every one is.
	Curtain error

	// NeedNode
	NodeErr  error
	NodeName string // display name
	CoreNode string

	// NeedPull: the pull state could not be read. PullNode is the name the
	// refusal uses.
	PullErr  error
	PullNode string

	// NeedRuntime. ActiveClaim renders the runtime's active claim for the
	// no-claim breadcrumb ("<nil>" when unset).
	RuntimeErr  error
	ActiveClaim string

	// NeedKind
	Drop          bool // the evac of a changeover drop
	ClaimResolved bool
	Produce       bool

	// NeedSupply
	SupplyErr error
	IsSupply  bool

	// NeedDeparts
	DepartErr error
	Departs   bool

	// NeedFlip
	Flip Flip
}

// Flip is the sequential partner a release on the feeding side moves the line
// to (L3).
type Flip struct {
	Applies    bool // a paired sequential position
	Node       string
	RuntimeErr error
	OwnPull    bool
	PartnerErr error
	Partner    string
	NotReady   string // flipTargetReady's reason, "" when ready
	// Blocked: the partner is not ready and a carrier is still bound there
	// (or its runtime cannot be read): its parts would land on that carrier.
	Blocked bool
}

// Pair is what the loader has read for the operator's RELEASE on a paired
// node (door 1).
type Pair struct {
	Loaded Need

	NodeID int64

	// NeedRoute: the node's changeover task's legs.
	TaskEvac, TaskSupply bool

	// NeedActive
	LoadErr       error
	NodeName      string
	ClaimResolved bool
	Mode          protocol.SwapMode

	// NeedPair
	ResolveErr   error
	Evac, Supply *int64
	// Relabelled: the legs' steps said the runtime slots had the pair
	// inverted; Slot* are what the slots said.
	Relabelled           bool
	SlotEvac, SlotSupply int64
	StagedPtr, ActivePtr *int64
	HasTask              bool
	DepartingReadErr     error

	// NeedDeparts: the evac takes a produce bin off the node.
	DepartErr error
	Departs   bool
}

// Changeover is what the loader has read for the changeover's release of its
// nodes (doors 5 and 6).
type Changeover struct {
	ReadErr error // the changeover or its tasks could not be read
	Sweep   bool  // every node, not one
	Tasks   []Task
}

// Task is one node task of the changeover.
type Task struct {
	NodeID    int64
	NodeName  string
	CoreName  string // what the board names a declined node by
	Situation string
	InScope   bool
	// Pulling / PullErr: whether the line pulls from the node (asked by a
	// sweep only).
	Pulling  bool
	PullErr  error
	PullNode string
	Evac     *int64
	Supply   *int64
}

// Material is what the loader has read for the Material page's release of a
// node's bin (doors 3 and 4).
type Material struct {
	Loaded Need

	// NeedActive
	LoadErr       error
	NodeName      string
	Qty           int64
	ClaimResolved bool // the node's claim, or the door's fallback
	ClaimNode     string
	OutboundDest  string

	// NeedCurtain
	Curtain error

	// NeedInFlight: the move order an earlier tap created for this bin.
	InFlightErr    error
	InFlightOrder  int64
	InFlightStatus protocol.Status
	InFlight       bool // a live move lifting from this node
}
