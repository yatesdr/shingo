package engine

import (
	"errors"
	"time"

	"shingo/protocol"
)

// core_answers.go — the shapes of Core's answers as the Edge reads them: the
// asks' request and response types (protocol's, aliased so call sites read
// short), a refusal, the occupancy outcome words, and the containment state.
// There is no HTTP client to Core: the Edge reaches Core by the ask pair
// (ask.go) and Core's feeds.

// BinLoadItem is one line of a bin-load request: part number + physical count.
type BinLoadItem = protocol.BinLoadItem

// OccupancyOutcome names why an occupancy read could not be decided from, for
// the station's occupied-check log (operator_stations.go). One word per outcome
// so an over-ordering incident can be reconstructed from logs alone: not_actable
// (BinsActable refused the table), unwatched (the table holds no row for the
// name), or unverifiable. The loader budget's "ok" arm went with the budget:
// a loader's carriers are Core's to count.
func OccupancyOutcome(err error) string {
	switch {
	case asBinsNotCurrent(err) != nil && asBinsNotCurrent(err).Why == NotActableUnwatched:
		return "unwatched"
	case asBinsNotCurrent(err) != nil:
		return "not_actable"
	default:
		return "unverifiable"
	}
}

// asBinsNotCurrent is err's *BinsNotCurrent, nil when it carries none.
func asBinsNotCurrent(err error) *BinsNotCurrent {
	var nc *BinsNotCurrent
	if errors.As(err, &nc) {
		return nc
	}
	return nil
}

// BinLoadRequest is a LOAD's body.
type BinLoadRequest = protocol.BinLoadRequest

// CoreRefusal is an act Core answered and refused: its status, its sentence,
// and what the refusal carries back for the Edge to apply before the sentence
// is shown. An ask not sent or not answered is not one: nobody answered.
type CoreRefusal struct {
	Status   int
	Sentence string
	// EpochRefresh is a count's refusal for an ended generation: the carrier's
	// current stamp (protocol.BinCountResponse.EpochRefresh).
	EpochRefresh *protocol.BinEpochRefresh
	// Rows is Core's current bins rows for the nodes the write touched, for
	// the bins copy (Engine.ApplyNodeBinsRows) before the sentence is shown.
	Rows []protocol.NodeBinsRow
}

func (r *CoreRefusal) Error() string { return r.Sentence }

// BinLoadResponse is the answer to a LOAD.
type BinLoadResponse = protocol.BinLoadResponse

// BinClearResponse is the answer to a CLEAR.
type BinClearResponse = protocol.BinClearResponse

// BinCountResponse is the answer to a count declared from the line.
type BinCountResponse = protocol.BinCountResponse

// ContainmentState is Core's containment state as the Edge reads it: the
// Edge's held copy of the containment feed (LocalContainment), the flags and
// held bins with the destinations and the two times the screens say "as of"
// with. The rows are the protocol's. The slices of a held copy are shared with
// it: readers must not modify them.
type ContainmentState struct {
	Containment  []ContainmentRow                  `json:"containment"`
	HeldBins     []HeldBinRow                      `json:"held_bins"`
	Destinations []protocol.ContainmentDestination `json:"destinations,omitempty"`
	// ReceivedAt is when the held copy arrived from Core; ConfirmedAt when a
	// heartbeat ack last quoted its digest back.
	ReceivedAt  time.Time `json:"-"`
	ConfirmedAt time.Time `json:"-"`
}

// ContainmentRow is one payload's containment flag state.
type ContainmentRow = protocol.PayloadContainmentRow

// HeldBinRow is one bin carrying the hold marker.
type HeldBinRow = protocol.HeldBinRow
