// node_generation.go — the counter that tells a station poll its cell picture
// may have moved, without the poll reading process_nodes.
//
// WHY A COUNTER AND NOT A QUERY. The station view is built every 500 ms per
// board while events flow, on a Pi with ONE SQLite connection, and the picture
// it used to carry cost two of those reads per poll per board. Taking the
// picture off the poll means the poll must still be able to say "it changed",
// and the only inputs it may use are ones it already holds. Everything else
// the picture is built from — the running style, its claims, the geometry
// cache — is already in hand on the poll. process_nodes is not, so this is the
// one fact that has to be published rather than read.
//
// SEEDED FROM THE CLOCK, NOT FROM ZERO. The counter lives in memory, so a
// restart would otherwise take it back to 0 and a browser holding a version
// computed at 5 could, in principle, be handed a version it had seen before.
// Starting at the process's boot time in nanoseconds makes every run's
// generations disjoint from every other run's.
//
// PROCESS-WIDE, NOT PER PROCESS ID, and the trade is stated rather than
// hidden: a node write on Press 4 bumps the version Press 6's board is
// holding, so Press 6 fetches its picture once more than it strictly needed
// to. That is one extra fetch of one small payload on an event that happens
// when an engineer edits a cell — perhaps a handful of times a week — against
// a per-process map that has to be kept correct through process deletion and
// node moves BETWEEN processes, which is exactly the kind of bookkeeping that
// goes stale and takes the picture with it. Over-invalidating is the safe
// direction; under-invalidating draws a cell that is not there.

package processes

import (
	"sync/atomic"
	"time"
)

var nodeGeneration atomic.Uint64

func init() {
	nodeGeneration.Store(uint64(time.Now().UnixNano()))
}

// NodeGeneration is the current value. It changes on every write to
// process_nodes and never repeats within a run.
func NodeGeneration() uint64 { return nodeGeneration.Load() }

// BumpNodeGeneration records that process_nodes was written.
//
// EVERY WRITER CALLS IT, AND A MISSED ONE IS THE ONLY WAY THE PICTURE GOES
// STALE. The writers inside this package call it directly; the two in
// service/ (StationService.SetNodes, which adopts and retires a station's
// nodes, and ChangeoverService's participant insert) write process_nodes with
// their own SQL inside their own transactions and call this after the commit.
//
// node_generation_drift_test.go is what keeps that list honest: it reads the
// tree for statements that write process_nodes and fails on one in a file that
// does not mention this counter.
func BumpNodeGeneration() { nodeGeneration.Add(1) }
