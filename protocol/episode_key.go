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
	// EpisodeKindMaintain is Core's maintained-group level keeper: a node group
	// is holding fewer unclaimed empty carriers of one type than it is declared
	// to hold.
	//
	// The SECOND kind Core mints, and the first whose demand is for an EMPTY
	// carrier rather than a payload. That is why its key carries a bin type
	// where the others carry a payload code: nothing about "four 45x58x32 on
	// hand" is expressible in payload terms, and the episode has to be
	// identifiable per type or one group's two declared types would share one
	// open episode and one counting stream.
	EpisodeKindMaintain = "maintain"
)

// ── AN EPISODE IS PRODUCE OR CONSUME, AND THAT IS THE CLAIM'S OWN WORD ────
//
// This was a separate two-valued vocabulary: EpisodeDirectionSupply = "supply"
// and EpisodeDirectionEvacuate = "evacuate", naming the direction material
// TRAVELS on one leg. It is retired in favour of ClaimRoleConsume /
// ClaimRoleProduce, which are the values already sitting in
// style_node_claims.role and already spelled in this package.
//
// ONE FACT HAD TWO SPELLINGS AND THE TREE CARRIED THE DICTIONARY BETWEEN THEM.
// demand_reconciler.go read `if ep.Direction == EpisodeDirectionEvacuate { role
// = ClaimRoleProduce }` — a hand-written translation, 1:1, from a value that is
// a function of the claim to the claim's own field. A claim has exactly one
// role, so an episode was only ever one direction; the direction was never
// independent information. This file's own header warns that two spellings of
// one identity is how they drift, and the drift is silent because a mismatched
// key does not error, it just fails to find the open episode.
//
// IT DRIFTED EXACTLY THERE. backfillCellOrigin hardcoded the supply spelling
// while a produce cell only ever opens in the other one, so the join asked for
// `cell|PRESS-2|PANEL-B|supply` against an open row reading
// `cell|PRESS-2|PANEL-B|evacuate`, missed, and every sequential backfill in the
// plant landed on Core as an orphan. Deleting the translation is what makes that
// unspellable: there is no second vocabulary left to pick the wrong word from.
//
// AND IT NAMES THE RIGHT THING UNDER THE CIRCLE DOCTRINE. An episode represents
// a process's full circular material handling — for a produce cell, empty in,
// fill, full out. "supply" and "evacuate" name single LEGS of that circle, which
// is the reading that invites one cell to hold two open episodes for one loop.
// "produce" and "consume" name the cell's standing role in it, which is one
// episode per circle.
//
// SPELLING CHANGE, PERSISTED IDENTITY: migration 87 rewrites Springfield's
// stored demand_origins keys and direction values. See its header for why this
// was not free — the "free until the first deploy" note further down this file
// had already expired.

// ParseEpisodeKey and CellEpisodeKey take a ClaimRole; the constants live in
// types.go beside the claim they belong to.

// Episode triggers.
const (
	// EpisodeTriggerAutoreorder is the level predicate firing on a PLC tick.
	EpisodeTriggerAutoreorder = "autoreorder"
	// EpisodeTriggerOperator is the HMI button. Distinct because neither entry
	// point checks the level: an operator can request on a node the system
	// considers fine.
	EpisodeTriggerOperator = "operator"
)

// THE STATION IS SCOPE FOR ONE KIND AND FRAGMENTATION FOR THE OTHER TWO.
//
// All three constructors used to embed the station. That was invisible while
// `station` was a CONSTANT across a plant — one edge_registry row, one value,
// and a one-valued component is not a discriminator. The day each edge gets a
// distinct id is the day it becomes one, and two of these three keys fragment.
//
// The rule is NOT "drop the station". It is:
//
//	A qualifier belongs in an identity exactly when the thing being identified
//	is not already unique at plant scope.
//
// Applied per kind, that gives three different answers, and a blanket rewrite
// would get the third one wrong:
//
//   - THRESHOLD — keyed on a Core node name. nodes.name is TEXT NOT NULL
//     UNIQUE, so the name is already plant-unique. Station DROPPED.
//   - CELL — keyed on the Edge process NAME, which Core already treats as
//     plant-unique everywhere else: process_styles' PRIMARY KEY is
//     (process_id, style_id) and style_claims carries no station column at
//     all. Station DROPPED — see CellEpisodeKey.
//   - CHANGEOVER — keyed on process_changeovers.id, an Edge-local SQLite row
//     id that two edges both reach. Station KEPT, as the counter space that
//     id lives in. Same reasoning as inventory_delta_dedup's station column.
//
// FREE TO CHANGE, AND ONLY UNTIL THE FIRST DEPLOY. demand_origins is created
// by migration v59; both live plants are measured at schema_migrations = 52,
// and their deployed binaries (Springfield 4527c4d6, Hopkinsville ef99421f)
// contain no v59 at all — max registered migration 52 in both trees. No plant
// has the table, so no stored key is being invalidated. After v59 ships, every
// change to these strings breaks episode continuity and needs a migration.

// ThresholdEpisodeKey identifies a Core threshold episode.
//
// NO STATION COMPONENT, and that is a deliberate narrowing from the original
// format, which reproduced Core's own bindingKey verbatim — station, node,
// payload. The bindingKey is a RUNTIME map key inside one process, where the
// station is free; this is a PERSISTED identity compared across services and
// across time, where it is not.
//
// nodes.name is TEXT NOT NULL UNIQUE (store/schema/postgres_ddl.go), so the
// Core node name alone names the place. Adding the reporting edge to it would
// mean one loader's replenishment demand splits into two episode streams the
// moment a second edge reports the same node, and neither side could detect
// it: the close carries a key with no matching open, SupersedeOpenEpisode
// finds nothing, a second episode mints, and the first sits open until the
// sweep gives up on it as unattributed.
func ThresholdEpisodeKey(coreNodeName, payloadCode string) string {
	return strings.Join([]string{"thr", coreNodeName, payloadCode}, "|")
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
// THE ROLE IS PART OF THE IDENTITY because one process can genuinely run a
// consume cell and a produce cell for the same payload at the same time, and
// those are two demands with two circles, not one. It is NOT here to separate
// the legs of a single cell's loop: a produce cell taking an empty in and
// sending a full out is ONE episode, because it is one circle. See the ClaimRole
// note above for what that distinction cost when the component named legs.
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
//
// AND THAT LAST SENTENCE IS WHY THERE IS NO STATION IN THIS KEY EITHER.
//
// v63 changed processID from the SQLite row id to the name so Core would stop
// carrying two unjoinable identity systems for the same processes. It fixed
// half the divergence. The other half was the station sitting in front of the
// name: Core's mirror of these same processes — process_styles, PRIMARY KEY
// (process_id, style_id), and style_claims — has NO station column anywhere.
// So Core already asserts, in the deployed schema, that a process name is
// plant-unique. A key of (station, process) asserts it is not. Two statements
// about one fact, and the query that tries to put an episode next to its
// process's claims is the one that finds out.
//
// The concrete failure the station caused, in the grain this comment already
// argues for: a press-index process served by two edges, or moved from one to
// another, is ONE place needing ONE payload. Keyed with the station, the close
// carries a key the open never had. SupersedeOpenEpisode matches nothing, a
// second episode mints for a demand already open, and the first is only ever
// resolved by the sweep marking it unattributed. Cross-service, so neither end
// sees a contradiction — Edge closed something, Core recorded something else.
// THE ROLE IS TYPED, and that is load-bearing rather than tidiness. The fourth
// component used to be a bare string, so every call site chose a word — and one
// of them chose the wrong one for its cell and cost the plant every sequential
// backfill's attribution. A ClaimRole has two values and comes off the claim,
// so the caller supplies the fact instead of naming it.
func CellEpisodeKey(processID, payloadCode string, role ClaimRole) string {
	return fmt.Sprintf("cell|%s|%s|%s", processID, payloadCode, role)
}

// ChangeoverEpisodeKey identifies an Edge changeover episode.
//
// One changeover is one episode: to_style_id is written only at INSERT and
// nothing re-targets a row, so the row's lifetime IS the episode's.
// Cancel-and-redirect cancels this row and inserts a fresh one — a new id, and
// correctly a new episode.
//
// THE STATION STAYS HERE, ALONE AMONG THE THREE, AND REMOVING IT IS THE
// MISTAKE THIS COMMENT EXISTS TO PREVENT.
//
// processChangeoverID is process_changeovers.id — an AUTOINCREMENT row id in
// ONE EDGE'S SQLite file. It is not plant-unique and never was; every edge
// starts at 1 and every edge eventually reaches 7. The other two kinds are
// keyed on names that Core has already made plant-unique (nodes.name is
// UNIQUE; process_styles keys on the process name with no station), so their
// station component was pure fragmentation. This one is the opposite: the
// station is the COUNTER SPACE the id is drawn from, and without it two
// plants' changeover 7 collide on one key under the partial unique index on
// demand_origins(episode_key) WHERE closed_at IS NULL — the second edge's
// changeover episode silently superseding the first edge's.
//
// This is the same distinction inventory_delta_dedup's station column carries
// ("whose Edge-local sequence counter space is this"), and it is why "strip
// the station everywhere" is the wrong shape for this change. A rewrite that
// treats all three keys alike gets exactly one of the three wrong, and it is
// this one.
func ChangeoverEpisodeKey(station string, processChangeoverID int64) string {
	return fmt.Sprintf("co|%s|%d", station, processChangeoverID)
}

// MaintainEpisodeKey identifies a Core maintained-group episode: one node group
// falling short of its declared level in ONE carrier type.
//
// NO STATION, for the threshold key's reason and more strongly: nodes.name is
// TEXT NOT NULL UNIQUE, so the group names the place by itself. The maintenance
// station the keeper's orders are projected to is CONFIG on the group, not part
// of what the demand IS — a group whose station is retargeted is the same group
// with the same shortfall, and an episode that changed key when somebody edited
// a dropdown would orphan itself mid-demand.
//
// THE TYPE IS PART OF THE IDENTITY, and it is the whole reason this kind exists
// separately from threshold. A group declaring four of one carrier and two of
// another has two demands that are satisfied independently, counted
// independently, and can be short at different moments. One episode per group
// would make "the group is short" a single fact, and the keeper would have no
// way to say which type it was short OF — which is precisely the information the
// ask has to carry (SYNTH round 2 §1: the episode is the carrier of the type).
//
// binTypeCode, not bin_type_id. The id is a local key; the code is what travels
// on every other carrier-typed surface, what a person reads, and what
// MaintainedTypeForOrigin hands back to the finder. An id here would make the
// key unreadable in a log and unresolvable across a restore.
//
// The code must not contain "|" — config save refuses one that does
// (service.ValidateMaintainedGroup), because this key is split on it.
func MaintainEpisodeKey(groupNodeName, binTypeCode string) string {
	return strings.Join([]string{"mnt", groupNodeName, binTypeCode}, "|")
}

// ParsedEpisodeKey is what an episode key says about itself.
type ParsedEpisodeKey struct {
	Kind string
	// Station is populated for the CHANGEOVER kind ONLY, because that is the
	// only kind whose key still carries one — it scopes an Edge-local row id.
	// Threshold and cell keys leave this empty by construction.
	//
	// Nothing reads it. Core takes the reporting station from env.Src.Station
	// (messaging/demand_origin_handler.go), which is the routing address and
	// the right source for "who told me"; the key answers "which place", and
	// those are different questions. Kept because ChangeoverID is meaningless
	// without the space it was counted in.
	Station string
	Payload string
	// Role is the cell's standing role in its circle — produce or consume. It
	// was Direction ("supply"/"evacuate"), a second vocabulary for the claim's
	// own field; see the ClaimRole note at the top of this file.
	Role     ClaimRole
	CoreNode string
	// ProcessID is the Edge process NAME, matching demand_origins.process_id,
	// process_styles.process_id and PlantClaimsReport.ProcessID. See
	// CellEpisodeKey for why it is not the SQLite row id.
	ProcessID    string
	ChangeoverID int64
	// BinType is populated for the MAINTAIN kind only — the carrier type the
	// group is short of. It sits where the other kinds carry a payload, and
	// deliberately does not reuse Payload: a maintained group's demand is for an
	// EMPTY carrier, and a reader that found a value in Payload would reasonably
	// conclude the episode wanted parts in it.
	BinType string
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
		if len(parts) != 3 {
			return ParsedEpisodeKey{}, fmt.Errorf("threshold episode key %q: want 3 parts, got %d", key, len(parts))
		}
		if parts[1] == "" {
			return ParsedEpisodeKey{}, fmt.Errorf(
				"threshold episode key %q: empty Core node name — the node IS the place here, so a "+
					"key without one identifies nothing and collides with every other such key", key)
		}
		return ParsedEpisodeKey{
			Kind: EpisodeKindThreshold, CoreNode: parts[1], Payload: parts[2],
		}, nil
	case "cell":
		if len(parts) != 4 {
			return ParsedEpisodeKey{}, fmt.Errorf("cell episode key %q: want 4 parts, got %d", key, len(parts))
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
		pid := parts[1]
		if pid == "" {
			return ParsedEpisodeKey{}, fmt.Errorf(
				"cell episode key %q: empty process name — the process IS the grain here, so a "+
					"key without one identifies no place and collides with every other such key", key)
		}
		role := ClaimRole(parts[3])
		if role != ClaimRoleConsume && role != ClaimRoleProduce {
			return ParsedEpisodeKey{}, fmt.Errorf(
				"cell episode key %q: unknown role %q — an episode is produce or consume, which are the "+
					"claim's own two roles. %q and %q were the old leg-naming spelling and are retired; "+
					"a key still carrying one predates migration 87", key, parts[3], "supply", "evacuate")
		}
		return ParsedEpisodeKey{
			Kind: EpisodeKindCell, ProcessID: pid, Payload: parts[2], Role: role,
		}, nil
	case "mnt":
		if len(parts) != 3 {
			return ParsedEpisodeKey{}, fmt.Errorf("maintain episode key %q: want 3 parts, got %d", key, len(parts))
		}
		if parts[1] == "" {
			return ParsedEpisodeKey{}, fmt.Errorf(
				"maintain episode key %q: empty group node name — the group IS the place here, so a "+
					"key without one identifies nothing and collides with every other such key", key)
		}
		if parts[2] == "" {
			return ParsedEpisodeKey{}, fmt.Errorf(
				"maintain episode key %q: empty carrier type — a maintained group's demand is per TYPE, "+
					"so a key without one cannot say what the group is short of and would merge every "+
					"declared type into one episode", key)
		}
		return ParsedEpisodeKey{
			Kind: EpisodeKindMaintain, CoreNode: parts[1], BinType: parts[2],
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
