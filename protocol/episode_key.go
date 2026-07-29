package protocol

import (
	"fmt"
	"strings"
)

// Demand-episode identity.
//
// A demand episode is a continuous period during which a specific PLACE needs
// material. "One open episode per place" is the invariant, and Core enforces it
// with a partial unique index on demand_origins(episode_key) WHERE
// closed_at IS NULL.
//
// A COLUMN TUPLE CANNOT EXPRESS THAT, because the three kinds have three
// different identities: a threshold episode is about a loader binding, a cell
// episode is about a process's need for a payload in one direction, and a
// changeover episode is about a single changeover row. A tuple wide enough for
// all three is mostly NULL, and NULLs do not participate in uniqueness the way
// this needs.
//
// So identity is a computed string, and it lives HERE — in protocol, the one
// package both services import — rather than being built at each mint site.
// Core mints threshold episodes, Edge mints the other two, and both sides have
// to agree on the string exactly. Two spellings of one identity is how they
// drift, and the drift would be silent: a mismatched key does not error, it
// just fails to find the open episode and mints a second one.
const (
	// EpisodeKindThreshold is Core's ThresholdMonitor: a loader's plant-wide
	// in-loop total for a payload fell below its configured threshold.
	EpisodeKindThreshold = "threshold"
	// EpisodeKindCell is a process's material need at a cell, either direction.
	EpisodeKindCell = "cell"
	// EpisodeKindChangeover is a style transition.
	EpisodeKindChangeover = "changeover"
)

// Cell-episode directions. Mirror images with the same dual-trigger shape:
// supply brings material IN (RequestNodeMaterial), evacuate takes it OUT
// (RequestProduceSwap).
const (
	EpisodeDirectionSupply   = "supply"
	EpisodeDirectionEvacuate = "evacuate"
)

// Episode triggers.
const (
	// EpisodeTriggerAutoreorder is the level predicate firing on a PLC tick.
	EpisodeTriggerAutoreorder = "autoreorder"
	// EpisodeTriggerOperator is the HMI button. Distinct because neither entry
	// point checks the level: an operator can request on a node the system
	// considers fine.
	EpisodeTriggerOperator = "operator"
)

// ThresholdEpisodeKey identifies a Core threshold episode.
//
// The format deliberately reproduces Core's own bindingKey — station, node,
// payload — with a kind prefix. Inventing a second format for a binding's
// identity would leave two strings meaning the same thing.
func ThresholdEpisodeKey(station, coreNodeName, payloadCode string) string {
	return strings.Join([]string{"thr", station, coreNodeName, payloadCode}, "|")
}

// CellEpisodeKey identifies an Edge cell episode.
//
// THE GRAIN IS THE PROCESS, not the node. A press-index cell is one process
// spanning several nodes and its swap is one demand served by a multi-node
// dance; an A/B pair is two claims on one process and the process needs the
// payload regardless of which half is currently pulling. Keying on the node
// would split one demand into several, and each half would mint its own
// episode for the same need.
//
// Direction is part of the identity because a cell can genuinely need material
// brought IN and taken OUT at the same time — those are two demands, not one.
//
// PROCESSID IS THE EDGE PROCESS NAME ("SNF2"), NOT ITS SQLITE ROW ID.
//
// It was the row id, and that made Core carry two mutually unjoinable identity
// systems for the same processes: demand_origins.process_id held a number while
// the deployed process_styles.process_id and PlantClaimsReport.ProcessID both
// hold the name. Two wires describing the same processes, and no query able to
// put them side by side. Changed here, before Springfield ran migration 59, at
// the cost of one column type and one protocol field.
//
// THE NAME IS ALSO THE BETTER KEY, not merely the one that matches. An Edge row
// id is meaningless to Core and does not survive a reinstall — and a reinstall
// is the failure that actually happens, after which id 42 can belong to a
// different process and silently join an old episode as though it were the same
// place. A recreated "SNF2" re-identifies the same place correctly.
//
// THE COST, STATED: renaming a process now changes the key of any episode open
// at that moment, orphaning it. That window is small (this table holds only what
// is open, for minutes) and it self-heals, because the reconciling sweep
// iterates the open ROWS rather than reconstructing keys — the renamed process
// no longer satisfies the precondition, so the sweep closes the episode. It is a
// real exposure and it is the same one process_styles and plant-claims already
// carry, which is the point: one exposure, not two identity systems.
func CellEpisodeKey(station, processID, payloadCode, direction string) string {
	return fmt.Sprintf("cell|%s|%s|%s|%s", station, processID, payloadCode, direction)
}

// ChangeoverEpisodeKey identifies an Edge changeover episode.
//
// One changeover is one episode: to_style_id is written only at INSERT and
// nothing re-targets a row, so the row's lifetime IS the episode's.
// Cancel-and-redirect cancels this row and inserts a fresh one — a new id, and
// correctly a new episode.
func ChangeoverEpisodeKey(station string, processChangeoverID int64) string {
	return fmt.Sprintf("co|%s|%d", station, processChangeoverID)
}

// ParsedEpisodeKey is what an episode key says about itself.
type ParsedEpisodeKey struct {
	Kind      string
	Station   string
	Payload   string
	Direction string
	CoreNode  string
	// ProcessID is the Edge process NAME, matching demand_origins.process_id,
	// process_styles.process_id and PlantClaimsReport.ProcessID. See
	// CellEpisodeKey for why it is not the SQLite row id.
	ProcessID    string
	ChangeoverID int64
}

// ParseEpisodeKey reads a key back. It is the guard behind "every mint site
// emits a parseable key": a site that builds the string by hand instead of
// calling the constructors above fails this, and the test that calls it.
func ParseEpisodeKey(key string) (ParsedEpisodeKey, error) {
	parts := strings.Split(key, "|")
	if len(parts) == 0 {
		return ParsedEpisodeKey{}, fmt.Errorf("empty episode key")
	}
	switch parts[0] {
	case "thr":
		if len(parts) != 4 {
			return ParsedEpisodeKey{}, fmt.Errorf("threshold episode key %q: want 4 parts, got %d", key, len(parts))
		}
		return ParsedEpisodeKey{
			Kind: EpisodeKindThreshold, Station: parts[1], CoreNode: parts[2], Payload: parts[3],
		}, nil
	case "cell":
		if len(parts) != 5 {
			return ParsedEpisodeKey{}, fmt.Errorf("cell episode key %q: want 5 parts, got %d", key, len(parts))
		}
		// NO NUMERIC PARSE, because the process component is a name now.
		//
		// THE EMPTINESS CHECK IS NOT NEW COVERAGE, IT IS PRESERVED COVERAGE.
		// The removed step was fmt.Sscanf(parts[2], "%d"), which rejected
		// "notanumber" — no longer a defect — but also rejected "", which very
		// much still is: a cell key with no process names no place, and two
		// unnamed processes would collide on one key under the partial unique
		// index. Dropping the Sscanf without this would have quietly narrowed
		// what the parser rejects.
		pid := parts[2]
		if pid == "" {
			return ParsedEpisodeKey{}, fmt.Errorf(
				"cell episode key %q: empty process name — the process IS the grain here, so a "+
					"key without one identifies no place and collides with every other such key", key)
		}
		if parts[4] != EpisodeDirectionSupply && parts[4] != EpisodeDirectionEvacuate {
			return ParsedEpisodeKey{}, fmt.Errorf("cell episode key %q: unknown direction %q", key, parts[4])
		}
		return ParsedEpisodeKey{
			Kind: EpisodeKindCell, Station: parts[1], ProcessID: pid, Payload: parts[3], Direction: parts[4],
		}, nil
	case "co":
		if len(parts) != 3 {
			return ParsedEpisodeKey{}, fmt.Errorf("changeover episode key %q: want 3 parts, got %d", key, len(parts))
		}
		var cid int64
		if _, err := fmt.Sscanf(parts[2], "%d", &cid); err != nil {
			return ParsedEpisodeKey{}, fmt.Errorf("changeover episode key %q: changeover id %q: %w", key, parts[2], err)
		}
		return ParsedEpisodeKey{Kind: EpisodeKindChangeover, Station: parts[1], ChangeoverID: cid}, nil
	default:
		return ParsedEpisodeKey{}, fmt.Errorf("episode key %q: unknown kind prefix %q", key, parts[0])
	}
}
